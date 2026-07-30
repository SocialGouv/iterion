package server

import (
	"context"
	"strings"
	"sync"

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
// deploy drained a review mid-flight (the lame-duck drain is not deployed, so
// a rollout cancels in-flight runs), and separately a bot bug made the publish
// step skip on every run. Both left pull requests blocked for hours with every
// other check green.
//
// So the last thing a gating run does, whether it succeeded or not, is leave a
// verdict the PR can display. When the run ends without one, this posts a
// `failure` naming the interruption and the way out. Failure rather than
// success because a review that did not happen has not approved anything, and
// failure rather than silence because a red check with a reason is a state an
// operator can act on.
const gateReconcilerName = "forge-gate-reconcile"

// gateInterruptedDescription is what the operator reads on the check. It has
// to carry the remedy: whoever finds it has no other clue about what happened.
const gateInterruptedDescription = "review ended without a verdict — push again or comment the bot's command to re-run"

// startGateReconciler attaches the reconciler to the event spine. It rides the
// same bus as the notification dispatcher — the shared cloud NATSBus, whose
// queue-group delivery hands each run outcome to exactly one replica, or the
// in-proc bus locally. No bus, or no publish grants in play, means nothing to
// reconcile.
func (s *Server) startGateReconciler() {
	if s == nil || s.forgePublishTokens == nil || s.forgeConnections == nil {
		return
	}
	bus := s.cfg.EventsBus
	if bus == nil && s.triggerCoord != nil {
		bus = s.triggerCoord.Bus()
	}
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
	runID := strings.TrimSpace(ev.Subject.ID)
	if runID == "" || s.cfg.Store == nil || s.forgePublishTokens == nil || s.forgeConnections == nil {
		return nil
	}
	run, err := s.cfg.Store.LoadRun(store.WithoutTenantFilter(ctx), runID)
	if err != nil || run == nil {
		return nil
	}
	// A resumable failure is not a dead run: the cloud runner republishes the
	// outcome on every delivery attempt, before it decides to retry, and
	// `iterion resume` picks one up locally. Such a run is still expected to
	// post its own verdict — the same reason paused is not reconciled.
	if run.Status == store.RunStatusFailedResumable || run.Status == store.RunStatusPausedWaitingHuman || run.Status == store.RunStatusPausedOperator {
		return nil
	}

	token := runInputString(run, forgePublishVarToken)
	prURL := runInputString(run, "pr_url")
	if token == "" || prURL == "" {
		return nil // not a gating run
	}
	grant, ok := s.forgePublishTokens.lookup(token)
	if !ok {
		return nil // grant already expired or revoked; nothing to speak for
	}

	// Holding a grant is NOT owing a verdict. The server mints one for any bot
	// launched with a pr_url — the brancher, the docs amender, the implementer
	// — and a repo's gate context is deliberately SHARED between the bots that
	// gate it. Reconciling on "has a token" would let a bot that owes nothing
	// paint another bot's required check red.
	//
	// What identifies an owing run is that this workflow has already gated
	// this repo: the server saw it post that context itself. Learned from
	// data — the engine never knows a bot by name.
	gateCtx := s.lastGateContextFor(grant.Repo, grant.Bot)
	if gateCtx == "" {
		return nil
	}
	if pinned := runInputString(run, "gate_context"); pinned != "" {
		// An operator pin overrides what we learned: it is the context the
		// repo actually gates on today.
		gateCtx = pinned
	}

	host, repo, number, err := forge.ParsePullURL(prURL)
	if err != nil {
		return nil
	}
	// pr_url is a LAUNCH VAR — whoever launched the run chose it, and
	// injectForgePublishVars deliberately honours a caller-pinned token. So
	// the grant's scope has to be re-enforced here exactly as the publish
	// endpoint enforces it, or a run could aim a status at any repo the
	// team's connection reaches (for a GitHub App installation, typically the
	// whole org). A red check is not a merge, but it is precisely the blast
	// radius the grant exists to bound.
	if !strings.EqualFold(strings.TrimSpace(repo), strings.TrimSpace(grant.Repo)) {
		if s.logger != nil {
			s.logger.Warn("forge gate: run %s carries a grant for %s but a pr_url on %s — refusing to post outside the grant's repo",
				runID, grant.Repo, repo)
		}
		return nil
	}
	conn, err := s.forgeConnections.Get(store.WithoutTenantFilter(ctx), grant.ConnectionID)
	if err != nil || conn.TenantID != grant.TeamID {
		return nil
	}
	if connHost := hostOfURL(conn.BaseURL()); connHost == "" || !strings.EqualFold(connHost, host) {
		return nil
	}
	gc, err := s.gateClientFor(ctx, conn)
	if err != nil || gc == nil {
		if s.logger != nil {
			s.logger.Warn("forge gate: cannot reach %s to reconcile run %s: %v", repo, runID, err)
		}
		return nil
	}

	pr, err := gc.GetPullRequest(ctx, repo, number)
	if err != nil || strings.TrimSpace(pr.HeadSHA) == "" {
		return nil
	}
	// Only the revision this run was reviewing. Between its start and its
	// death the head routinely moves — the author pushes a fix, a brancher
	// commits, review_on_sync already has a fresh review in flight. Painting
	// the CURRENT head red would report on a commit this run never read, and
	// a newer head is a newer review's responsibility.
	if reviewed := runInputString(run, "head_sha"); reviewed != "" && !strings.EqualFold(reviewed, pr.HeadSHA) {
		return nil
	}
	// The forge is the authority on whether the verdict landed — not any
	// bookkeeping of ours, which a second replica would not share and a
	// restart would lose.
	posted, err := gateAlreadyPosted(ctx, gc, repo, pr.HeadSHA, gateCtx)
	if err != nil {
		if s.logger != nil {
			s.logger.Warn("forge gate: cannot read statuses on %s@%s: %v", repo, pr.HeadSHA[:7], err)
		}
		return nil
	}
	if posted {
		return nil
	}

	st := forge.CommitStatus{
		State:       forge.CommitStateFailure,
		Context:     gateCtx,
		Description: gateInterruptedDescription,
		TargetURL:   gateRunURL(strings.TrimRight(strings.TrimSpace(s.cfg.PublicURL), "/"), runID),
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
	return nil
}

// gateAlreadyPosted reports whether ctxName is already present on sha.
func gateAlreadyPosted(ctx context.Context, gc forgeGateClient, repo, sha, ctxName string) (bool, error) {
	lister, ok := gc.(forge.CommitStatusLister)
	if !ok {
		// Without a read capability we cannot tell an absent verdict from a
		// posted one. Say "posted": overwriting a real success with a
		// failure is a worse outcome than leaving a stuck PR stuck.
		return true, nil
	}
	sts, err := lister.ListCommitStatuses(ctx, repo, sha)
	if err != nil {
		return false, err
	}
	for _, st := range sts {
		if strings.EqualFold(strings.TrimSpace(st.Context), ctxName) {
			return true, nil
		}
	}
	return false, nil
}

// ---------------------------------------------------------------------------
// Learned gate contexts
// ---------------------------------------------------------------------------

// gateContextMemory remembers, per repo, the last context a gate was posted
// under. It is the only way the server can name the check a dead run owed:
// the context is declared in the .bot, and a run that died may never have
// spoken to the server at all.
//
// Per-replica and lossy on restart, deliberately: it only ever makes the
// reconciler abstain, never post the wrong context.
type gateContextMemory struct {
	mu sync.RWMutex
	by map[string]string // "<repo>\x00<workflow>" -> context
}

func gateMemoryKey(repo, workflow string) string {
	return strings.TrimSpace(repo) + "\x00" + strings.TrimSpace(workflow)
}

func (m *gateContextMemory) remember(repo, workflow, ctxName string) {
	if strings.TrimSpace(repo) == "" || strings.TrimSpace(workflow) == "" || strings.TrimSpace(ctxName) == "" {
		return
	}
	m.mu.Lock()
	if m.by == nil {
		m.by = map[string]string{}
	}
	m.by[gateMemoryKey(repo, workflow)] = strings.TrimSpace(ctxName)
	m.mu.Unlock()
}

func (m *gateContextMemory) get(repo, workflow string) string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.by[gateMemoryKey(repo, workflow)]
}

func (s *Server) rememberGateContext(repo, workflow, ctxName string) {
	if s == nil {
		return
	}
	s.gateContexts.remember(repo, workflow, ctxName)
}

func (s *Server) lastGateContextFor(repo, workflow string) string {
	if s == nil {
		return ""
	}
	return s.gateContexts.get(repo, workflow)
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
