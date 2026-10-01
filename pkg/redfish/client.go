// Package redfish is a small, dependency-free Redfish client with the Dell
// iDRAC7/8/9 URIs and OEM actions wired in. Everything is reachable through
// the generic Get/Post/Patch/Delete methods, so anything not wrapped here can
// still be called.
package redfish

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Client talks to one iDRAC.
type Client struct {
	BaseURL  string // https://host[:port]
	Username string
	Password string
	HTTP     *http.Client
	Logger   interface{ Printf(string, ...any) }
	// UseSessions makes Login() create a Redfish session (X-Auth-Token) rather
	// than sending basic auth on every call. Sessions count against the
	// iDRAC's session limit, so basic auth is the default for one-shot CLI use.
	UseSessions bool

	token      string
	sessionURI string
}

// Option configures a Client.
type Option func(*Client)

// WithInsecure disables certificate verification (iDRACs ship self-signed certs).
func WithInsecure(insecure bool) Option {
	return func(c *Client) {
		c.HTTP.Transport.(*http.Transport).TLSClientConfig.InsecureSkipVerify = insecure
	}
}

// WithTimeout sets the per-request timeout.
func WithTimeout(d time.Duration) Option { return func(c *Client) { c.HTTP.Timeout = d } }

// WithLogger enables request logging.
func WithLogger(l interface{ Printf(string, ...any) }) Option {
	return func(c *Client) { c.Logger = l }
}

// WithSessions toggles X-Auth-Token sessions (see Client.UseSessions).
func WithSessions(on bool) Option { return func(c *Client) { c.UseSessions = on } }

// New creates a client for https://host.
func New(baseURL, username, password string, opts ...Option) *Client {
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: true, //nolint:gosec // overridden by WithInsecure(false)
			MinVersion:         tls.VersionTLS10,
			CipherSuites:       legacyCipherSuites(),
		},
		MaxIdleConnsPerHost: 2,
		IdleConnTimeout:     30 * time.Second,
	}
	c := &Client{
		BaseURL:  strings.TrimRight(baseURL, "/"),
		Username: username,
		Password: password,
		HTTP:     &http.Client{Transport: tr, Timeout: 90 * time.Second},
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

// legacyCipherSuites lists every suite Go implements, including the
// RSA-key-exchange/CBC ones that older iDRAC firmware needs; Go 1.27 only
// enables those when they are listed explicitly.
func legacyCipherSuites() []uint16 {
	var ids []uint16
	for _, s := range tls.CipherSuites() {
		ids = append(ids, s.ID)
	}
	for _, s := range tls.InsecureCipherSuites() {
		ids = append(ids, s.ID)
	}
	return ids
}

// Error is a non-2xx Redfish response, with the ExtendedInfo messages flattened.
type Error struct {
	Status   int
	Method   string
	Path     string
	Messages []string
	Body     string
}

func (e *Error) Error() string {
	msg := strings.Join(e.Messages, "; ")
	if msg == "" {
		msg = strings.TrimSpace(e.Body)
		if len(msg) > 300 {
			msg = msg[:300] + "..."
		}
	}
	return fmt.Sprintf("%s %s: HTTP %d: %s", e.Method, e.Path, e.Status, msg)
}

// IsNotFound reports whether err is a 404.
func IsNotFound(err error) bool {
	var e *Error
	return errors.As(err, &e) && e.Status == http.StatusNotFound
}

// Response is what Do returns: status, headers of interest and the raw body.
type Response struct {
	Status   int
	Location string // task/job monitor for 202 responses
	Headers  http.Header
	Body     []byte
}

// JSON decodes the body into v.
func (r *Response) JSON(v any) error {
	if len(bytes.TrimSpace(r.Body)) == 0 {
		return nil
	}
	return json.Unmarshal(r.Body, v)
}

// Login creates a Redfish session when UseSessions is set; otherwise it just
// verifies the credentials with a GET on the service root's Systems collection.
func (c *Client) Login(ctx context.Context) error {
	if !c.UseSessions {
		_, err := c.Do(ctx, http.MethodGet, "/redfish/v1/Systems", nil)
		return err
	}
	// iDRAC8 (Redfish 1.4) exposes the collection at /redfish/v1/Sessions,
	// iDRAC9 at /redfish/v1/SessionService/Sessions; the root's Links tell.
	sessPath := "/redfish/v1/SessionService/Sessions"
	var root struct {
		Links struct {
			Sessions struct {
				ID string `json:"@odata.id"`
			} `json:"Sessions"`
		} `json:"Links"`
	}
	if err := c.Get(ctx, "/redfish/v1/", &root); err == nil && root.Links.Sessions.ID != "" {
		sessPath = root.Links.Sessions.ID
	}
	resp, err := c.Do(ctx, http.MethodPost, sessPath, map[string]string{"UserName": c.Username, "Password": c.Password})
	if err != nil {
		return err
	}
	c.token = resp.Headers.Get("X-Auth-Token")
	c.sessionURI = resp.Headers.Get("Location")
	if c.token == "" {
		return errors.New("session created but no X-Auth-Token returned")
	}
	return nil
}

// Logout deletes the Redfish session, if one was created.
func (c *Client) Logout(ctx context.Context) error {
	if c.token == "" || c.sessionURI == "" {
		return nil
	}
	_, err := c.Do(ctx, http.MethodDelete, c.sessionURI, nil)
	c.token, c.sessionURI = "", ""
	return err
}

// Do performs one request. path may be absolute (https://...) or start with /.
// body is JSON-encoded unless it is nil, []byte or io.Reader.
func (c *Client) Do(ctx context.Context, method, path string, body any) (*Response, error) {
	u := path
	if !strings.HasPrefix(path, "http") {
		if !strings.HasPrefix(path, "/") {
			path = "/" + path
		}
		u = c.BaseURL + path
	} else {
		// Location headers may point at the iDRAC by a different name; keep our host.
		pu, err := url.Parse(path)
		if err == nil {
			u = c.BaseURL + pu.RequestURI()
			path = pu.RequestURI()
		}
	}
	var rd io.Reader
	switch b := body.(type) {
	case nil:
	case []byte:
		rd = bytes.NewReader(b)
	case io.Reader:
		rd = b
	default:
		j, err := json.Marshal(b)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(j)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("OData-Version", "4.0")
	if rd != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("X-Auth-Token", c.token)
	} else if c.Username != "" && !(method == http.MethodPost && strings.HasSuffix(path, "/Sessions")) {
		req.SetBasicAuth(c.Username, c.Password)
	}
	if c.Logger != nil {
		c.Logger.Printf("redfish %s %s", method, path)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, fmt.Errorf("%s %s: reading body: %w", method, path, err)
	}
	if c.Logger != nil {
		c.Logger.Printf("redfish %s %s -> %d (%d bytes)", method, path, resp.StatusCode, len(data))
	}
	out := &Response{Status: resp.StatusCode, Location: resp.Header.Get("Location"), Headers: resp.Header, Body: data}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return out, parseError(method, path, resp.StatusCode, data)
	}
	return out, nil
}

func parseError(method, path string, status int, body []byte) error {
	e := &Error{Status: status, Method: method, Path: path, Body: string(body)}
	var env struct {
		Error struct {
			Message      string `json:"message"`
			ExtendedInfo []struct {
				Message    string `json:"Message"`
				MessageID  string `json:"MessageId"`
				Resolution string `json:"Resolution"`
			} `json:"@Message.ExtendedInfo"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &env) == nil {
		for _, m := range env.Error.ExtendedInfo {
			s := m.Message
			if m.MessageID != "" {
				s = m.MessageID + ": " + s
			}
			if m.Resolution != "" && !strings.EqualFold(m.Resolution, "None") {
				s += " (" + m.Resolution + ")"
			}
			e.Messages = append(e.Messages, s)
		}
		if len(e.Messages) == 0 && env.Error.Message != "" {
			e.Messages = []string{env.Error.Message}
		}
	}
	return e
}

// Get fetches path and decodes it into v (pass nil to discard).
func (c *Client) Get(ctx context.Context, path string, v any) error {
	resp, err := c.Do(ctx, http.MethodGet, path, nil)
	if err != nil {
		return err
	}
	if v == nil {
		return nil
	}
	return resp.JSON(v)
}

// GetRaw fetches path and returns the JSON body.
func (c *Client) GetRaw(ctx context.Context, path string) (json.RawMessage, error) {
	resp, err := c.Do(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

// Post sends a JSON body.
func (c *Client) Post(ctx context.Context, path string, body any) (*Response, error) {
	return c.Do(ctx, http.MethodPost, path, body)
}

// Patch sends a JSON body.
func (c *Client) Patch(ctx context.Context, path string, body any) (*Response, error) {
	return c.Do(ctx, http.MethodPatch, path, body)
}

// Delete removes a resource.
func (c *Client) Delete(ctx context.Context, path string) (*Response, error) {
	return c.Do(ctx, http.MethodDelete, path, nil)
}

// Object is a generic decoded Redfish resource.
type Object map[string]any

// Str returns a nested string value ("Status.Health" style path), or "".
func (o Object) Str(path string) string {
	v := o.lookup(path)
	switch t := v.(type) {
	case string:
		return t
	case nil:
		return ""
	case float64:
		if t == float64(int64(t)) {
			return fmt.Sprintf("%d", int64(t))
		}
		return fmt.Sprintf("%g", t)
	case bool:
		if t {
			return "true"
		}
		return "false"
	}
	b, _ := json.Marshal(v)
	return string(b)
}

// Float returns a nested numeric value.
func (o Object) Float(path string) (float64, bool) {
	f, ok := o.lookup(path).(float64)
	return f, ok
}

// Obj returns a nested object.
func (o Object) Obj(path string) Object {
	m, _ := o.lookup(path).(map[string]any)
	return Object(m)
}

// List returns a nested array of objects.
func (o Object) List(path string) []Object {
	arr, _ := o.lookup(path).([]any)
	out := make([]Object, 0, len(arr))
	for _, e := range arr {
		if m, ok := e.(map[string]any); ok {
			out = append(out, Object(m))
		}
	}
	return out
}

// ID returns @odata.id.
func (o Object) ID() string { return o.Str("@odata.id") }

func (o Object) lookup(path string) any {
	// Exact key first: Redfish keys such as "@odata.type" contain dots.
	if v, ok := o[path]; ok {
		return v
	}
	var cur any = map[string]any(o)
	for _, p := range strings.Split(path, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur, ok = m[p]
		if !ok {
			return nil
		}
	}
	return cur
}

// GetObject fetches a resource as an Object.
func (c *Client) GetObject(ctx context.Context, path string) (Object, error) {
	var o Object
	if err := c.Get(ctx, path, &o); err != nil {
		return nil, err
	}
	return o, nil
}

// Members lists the @odata.id of every member of a collection, following
// Members@odata.nextLink pages.
func (c *Client) Members(ctx context.Context, path string) ([]string, error) {
	var ids []string
	for path != "" {
		var col struct {
			Members []struct {
				ID string `json:"@odata.id"`
			} `json:"Members"`
			Next string `json:"Members@odata.nextLink"`
		}
		if err := c.Get(ctx, path, &col); err != nil {
			return ids, err
		}
		for _, m := range col.Members {
			ids = append(ids, m.ID)
		}
		path = col.Next
	}
	return ids, nil
}

// MemberObjects fetches every member of a collection.
func (c *Client) MemberObjects(ctx context.Context, path string) ([]Object, error) {
	ids, err := c.Members(ctx, path)
	if err != nil {
		return nil, err
	}
	out := make([]Object, 0, len(ids))
	for _, id := range ids {
		o, err := c.GetObject(ctx, id)
		if err != nil {
			return out, err
		}
		out = append(out, o)
	}
	return out, nil
}
