package tools

import (
	"fmt"
	"github.com/SocialGouv/claw-code-go/internal/api"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
)

const (
	defaultMaxBytes = 50 * 1024 // 50 KB
	webFetchTimeout = 15 * time.Second
)

var (
	reHTMLTag    = regexp.MustCompile(`<[^>]+>`)
	reWhitespace = regexp.MustCompile(`[ \t]+`)
	reBlankLines = regexp.MustCompile(`\n{3,}`)
)

// WebFetchTool returns the tool definition for fetching URLs.
func WebFetchTool() api.Tool {
	return api.Tool{
		Name:        "web_fetch",
		Description: "Fetch a URL and return its text content. HTML tags are stripped for readability.",
		InputSchema: api.InputSchema{
			Type: "object",
			Properties: map[string]api.Property{
				"url": {
					Type:        "string",
					Description: "The URL to fetch",
				},
				"max_bytes": {
					Type:        "integer",
					Description: fmt.Sprintf("Maximum bytes to read (default %d)", defaultMaxBytes),
				},
			},
			Required: []string{"url"},
		},
	}
}

// ExecuteWebFetch fetches a URL and returns its text content.
func ExecuteWebFetch(input map[string]any) (string, error) {
	url, ok := input["url"].(string)
	if !ok || url == "" {
		return "", fmt.Errorf("web_fetch: 'url' is required")
	}

	maxBytes := defaultMaxBytes
	if v, ok := input["max_bytes"]; ok {
		switch n := v.(type) {
		case float64:
			maxBytes = int(n)
		case int:
			maxBytes = n
		}
	}

	client := &http.Client{Timeout: webFetchTimeout}
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return "", fmt.Errorf("web_fetch: build request: %w", err)
	}
	req.Header.Set("User-Agent", "claw-code/0.1 (text fetcher)")

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("web_fetch: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("web_fetch: HTTP %d for %s", resp.StatusCode, url)
	}

	limited := io.LimitReader(resp.Body, int64(maxBytes))
	body, err := io.ReadAll(limited)
	if err != nil {
		return "", fmt.Errorf("web_fetch: read body: %w", err)
	}

	text := stripHTML(string(body))
	return fmt.Sprintf("URL: %s\n\n%s", url, text), nil
}

// stripHTML removes HTML tags and normalizes whitespace.
func stripHTML(s string) string {
	// Remove script and style blocks entirely
	s = regexp.MustCompile(`(?is)<(script|style)[^>]*>.*?</(script|style)>`).ReplaceAllString(s, "")
	// Replace block-level tags with newlines
	s = regexp.MustCompile(`(?i)<(br|p|div|h[1-6]|li|tr|blockquote)[^>]*>`).ReplaceAllLiteralString(s, "\n")
	// Strip remaining tags
	s = reHTMLTag.ReplaceAllString(s, "")
	// Decode common HTML entities
	s = strings.NewReplacer(
		"&amp;", "&",
		"&lt;", "<",
		"&gt;", ">",
		"&quot;", `"`,
		"&#39;", "'",
		"&nbsp;", " ",
	).Replace(s)
	// Normalize whitespace
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
