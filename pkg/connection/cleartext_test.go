package connection_test

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/connection"
)

// The warning exists because nothing else in the path mentions the scheme: the
// guarded dialer checks an address CLASS, and a credential is sent on every
// call thereafter. So what it must get right is both halves — saying it when
// the credential really does travel in the clear, and staying quiet when it
// does not, since a warning an operator learns to read past protects nothing.
func TestCleartextOriginWarnsOnlyWhenTheCredentialLeavesInTheClear(t *testing.T) {
	for _, tc := range []struct {
		name    string
		baseURL string
		warns   bool
	}{
		{"https is the case this exists to distinguish", "https://forge.example.org", false},
		{"plain http to a named host", "http://forge.example.org", true},
		{"plain http to a LAN address", "http://192.168.1.10:3000", true},
		{"plain http to a public IP", "http://203.0.113.7", true},
		{"loopback by name never leaves the machine", "http://localhost:3000", false},
		{"loopback by address never leaves the machine", "http://127.0.0.1:3000", false},
		{"IPv6 loopback", "http://[::1]:3000", false},
		{"a .localhost name resolves to loopback", "http://forge.localhost:3000", false},
		{"empty is the package default, decided elsewhere", "", false},
		{"a URL that does not parse is validateBaseURL's refusal, not a warning", "://nope", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := connection.CleartextOrigin(tc.baseURL)
			if tc.warns && got == "" {
				t.Fatalf("CleartextOrigin(%q) said nothing, want a warning", tc.baseURL)
			}
			if !tc.warns && got != "" {
				t.Fatalf("CleartextOrigin(%q) = %q, want silence", tc.baseURL, got)
			}
			if !tc.warns {
				return
			}
			// A warning that names neither the host nor the remedy is one an
			// operator cannot act on.
			if !strings.Contains(got, "http") {
				t.Errorf("warning = %q, want it to name the scheme it is about", got)
			}
			if !strings.Contains(got, "https") {
				t.Errorf("warning = %q, want it to name the way out", got)
			}
		})
	}
}
