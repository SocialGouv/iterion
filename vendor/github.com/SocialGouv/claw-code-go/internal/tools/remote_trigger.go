package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/SocialGouv/claw-code-go/internal/api"
)

const (
	remoteTriggerDefaultTimeout = 30 * time.Second
	remoteTriggerMaxTimeout     = 5 * time.Minute
	remoteTriggerDefaultMaxBody = 1 << 20 // 1 MiB
	remoteTriggerHardMaxBody    = 8 << 20 // 8 MiB ceiling for caller override
)

// remoteTriggerInputBlocklist is the set of headers a tool input must not
// supply. Authorization is allowed only when AllowAuthHeader is set in
// RemoteTriggerOptions; everything else is unconditionally rejected.
var remoteTriggerInputBlocklist = map[string]struct{}{
	"cookie":              {},
	"set-cookie":          {},
	"proxy-authorization": {},
	"x-forwarded-for":     {},
}

// remoteTriggerResponseHeaderAllowlist is the set of response headers we
// surface to the model. Anything else is dropped to keep auth material and
// tracking artifacts out of the agent context window.
var remoteTriggerResponseHeaderAllowlist = map[string]struct{}{
	"content-type":     {},
	"content-length":   {},
	"content-encoding": {},
	"content-language": {},
	"etag":             {},
	"last-modified":    {},
	"location":         {},
	"retry-after":      {},
	"x-request-id":     {},
}

// RemoteTriggerOptions tunes the runtime defence-in-depth knobs without
// exposing them through the LLM-facing tool schema.
type RemoteTriggerOptions struct {
	HTTPClient      *http.Client // nil => internal default with per-request timeout
	MaxBodyBytes    int          // <=0 => remoteTriggerDefaultMaxBody
	AllowAuthHeader bool         // permit caller to set Authorization in input
	URLValidator    func(*url.URL) error
}

func RemoteTriggerTool() api.Tool {
	return api.Tool{
		Name:        "remote_trigger",
		Description: "Make an HTTP request to a remote endpoint. Returns status_code, body (truncated), filtered headers, and duration_ms. Cookie / Proxy-Authorization headers are rejected.",
		InputSchema: api.InputSchema{
			Type: "object",
			Properties: map[string]api.Property{
				"url":             {Type: "string", Description: "Absolute http(s) URL to call."},
				"method":          {Type: "string", Description: "HTTP method (default POST)."},
				"headers":         {Type: "object", Description: "Optional request headers. Cookie/Proxy-Authorization are rejected; Authorization is rejected unless explicitly allowed by the host."},
				"body":            {Type: "string", Description: "Optional request body (string)."},
				"json":            {Type: "object", Description: "Optional JSON body. When set, Content-Type is forced to application/json. Mutually exclusive with `body`."},
				"timeout_seconds": {Type: "integer", Description: "Per-request timeout in seconds (default 30, max 300)."},
			},
			Required: []string{"url"},
		},
	}
}

// ExecuteRemoteTrigger runs a remote_trigger invocation with the default
// runtime options. ctx governs timeout/cancellation.
func ExecuteRemoteTrigger(ctx context.Context, input map[string]any) (string, error) {
	return ExecuteRemoteTriggerWith(ctx, input, RemoteTriggerOptions{})
}

// ExecuteRemoteTriggerWith is the variant that accepts custom options for
// tests or hosts that need to relax / tighten the defaults.
func ExecuteRemoteTriggerWith(ctx context.Context, input map[string]any, opts RemoteTriggerOptions) (string, error) {
	rawURL, ok := input["url"].(string)
	if !ok || strings.TrimSpace(rawURL) == "" {
		return "", fmt.Errorf("remote_trigger: 'url' is required")
	}
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return "", fmt.Errorf("remote_trigger: invalid url: %w", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", fmt.Errorf("remote_trigger: scheme %q not allowed (need http or https)", parsed.Scheme)
	}
	if opts.URLValidator != nil {
		if err := opts.URLValidator(parsed); err != nil {
			return "", fmt.Errorf("remote_trigger: %w", err)
		}
	}

	method := "POST"
	if m, ok := input["method"].(string); ok && strings.TrimSpace(m) != "" {
		method = strings.ToUpper(strings.TrimSpace(m))
	}
	switch method {
	case http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch,
		http.MethodDelete, http.MethodHead, http.MethodOptions:
	default:
		return "", fmt.Errorf("remote_trigger: unsupported method %q", method)
	}

	timeout := remoteTriggerDefaultTimeout
	if v, ok := input["timeout_seconds"]; ok {
		secs, err := toPositiveInt(v)
		if err != nil {
			return "", fmt.Errorf("remote_trigger: timeout_seconds: %w", err)
		}
		timeout = time.Duration(secs) * time.Second
		if timeout > remoteTriggerMaxTimeout {
			timeout = remoteTriggerMaxTimeout
		}
	}

	maxBody := opts.MaxBodyBytes
	if maxBody <= 0 {
		maxBody = remoteTriggerDefaultMaxBody
	}
	if maxBody > remoteTriggerHardMaxBody {
		maxBody = remoteTriggerHardMaxBody
	}

	bodyReader, contentType, err := buildBody(input)
	if err != nil {
		return "", err
	}

	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, method, rawURL, bodyReader)
	if err != nil {
		return "", fmt.Errorf("remote_trigger: invalid request: %w", err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}

	if err := applyHeaders(req, input["headers"], opts.AllowAuthHeader); err != nil {
		return "", err
	}

	client := opts.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: timeout}
	}

	start := time.Now()
	resp, err := client.Do(req)
	duration := time.Since(start)
	if err != nil {
		return marshalErrorResult(rawURL, method, duration, err), nil
	}
	defer resp.Body.Close()

	bodyBytes, truncated, readErr := readLimited(resp.Body, maxBody)
	if readErr != nil && !errors.Is(readErr, io.EOF) {
		return "", fmt.Errorf("remote_trigger: read body: %w", readErr)
	}

	result := map[string]any{
		"url":         rawURL,
		"method":      method,
		"status_code": resp.StatusCode,
		"success":     resp.StatusCode >= 200 && resp.StatusCode < 300,
		"body":        string(bodyBytes),
		"truncated":   truncated,
		"headers":     filterResponseHeaders(resp.Header),
		"duration_ms": duration.Milliseconds(),
	}
	out, _ := json.MarshalIndent(result, "", "  ")
	return string(out), nil
}

func buildBody(input map[string]any) (io.Reader, string, error) {
	bodyStr, hasBody := input["body"].(string)
	jsonBody, hasJSON := input["json"]

	if hasJSON && jsonBody != nil {
		if hasBody && bodyStr != "" {
			return nil, "", fmt.Errorf("remote_trigger: 'body' and 'json' are mutually exclusive")
		}
		raw, err := json.Marshal(jsonBody)
		if err != nil {
			return nil, "", fmt.Errorf("remote_trigger: encoding json body: %w", err)
		}
		return strings.NewReader(string(raw)), "application/json", nil
	}
	if hasBody && bodyStr != "" {
		return strings.NewReader(bodyStr), "", nil
	}
	return nil, "", nil
}

func applyHeaders(req *http.Request, raw any, allowAuth bool) error {
	if raw == nil {
		return nil
	}
	headers, ok := raw.(map[string]any)
	if !ok {
		return fmt.Errorf("remote_trigger: 'headers' must be an object")
	}
	for k, v := range headers {
		key := strings.TrimSpace(k)
		if key == "" {
			continue
		}
		lower := strings.ToLower(key)
		if _, denied := remoteTriggerInputBlocklist[lower]; denied {
			return fmt.Errorf("remote_trigger: header %q is not allowed", key)
		}
		if lower == "authorization" && !allowAuth {
			return fmt.Errorf("remote_trigger: header %q is not allowed in this context", key)
		}
		val, ok := v.(string)
		if !ok {
			return fmt.Errorf("remote_trigger: header %q must be a string", key)
		}
		if strings.ContainsAny(val, "\r\n") {
			return fmt.Errorf("remote_trigger: header %q contains illegal CR/LF", key)
		}
		req.Header.Set(key, val)
	}
	return nil
}

func filterResponseHeaders(h http.Header) map[string]string {
	out := make(map[string]string, len(h))
	for k, vals := range h {
		if _, allow := remoteTriggerResponseHeaderAllowlist[strings.ToLower(k)]; !allow {
			continue
		}
		if len(vals) > 0 {
			out[k] = vals[0]
		}
	}
	return out
}

func readLimited(r io.Reader, max int) ([]byte, bool, error) {
	limited := io.LimitReader(r, int64(max)+1)
	buf, err := io.ReadAll(limited)
	if err != nil {
		return buf, false, err
	}
	if len(buf) > max {
		return buf[:max], true, nil
	}
	return buf, false, nil
}

func toPositiveInt(v any) (int, error) {
	switch n := v.(type) {
	case int:
		if n <= 0 {
			return 0, fmt.Errorf("must be > 0")
		}
		return n, nil
	case int64:
		if n <= 0 {
			return 0, fmt.Errorf("must be > 0")
		}
		return int(n), nil
	case float64:
		i := int(n)
		if i <= 0 || float64(i) != n {
			return 0, fmt.Errorf("must be a positive integer")
		}
		return i, nil
	default:
		return 0, fmt.Errorf("must be an integer, got %T", v)
	}
}

func marshalErrorResult(url, method string, dur time.Duration, err error) string {
	result := map[string]any{
		"url":         url,
		"method":      method,
		"success":     false,
		"error":       err.Error(),
		"duration_ms": dur.Milliseconds(),
	}
	out, _ := json.MarshalIndent(result, "", "  ")
	return string(out)
}
