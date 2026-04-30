package tools

import (
	"encoding/json"
	"fmt"
	"github.com/SocialGouv/claw-code-go/internal/api"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

const (
	defaultBraveSearchURL = "https://api.search.brave.com/res/v1/web/search"
	defaultDdgLiteURL     = "https://lite.duckduckgo.com/lite/"
	searchTimeout         = 15 * time.Second
	defaultNumResults     = 5
)

// braveSearchEndpoint and ddgSearchEndpoint return the upstream URL,
// optionally overridden via CLAW_WEB_SEARCH_BRAVE_URL /
// CLAW_WEB_SEARCH_DDG_URL. The override is intended for hosts that
// drive web_search through a fixture httptest server in tests; it is
// not a public configuration knob for production callers.
func braveSearchEndpoint() string {
	if v := os.Getenv("CLAW_WEB_SEARCH_BRAVE_URL"); v != "" {
		return v
	}
	return defaultBraveSearchURL
}

func ddgSearchEndpoint() string {
	if v := os.Getenv("CLAW_WEB_SEARCH_DDG_URL"); v != "" {
		return v
	}
	return defaultDdgLiteURL
}

// WebSearchTool returns the tool definition for web search.
func WebSearchTool() api.Tool {
	return api.Tool{
		Name:        "web_search",
		Description: "Search the web and return a list of results with title, URL, and snippet. Uses Brave Search API if BRAVE_API_KEY is set, otherwise DuckDuckGo.",
		InputSchema: api.InputSchema{
			Type: "object",
			Properties: map[string]api.Property{
				"query": {
					Type:        "string",
					Description: "Search query",
				},
				"num_results": {
					Type:        "integer",
					Description: fmt.Sprintf("Number of results to return (default %d)", defaultNumResults),
				},
			},
			Required: []string{"query"},
		},
	}
}

// ExecuteWebSearch performs a web search and returns formatted results.
func ExecuteWebSearch(input map[string]any) (string, error) {
	query, ok := input["query"].(string)
	if !ok || query == "" {
		return "", fmt.Errorf("web_search: 'query' is required")
	}

	numResults := defaultNumResults
	if v, ok := input["num_results"]; ok {
		switch n := v.(type) {
		case float64:
			numResults = int(n)
		case int:
			numResults = n
		}
	}
	if numResults < 1 {
		numResults = 1
	}
	if numResults > 20 {
		numResults = 20
	}

	if apiKey := os.Getenv("BRAVE_API_KEY"); apiKey != "" {
		return braveSearch(query, numResults, apiKey)
	}
	return ddgSearch(query, numResults)
}

// braveSearch queries the Brave Search API.
func braveSearch(query string, numResults int, apiKey string) (string, error) {
	params := url.Values{}
	params.Set("q", query)
	params.Set("count", fmt.Sprintf("%d", numResults))

	req, err := http.NewRequest("GET", braveSearchEndpoint()+"?"+params.Encode(), nil)
	if err != nil {
		return "", fmt.Errorf("web_search: build request: %w", err)
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

// ddgSearch scrapes DuckDuckGo Lite for search results.
func ddgSearch(query string, numResults int) (string, error) {
	params := url.Values{}
	params.Set("q", query)

	req, err := http.NewRequest("POST", ddgSearchEndpoint(), strings.NewReader(params.Encode()))
	if err != nil {
		return "", fmt.Errorf("web_search (ddg): build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", "claw-code/0.1 (text search)")

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

// parseDDGLite extracts [title, url, snippet] triples from DDG Lite HTML.
// DDG Lite wraps results in table rows — we extract links and adjacent text.
func parseDDGLite(html string, max int) [][3]string {
	var results [][3]string

	// Find result links: <a class="result-link" href="...">title</a>
	linkRe := reHTMLTag // reuse from web_fetch.go
	_ = linkRe

	// Simple approach: look for <a ...href="http..."...>title</a> patterns
	lines := strings.Split(html, "\n")
	var pendingTitle, pendingURL string

	for _, line := range lines {
		line = strings.TrimSpace(line)

		// Look for result links
		if strings.Contains(line, `href="http`) {
			hrefStart := strings.Index(line, `href="`)
			if hrefStart >= 0 {
				hrefStart += 6
				hrefEnd := strings.Index(line[hrefStart:], `"`)
				if hrefEnd >= 0 {
					u := line[hrefStart : hrefStart+hrefEnd]
					if strings.HasPrefix(u, "http") && !strings.Contains(u, "duckduckgo.com") {
						// Extract link text
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

		// Look for snippet text after a result link
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
