// Package webapi drives the legacy iDRAC web-UI API shared by iDRAC6, 7 and 8:
// form login on /data/login (cookie + ST1/ST2 anti-CSRF tokens), the XML
// key/value endpoints /data?get= and /data?set=, the /sysmgmt JSON endpoints
// (iDRAC7/8), and the viewer.jnlp download that yields the one-time
// credentials the virtual console uses on port 5900.
//
// On iDRAC6 this is the only HTTP API; on iDRAC7/8 Redfish is preferred and
// this package is mainly used to launch the console.
package webapi

import (
	"context"
	"crypto/tls"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Client is one authenticated web-UI session.
type Client struct {
	BaseURL  string
	Username string
	Password string
	HTTP     *http.Client
	Logger   interface{ Printf(string, ...any) }

	// ST1 and ST2 are the anti-CSRF tokens issued at login. ST2 is sent as a
	// request header on every call; ST1 is embedded in the console JNLP URL.
	ST1, ST2 string
	// ForwardURL is what the login response asked the browser to load.
	ForwardURL string
}

// New creates a client for https://host with a fresh cookie jar and a TLS
// configuration lax enough for iDRAC6 (TLS 1.0, RSA key exchange, 1024-bit
// self-signed certificate).
func New(baseURL, username, password string, insecure bool, timeout time.Duration) *Client {
	jar, _ := cookiejar.New(nil)
	var suites []uint16
	for _, s := range tls.CipherSuites() {
		suites = append(suites, s.ID)
	}
	for _, s := range tls.InsecureCipherSuites() {
		suites = append(suites, s.ID)
	}
	tr := &http.Transport{
		TLSClientConfig:     &tls.Config{InsecureSkipVerify: insecure, MinVersion: tls.VersionTLS10, CipherSuites: suites}, //nolint:gosec
		MaxIdleConnsPerHost: 1,
		DisableCompression:  false,
	}
	if timeout == 0 {
		timeout = 60 * time.Second
	}
	return &Client{
		BaseURL:  strings.TrimRight(baseURL, "/"),
		Username: username,
		Password: password,
		HTTP: &http.Client{Transport: tr, Jar: jar, Timeout: timeout, CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse // the API signals with 302s; never follow
		}},
	}
}

// LoginError carries the iDRAC's authResult code.
type LoginError struct {
	AuthResult   int
	BlockingTime int // seconds the iDRAC wants the client to wait before retrying
	Message      string
}

func (e *LoginError) Error() string {
	desc := map[int]string{1: "invalid credentials", 2: "missing username", 3: "missing password", 4: "insufficient privilege", 5: "session limit reached", 99: "login failed"}[e.AuthResult]
	if desc == "" {
		desc = fmt.Sprintf("authResult %d", e.AuthResult)
	}
	if e.BlockingTime > 0 {
		desc += fmt.Sprintf(" (blocked for %ds)", e.BlockingTime)
	}
	if e.Message != "" {
		desc += ": " + e.Message
	}
	return "idrac login: " + desc
}

type loginResponse struct {
	Status       string `xml:"status"`
	AuthResult   int    `xml:"authResult"`
	BlockingTime int    `xml:"blockingTime"`
	ForwardURL   string `xml:"forwardUrl"`
	ErrorMsg     string `xml:"errorMsg"`
	ST1          string `xml:"ST1"`
	ST2          string `xml:"ST2"`
}

var tokenRe = regexp.MustCompile(`ST([12])=([A-Za-z0-9+/=_-]+)`)

// Login authenticates. It makes exactly one attempt: iDRACs count failures
// and lock the account, so callers must not retry on *LoginError.
func (c *Client) Login(ctx context.Context) error {
	form := url.Values{"user": {c.Username}, "password": {c.Password}}
	resp, body, err := c.do(ctx, http.MethodPost, "/data/login", strings.NewReader(form.Encode()), "application/x-www-form-urlencoded", false)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("idrac login: HTTP %d", resp.StatusCode)
	}
	var lr loginResponse
	if err := xml.Unmarshal(body, &lr); err != nil {
		return fmt.Errorf("idrac login: unexpected response: %w (%s)", err, snippet(body))
	}
	if lr.AuthResult != 0 {
		return &LoginError{AuthResult: lr.AuthResult, BlockingTime: lr.BlockingTime, Message: lr.ErrorMsg}
	}
	c.ForwardURL = lr.ForwardURL
	c.ST1, c.ST2 = lr.ST1, lr.ST2
	for _, m := range tokenRe.FindAllStringSubmatch(lr.ForwardURL+" "+string(body), -1) {
		if m[1] == "1" && c.ST1 == "" {
			c.ST1 = m[2]
		}
		if m[1] == "2" && c.ST2 == "" {
			c.ST2 = m[2]
		}
	}
	if c.ST2 == "" {
		// iDRAC6 firmware before 1.9x did not use tokens at all; older 2.x
		// builds return them on index.html. Try to pick them up from there.
		if _, idx, err := c.do(ctx, http.MethodGet, "/index.html", nil, "", true); err == nil {
			for _, m := range tokenRe.FindAllSubmatch(idx, -1) {
				if string(m[1]) == "1" && c.ST1 == "" {
					c.ST1 = string(m[2])
				}
				if string(m[1]) == "2" && c.ST2 == "" {
					c.ST2 = string(m[2])
				}
			}
		}
	}
	if c.Logger != nil {
		c.Logger.Printf("webapi: logged in to %s (forward %q, ST1 %v, ST2 %v)", c.BaseURL, lr.ForwardURL, c.ST1 != "", c.ST2 != "")
	}
	return nil
}

// Logout ends the session (GET /data/logout).
func (c *Client) Logout(ctx context.Context) error {
	_, _, err := c.do(ctx, http.MethodGet, "/data/logout", nil, "", true)
	return err
}

// Get reads one or more /data?get= keys and returns them as a map of raw
// inner XML per key (values may themselves be XML fragments).
func (c *Client) Get(ctx context.Context, keys ...string) (map[string]string, []byte, error) {
	path := "/data?get=" + strings.Join(keys, ",")
	resp, body, err := c.do(ctx, http.MethodPost, path, nil, "application/x-www-form-urlencoded", true)
	if err != nil {
		return nil, nil, err
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusFound {
		return nil, body, ErrSessionExpired
	}
	if resp.StatusCode != http.StatusOK {
		return nil, body, fmt.Errorf("GET %s: HTTP %d: %s", path, resp.StatusCode, snippet(body))
	}
	m, err := parseRoot(body)
	if err != nil {
		return nil, body, fmt.Errorf("%s: %w", path, err)
	}
	if st, ok := m["status"]; ok && st != "ok" {
		return m, body, fmt.Errorf("%s: status %q: %s", path, st, m["message"])
	}
	return m, body, nil
}

// ErrSessionExpired is returned when the iDRAC answers 401/302 to an API call.
var ErrSessionExpired = errors.New("idrac session expired or not logged in")

// Set posts key:value pairs to /data?set=. The iDRAC6 UI sends the pairs in
// the query string, the iDRAC7/8 UI in the body; both accept either.
func (c *Client) Set(ctx context.Context, pairs map[string]string) (map[string]string, error) {
	var parts []string
	for k, v := range pairs {
		parts = append(parts, k+":"+encodeSetValue(v))
	}
	body := strings.Join(parts, ",")
	path := "/data?set=" + body
	resp, out, err := c.do(ctx, http.MethodPost, path, strings.NewReader(body), "application/x-www-form-urlencoded", true)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusFound {
		return nil, ErrSessionExpired
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("SET %s: HTTP %d: %s", body, resp.StatusCode, snippet(out))
	}
	m, err := parseRoot(out)
	if err != nil {
		return nil, err
	}
	if st := m["status"]; st != "ok" && st != "" {
		return m, fmt.Errorf("set %s: status %q: %s", body, st, m["message"])
	}
	return m, nil
}

// encodeSetValue mirrors the UI's encodeSetValue(): the separators used by the
// key:value,key:value grammar are percent-encoded.
func encodeSetValue(v string) string {
	r := strings.NewReplacer("%", "%25", ",", "%2C", ":", "%3A", "&", "%26", "#", "%23", "+", "%2B", "=", "%3D", "?", "%3F", " ", "%20")
	return r.Replace(v)
}

// GetJSON fetches a /sysmgmt or /session endpoint (iDRAC7/8) as raw JSON.
func (c *Client) GetJSON(ctx context.Context, path string) ([]byte, error) {
	resp, body, err := c.do(ctx, http.MethodGet, path, nil, "", true)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusFound {
		return body, ErrSessionExpired
	}
	if resp.StatusCode != http.StatusOK {
		return body, fmt.Errorf("GET %s: HTTP %d: %s", path, resp.StatusCode, snippet(body))
	}
	return body, nil
}

// Raw performs an arbitrary authenticated request (for `idrac web ...`).
func (c *Client) Raw(ctx context.Context, method, path string, body string) (int, []byte, error) {
	var rd io.Reader
	ct := ""
	if body != "" {
		rd = strings.NewReader(body)
		ct = "application/x-www-form-urlencoded"
	}
	resp, out, err := c.do(ctx, method, path, rd, ct, true)
	if err != nil {
		return 0, nil, err
	}
	return resp.StatusCode, out, nil
}

func (c *Client) do(ctx context.Context, method, path string, body io.Reader, contentType string, withToken bool) (*http.Response, []byte, error) {
	u := c.BaseURL + path
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return nil, nil, err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if withToken && len(c.ST2) >= 8 {
		req.Header.Set("ST2", c.ST2)
	}
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Accept-Encoding", "gzip") // iDRAC8 serves some assets only gzipped
	req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64) idrac-go")
	if c.Logger != nil {
		c.Logger.Printf("webapi %s %s", method, path)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	var rd io.Reader = resp.Body
	if strings.EqualFold(resp.Header.Get("Content-Encoding"), "gzip") {
		gz, err := gzipReader(resp.Body)
		if err != nil {
			return nil, nil, err
		}
		defer gz.Close()
		rd = gz
	}
	data, err := io.ReadAll(io.LimitReader(rd, 32<<20))
	if err != nil {
		return nil, nil, fmt.Errorf("%s %s: %w", method, path, err)
	}
	if c.Logger != nil {
		c.Logger.Printf("webapi %s %s -> %d (%d bytes)", method, path, resp.StatusCode, len(data))
	}
	return resp, data, nil
}

// parseRoot flattens <root><k>v</k>...</root> into a map. Values keep their
// inner XML so callers can parse nested structures (e.g. SEL entries).
func parseRoot(body []byte) (map[string]string, error) {
	dec := xml.NewDecoder(strings.NewReader(string(body)))
	dec.Strict = false
	m := map[string]string{}
	depth := 0
	var key string
	var buf strings.Builder
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return m, fmt.Errorf("xml: %w (%s)", err, snippet(body))
		}
		switch t := tok.(type) {
		case xml.StartElement:
			depth++
			if depth == 2 {
				key = t.Name.Local
				buf.Reset()
			} else if depth > 2 {
				buf.WriteString("<" + t.Name.Local)
				for _, a := range t.Attr {
					buf.WriteString(fmt.Sprintf(" %s=%q", a.Name.Local, a.Value))
				}
				buf.WriteString(">")
			}
		case xml.EndElement:
			if depth == 2 {
				if _, dup := m[key]; !dup {
					m[key] = strings.TrimSpace(buf.String())
				}
			} else if depth > 2 {
				buf.WriteString("</" + t.Name.Local + ">")
			}
			depth--
		case xml.CharData:
			if depth >= 2 {
				buf.Write(t)
			}
		}
	}
	return m, nil
}

func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 200 {
		s = s[:200] + "..."
	}
	return s
}

// Int parses a numeric value from a Get result.
func Int(m map[string]string, key string) int {
	n, _ := strconv.Atoi(strings.TrimSpace(m[key]))
	return n
}
