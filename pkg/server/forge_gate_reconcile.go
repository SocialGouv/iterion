package server

import (
	"context"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/SocialGouv/iterion/pkg/eventbus"
	"github.com/SocialGouv/iterion/pkg/forge"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/trigger"
)

// A run that owes a merge-gate status and dies without posting one leaves the
// required check ABSENT — and an absent required check is indistinguishable
// from one still running. The pull request waits for a context that will never
// arrive, no error appears on the run, the PR or the check, and only someone
// who knows to re-trigger the bot can unstick it.
//
// It is not a rare path. Observed in production twice in one day: a rolling
// deploy drained a review mid-flight (whenever the lame-duck drain is not
// active — or a run outlives its drain window — a rollout cancels in-flight
// runs), and separately a bot bug made the publish step skip on every run.
// Both left pull requests blocked for hours with every other check green.
//
// So the last thing a gating run does, whether it succeeded or not, is leave a
// verdict the PR can display. When the run ends without one, this posts a
// `failure` naming the interruption and the way out. Failure rather than
// success because a review that did not happen has not approved anything, and
// failure rather than silence because a red check with a reason is a state an
// operator can act on.
const gateReconcilerName = "forge-gate-reconcile"

// The two triggers of the same repair. Which one fired decides how loudly a
// declined repair speaks: the event fires once per run, the sweep re-offers
// the same run every minute for its whole lookback.
const (
	gateTriggerEvent = "event"
	gateTriggerSweep = "sweep"
)

// gateInterruptedDescription is what the operator reads on the check. It has
// to carry the remedy: whoever finds it has no other clue about what happened.
const gateInterruptedDescription = "review ended without a verdict — push again or comment the bot's command to re-run"

// gateReasonWrappers are mechanical prefixes the queue and the runner wrap
// around the actual cause of death. The 60-rune budget below cannot survive
// them: the wrapped form of a runner reject reads "max deliveries exhausted:
// runner: prepare repo workspace for 01a0…: runner: reject repo ref: …" and
// truncates before the only part an operator can act on. Stripping is
// cosmetic — the full error stays on the run the status links to.
var gateReasonWrappers = []*regexp.Regexp{
	regexp.MustCompile(`^max deliveries exhausted: `),
	regexp.MustCompile(`^runner: prepare repo workspace for [^:]*: `),
	regexp.MustCompile(`^runner: `),
	regexp.MustCompile(`^git: `),
}

// gateInterruptedDescriptionFor prefixes the remedy with WHY the run died when
// the run doc can say (budget exceeded, provider error, …). GitHub truncates
// commit-status descriptions at 140 characters, so the reason is bounded and
// the remedy — the part the operator cannot reconstruct — keeps priority.
func gateInterruptedDescriptionFor(run *store.Run) string {
	reason := ""
	if run != nil {
		reason = strings.TrimSpace(run.Error)
	}
	if i := strings.IndexByte(reason, '\n'); i >= 0 {
		reason = strings.TrimSpace(reason[:i])
	}
	for stripped := true; stripped; {
		stripped = false
		for _, re := range gateReasonWrappers {
			if next := re.ReplaceAllString(reason, ""); next != reason {
				reason, stripped = next, true
			}
		}
	}
	if reason == "" {
		return gateInterruptedDescription
	}
	// Truncate on a rune boundary: run.Error routinely carries provider prose
	// (accents, ellipses), and a raw byte slice can split a multi-byte rune
	// into invalid UTF-8 that some forges reject with a 422 — turning a long
	// reason into no synthetic status at all.
	const maxReason = 60
	if len(reason) > maxReason {
		cut := maxReason - 1
		for cut > 0 && !utf8.RuneStart(reason[cut]) {
			cut--
		}
		reason = reason[:cut] + "…"
	}
	return gateDiedDescriptionPrefix + reason + ") — push again or comment the bot's command to re-run"
}

// gateDiedDescriptionPrefix opens the reasoned form of the synthetic status.
const gateDiedDescriptionPrefix = "review died ("

// isSyntheticGateInterruption reports whether a gate status description is one
// of the reconciler's own synthetic failures — a review that never happened —
// as opposed to a real verdict a bot posted. The auto-fix lane keys off it:
// there are no findings behind a synthetic failure for a fixer to address.
func isSyntheticGateInterruption(description string) bool {
	d := strings.TrimSpace(description)
	return d == gateInterruptedDescription || strings.HasPrefix(d, gateDiedDescriptionPrefix)
}

// startGateReconciler attaches the reconciler to the event spine. It rides the
// same bus as the notification dispatcher — the shared cloud NATSBus, whose
// queue-group delivery hands each run outcome to exactly one replica, or the
// in-proc bus locally. No bus, or no publish grants in play, means nothing to
// reconcile.
func (s *Server) startGateReconciler() {
	if s == nil || s.forgePublishTokens == nil || s.forgeConnections == nil {
		return
	}
	bus := s.eventsBus()
	if bus == nil {
		return
	}
	cancel, err := s.attachGateReconciler(bus)
	if err != nil {
		if s.logger != nil {
			s.logger.Warn("server: merge-gate reconciler subscribe failed: %v — an interrupted review will leave its required check absent", err)
		}
		return
	}
	s.gateReconcileCancel = cancel
	if s.logger != nil {
		s.logger.Info("server: merge-gate reconciler attached (an interrupted review posts a failure instead of leaving the check absent)")
	}
}

// attachGateReconciler subscribes the reconciler to run-terminal events.
// Paused is deliberately absent: a paused run is expected to resume and post
// its own verdict.
func (s *Server) attachGateReconciler(bus eventbus.Bus) (func(), error) {
	if s == nil || bus == nil {
		return func() {}, nil
	}
	return bus.Subscribe(gateReconcilerName, trigger.Matcher{
		Sources: []trigger.Source{trigger.SourceRun},
		Kinds: []string{
			trigger.KindRunFinished,
			trigger.KindRunFailed,
			trigger.KindRunCancelled,
		},
	}, s.reconcileGateForRun)
}

// reconcileGateForRun is the eventbus handler. It is deliberately quiet about
// runs that owed nothing: most runs hold no publish grant at all.
func (s *Server) reconcileGateForRun(ctx context.Context, ev trigger.Event) error {
	return s.reconcileGateForRunID(ctx, strings.TrimSpace(ev.Subject.ID), gateTriggerEvent)
}

// reconcileGateForRunID is the repair itself, reachable from either of its two
// triggers: the run-outcome event (immediate) and the periodic sweep (the net
// under it). `via` names which one, because a repair that only ever fires from
// the sweep is a broken event path, and the two are indistinguishable in the
// logs otherwise.
//
// Idempotent by construction: it re-reads the live status on the head and
// stands down unless the check is still unanswered, so the two triggers racing
// on the same run costs one redundant read.
func (s *Server) reconcileGateForRunID(ctx context.Context, runID, via string) error {
	if runID == "" || s.cfg.Store == nil || s.forgePublishTokens == nil || s.forgeConnections == nil {
		return nil
	}
	run, err := s.cfg.Store.LoadRun(store.WithoutTenantFilter(ctx), runID)
	if err != nil || run == nil {
		return nil
	}
	// A paused run is expected to resume and post its own verdict.
	if run.Status == store.RunStatusPausedWaitingHuman || run.Status == store.RunStatusPausedOperator {
		return nil
	}
	// A resumable failure is only "not dead" when something will actually
	// resume it. The runner arms a durable retry for usage-window failures
	// (persisted BEFORE the outcome event fires — pkg/runner/loop.go parks
	// first, then fires), and that armed retry is the whole promise. Every
	// other failed_resumable — budget exceeded, retries exhausted, a plain
	// execution failure — sits until a human notices, which with an absent
	// required check is never. Observed in production 2026-08-03: a Vetty run
	// died on its own duration budget mid-audit and the PR it gated stayed
	// silently unmergeable. Those runs ARE dead; reconcile them.
	if run.Status == store.RunStatusFailedResumable &&
		run.RetryState != nil && run.RetryState.RetryAfter != nil {
		return nil
	}

	token := runInputString(run, forgePublishVarToken)
	prURL := runInputString(run, "pr_url")
	if token == "" || prURL == "" {
		return nil // not a gating run
	}

	// Past this line the run held a publish grant and died: it MAY owe a
	// verdict, and every remaining branch that declines to post one is a
	// decision to leave a pull request waiting. Those branches used to return
	// bare nil, so a PR stuck behind an absent check produced not one line
	// anywhere — the failure and the healthy "owed nothing" path were the same
	// silence, and four blocked PRs could not be told apart from four runs
	// that never gated anything. Naming the reason is what makes the next
	// occurrence a grep instead of an investigation.
	//
	// Warn on the EVENT path only. The sweep re-offers the same run every
	// minute for the whole lookback, so a run sitting in a permanent abstain
	// branch — a lost grant, an unreachable forge — would log the identical
	// line ~60 times an hour per replica and bury the branches that carry new
	// information, defeating the point of naming the reason at all. The event
	// fires once per run, which is exactly one line per occurrence.
	abstain := func(format string, args ...any) error {
		if s.logger != nil {
			msg := "forge gate: run %s (via %s) held a grant on %s but posts nothing: " + format
			args = append([]any{runID, via, prURL}, args...)
			if via == gateTriggerSweep {
				s.logger.Debug(msg, args...)
			} else {
				s.logger.Warn(msg, args...)
			}
		}
		return nil
	}

	grant, ok := s.forgePublishTokens.lookup(token)
	if !ok {
		// The grant outlives the longest retry wait by construction
		// (forgePublishDefaultTTL), so reaching here means the run sat dead
		// longer than that, or the token store lost it.
		return abstain("its publish grant is expired or revoked")
	}

	// Holding a grant is NOT owing a verdict. The server mints one for any bot
	// launched with a pr_url — the brancher, the docs amender, the implementer
	// — and a repo's gate context is deliberately SHARED between the bots that
	// gate it. Reconciling on "has a token" would let a bot that owes nothing
	// paint another bot's required check red.
	//
	// The anchor is therefore the context the OPERATOR pinned for this repo
	// (`launch_vars.gate_context` on the integration), which is also what a
	// repo must do to make a check required across several bots. Nothing
	// learned, nothing remembered: a first attempt inferred it from contexts
	// the server had posted before, which is empty in exactly the two
	// situations this repair exists for — a bot whose publish step never
	// succeeds, and a rollout that restarts every replica.
	gateCtx := runInputString(run, "gate_context")
	if gateCtx == "" {
		return nil
	}

	host, repo, number, err := forge.ParsePullURL(prURL)
	if err != nil {
		return abstain("its pr_url does not parse: %v", err)
	}
	// pr_url is a LAUNCH VAR — whoever launched the run chose it, and
	// injectForgePublishVars deliberately honours a caller-pinned token. So
	// the grant's scope has to be re-enforced here exactly as the publish
	// endpoint enforces it, or a run could aim a status at any repo the
	// team's connection reaches (for a GitHub App installation, typically the
	// whole org). A red check is not a merge, but it is precisely the blast
	// radius the grant exists to bound.
	if !strings.EqualFold(strings.TrimSpace(repo), strings.TrimSpace(grant.Repo)) {
		return abstain("its grant covers %s — refusing to post outside the grant's repo", grant.Repo)
	}
	conn, err := s.forgeConnections.Get(store.WithoutTenantFilter(ctx), grant.ConnectionID)
	if err != nil {
		return abstain("its connection %s is unreadable: %v", grant.ConnectionID, err)
	}
	if conn.TenantID != grant.TeamID {
		return abstain("its connection %s belongs to another tenant", grant.ConnectionID)
	}
	if connHost := hostOfURL(conn.BaseURL()); connHost == "" || !strings.EqualFold(connHost, host) {
		return abstain("its connection points at %q, not %q", hostOfURL(conn.BaseURL()), host)
	}
	gc, err := s.gateClientFor(ctx, conn)
	if err != nil {
		return abstain("the forge client for %s is unreachable: %v", repo, err)
	}
	if gc == nil {
		return abstain("provider %s cannot post commit statuses", conn.Provider)
	}

	pr, err := gc.GetPullRequest(ctx, repo, number)
	if err != nil {
		return abstain("the pull request is unreadable: %v", err)
	}
	if strings.TrimSpace(pr.HeadSHA) == "" {
		return abstain("the forge returned no head sha")
	}
	// Only the revision this run was reviewing. Between its start and its
	// death the head routinely moves — the author pushes a fix, a brancher
	// commits, review_on_sync already has a fresh review in flight. Painting
	// the CURRENT head red would report on a commit this run never read, and
	// a newer head is a newer review's responsibility.
	//
	// REQUIRED, not best-effort: a run that cannot name the revision it
	// reviewed cannot speak for any of them. (An earlier version treated an
	// absent value as "no constraint", which made the guard vacuous — nothing
	// set the var at the time, so it never once fired outside its own test.)
	//
	// Not routed through abstain(): a head that moved while the review ran is
	// the ORDINARY case (every re-push does it), and the newer head already
	// has its own review claiming its own check. Warning on it would bury the
	// branches that mean something under routine noise.
	reviewed := runInputString(run, "head_sha")
	if reviewed == "" || !strings.EqualFold(reviewed, pr.HeadSHA) {
		if s.logger != nil {
			s.logger.Debug("forge gate: run %s reviewed %s but %s is now at %s — leaving the newer head to its own review",
				runID, shortSHA(reviewed), prURL, shortSHA(pr.HeadSHA))
		}
		return nil
	}
	// The forge is the authority on whether the verdict landed — not any
	// bookkeeping of ours, which a second replica would not share and a
	// restart would lose. Only a REAL verdict stands the reconciler down: its
	// own synthetic failure does not, or the second death on a head — the
	// relaunched run dying too, exactly the case that must escalate to the
	// board — would find the marker from the first death, read it as "already
	// posted", and go silent right where the recovery runs out.
	gate, readable, err := gateStatusOn(ctx, gc, repo, pr.HeadSHA, gateCtx)
	if err != nil {
		return abstain("the statuses on %s@%s are unreadable: %v", repo, shortSHA(pr.HeadSHA), err)
	}
	if !readable {
		// Cannot tell absent from posted: overwriting a real success with a
		// synthetic failure is a worse outcome than leaving a stuck PR stuck.
		return abstain("provider %s cannot list commit statuses, so a verdict cannot be told from an absence", conn.Provider)
	}
	// Whether this run still owes an answer is decided by WHO the status on the
	// head belongs to, not merely by its state. Every status iterion writes
	// carries the run it speaks for in its target URL, which is what makes the
	// question answerable across replicas with no shared bookkeeping.
	runURL := gateRunURL(strings.TrimRight(strings.TrimSpace(s.cfg.PublicURL), "/"), runID)
	switch {
	case gate.State == "":
		// Nothing on the head: this run owes the answer.

	case !isGateInFlight(gate) && !isSyntheticGateInterruption(gate.Description):
		return nil // a real verdict — never overwrite one

	case isGateInFlight(gate):
		// A CLAIM is not an answer, so a run that died still holding its OWN
		// claim is exactly what this repair exists for. But a claim belonging
		// to a DIFFERENT run means someone else is actively reviewing this
		// head right now — the recovery run this repair just launched, or a
		// second bot sharing the repo's one gate context. Painting "review
		// died" over it reports a death that did not happen, and re-enters a
		// relaunch whose idempotency key is already spent, which files a board
		// card telling a human the automation is out of moves while the other
		// run is alive and working.
		if !gateStatusSpeaksFor(gate, runURL) {
			if s.logger != nil {
				s.logger.Debug("forge gate: run %s died on %s@%s but another run holds the in-flight claim — leaving the live review alone",
					runID, repo, shortSHA(pr.HeadSHA))
			}
			return nil
		}

	default: // a synthetic interruption
		// The same run offered twice — the event and the sweep racing, or the
		// sweep re-reading its window every minute for an hour — is already
		// answered.
		if gateStatusSpeaksFor(gate, runURL) {
			return nil
		}
		// Unattributable: with no PublicURL configured every status is written
		// with an empty target URL, so "mine" and "another run's" are the same
		// observation. Repairing then re-posts and re-enters the recovery on
		// every sweep pass — dozens of forge writes and false escalations per
		// stuck run. Standing down costs the second-death escalation only, and
		// only on a deployment that has not set PublicURL.
		if runURL == "" {
			return abstain("a synthetic failure is already on %s@%s and PublicURL is unset, so it cannot be told from this run's own — set PublicURL to enable second-death escalation",
				repo, shortSHA(pr.HeadSHA))
		}
		// ANOTHER dead run's synthetic failure already marks this head. One
		// marker is enough: re-posting would only re-point the status's target
		// URL at THIS run, and the next sweep pass on the other run would
		// point it back — observed in production as 116 status writes on one
		// head in 15 minutes, two dead runs alternating every sweep tick
		// (buildkit-operator#21, 2026-08-17). The relaunch tail still runs,
		// because a second death on the head must escalate rather than
		// mistake the first death's marker for an answer; its idempotency
		// claim and the board card's (PR, head) dedup make repeat offers
		// no-ops.
		s.relaunchDeadGateRun(ctx, deadGateRun{
			run: run, grant: grant, conn: conn, gc: gc,
			repo: repo, number: number, pr: pr, gateCtx: gateCtx, prURL: prURL,
		})
		return nil
	}

	st := forge.CommitStatus{
		State:       forge.CommitStateFailure,
		Context:     gateCtx,
		Description: gateInterruptedDescriptionFor(run),
		TargetURL:   runURL,
	}
	if err := gc.SetCommitStatus(ctx, repo, pr.HeadSHA, st); err != nil {
		if s.logger != nil {
			s.logger.Error("forge gate: run %s left %s on %s unanswered and the failure status could not be posted: %v — that PR is blocked on a check that will never arrive",
				runID, gateCtx, prURL, err)
		}
		return nil
	}
	if s.logger != nil {
		s.logger.Info("forge gate: run %s ended without a verdict; posted %s=failure on %s so the PR is not stuck waiting", runID, gateCtx, prURL)
	}
	// The failure status is the truth; the relaunch is the recovery. Posted
	// first so the PR is never blind even when the relaunch cannot happen
	// (local mode, quota, hold label) — the fresh run overwrites the failure
	// with its own verdict when it completes.
	s.relaunchDeadGateRun(ctx, deadGateRun{
		run: run, grant: grant, conn: conn, gc: gc,
		repo: repo, number: number, pr: pr, gateCtx: gateCtx, prURL: prURL,
	})
	return nil
}

// gateStatusSpeaksFor reports whether a status iterion wrote belongs to the
// run at runURL. Every status this package posts — the in-flight claim and the
// synthetic failure alike — points at the run it speaks for, so ownership is
// readable straight off the forge, with no bookkeeping a second replica would
// not share and a restart would lose.
//
// False when runURL is empty (no PublicURL configured) or the status carries
// no target URL: ownership is then unknowable, and each caller decides which
// way that ambiguity is safe to resolve rather than having a bare string
// compare silently pick one.
func gateStatusSpeaksFor(st forge.CommitStatus, runURL string) bool {
	target := strings.TrimSpace(st.TargetURL)
	if runURL == "" || target == "" {
		return false
	}
	return strings.EqualFold(target, runURL)
}

// gateRunURL points the check at the run that owed it, so the operator lands
// on the evidence rather than on a bare red cross.
func gateRunURL(base, runID string) string {
	if base == "" {
		return ""
	}
	return base + "/runs/" + runID
}

// runInputString reads one launch input as a trimmed string.
func runInputString(run *store.Run, key string) string {
	if run == nil || run.Inputs == nil {
		return ""
	}
	v, ok := run.Inputs[key]
	if !ok {
		return ""
	}
	sv, ok := v.(string)
	if !ok {
		return ""
	}
	return strings.TrimSpace(sv)
}
