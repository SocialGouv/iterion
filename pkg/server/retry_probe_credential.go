package server

import (
	"context"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/cloud/metrics"
	"github.com/SocialGouv/iterion/pkg/retrypolicy"
	"github.com/SocialGouv/iterion/pkg/store"
	mongostore "github.com/SocialGouv/iterion/pkg/store/mongo"
)

// Credential probe: the pull-forward probe of a wait its usage window still
// holds, paid ONCE per credential window instead of once per parked run.
//
// A provider usage window belongs to a credential, not to a run: every run
// parked on the same forfait waits on the same wall, and asking the provider
// whether that wall fell needs one of them, not all. Probing each parked run
// on its own priced one wall at one pod an hour PER RUN — and it relaunched
// every one of them before the date the pull request had been told (bko#82,
// #3127). So for a wait whose record names a window still ahead
// (retrypolicy.UsageWindowNotBefore), this pass groups the parked runs by the
// credentials their park names (store.UsageWindowPark.Credential) and:
//
//   - pulls ONE of them forward per probe floor — the canary, the wait pulled
//     least recently (RunRetryState.LastProbeAt, never-pulled first), then the
//     earliest-parked (ScheduledAt), then the run id: keys the records carry,
//     never the order a scan answered in, so two replicas and a restarted one
//     pick the same run, and a canary whose resume never reaches the provider
//     (a transient refusal before publish, its tenant's quota) hands the next
//     probe to another run instead of taking every one of them;
//   - when the canary's resume gets past the wall (a hand top-up, a wall that
//     fell early), releases every other run parked on the same credentials in
//     the same pass;
//   - when it meets the wall again, moves nobody: the free re-arm puts the
//     canary back on its announced instant and its pull stamp holds the
//     window's pace for a floor.
//
// Everything is read from the run records the scan returns, so the decision
// is the same on every replica and survives a restart: the PACE is the latest
// pull any of the window's rows carries (last_probe_at), the BUDGET the pulls
// its rows carry on the walls they wait on, summed (probe_count, which belongs
// to a wall — summed because the canary rotates, so no one row carries the
// window's count), and a probe's ANSWER the state of the run it pulled —
// found through the marker its pull left, read through its wall door
// (RunRetryState.AnnouncedProbeFrom), which only an arming consumes. The
// window itself is the credentials the wall was met on, copied by the paid
// arming (RunRetryState.WallCredentials) — never the run's current stamp,
// which the canary's own resume re-writes. Every write is the store's CAS on
// the instant the scan read: a replica that lost the race moves on.

const (
	// EnvRetryCredentialProbeScan overrides how many rows the credential pass
	// reads: every armed usage-window wait and every marked probe,
	// platform-wide. A window cut by the bound can lose the row that carries
	// its pace or its canary and be probed again within its floor, so a pass
	// that comes back full says so — and this raises the bound.
	EnvRetryCredentialProbeScan = "ITERION_RETRY_CREDENTIAL_PROBE_SCAN"
	// defaultCredentialProbeScan is the bound a deployment gets unset.
	defaultCredentialProbeScan = 500
)

// credentialProbeScan resolves the pass's scan bound: the operator's
// override when it parses to a positive count, the default otherwise.
func credentialProbeScan() int {
	if raw := strings.TrimSpace(envOperator(EnvRetryCredentialProbeScan)); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			return n
		}
	}
	return defaultCredentialProbeScan
}

// credentialProbeLookback is how far back a pull still speaks for its
// credential window: long enough for its answer (store.ProbeMarkerTTL, the
// time a pulled resume takes to reach the provider) and then for one floor of
// pace after it.
func credentialProbeLookback(floor time.Duration) time.Duration {
	return store.ProbeMarkerTTL + floor
}

// credentialWindow is one credential window as its records stand.
type credentialWindow struct {
	key string
	// waits are the runs parked on the window that it still holds — the
	// canary is chosen among them, and the release moves them.
	waits []mongostore.RetryDueRef
	// probes are the runs a pull still marks while the wall it set aside is
	// ahead (RunRetryState.AnnouncedProbeFrom): pulled forward and not yet
	// re-armed. Their state is the answer of the probe.
	probes []mongostore.RetryDueRef
}

// credentialWindowsOf groups the scan's rows by credential window. Rows that
// name no credential belong to no window, and an unmarked wait its window no
// longer holds is the due pass's (or the per-run probe's).
func credentialWindowsOf(rows []mongostore.RetryDueRef, now time.Time) []*credentialWindow {
	byKey := map[string]*credentialWindow{}
	for _, row := range rows {
		key := row.RetryState.CredentialWindow()
		if key == "" {
			continue
		}
		w := byKey[key]
		if w == nil {
			w = &credentialWindow{key: key}
			byKey[key] = w
		}
		// A pull whose set-aside wall is still ahead is a probe of the
		// window: its run's state is the answer. Once that wall is behind
		// us the wait is over for every run on it, and no answer is owed.
		if row.RetryState.AnnouncedProbeFrom(now) != nil {
			w.probes = append(w.probes, row)
			continue
		}
		if _, waits := retrypolicy.UsageWindowNotBefore(row.Record(), now); waits {
			w.waits = append(w.waits, row)
		}
	}
	out := make([]*credentialWindow, 0, len(byKey))
	for _, w := range byKey {
		out = append(out, w)
	}
	slices.SortFunc(out, func(a, b *credentialWindow) int { return strings.Compare(a.key, b.key) })
	return out
}

// credentialProbeAnswer is what a pulled run's record says about the wall.
type credentialProbeAnswer int

const (
	// probeUnanswered: the pulled run has not reached the provider yet, or
	// may still meet the wall — its pull is younger than ProbeMarkerTTL and
	// it is neither past the wall nor parked again.
	probeUnanswered credentialProbeAnswer = iota
	// probeGotThrough: the pulled run is past the wall — it has run longer
	// than a refused resume takes to come back, reached a human gate, or
	// finished.
	probeGotThrough
	// probeNoAnswer: the pulled run ended in a way that says nothing about
	// the wall (failed, cancelled, paused by an operator, a resume that never
	// started). It still paces the window: its pull was spent.
	probeNoAnswer
)

// answerOf reads a marked run's answer. A pull that met the wall again is not
// here at all: the free re-arm consumes the marker, and the run is a wait of
// the window again, its pull stamp holding the pace.
//
// A marker with no pull stamp answers nothing: PullRunRetry writes the pair in
// one update, so only a row older than the stamp carries one, and the age of
// its pull is unknown.
func answerOf(row mongostore.RetryDueRef, now time.Time) credentialProbeAnswer {
	if row.RetryState.LastProbeAt == nil {
		return probeNoAnswer
	}
	young := now.Before(row.RetryState.LastProbeAt.Add(store.ProbeMarkerTTL))
	switch row.Status {
	case store.RunStatusFinished, store.RunStatusPausedWaitingHuman:
		return probeGotThrough
	case store.RunStatusRunning:
		if young {
			return probeUnanswered
		}
		return probeGotThrough
	case store.RunStatusQueued, store.RunStatusFailedResumable:
		if young {
			return probeUnanswered
		}
		return probeNoAnswer
	default:
		return probeNoAnswer
	}
}

// parkedAt is the instant a wait last met the wall after work — its last paid
// park (RunRetryState.ScheduledAt), which a free re-arm, a hold and a pull all
// leave alone. Zero on a row armed before the field existed, which sorts it
// first: the oldest wait there is.
func parkedAt(row mongostore.RetryDueRef) time.Time {
	if row.RetryState == nil || row.RetryState.ScheduledAt == nil {
		return time.Time{}
	}
	return row.RetryState.ScheduledAt.UTC()
}

// lastPulledAt is the instant a wait was last pulled forward — as a canary or
// a release — zero when never.
func lastPulledAt(row mongostore.RetryDueRef) time.Time {
	if row.RetryState == nil || row.RetryState.LastProbeAt == nil {
		return time.Time{}
	}
	return row.RetryState.LastProbeAt.UTC()
}

// canaryOrder is the stable order a credential window picks its probe in:
// the wait pulled least recently first (never pulled before any), then the
// earliest-parked, then the run id.
func canaryOrder(a, b mongostore.RetryDueRef) int {
	if c := lastPulledAt(a).Compare(lastPulledAt(b)); c != 0 {
		return c
	}
	if c := parkedAt(a).Compare(parkedAt(b)); c != 0 {
		return c
	}
	return strings.Compare(a.ID, b.ID)
}

// sweepCredentialProbes runs the credential half of the probe pass and
// reports how many canaries it pulled, which count against the pass's batch.
// Releases do not: they are the answer of a probe already paid for.
func (s *Server) sweepCredentialProbes(ctx context.Context, retryStore store.RunRetryStore, lister retryDueLister, bounds retryProbeBounds, now time.Time) int {
	since := now.Add(-credentialProbeLookback(bounds.floor))
	scan := credentialProbeScan()
	rows, err := lister.ListRunsForCredentialProbe(store.WithoutTenantFilter(ctx), since, scan)
	if err != nil {
		s.warnf("retry sweeper: credential probe scan: %v", err)
		return 0
	}
	if len(rows) >= scan {
		s.warnf("retry sweeper: the credential probe read %d rows, its bound — a credential window cut by it can be probed twice within %s; raise %s", len(rows), bounds.floor, EnvRetryCredentialProbeScan)
	}
	pulled := 0
	for _, w := range credentialWindowsOf(rows, now) {
		released, outstanding := s.releaseCredentialWindow(ctx, retryStore, w, now)
		if released > 0 || outstanding || pulled >= retryProbeBatch {
			continue
		}
		if s.probeCredentialWindow(ctx, retryStore, w, bounds, now) {
			pulled++
		}
	}
	return pulled
}

// releaseCredentialWindow moves the window's waits when a probe of it got
// past the wall, and reports how many it moved and whether a probe of the
// window is still out (no new probe is taken while one is).
//
// A probe pulled at T answers for the waits parked at or before T and not
// pulled since: a run that met the wall AFTER the pull carries newer news
// than the probe, and a run already pulled since — released, or a later
// probe — has its own answer coming. That is also what keeps one answer from
// releasing the same wait twice.
//
// The release is a pull that records its answer (RunRetryState.ReleasedBy):
// the wait is moved to now with the provider's instant held aside, and a wait
// whose resume still meets the wall is re-armed free on that instant — a
// release on a wrong answer costs a pod, never the run's life. The recorded
// answer is what keeps the release from being read as a probe's guess: the
// window no longer holds the run however late its resume comes (behind the
// shared circuit), a transient refusal of that resume — a mass release is
// what trips the tenant's concurrency cap — re-arms it on the ordinary
// backoff instead of the provider's announced instant (reArmRetry), and its
// resume is metered like any launch.
func (s *Server) releaseCredentialWindow(ctx context.Context, retryStore store.RunRetryStore, w *credentialWindow, now time.Time) (released int, outstanding bool) {
	var answeredAt time.Time
	var answeredBy string
	for _, p := range w.probes {
		switch answerOf(p, now) {
		case probeUnanswered:
			outstanding = true
		case probeGotThrough:
			if at := p.RetryState.LastProbeAt.UTC(); at.After(answeredAt) {
				answeredAt, answeredBy = at, p.ID
			}
		}
	}
	if answeredAt.IsZero() {
		return 0, outstanding
	}
	for _, wait := range w.waits {
		if parkedAt(wait).After(answeredAt) {
			continue
		}
		if last := wait.RetryState.LastProbeAt; last != nil && !last.Before(answeredAt) {
			continue
		}
		runCtx := store.WithIdentity(ctx, wait.TenantID, "retry-sweeper")
		announced := wait.RetryAfter()
		ok, err := retryStore.ReleaseRunRetry(runCtx, wait.ID, announced, now, answeredBy)
		if err != nil {
			s.warnf("retry sweeper: release %s: %v", wait.ID, err)
			continue
		}
		if !ok {
			continue
		}
		released++
		s.countRetryProbe(metrics.RetryProbePulled)
		s.auditRetry(wait, "run.retry.released", map[string]any{
			"announced_retry_after": announced,
			"credential_window":     w.key,
			"released_by":           answeredBy,
		})
		s.infof("retry sweeper: run %s (tenant %s) released — run %s, the probe of the same credentials pulled at %s, got past the provider's wall; its announced reset was %s",
			wait.ID, wait.TenantID, answeredBy, answeredAt.Format(time.RFC3339), announced.Format(time.RFC3339))
	}
	return released, outstanding
}

// probeCredentialWindow pulls the window's canary when its pace and budget
// allow, and reports whether a pull landed.
//
// The budget is the window's: the pulls its rows carry on the walls they wait
// on, summed — the canary rotates, so the window's count is spread over its
// rows and the largest of them would let it probe once per budget PER RUN.
// Releases are pulls too and count: each is a pod relaunched into the wall.
func (s *Server) probeCredentialWindow(ctx context.Context, retryStore store.RunRetryStore, w *credentialWindow, bounds retryProbeBounds, now time.Time) bool {
	var last time.Time
	spent := 0
	for _, rows := range [][]mongostore.RetryDueRef{w.waits, w.probes} {
		for _, row := range rows {
			if at := row.RetryState.LastProbeAt; at != nil && at.After(last) {
				last = at.UTC()
			}
			spent += row.RetryState.ProbeCount
		}
	}
	if !last.IsZero() && now.Before(last.Add(bounds.floor)) {
		s.debugf("retry sweeper: credential window %s probed at %s — next probe after %s", w.key, last.Format(time.RFC3339), last.Add(bounds.floor).Format(time.RFC3339))
		return false
	}
	if spent >= bounds.budget {
		s.debugf("retry sweeper: credential window %s spent its %d probes — its runs wait for their window", w.key, bounds.budget)
		return false
	}
	candidates := make([]mongostore.RetryDueRef, 0, len(w.waits))
	for _, wait := range w.waits {
		// A wait coming due within the floor is the due pass's: probing it
		// buys nothing. A live lease means a replica is acting on it.
		if !wait.RetryAfter().After(now.Add(bounds.floor)) || wait.Leased(now) {
			continue
		}
		candidates = append(candidates, wait)
	}
	if len(candidates) == 0 {
		return false
	}
	canary := slices.MinFunc(candidates, canaryOrder)
	return s.probeRetry(ctx, retryStore, canary, bounds, now, w.key)
}

// usageWindowProbeCadence is what the pull-request notice says about a parked
// run beside the date it promises: how often the provider is asked whether
// the wall fell early and at most how many times (the probe budget of one
// wall: the window stops asking once it is spent, and the run then waits for
// its date), or false when nothing asks before the window — probing off, the
// window not holding the run, or a run that names no credential (no
// credential window speaks for it) under the default.
func usageWindowProbeCadence(run *store.Run, now time.Time) (cadence time.Duration, budget int, perRun, ok bool) {
	bounds := retryProbeBoundsFromEnv()
	if bounds.budget <= 0 {
		return 0, 0, false, false
	}
	if _, waits := retrypolicy.UsageWindowNotBefore(run, now); !waits {
		return 0, 0, false, false
	}
	if bounds.beforeWindow {
		return bounds.floor, bounds.budget, true, true
	}
	if run.UsageWindowPark().Credential == "" {
		return 0, 0, false, false
	}
	return bounds.floor, bounds.budget, false, true
}
