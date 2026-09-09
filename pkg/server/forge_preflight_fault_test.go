package server

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/forge"
)

// writeForgeUpstreamError is the single place all three 502-defaulting arms
// cross, so it is where a pre-flight failure stops being reported as the
// forge's. Marking alone changes nothing — forgeUpstreamStatus has no case
// for it, by design — which is exactly why this junction is asserted.
func TestWriteForgeUpstreamError_PreflightIsIterionsOwn(t *testing.T) {
	w := httptest.NewRecorder()
	err := fmt.Errorf("sign app jwt: %w", forge.ErrLocalPreflight)
	if !writeForgeUpstreamError(w, err, "security-read token mint: %v", err) {
		t.Fatal("a pre-flight failure was handed back to the caller, whose default arm answers 502")
	}
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("code = %d, want 500 — the forge never saw this request", w.Code)
	}
}

// The classifier keeps its own contract: it answers what the FORGE answered,
// and the forge answered nothing. Deciding fault is the writer's job, and
// pinning that here keeps a later "simplification" from folding the 500 into
// the table, where it would outrank a genuine upstream code.
func TestForgeUpstreamStatus_PreflightIsNotAForgeAnswer(t *testing.T) {
	if code, _ := forgeUpstreamStatus(fmt.Errorf("marshal: %w", forge.ErrLocalPreflight)); code != 0 {
		t.Fatalf("forgeUpstreamStatus = %d, want 0 (not an answer from the forge)", code)
	}
}

// The composition, which is what the operator actually meets: the
// security-read arm of PATCH /forge/connections/{id}. A mint that failed
// before reaching GitHub must answer 500, not 502.
func TestSecurityReadPatch_PreflightMintAnswers500NotBadGateway(t *testing.T) {
	s := newForgeTestServer(t)
	seedAppConn(t, s, "c1", "SocialGouv", "", false)
	s.forgeSecurityMint = func(context.Context, forge.Connection) (string, time.Time, error) {
		return "", time.Time{}, fmt.Errorf("github: app private key is not valid PEM: %w", forge.ErrLocalPreflight)
	}
	w := patchSecurityRead(t, s, "c1", true)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("code=%d body=%s, want 500 — a key iterion cannot read is not GitHub being down", w.Code, w.Body.String())
	}
}

// And the arm still tells the truth the other way round: a forge that really
// did fail keeps its 502.
func TestSecurityReadPatch_GenuineForgeFailureKeepsBadGateway(t *testing.T) {
	s := newForgeTestServer(t)
	seedAppConn(t, s, "c1", "SocialGouv", "", false)
	s.forgeSecurityMint = func(context.Context, forge.Connection) (string, time.Time, error) {
		return "", time.Time{}, fmt.Errorf("github said no")
	}
	w := patchSecurityRead(t, s, "c1", true)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("code=%d body=%s, want 502 — an unclassified failure on this arm is still the forge's", w.Code, w.Body.String())
	}
}
