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
	token := runInputString(run, forgePublishVarToken)
	prURL := runInputString(run, "pr_url")
	if token == "" || prURL == "" {
		return nil // not a gating run
	}
	grant, ok := s.forgePublishTokens.lookup(token)
	if !ok {
		return nil // grant already expired or revoked; nothing to speak for
	}

	gateCtx := runInputString(run, "gate_context")
	if gateCtx == "" {
		// The bot's own default never reaches the server — it lives in the
		// .bot, not in the launch vars. What the server DOES know is the
		// context it last posted for this repo, which is the same one this
		// bot would have used. Learned from data, never from a bot id.
		gateCtx = s.lastGateContextFor(grant.Repo)
	}
	if gateCtx == "" {
		if s.logger != nil {
			s.logger.Warn("forge gate: run %s ended without publishing, and no gate context is known for %s — if a required check is missing on %s, re-run the bot",
				runID, grant.Repo, prURL)
		}
		return nil
	}

	host, repo, number, err := forge.ParsePullURL(prURL)
	if err != nil {
		return nil
	}
	_ = host
	conn, err := s.forgeConnections.Get(store.WithoutTenantFilter(ctx), grant.ConnectionID)
	if err != nil {
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
	by map[string]string
}

func (m *gateContextMemory) remember(repo, ctxName string) {
	repo, ctxName = strings.TrimSpace(repo), strings.TrimSpace(ctxName)
	if repo == "" || ctxName == "" {
		return
	}
	m.mu.Lock()
	if m.by == nil {
		m.by = map[string]string{}
	}
	m.by[repo] = ctxName
	m.mu.Unlock()
}

func (m *gateContextMemory) get(repo string) string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.by[strings.TrimSpace(repo)]
}

func (s *Server) rememberGateContext(repo, ctxName string) {
	if s == nil {
		return
	}
	s.gateContexts.remember(repo, ctxName)
}

func (s *Server) lastGateContextFor(repo string) string {
	if s == nil {
		return ""
	}
	return s.gateContexts.get(repo)
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
