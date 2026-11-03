package cloudpublisher

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/SocialGouv/iterion/pkg/retrypolicy"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
)

// The HARD per-key concurrency cap (secrets.ApiKey.MaxConcurrentRuns).
//
// The walk's skip (apiKeyUsable) is the CHOOSER: it passes a full key over so
// the next key of the provider serves. It cannot be the enforcer — it reads
// the run documents before this run is written, so two publisher replicas
// admit on the same reading; a key_overrides pin bypasses it by design; and
// the refused-key restore undoes it on purpose. Enforcement is this
// admission step, AFTER the key is chosen and before anything is persisted or
// published: one atomic reservation per sealed capped key on the ledger
// store.CredSlotStore. A refused reservation PARKS the run — persisted as
// failed_resumable under FailureCredSlotWait with a durable retry armed — and
// the retry sweeper re-asks for a slot on a short cadence until one frees or
// the run's max_wait passes. A park, not a refusal: a webhook delivery has
// nobody to click again, and a parked run keeps its place, its inputs and its
// pull request's pending check.

// ErrCredentialAtCapacity is the admission's refusal. SubmitResume wraps it
// when a resumed run finds its key full, so the sweeper can tell a slot wait
// — re-asked for free — from a resume that failed for a reason worth an
// attempt.
var ErrCredentialAtCapacity = errors.New("credential at its concurrency cap")

// CredSlotProbeInterval paces how often a parked run asks for a slot again:
// the first ask the park arms, and every free deferral the retry sweeper
// grants after a refused resume. A cap lapses when a run ends, not at an
// instant anyone can name, so the wait is a poll: short enough that a freed
// slot is taken within minutes, long enough that a key busy for hours is not
// asked on every sweep tick. The run's own retry policy bounds the whole wait
// (max_wait).
const CredSlotProbeInterval = 2 * time.Minute

// credSlotReleaseTimeout bounds the undo of an admission whose launch or
// resume did not land: detached from the request, like every other rollback
// the publisher owes.
const credSlotReleaseTimeout = 5 * time.Second

// credSlotWant is one capped API key the sealed bundle carries: the slot the
// admission must hold before the run may be queued, and the key's label for
// the park's own words.
type credSlotWant struct {
	fingerprint string
	max         int
	provider    secrets.Provider
	name        string
}

// credSlotWants collects, during the walk, every capped key a tier sealed,
// by fingerprint. The resolution narrows it to the stamp at the end (sealed):
// a key sealed for a route the run never takes holds no slot, exactly as it
// counts toward no ceiling (spendsKey).
type credSlotWants map[string]credSlotWant

// note records a sealed key. An uncapped key (max 0) or an unstamped one (no
// fingerprint) reserves nothing and is not recorded.
func (w credSlotWants) note(fingerprint string, max int, prov secrets.Provider, name string) {
	if w == nil || fingerprint == "" || max <= 0 {
		return
	}
	w[fingerprint] = credSlotWant{fingerprint: fingerprint, max: max, provider: prov, name: name}
}

// sealed returns the wants whose key is in the run's stamp, in stamp order —
// the stamp is sorted, so the admission reserves in one order on every
// replica.
func (w credSlotWants) sealed(fingerprints []string) []credSlotWant {
	var out []credSlotWant
	for _, fp := range fingerprints {
		if want, ok := w[fp]; ok {
			out = append(out, want)
		}
	}
	return out
}

// credSlotRefusal is the admission's answer when a key is full: which key,
// and how many runs hold it. It wraps ErrCredentialAtCapacity so a caller
// that turns it into an error keeps the class.
type credSlotRefusal struct {
	want credSlotWant
	held int
}

func (r *credSlotRefusal) Error() string {
	return fmt.Sprintf("%v: api-key(%s) %q holds %d/%d alive runs", ErrCredentialAtCapacity, r.want.provider, r.want.name, r.held, r.want.max)
}

func (r *credSlotRefusal) Unwrap() error { return ErrCredentialAtCapacity }

// admitCredSlots reserves one slot per capped key the resolution sealed, in
// stamp order. The first full key refuses the whole admission: the slots
// taken before it are given back, and the refusal names the key. A store
// error is an error, never a pass — a hard cap that fails open on a blip is
// the advisory ceiling this step replaces.
func (p *Publisher) admitCredSlots(ctx context.Context, runID string, wants []credSlotWant) (*credSlotRefusal, error) {
	if len(wants) == 0 {
		return nil, nil
	}
	ledger := store.AsCredSlotStore(p.store)
	if ledger == nil {
		w := wants[0]
		return nil, fmt.Errorf("cloudpublisher: run %s: api-key(%s) %q carries max_concurrent_runs=%d but this run store hosts no credential-slot ledger (store.CredSlotStore) — the cap cannot be enforced", runID, w.provider, w.name, w.max)
	}
	var held []credSlotWant
	for _, w := range wants {
		adm, err := ledger.ReserveCredSlot(ctx, w.fingerprint, runID, w.max)
		if err != nil {
			p.releaseCredSlots(ctx, runID, held)
			return nil, fmt.Errorf("cloudpublisher: run %s: reserve a slot of api-key(%s) %q: %w", runID, w.provider, w.name, err)
		}
		if !adm.Granted {
			p.releaseCredSlots(ctx, runID, held)
			return &credSlotRefusal{want: w, held: adm.Held}, nil
		}
		held = append(held, w)
	}
	return nil, nil
}

// releaseCredSlots gives back the slots of wants for runID — the undo of an
// admission whose launch or resume did not land. Detached from the caller's
// context: a cancelled request must still give the slots back, or the key
// stays short of them until the reconcile of a later refusal evicts the
// holder past its grace.
func (p *Publisher) releaseCredSlots(ctx context.Context, runID string, wants []credSlotWant) {
	if len(wants) == 0 {
		return
	}
	ledger := store.AsCredSlotStore(p.store)
	if ledger == nil {
		return
	}
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), credSlotReleaseTimeout)
	defer cancel()
	for _, w := range wants {
		if err := ledger.ReleaseCredSlot(rctx, w.fingerprint, runID); err != nil {
			p.logger.Warn("cloudpublisher: run %s: release the slot of api-key(%s) %q: %v — the key stays one slot short until the ledger's reconcile evicts the holder", runID, w.provider, w.name, err)
		}
	}
}

// parkRunOnCredSlot writes the park onto the IN-MEMORY launch document, the
// launch path's one persist being what lands it: failed_resumable under
// FailureCredSlotWait, with the refusal as the run's own words. The
// continuation is promoted to retry_armed by the arming that follows
// (armCredSlotRetry), never here — a row that says retry_armed with no retry
// armed is a wait nothing ends.
func parkRunOnCredSlot(r *store.Run, refusal *credSlotRefusal) {
	r.Status = store.RunStatusFailedResumable
	r.FailureCode = store.FailureCredSlotWait
	r.Error = fmt.Sprintf("parked: %v — the run resumes by itself once a slot frees (checked every %s, up to the run's max_wait)", refusal, CredSlotProbeInterval)
	r.ContinuationState = ""
}

// armCredSlotRetry arms the parked run's durable retry: the first ask for a
// slot, one probe interval out. The arming is paid (it is the one that
// creates the retry row); every later ask is the sweeper's free deferral. A
// store with no durable retry cannot wake the run — said loudly, and the run
// stays parked for an operator's resume.
func (p *Publisher) armCredSlotRetry(ctx context.Context, r *store.Run, refusal *credSlotRefusal) error {
	retryStore := store.AsRunRetryStore(p.store)
	if retryStore == nil {
		p.logger.Warn("cloudpublisher: run %s parked (%v), and this run store hosts no durable retry (store.RunRetryStore): nothing resumes it by itself — resume it by hand once a slot frees", r.ID, refusal)
		return nil
	}
	maxAttempts := retrypolicy.DefaultMaxAttempts
	if r.RetryPolicy != nil && r.RetryPolicy.MaxAttempts > 0 {
		maxAttempts = r.RetryPolicy.MaxAttempts
	}
	at := time.Now().UTC().Add(CredSlotProbeInterval)
	scheduled, attempt, err := retryStore.ScheduleRunRetry(ctx, r.ID, at, time.Time{}, store.RetryReasonCredSlot, string(store.FailureCredSlotWait), maxAttempts)
	if err != nil {
		return fmt.Errorf("cloudpublisher: arm the credential-slot retry of parked run %s: %w", r.ID, err)
	}
	if !scheduled {
		return fmt.Errorf("cloudpublisher: run %s parked (%v) but its retry could not be armed: the row is no longer failed_resumable, or its attempt budget is spent", r.ID, refusal)
	}
	p.logger.Warn("cloudpublisher: run %s PARKED — %v; retry armed for %s (attempt %d/%d), then asked again every %s until a slot frees or the run's max_wait passes",
		r.ID, refusal, at.Format(time.RFC3339), attempt, maxAttempts, CredSlotProbeInterval)
	return nil
}
