package runview

import (
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
)

// #659 pt 3 — the run API carries the credential audit the ceiling counts
// on: the stamped fingerprints, the model-idle marker and the skipped
// credential's reopening, so an operator audits a key's occupancy from the
// run list instead of the server logs.
func TestHeaderFromRun_ExposesTheCredentialStamp(t *testing.T) {
	idle := time.Date(2026, 9, 4, 14, 0, 0, 0, time.UTC)
	reopens := time.Date(2026, 9, 4, 16, 40, 0, 0, time.UTC)
	h := headerFromRun(&store.Run{
		ID:                   "run-659",
		Status:               store.RunStatusRunning,
		CredFingerprints:     []string{"0b5c74421234abcd", "e4ecd2283afb305f"},
		LLMIdleSince:         &idle,
		SkippedCredReopensAt: &reopens,
	})
	if len(h.CredFingerprints) != 2 || h.CredFingerprints[0] != "0b5c74421234abcd" {
		t.Fatalf("cred_fingerprints = %v, want the run's stamp", h.CredFingerprints)
	}
	if h.LLMIdleSince == nil || !h.LLMIdleSince.Equal(idle) {
		t.Fatalf("llm_idle_since = %v, want %v", h.LLMIdleSince, idle)
	}
	if h.SkippedCredReopensAt == nil || !h.SkippedCredReopensAt.Equal(reopens) {
		t.Fatalf("skipped_cred_reopens_at = %v, want %v", h.SkippedCredReopensAt, reopens)
	}
	// A local run stamps nothing: the fields stay absent, not empty.
	if h := headerFromRun(&store.Run{ID: "local"}); h.CredFingerprints != nil || h.LLMIdleSince != nil || h.SkippedCredReopensAt != nil {
		t.Fatalf("an unstamped run exposed %v / %v / %v", h.CredFingerprints, h.LLMIdleSince, h.SkippedCredReopensAt)
	}
}
