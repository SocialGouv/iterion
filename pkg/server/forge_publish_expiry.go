package server

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/eventbus"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/trigger"
)

// errForgePublishGrantUnavailable marks a launch that could not be given the
// publish grant it needs — the token store refused the registration (a
// saturated in-memory registry, an unreachable Valkey).
//
// It is a REFUSAL, not a degradation, because of what the launch does next: it
// claims the repo's gate context on the head (markGateInFlight), and the
// reconciler that answers a dead claim needs the grant to know which repo and
// connection to speak through. A run launched without one leaves a `pending`
// nobody can resolve — the "pending forever" shape docs/merge-gate.md exists
// to close. Refusing before the launch leaves no claim to release.
var errForgePublishGrantUnavailable = errors.New("forge publish grant unavailable")

// forgePublishPostRunGrace is how long the grant of a run that CLAIMED a
// required check outlives it.
//
// It is not zero because such a run's death is exactly when the grant is
// needed most: the merge-gate reconciler reads it to post the synthetic
// verdict a dead review owes, and its net — the sweep — re-offers the same run
// until gateSweepHorizon afterwards. Revoking on the outcome event would race
// the repair and silence it ("its publish grant is expired or revoked").
//
// Derived from that horizon rather than restated, because the two are one
// decision: a grant that dies first turns every later pass of the net into a
// guaranteed abstain, which reads exactly like a net that is still trying.
//
// Past that window nothing revisits the run, so the grant has no reader left.
//
// It is applied by RE-ANCHORING, not by shortening. The expiry a grant is born
// with is measured from LAUNCH and the window above is measured from TERMINAL,
// and those are up to retrypolicy.DefaultMaxWait apart for precisely the runs
// this whole net exists for — one parked on a provider usage window. See
// ForgePublishTokenStore.reanchorIn.
const forgePublishPostRunGrace = gateSweepHorizon + 30*time.Minute

// forgePublishDeadRunGrace is how long EVERY OTHER dead run's grant lives —
// the overwhelming majority, since the server mints a grant for any bot
// launched with a pr_url while only the ones that also claimed a gate context
// owe a required check anything.
//
// A run that claims no gate has no reader past the outcome event: both lanes
// that read a dead run's grant stand down on it before touching the token
// (runClaimsGate is weaker than either). So the horizon buys it nothing and
// costs two things it should not — a crashed run's forge-WRITE credential
// stays usable for days instead of minutes, against a TTL that justifies
// itself as "short enough that a leaked token from a crashed run expires on
// its own"; and terminal eviction stops trimming the registry, which is the
// invariant forgePublishMaxTokens is explicitly sized against ("the steady
// state near 'gating launches in flight' instead of 'gating launches per
// TTL'"). On the in-memory backend that second one is not a leak but a
// REFUSAL: a saturated registry turns the next launch away.
//
// The fast lookback plus the same margin, which is what the grace was before
// the horizon existed: nothing reads it, and one ordinary sweep window is a
// generous allowance for an event processed late.
const forgePublishDeadRunGrace = gateSweepLookback + 30*time.Minute

// forgePublishExpiryName is the eventbus subscriber name (the NATS queue
// group), so one replica shortens each grant.
const forgePublishExpiryName = "forge-publish-grant-expiry"

// startForgePublishGrantExpiry attaches the grant reaper to the event spine.
func (s *Server) startForgePublishGrantExpiry() {
	if s == nil || s.forgePublishTokens == nil || s.cfg.Store == nil {
		return
	}
	bus := s.eventsBus()
	if bus == nil {
		return
	}
	cancel, err := s.attachForgePublishGrantExpiry(bus)
	if err != nil {
		if s.logger != nil {
			s.logger.Warn("server: forge publish grant expiry subscribe failed: %v — a finished run's grant stays live for its full TTL (%s)", err, forgePublishDefaultTTL)
		}
		return
	}
	s.forgePublishExpiryCancel = cancel
}

// attachForgePublishGrantExpiry subscribes the reaper to run-terminal events —
// the same ones the notification dispatcher and the merge-gate reconciler
// consume.
func (s *Server) attachForgePublishGrantExpiry(bus eventbus.Bus) (func(), error) {
	if s == nil || bus == nil {
		return func() {}, nil
	}
	return bus.Subscribe(forgePublishExpiryName, trigger.Matcher{
		Sources: []trigger.Source{trigger.SourceRun},
		Kinds: []string{
			trigger.KindRunFinished,
			trigger.KindRunFailed,
			trigger.KindRunCancelled,
		},
	}, func(ctx context.Context, ev trigger.Event) error {
		return s.expireForgePublishGrantForRun(ctx, strings.TrimSpace(ev.Subject.ID))
	})
}

// expireForgePublishGrantForRun re-anchors the publish grant of a run that has
// ended on the run's death, in whichever direction that instant falls. It is
// the eviction the TTL alone could not do: the TTL is sized for the longest
// usage-window retry (~10 days), so without this every grant a deployment ever
// minted stays live for that long, and the in-memory registry's cap is really
// "grants per TTL".
//
// "Whichever direction" is the part that is easy to get wrong, and this lane
// got it wrong once: the TTL is measured from LAUNCH and the repair window
// from TERMINAL, so for a run parked on a usage window the target falls PAST
// the existing expiry and a shorten-only write is a no-op — the grant then
// dies with days of the net's reach still to run, and every remaining pass
// abstains in silence. See forgePublishPostRunGrace and reanchorIn.
//
// Two shapes keep their grant at full length, because something WILL come back
// and post their own verdict:
//
//   - a paused run — it is expected to resume;
//   - a failed_resumable run with an ARMED retry (RetryAfter set, persisted
//     before the outcome event fires). "Abandoned" is defined by the retry
//     machinery, not re-derived here: the sweeper enforces the policy's
//     max_wait, unsets retry_after when it gives up, and REPUBLISHES the run
//     outcome — so this handler runs again on the abandon and re-anchors the
//     grant then.
//
// Every other terminal shape is dead: budget exceeded, retries exhausted, a
// plain execution failure, a cancel, a normal finish.
func (s *Server) expireForgePublishGrantForRun(ctx context.Context, runID string) error {
	if runID == "" || s == nil || s.forgePublishTokens == nil || s.cfg.Store == nil {
		return nil
	}
	run, err := s.cfg.Store.LoadRun(store.WithoutTenantFilter(ctx), runID)
	if err != nil || run == nil {
		return nil
	}
	token := runInputString(run, forgePublishVarToken)
	if token == "" {
		return nil // the run held no grant
	}
	switch {
	case run.Status == store.RunStatusPausedWaitingHuman || run.Status == store.RunStatusPausedOperator:
		return nil
	case run.Status == store.RunStatusFailedResumable &&
		run.FailureCode != store.FailureDLQParked &&
		run.RetryState != nil && run.RetryState.RetryAfter != nil:
		// A DLQ park is final for automation whatever RetryState says, which
		// is why it is excluded above: only an operator replay wakes it, and
		// the reconciler already treats it as dead.
		return nil
	}
	// Two directions, one predicate. A run that claimed a required check has a
	// reader for its grant until the sweep's horizon, measured from HERE — so
	// its expiry is re-anchored on this instant, which for a long-parked run
	// means pushing it OUT past a launch-stamped one that would otherwise have
	// died mid-window. Everything else is retired on the spot: nothing will
	// read it, and the shorten-only expireIn is the right primitive for a
	// grant that owes nothing.
	if runClaimsGate(run) {
		s.forgePublishTokens.reanchorIn(token, forgePublishPostRunGrace)
		return nil
	}
	s.forgePublishTokens.expireIn(token, forgePublishDeadRunGrace)
	return nil
}
