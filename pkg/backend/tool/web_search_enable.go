package tool

import (
	"os"
	"strings"
)

// searchBackendEnvs are the env vars that indicate a real, configured
// web-search backend. SEARXNG_URL/SEARXNG_ENDPOINT point at a
// self-hosted SearXNG instance (the sovereign default); BRAVE_API_KEY
// keys the Brave Search API. Their presence is what flips the "auto"
// mode on. The DuckDuckGo Lite scrape is intentionally NOT represented
// here: it needs no config, is brittle, and leaks queries to a third
// party, so it is never a silent default — reach it only by forcing
// ITERION_WEB_SEARCH=on.
var searchBackendEnvs = []string{"SEARXNG_URL", "SEARXNG_ENDPOINT", "BRAVE_API_KEY"}

// ResolveWebSearchEnabled decides whether the claw `web_search` tool is
// registered for a run. Precedence mirrors backend auto-detection:
//
//   - ITERION_WEB_SEARCH=on|true|1   → always register (DDG scrape included)
//   - ITERION_WEB_SEARCH=off|false|0 → never register
//   - unset / auto (the default)     → register only when a search backend
//     is configured (SEARXNG_URL / SEARXNG_ENDPOINT / BRAVE_API_KEY)
//
// The zero-config DuckDuckGo fallback is thus opt-in via `on`, keeping
// the sovereign SearXNG / keyed-Brave path the only automatic surface.
func ResolveWebSearchEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("ITERION_WEB_SEARCH"))) {
	case "on", "true", "1":
		return true
	case "off", "false", "0":
		return false
	default: // "", "auto", or anything unrecognized → auto
		return searchBackendConfigured()
	}
}

// searchBackendConfigured reports whether any configured search backend
// env is set to a non-empty value.
func searchBackendConfigured() bool {
	for _, k := range searchBackendEnvs {
		if strings.TrimSpace(os.Getenv(k)) != "" {
			return true
		}
	}
	return false
}
