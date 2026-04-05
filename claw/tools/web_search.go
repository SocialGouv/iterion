package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/zendev-sh/goai"
)

const (
	braveSearchURL    = "https://api.search.brave.com/res/v1/web/search"
	ddgLiteURL        = "https://lite.duckduckgo.com/lite/"
	searchTimeout     = 15 * time.Second
	defaultNumResults = 5
)

var webSearchSchema = json.RawMessage(`{
	"type": "object",
	"properties": {
		"query": {
			"type": "string",
			"description": "Search query"
		},
		"num_results": {
			"type": "integer",
			"description": "Number of results to return (default 5)"
		}
	},
	"required": ["query"]
}`)

// WebSearch returns a goai.Tool that searches the web.
func WebSearch() goai.Tool {
	return goai.Tool{
		Name:        "web_search",
		Description: "Search the web and return a list of results with title, URL, and snippet. Uses Brave Search API if BRAVE_API_KEY is set, otherwise DuckDuckGo.",
		InputSchema: webSearchSchema,
		Execute: func(ctx context.Context, input json.RawMessage) (string, error) {
			var params struct {
				Query      string `json:"query"`
				NumResults int    `json:"num_results"`
			}
			if err := json.Unmarshal(input, &params); err != nil {
				return "", fmt.Errorf("web_search: parse input: %w", err)
			}
			if params.Query == "" {
				return "", fmt.Errorf("web_search: 'query' is required")
			}

			numResults := defaultNumResults
			if params.NumResults > 0 {
				numResults = params.NumResults
			}
			if numResults > 20 {
				numResults = 20
			}

			if apiKey := os.Getenv("BRAVE_API_KEY"); apiKey != "" {
				return braveSearch(ctx, params.Query, numResults, apiKey)
			}
			return ddgSearch(ctx, params.Query, numResults)
		},
	}
}

func braveSearch(ctx context.Context, query string, numResults int, apiKey string) (string, error) {
	params := url.Values{}
	params.Set("q", query)
	params.Set("count", fmt.Sprintf("%d", numResults))

	req, err := http.NewRequestWithContext(ctx, "GET", braveSearchURL+"?"+params.Encode(), nil)
	if err != nil {
		return "", fmt.Errorf("web_search (brave): build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Subscription-Token", apiKey)

	client := &http.Client{Timeout: searchTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("web_search (brave): %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("web_search (brave): HTTP %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("web_search (brave): read body: %w", err)
	}

	var result struct {
		Web struct {
			Results []struct {
				Title       string `json:"title"`
				URL         string `json:"url"`
				Description string `json:"description"`
			} `json:"results"`
		} `json:"web"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", fmt.Errorf("web_search (brave): parse response: %w", err)
	}

	if len(result.Web.Results) == 0 {
		return "No results found.", nil
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "Search results for: %s\n\n", query)
	for i, r := range result.Web.Results {
		fmt.Fprintf(&sb, "%d. %s\n   %s\n   %s\n\n", i+1, r.Title, r.URL, r.Description)
	}
	return strings.TrimSpace(sb.String()), nil
}

func ddgSearch(ctx context.Context, query string, numResults int) (string, error) {
	params := url.Values{}
	params.Set("q", query)

	req, err := http.NewRequestWithContext(ctx, "POST", ddgLiteURL, strings.NewReader(params.Encode()))
	if err != nil {
		return "", fmt.Errorf("web_search (ddg): build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", "claw/0.1 (text search)")

	client := &http.Client{Timeout: searchTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("web_search (ddg): %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("web_search (ddg): HTTP %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 200*1024))
	if err != nil {
		return "", fmt.Errorf("web_search (ddg): read body: %w", err)
	}

	results := parseDDGLite(string(body), numResults)
	if len(results) == 0 {
		return "No results found.", nil
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "Search results for: %s\n\n", query)
	for i, r := range results {
		fmt.Fprintf(&sb, "%d. %s\n   %s\n   %s\n\n", i+1, r[0], r[1], r[2])
	}
	return strings.TrimSpace(sb.String()), nil
}

// parseDDGLite extracts search results from DuckDuckGo Lite HTML using
// heuristic string matching. This is inherently fragile and may break if
// DDG changes their HTML structure.
func parseDDGLite(html string, max int) [][3]string {
	var results [][3]string
	lines := strings.Split(html, "\n")
	var pendingTitle, pendingURL string

	for _, line := range lines {
		line = strings.TrimSpace(line)

		if strings.Contains(line, `href="http`) {
			hrefStart := strings.Index(line, `href="`)
			if hrefStart >= 0 {
				hrefStart += 6
				hrefEnd := strings.Index(line[hrefStart:], `"`)
				if hrefEnd >= 0 {
					u := line[hrefStart : hrefStart+hrefEnd]
					if strings.HasPrefix(u, "http") && !strings.Contains(u, "duckduckgo.com") {
						textStart := strings.Index(line, ">")
						textEnd := strings.LastIndex(line, "<")
						title := ""
						if textStart >= 0 && textEnd > textStart {
							title = reHTMLTag.ReplaceAllString(line[textStart+1:textEnd], "")
							title = strings.TrimSpace(title)
						}
						if title != "" {
							pendingTitle = title
							pendingURL = u
						}
					}
				}
			}
		}

		if pendingURL != "" && !strings.Contains(line, "<a ") {
			stripped := reHTMLTag.ReplaceAllString(line, "")
			stripped = strings.TrimSpace(stripped)
			if len(stripped) > 20 {
				results = append(results, [3]string{pendingTitle, pendingURL, stripped})
				pendingTitle = ""
				pendingURL = ""
				if len(results) >= max {
					break
				}
			}
		}
	}

	return results
}
