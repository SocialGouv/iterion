package boardmongo

import (
	"context"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
)

// In-package on purpose: the refusals below fire BEFORE any Mongo call,
// and no exported caller names these keys (that is the point) — the only
// way to pin the guard is to call replace directly.

// A caller naming a claim-owned key must be refused LOUDLY: the skip in
// the $set builder would otherwise drop the caller's own write in
// silence — a fenced-family write smuggled through the unfenced path,
// lost with every suite green.
func TestReplace_ClaimOwnedKeyIsRefusedLoudly(t *testing.T) {
	s := &Store{}
	iss := &native.Issue{ID: "native:1", Title: "x"}
	for _, k := range []string{"claim", "claimepoch", "claimedat", "claimleaseuntil", "claim_epoch", "claim_lease_until"} {
		err := s.replace(context.Background(), iss, k)
		if err == nil || !strings.Contains(err.Error(), "claim-owned") {
			t.Fatalf("replace(%q) = %v, want a loud claim-owned refusal — a silent skip loses the caller's write", k, err)
		}
	}
}

// A typo'd key would silently write nothing — the caller's own mutation
// lost with no error, worse than the clobber the scoping replaced.
func TestReplace_UnknownKeyIsRefusedLoudly(t *testing.T) {
	s := &Store{}
	err := s.replace(context.Background(), &native.Issue{ID: "native:1"}, "labelz")
	if err == nil || !strings.Contains(err.Error(), "unknown issue field") {
		t.Fatalf("replace with a typo'd key = %v, want a loud unknown-field refusal", err)
	}
}
