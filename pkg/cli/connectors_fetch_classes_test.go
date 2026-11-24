package cli

import (
	"errors"
	"testing"

	"github.com/SocialGouv/iterion/pkg/secure/httpdial"
)

// The description fetch is always strict — a vendor description lives on
// the public internet — so the shared (CGNAT) space and a non-public IPv6
// transition address are refused before any byte leaves.
func TestReadSpecRefusesTheSharedAndTransitionClasses(t *testing.T) {
	for _, source := range []string{"http://100.64.0.1:9/openapi.json", "http://[64:ff9b::7f00:1]:9/openapi.json"} {
		if _, _, err := readSpec(source); !errors.Is(err, httpdial.ErrNotPublic) {
			t.Errorf("readSpec(%s) = %v; want the guard's refusal", source, err)
		}
	}
}
