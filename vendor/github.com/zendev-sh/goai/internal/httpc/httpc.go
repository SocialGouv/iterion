// Package httpc provides HTTP helper functions for provider implementations.
package httpc

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

// MustMarshalJSON marshals v to JSON. Panics if marshaling fails,
// which is impossible for the map[string]any values providers construct.
func MustMarshalJSON(v any) []byte {
	data, err := json.Marshal(v)
	if err != nil {
		panic("httpc: json.Marshal failed: " + err.Error())
	}
	return data
}

// MustNewRequest creates an HTTP request. Panics if creation fails,
// which is impossible with valid HTTP method and context.
func MustNewRequest(ctx context.Context, method, url string, body []byte) *http.Request {
	var bodyReader io.Reader
	if body != nil {
		bodyReader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, bodyReader)
	if err != nil {
		panic("httpc: http.NewRequestWithContext failed: " + err.Error())
	}
	return req
}

// ParseDataURL extracts media type and base64 data from a data URL.
// Format: data:<mediaType>;base64,<data>
func ParseDataURL(url string) (mediaType, data string, ok bool) {
	if !strings.HasPrefix(url, "data:") {
		return "", "", false
	}
	rest := url[5:]
	semicolon := strings.Index(rest, ";base64,")
	if semicolon < 0 {
		return "", "", false
	}
	return rest[:semicolon], rest[semicolon+8:], true
}
