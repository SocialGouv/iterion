package forge

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// RedirectError refuses a redirected write. The first request may already
// have been applied: this is not evidence that the forge rejected the write.
// Location names the target without URL credentials, query or fragment.
type RedirectError struct {
	Method   string
	Code     int
	Location string
}

func (e *RedirectError) Error() string {
	return fmt.Sprintf("forge: %s returned HTTP %d redirect to %s; write not followed — correct the forge base URL; the original write may have applied", e.Method, e.Code, e.Location)
}

// DoHTTP preserves the caller's client settings and read redirect policy, but
// refuses to follow write redirects before net/http rewrites the method or
// parses Location. The shared client and transport are never mutated.
func DoHTTP(client *http.Client, req *http.Request, sensitive ...string) (*http.Response, error) {
	if client == nil {
		client = http.DefaultClient
	}
	copy := *client
	transport := copy.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	copy.Transport = writeRedirectTransport{base: transport}
	resp, err := copy.Do(req)
	var redirect *RedirectError
	if errors.As(err, &redirect) {
		for _, value := range sensitive {
			redirect.Location = scrubSecretFromLine(redirect.Location, value)
		}
		for _, header := range []string{"Authorization", "Private-Token", "X-Api-Key"} {
			value := req.Header.Get(header)
			redirect.Location = scrubSecretFromLine(redirect.Location, value)
			if fields := strings.Fields(value); len(fields) == 2 {
				redirect.Location = scrubSecretFromLine(redirect.Location, fields[1])
			}
		}
		// net/http wraps transport errors with the request URL, which can
		// contain an OAuth exchange code. The typed refusal needs no URL copy.
		return nil, redirect
	}
	return resp, err
}

// OAuthRedirectSecrets inventories credential-bearing form fields at the shared
// exchange/refresh boundary, including fields a provider adds in the future.
func OAuthRedirectSecrets(values url.Values) []string {
	var out []string
	for key, values := range values {
		if key == "grant_type" || key == "redirect_uri" || key == "client_id" || key == "scope" {
			continue
		}
		out = append(out, values...)
	}
	return out
}

type writeRedirectTransport struct{ base http.RoundTripper }

// CancelRequest preserves http.Client.Timeout for injected legacy transports.
func (t writeRedirectTransport) CancelRequest(req *http.Request) {
	if canceler, ok := t.base.(interface{ CancelRequest(*http.Request) }); ok {
		canceler.CancelRequest(req)
	}
}

func (t writeRedirectTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(req)
	if err != nil || resp == nil || req.Method == http.MethodGet || req.Method == http.MethodHead || req.Method == "" {
		return resp, err
	}
	switch resp.StatusCode {
	case 301, 302, 303, 307, 308:
		if resp.Body != nil {
			_ = resp.Body.Close()
		}
		return nil, &RedirectError{Method: req.Method, Code: resp.StatusCode, Location: safeRedirectLocation(resp.Header.Get("Location"))}
	}
	return resp, nil
}

func safeRedirectLocation(raw string) string {
	if raw == "" {
		return "(missing Location)"
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "(invalid Location)"
	}
	u.User, u.RawQuery, u.Fragment, u.RawFragment, u.RawPath = nil, "", "", "", ""
	u.ForceQuery = false
	// A decoded path lets the hook caller scrub an echoed secret even when
	// the forge percent-encodes its bytes. Do not reintroduce URL escaping.
	base := ""
	if u.Host != "" {
		base = "//" + u.Host
		if u.Scheme != "" {
			base = u.Scheme + ":" + base
		}
	} else if u.Scheme != "" || u.Opaque != "" {
		return "(non-HTTP Location)"
	}
	return strings.Map(func(r rune) rune {
		if r < 32 || r == 127 {
			return -1
		}
		return r
	}, base+u.Path)
}
