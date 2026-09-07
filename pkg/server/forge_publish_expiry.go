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

// forgePublishPostRunGrace is how long a grant outlives the run it was minted
// for.
//
// It is not zero because the run's death is exactly when the grant is needed
// most: the merge-gate reconciler reads it to post the synthetic verdict a
// dead review owes, and its net — the sweep — re-offers the same run for
// gateSweepLookback afterwards. Revoking on the outcome event would race the
// repair and silence it ("its publish grant is expired or revoked").
//
// Past that window nothing revisits the run, so the grant has no reader left.
const forgePublishPostRunGrace = gateSweepLookback + 30*time.Minute

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

// expireForgePublishGrantForRun shortens the publish grant of a run that has
// nothing left to publish. It is the eviction the TTL alone could not do: the
// TTL is sized for the longest usage-window retry (~10 days), so without this
// every grant a deployment ever minted stays live for that long, and the
// in-memory registry's cap is really "gating launches per TTL".
//
// Two shapes keep their grant at full length, because something WILL come back
// and post their own verdict:
//
//   - a paused run — it is expected to resume;
//   - a failed_resumable run with an ARMED retry (RetryAfter set, persisted
//     before the outcome event fires). "Abandoned" is defined by the retry
//     machinery, not re-derived here: the sweeper enforces the policy's
//     max_wait, unsets retry_after when it gives up, and REPUBLISHES the run
//     outcome — so this handler runs again on the abandon and shortens the
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
	s.forgePublishTokens.expireIn(token, forgePublishPostRunGrace)
	return nil
}
