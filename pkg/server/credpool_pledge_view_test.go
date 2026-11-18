package server

import (
	"net/http/httptest"
	"testing"
	"time"

	iterlog "github.com/SocialGouv/iterion/pkg/log"

	"github.com/SocialGouv/iterion/pkg/credpool"
)

// TestToPledgeViewCarriesFallbackUse: the view must carry the donor's
// consent back — a GET that drops it makes every CLI pause/enable and
// every studio save silently revoke it (the clobber class on the READ
// path; Revi R6b8d5b).
// Mutant: FallbackUse dropped from the toPledgeView literal → this test
// reds.
func TestToPledgeViewCarriesFallbackUse(t *testing.T) {
	s := &Server{
		credPoolLedger: credpool.NewMemoryLedger(),
		logger:         iterlog.New(iterlog.LevelError, nil),
	}
	v := s.toPledgeView(httptest.NewRequest("GET", "/", nil), credpool.Pledge{
		UserID: "d", Credential: credpool.Credential{Source: credpool.SourceAPIKey, Ref: "zai"},
		Enabled: true, FallbackUse: true,
	}, time.Now(), true)
	if !v.FallbackUse {
		t.Fatal("toPledgeView dropped fallback_use — every read-modify-write client would revoke the consent")
	}
}

// TestToPledgeViewCarriesThePledgeID: the view's id is what the UI and the
// CLI withdraw a single contribution BY. Without it a donor holding two keys
// of one provider can only name the (source, ref) pair, and the withdrawal
// either refuses or takes the wrong half.
// Mutant: ID dropped from the toPledgeView literal → the id assertion reds.
func TestToPledgeViewCarriesThePledgeID(t *testing.T) {
	s := &Server{
		credPoolLedger: credpool.NewMemoryLedger(),
		logger:         iterlog.New(iterlog.LevelError, nil),
	}
	// Two keys of one lender and provider: the ids must differ, or the
	// second pledge would overwrite the first.
	for _, keyID := range []string{"key-forfait-1", "key-forfait-2"} {
		p := credpool.Pledge{
			ID: credpool.APIKeyPledgeID("jo", "anthropic", keyID), UserID: "jo",
			Credential: credpool.Credential{Source: credpool.SourceAPIKey, Ref: "anthropic", KeyID: keyID},
			Enabled:    true,
		}
		v := s.toPledgeView(httptest.NewRequest("GET", "/", nil), p, time.Now(), true)
		if v.ID != p.ID {
			t.Fatalf("toPledgeView id = %q, want %q — a donor cannot address this contribution", v.ID, p.ID)
		}
	}
}
