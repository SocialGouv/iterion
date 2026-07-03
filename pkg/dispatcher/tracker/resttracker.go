package tracker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// restClient is the HTTP request/response/error-mapping machinery shared
// by the GitLab and Forgejo adapters — both talk direct REST APIs that
// only differ in base path, auth header, and (Forgejo only) a 409
// sentinel. GitHub instead shells out to `gh` and has no equivalent.
type restClient struct {
	baseURL string // e.g. opts.Host + "/api/v4"
	hc      *http.Client

	// setAuth sets the provider-specific auth header on the request, when
	// a token is configured. May be nil if the adapter never has a token.
	setAuth func(*http.Request)

	// conflictErr, when non-nil, is returned in place of the generic
	// status-code error for a 409 response. GitLab has no such sentinel;
	// Forgejo uses it for "label already exists" races.
	conflictErr error

	// errPrefix names the adapter in wrapped error messages (e.g.
	// "gitlab", "forgejo").
	errPrefix string
}

// do performs an authenticated request against the configured REST API.
// The response body is decoded into out (when non-nil). 404 maps to
// ErrNotFound; 409 maps to conflictErr (if set); other non-2xx statuses
// return a wrapped error with the body excerpt for diagnostics.
func (c *restClient) do(ctx context.Context, method, path string, in any, out any) error {
	var body io.Reader
	if in != nil {
		data, err := json.Marshal(in)
		if err != nil {
			return fmt.Errorf("%s: marshal: %w", c.errPrefix, err)
		}
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return fmt.Errorf("%s: build request: %w", c.errPrefix, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	if c.setAuth != nil {
		c.setAuth(req)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("%s: do: %w", c.errPrefix, err)
	}
	// Drain before close on every return path: the 404/409 cases below
	// return without reading the body, which prevents the keep-alive
	// connection from being reused (the transport can only reuse a
	// fully-drained connection).
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return ErrNotFound
	case resp.StatusCode == http.StatusConflict && c.conflictErr != nil:
		return c.conflictErr
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		if out == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			return nil
		}
		return json.NewDecoder(resp.Body).Decode(out)
	default:
		buf, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("%s: %s %s: status %d: %s", c.errPrefix, method, path, resp.StatusCode, strings.TrimSpace(string(buf)))
	}
}
