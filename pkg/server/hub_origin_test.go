package server

import (
	"net/http/httptest"
	"testing"
)

// TestSameWSOrigin locks the same-origin WS allow rule that fixes the cloud
// studio (the SPA dialing wss:// on the host that served it): the Origin
// header's host must match the request Host, case-insensitively.
//
// The scheme rule is deliberately ONE-SIDED — see sameOrigin. A plaintext
// Origin is refused when the request demonstrably arrived over TLS (an
// attacker who can MITM http://<same-host> must not be "same-origin"), but an
// https Origin is accepted whatever the request scheme resolves to, because
// a TLS-terminating proxy that omits X-Forwarded-Proto would otherwise make
// the server refuse its own SPA.
func TestSameWSOrigin(t *testing.T) {
	cases := []struct {
		name    string
		origin  string
		host    string
		fwdedTo string // X-Forwarded-Proto, empty = unset
		want    bool
	}{
		{"cloud same-origin", "https://iterion.ovh.fabrique.social.gouv.fr", "iterion.ovh.fabrique.social.gouv.fr", "", true},
		{"local same-origin", "http://localhost:4891", "localhost:4891", "", true},
		{"case-insensitive host", "https://Iterion.Example.com", "iterion.example.com", "", true},
		{"cross-site drive-by", "https://evil.example", "iterion.ovh.fabrique.social.gouv.fr", "", false},
		{"different port", "http://localhost:5173", "localhost:4891", "", false},
		{"empty origin", "", "iterion.ovh.fabrique.social.gouv.fr", "", false},

		// Scheme rule.
		{"plaintext origin on a TLS request is refused", "http://iterion.cloud", "iterion.cloud", "https", false},
		{"https origin behind the ingress", "https://iterion.cloud", "iterion.cloud", "https", true},
		{"plaintext origin on a plaintext request (local studio)", "http://localhost:4891", "localhost:4891", "http", true},
		// A proxy chain appends; the client's own value is first.
		{"proxy chain, client used https", "http://iterion.cloud", "iterion.cloud", "https, http", false},
		// No forwarding header + no TLS = a local plaintext server: stay permissive.
		{"https origin with no forwarding header", "https://iterion.cloud", "iterion.cloud", "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/api/ws/runs/x", nil)
			r.Host = c.host
			if c.fwdedTo != "" {
				r.Header.Set("X-Forwarded-Proto", c.fwdedTo)
			}
			if got := sameOrigin(c.origin, r); got != c.want {
				t.Errorf("sameOrigin(%q, host=%q, xfp=%q) = %v; want %v", c.origin, c.host, c.fwdedTo, got, c.want)
			}
		})
	}
}
