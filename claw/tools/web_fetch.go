package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/zendev-sh/goai"
)

const (
	defaultMaxBytes = 50 * 1024 // 50 KB
	webFetchTimeout = 15 * time.Second
)

var (
	reHTMLTag       = regexp.MustCompile(`<[^>]+>`)
	reWhitespace    = regexp.MustCompile(`[ \t]+`)
	reBlankLines    = regexp.MustCompile(`\n{3,}`)
	reScriptStyle   = regexp.MustCompile(`(?is)<(script|style)[^>]*>.*?</(script|style)>`)
	reBlockElements = regexp.MustCompile(`(?i)<(br|p|div|h[1-6]|li|tr|blockquote)[^>]*>`)
)

var webFetchSchema = json.RawMessage(`{
	"type": "object",
	"properties": {
		"url": {
			"type": "string",
			"description": "The URL to fetch"
		},
		"max_bytes": {
			"type": "integer",
			"description": "Maximum bytes to read (default 51200)"
		}
	},
	"required": ["url"]
}`)

// WebFetch returns a goai.Tool that fetches a URL and returns its text content.
func WebFetch() goai.Tool {
	return goai.Tool{
		Name:        "web_fetch",
		Description: "Fetch a URL and return its text content. HTML tags are stripped for readability.",
		InputSchema: webFetchSchema,
		Execute: func(ctx context.Context, input json.RawMessage) (string, error) {
			var params struct {
				URL      string `json:"url"`
				MaxBytes int    `json:"max_bytes"`
			}
			if err := json.Unmarshal(input, &params); err != nil {
				return "", fmt.Errorf("web_fetch: parse input: %w", err)
			}
			if params.URL == "" {
				return "", fmt.Errorf("web_fetch: 'url' is required")
			}

			maxBytes := defaultMaxBytes
			if params.MaxBytes > 0 {
				maxBytes = params.MaxBytes
			}

			client := &http.Client{Timeout: webFetchTimeout}
			req, err := http.NewRequestWithContext(ctx, "GET", params.URL, nil)
			if err != nil {
				return "", fmt.Errorf("web_fetch: build request: %w", err)
			}
			req.Header.Set("User-Agent", "claw/0.1 (text fetcher)")

			resp, err := client.Do(req)
			if err != nil {
				return "", fmt.Errorf("web_fetch: %w", err)
			}
			defer resp.Body.Close()

			if resp.StatusCode >= 400 {
				return "", fmt.Errorf("web_fetch: HTTP %d for %s", resp.StatusCode, params.URL)
			}

			body, err := io.ReadAll(io.LimitReader(resp.Body, int64(maxBytes)))
			if err != nil {
				return "", fmt.Errorf("web_fetch: read body: %w", err)
			}

			text := stripHTML(string(body))
			return fmt.Sprintf("URL: %s\n\n%s", params.URL, text), nil
		},
	}
}

func stripHTML(s string) string {
	s = reScriptStyle.ReplaceAllString(s, "")
	s = reBlockElements.ReplaceAllLiteralString(s, "\n")
	s = reHTMLTag.ReplaceAllString(s, "")
	s = strings.NewReplacer(
		"&amp;", "&", "&lt;", "<", "&gt;", ">",
		"&quot;", `"`, "&#39;", "'", "&nbsp;", " ",
	).Replace(s)
	lines := strings.Split(s, "\n")
	var cleaned []string
	for _, line := range lines {
		line = reWhitespace.ReplaceAllString(line, " ")
		line = strings.TrimSpace(line)
		cleaned = append(cleaned, line)
	}
	result := strings.Join(cleaned, "\n")
	result = reBlankLines.ReplaceAllString(result, "\n\n")
	return strings.TrimSpace(result)
}
