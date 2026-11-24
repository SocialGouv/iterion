package oidc

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/SocialGouv/iterion/pkg/secure/httpdial"
)

// A per-org connector's client honours the strictness its caller hands it:
// strict refuses the shared (CGNAT) space and a non-public IPv6 transition
// address on the discovery fetch itself; lifted admits them, read past the
// guard on a dial whose context is already canceled, so no packet leaves.
func TestSafeGenericClientHonoursItsStrictness(t *testing.T) {
	targets := []string{"100.64.0.1:9", "[64:ff9b::7f00:1]:9"}
	for _, target := range targets {
		if _, err := SafeGenericClient(true).Get("http://" + target + "/.well-known/openid-configuration"); !errors.Is(err, httpdial.ErrNotPublic) {
			t.Errorf("strict: %s = %v; want the guard's refusal", target, err)
		}
	}
	tr, ok := SafeGenericClient(false).Transport.(*http.Transport)
	if !ok {
		t.Fatalf("SafeGenericClient(false).Transport = %T; want the guarded *http.Transport", SafeGenericClient(false).Transport)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	for _, target := range targets {
		if _, err := tr.DialContext(canceled, "tcp", target); !errors.Is(err, context.Canceled) || errors.Is(err, httpdial.ErrNotPublic) {
			t.Errorf("lifted: dial %s = %v; want the dialer's cancellation, past the guard", target, err)
		}
	}
}
