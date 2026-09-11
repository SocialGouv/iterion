package cli

import (
	"strings"
	"testing"
)

// TestAFetchURLsCredentialStaysOutOfTheProvenance.
//
// In-package, because the redaction happens on the way OUT of a network fetch
// and the guarded client this lane uses refuses a loopback address on purpose
// — so there is no test server to fetch from, and the function itself is the
// only honest seam.
//
// The install-time lane exists for a description iterion may not redistribute
// — i.e. exactly the one that sits behind auth — and the only credential a
// fetch URL can carry is in its userinfo or its query (`?private_token=…`).
// Recorded verbatim, it was written into `connector.yaml` at 0644, in a
// directory whose whole point is to be committed. Go itself redacts userinfo
// when it prints a URL in an error; only what iterion persisted kept it in the
// clear.
func TestAFetchURLsCredentialStaysOutOfTheProvenance(t *testing.T) {
	for _, tc := range []struct{ name, raw, secret, keep string }{
		{"userinfo", "https://bob:ghp_supersecret@vendor.example/openapi.json", "ghp_supersecret", "vendor.example/openapi.json"},
		{"a token in the query", "https://vendor.example/openapi.json?private_token=ghp_supersecret", "ghp_supersecret", "vendor.example/openapi.json"},
		{"a token under any other name", "https://vendor.example/spec?apikey=ghp_supersecret", "ghp_supersecret", "vendor.example/spec"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := redactedSpecURL(tc.raw)
			if strings.Contains(got, tc.secret) {
				t.Errorf("the provenance would carry %q — a credential must not be written into a committed file", got)
			}
			// Still recognisable as the source it was, or the provenance stops
			// answering the question it exists for.
			if !strings.Contains(got, tc.keep) {
				t.Errorf("provenance = %q, want it to still name the source (%s)", got, tc.keep)
			}
		})
	}

	// The ordinary URL is untouched: redaction that rewrote every source would
	// make a reproducible package unreproducible.
	const plain = "https://vendor.example/openapi.json"
	if got := redactedSpecURL(plain); got != plain {
		t.Errorf("redactedSpecURL(%q) = %q, want it unchanged", plain, got)
	}
}
