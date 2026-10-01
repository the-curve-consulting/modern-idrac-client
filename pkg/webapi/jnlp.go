package webapi

import (
	"context"
	"encoding/xml"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ConsoleLaunch holds what the iDRAC's generated viewer.jnlp passes to the
// Avocent viewer: the host/ports and, importantly, the temporary one-time
// console credentials (user/passwd) minted for this launch.
type ConsoleLaunch struct {
	Host        string
	KMPort      int
	VPort       int
	User        string
	Password    string
	APCP        bool
	Version     string
	VMPrivilege bool
	Title       string
	Args        map[string]string // every <argument> key=value as received
	JNLP        []byte
}

type jnlpDoc struct {
	Args []string `xml:"application-desc>argument"`
}

// JNLPURL builds the console launch URL the web UI uses. Generation-specific:
// iDRAC7/8 embed the ST1 token in the parenthesised "file name"; iDRAC6
// uses the same shape without it. host is the address the viewer should
// connect to (normally c.BaseURL's host).
func (c *Client) JNLPURL(host, title string, withST1 bool) string {
	ts := time.Now().UnixMilli()
	name := fmt.Sprintf("%s@0@%s@%d", host, title, ts)
	if withST1 && c.ST1 != "" {
		name += "@ST1=" + c.ST1
	}
	return "/viewer.jnlp(" + name + ")"
}

// ConsoleCredentials logs the console launch in and returns the parsed JNLP.
// It tries the iDRAC7/8 URL form first and falls back to the iDRAC6 form.
func (c *Client) ConsoleCredentials(ctx context.Context, title string) (*ConsoleLaunch, error) {
	host := c.BaseURL
	if u, err := url.Parse(c.BaseURL); err == nil {
		host = u.Hostname()
	}
	if title == "" {
		title = "idrac-go"
	}
	var lastErr error
	for _, path := range []string{c.JNLPURL(host, title, true), c.JNLPURL(host, title, false)} {
		resp, body, err := c.do(ctx, http.MethodGet, path, nil, "", true)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "<jnlp") {
			lastErr = fmt.Errorf("GET %s: HTTP %d (%s)", path, resp.StatusCode, snippet(body))
			if resp.StatusCode == http.StatusFound || resp.StatusCode == http.StatusUnauthorized {
				lastErr = fmt.Errorf("%w (%s)", ErrSessionExpired, path)
			}
			continue
		}
		return ParseJNLP(body)
	}
	return nil, lastErr
}

// ParseJNLP extracts the viewer arguments from a JNLP document.
func ParseJNLP(body []byte) (*ConsoleLaunch, error) {
	var doc jnlpDoc
	dec := xml.NewDecoder(strings.NewReader(string(body)))
	dec.Strict = false
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("jnlp: %w", err)
	}
	l := &ConsoleLaunch{Args: map[string]string{}, JNLP: body, KMPort: 5900, VPort: 5900}
	for _, a := range doc.Args {
		k, v, _ := strings.Cut(strings.TrimSpace(a), "=")
		l.Args[k] = v
		switch k {
		case "ip":
			l.Host = v
		case "kmport":
			fmt.Sscanf(v, "%d", &l.KMPort)
		case "vport":
			fmt.Sscanf(v, "%d", &l.VPort)
		case "user":
			l.User = v
		case "passwd", "password":
			l.Password = v
		case "apcp":
			l.APCP = v == "1" || v == "true"
		case "version":
			l.Version = v
		case "vmprivilege":
			l.VMPrivilege = v == "true" || v == "1"
		case "title":
			l.Title = v
		}
	}
	if l.User == "" || l.Password == "" {
		return l, fmt.Errorf("jnlp: no user/passwd arguments found (keys: %v)", keys(l.Args))
	}
	return l, nil
}

func keys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
