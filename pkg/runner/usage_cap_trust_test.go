package runner

import (
	"errors"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/usagecap"
)

// #690 — the production shape that blocked the merge gate. The platform
// forfait's meter held ONE reading: seven_day at 0.99, reset four days
// out, observed 17h37m earlier — taken before the provider reset the
// window early. Every claude_code run was refused HERE, at admission, on
// it; and a run refused at admission never issues a call, never fires
// OnUsageWindow, never refreshes the reading. "Blocked and unrefreshable"
// is one case, so the reading's authority must be bounded by its AGE, not
// only by its reset instant: past the trust window the run is admitted
// and its own session re-measures.
func TestUsageCapPreflight_StaleReadingDoesNotRefuseAdmission(t *testing.T) {
	ctx := t.Context()
	now := time.Now().UTC()
	msg := &queue.RunMessage{RunID: "01a07047", TenantID: "team-7"}
	// The platform forfait, as the runner sees it: an OAuth dir the
	// platform tier filled, metered fleet-wide under its own fingerprint.
	ctx = secrets.WithCredentials(ctx, secrets.Credentials{
		OAuthCredentialFiles: map[string]string{delegate.BackendClaudeCode: t.TempDir()},
		PlatformSourced:      map[string]bool{delegate.BackendClaudeCode: true},
		Fingerprints:         map[string]string{delegate.BackendClaudeCode: "e4ecd2283afb305f"},
	})
	caps := usagecap.NewMemStore()
	key := usageCapKey(ctx, msg)
	if want := usagecap.Key(delegate.BackendClaudeCode, usagecap.ScopePlatform, "e4ecd2283afb305f"); key != want {
		t.Fatalf("meter key = %q, want the platform forfait's own %q", key, want)
	}
	stale := usagecap.Reading{
		Window: usagecap.WindowSevenDay, Utilization: 0.99, Status: usagecap.StatusWarning,
		ResetsAt:   now.Add(4 * 24 * time.Hour),
		ObservedAt: now.Add(-(17*time.Hour + 37*time.Minute)),
	}
	if err := caps.Record(ctx, key, stale); err != nil {
		t.Fatal(err)
	}
	pol := usagecap.Policy{Week: usagecap.WindowPolicy{MaxPercent: 85, Mode: usagecap.ModeHard}}
	rs := &capStatusStore{}
	// Zero UsageCapTrust: the package defaults (3h) apply.
	r := capRunner(pol, caps, rs)

	if err := r.usageCapPreflight(ctx, capLLMWorkflow(), msg, iterlog.Nop()); err != nil {
		t.Fatalf("a 17h-old reading refused the run at admission — the self-sustaining lock of #690: %v", err)
	}
	if rs.calls != 0 {
		t.Fatalf("the run was flipped to failed_resumable %d time(s) on a stale reading", rs.calls)
	}

	// No hysteria: the same reading taken ten minutes ago is authoritative
	// and still parks the run, with the window's own reset as its retry.
	fresh := stale
	fresh.ObservedAt = now.Add(-10 * time.Minute)
	if err := caps.Record(ctx, key, fresh); err != nil {
		t.Fatal(err)
	}
	err := r.usageCapPreflight(ctx, capLLMWorkflow(), msg, iterlog.Nop())
	var rl *delegate.ErrRateLimited
	if !errors.As(err, &rl) || !rl.ResetAt.Equal(stale.ResetsAt) {
		t.Fatalf("a ten-minute-old 99%% reading must refuse the run with the window's reset: got %v", err)
	}

	// The operator's own bound applies: a tighter trust window forgets
	// the same reading sooner.
	r.cfg.UsageCapTrust = usagecap.Trust{Window: 5 * time.Minute}
	if err := r.usageCapPreflight(ctx, capLLMWorkflow(), msg, iterlog.Nop()); err != nil {
		t.Fatalf("a 10-minute-old reading outlived a 5-minute trust window: %v", err)
	}
}
