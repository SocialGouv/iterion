package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/SocialGouv/claw-code-go/internal/api"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	remoteTriggerTimeout    = 30 * time.Second
	remoteTriggerMaxBodyLen = 8192
)

func RemoteTriggerTool() api.Tool {
	return api.Tool{
		Name:        "remote_trigger",
		Description: "Make an HTTP request to a remote URL.",
		InputSchema: api.InputSchema{
			Type: "object",
			Properties: map[string]api.Property{
				"url":    {Type: "string", Description: "The URL to request."},
				"method": {Type: "string", Description: "HTTP method: GET, POST, PUT, DELETE, PATCH, HEAD."},
				"body":   {Type: "string", Description: "Optional request body."},
			},
			Required: []string{"url"},
		},
	}
}

func ExecuteRemoteTrigger(input map[string]any) (string, error) {
	rawURL, ok := input["url"].(string)
	if !ok || rawURL == "" {
		return "", fmt.Errorf("remote_trigger: 'url' is required")
	}

	method := "GET"
	if m, ok := input["method"].(string); ok && m != "" {
		method = strings.ToUpper(m)
	}

	var bodyReader io.Reader
	if body, ok := input["body"].(string); ok && body != "" {
		bodyReader = strings.NewReader(body)
	}

	ctx, cancel := context.WithTimeout(context.Background(), remoteTriggerTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, method, rawURL, bodyReader)
	if err != nil {
		return "", fmt.Errorf("remote_trigger: invalid request: %v", err)
	}

	// Apply custom headers
	if headers, ok := input["headers"].(map[string]any); ok {
		for k, v := range headers {
			if s, ok := v.(string); ok {
				req.Header.Set(k, s)
			}
		}
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		result := map[string]any{
			"url":     rawURL,
			"method":  method,
			"success": false,
			"error":   err.Error(),
		}
		out, _ := json.MarshalIndent(result, "", "  ")
		return string(out), nil
	}
	defer resp.Body.Close()

	bodyBytes, err := io.ReadAll(io.LimitReader(resp.Body, int64(remoteTriggerMaxBodyLen+1)))
	if err != nil {
		bodyBytes = []byte(fmt.Sprintf("Error reading body: %v", err))
	}

	respBody := string(bodyBytes)
	if len(bodyBytes) > remoteTriggerMaxBodyLen {
		respBody = string(bodyBytes[:remoteTriggerMaxBodyLen]) + "\n... (truncated)"
	}

	success := resp.StatusCode >= 200 && resp.StatusCode < 300

	result := map[string]any{
		"url":         rawURL,
		"method":      method,
		"status_code": resp.StatusCode,
		"body":        respBody,
		"success":     success,
	}
	out, _ := json.MarshalIndent(result, "", "  ")
	return string(out), nil
}
