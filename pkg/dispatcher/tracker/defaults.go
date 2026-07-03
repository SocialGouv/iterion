package tracker

import (
	"net/http"
	"time"
)

// defaultClaimedLabel is shared by all three constructors (github, gitlab,
// forgejo): when the operator doesn't configure ClaimedLabel, adapters
// fall back to the same "iterion-claimed" label name.
func defaultClaimedLabel(label string) string {
	if label == "" {
		return "iterion-claimed"
	}
	return label
}

// defaultHTTPTrackerClient returns hc unchanged, or a plain 30s-timeout
// client when hc is nil. Shared by the gitlab and forgejo constructors;
// github shells out to `gh` instead and has no HTTP client of its own.
func defaultHTTPTrackerClient(hc *http.Client) *http.Client {
	if hc == nil {
		return &http.Client{Timeout: 30 * time.Second}
	}
	return hc
}
