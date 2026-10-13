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
