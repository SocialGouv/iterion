package delegate

import "testing"

// TestIsTransientAPIErrorResult guards the overload/5xx guard added after a
// test-coverage dogfood saw the claude CLI return "API Error: 529 Overloaded"
// as a "successful" result text, which poisoned the node output and the
// session that inherited it. Transient classes must retry; client/auth errors
// and genuine assistant answers must not.
func TestIsTransientAPIErrorResult(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want bool
	}{
		// Transient — must retry.
		{"529 overloaded", "API Error: 529 Overloaded", true},
		{"529 json body", `API Error: 529 {"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`, true},
		{"500 internal", "API Error: 500 Internal Server Error", true},
		{"503 unavailable", "API Error: 503 Service Unavailable", true},
		{"502 bad gateway", "API Error: 502 Bad Gateway", true},
		{"504 gateway timeout", "API Error: 504 Gateway Timeout", true},
		{"429 rate limited", "API Error: 429 Too Many Requests", true},
		{"no-colon overloaded", "API Error 529 Overloaded", true},
		{"leading/trailing space", "  API Error: 503 Service Unavailable\n", true},
		{"no code but connectivity marker", "API Error: Connection error.", true},
		{"case-insensitive prefix", "api error: 500 boom", true},
		// The bracketed render of an Anthropic-shaped facade: measured as a
		// node's "answer" once, the graph continued on it.
		{"facade bracketed 500", "API Error: [500][Operation failed][2026090610430471b2ed5a5eaa4de7]", true},
		{"facade bracketed 429", "API Error: [429][Rate limit reached][2026090610430471b2ed5a5eaa4de7]", true},
		{"facade bracketed 529", "API Error: [529][Overloaded][abc]", true},
		{"facade bracketed, no space", "API Error:[503][Service unavailable]", true},
		// A CDN or proxy in front of the facade: 5xx outside any allow-list,
		// and a 4xx whose text carries a connectivity marker — both were
		// retried through the marker fallback before the status parsed.
		{"proxy bracketed 524", "API Error: [524][Gateway timeout][2026090610430471b2ed5a5eaa4de7]", true},
		{"proxy bracketed 522", "API Error: [522][Connection timed out][abc]", true},
		{"proxy bracketed 530", "API Error: [530][Service unavailable][abc]", true},
		{"proxy bracketed 499 with marker", "API Error: [499][Connection error][abc]", true},
		{"bare unlisted 5xx", "API Error: 520 Bad gateway", true},
		// No connectivity marker in the text: only the 5xx rule can retry
		// these — an allow-list would ship them as the node's answer.
		{"bare unlisted 5xx without a marker", "API Error: 598 Upstream hiccup", true},
		{"proxy bracketed 526 without a marker", "API Error: [526][Invalid SSL certificate][abc]", true},

		// Non-transient client/auth errors — must surface, not loop.
		{"400 bad request", "API Error: 400 Bad Request", false},
		{"401 unauthorized", "API Error: 401 Unauthorized", false},
		{"403 forbidden", "API Error: 403 Forbidden", false},
		{"404 not found", "API Error: 404 Not Found", false},
		{"422 unprocessable", "API Error: 422 Unprocessable Entity", false},
		{"facade bracketed 400", "API Error: [400][Invalid request][abc]", false},
		{"facade bracketed 401", "API Error: [401][Unauthorized][abc]", false},
		{"four digits are not a status", "API Error: [5000][x]", false},
		{"facade bracketed 404", "API Error: [404][Not found][abc]", false},

		// Genuine assistant output — must never be mistaken for an error.
		{"empty", "", false},
		{"normal answer", "Here is the test plan: add unit tests for pkg/log.", false},
		{"discusses api error mid-text", "The handler should retry when it sees an API Error: 529 from upstream, so add a test for that path.", false},
		{"long text starting with api error word", "API errors are common; this suite asserts the client retries 5xx responses and surfaces 4xx ones, covering the full matrix of " + longPad(), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isTransientAPIErrorResult(tt.in); got != tt.want {
				t.Errorf("isTransientAPIErrorResult(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

// longPad returns filler pushing a string past the 400-char brevity guard so
// the "long text" case exercises the length cutoff.
func longPad() string {
	s := ""
	for i := 0; i < 400; i++ {
		s += "x"
	}
	return s
}
