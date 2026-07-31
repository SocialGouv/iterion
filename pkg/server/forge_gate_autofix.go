package server

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/botregistry"
	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/eventbus"
	"github.com/SocialGouv/iterion/pkg/forge"
	"github.com/SocialGouv/iterion/pkg/knowledge"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/trigger"
	"github.com/SocialGouv/iterion/pkg/webhooks"
)

// Auto-fix closes the review→fix loop without a human in it: a review leaves the
// merge gate red, and the bot that consumes reviews is launched on that head to
// answer the findings.
//
// It is OFF unless a repo turns it on, and that default is a decision, not
// caution. A reviewer alone already leaves the human in the middle — findings
// land, they decide what to act on, and they can hand the work over with a
// command whenever they want. Making the hand-over automatic everywhere takes
// that arbitration away from every developer on the repo to save one comment.
// So it is a per-repo opt-in for teams that want the zero-touch lane, and the
// command stays the default road.
//
// The loop terminates on PROGRESS, not on a countdown: at most one attempt per
// head sha. The fixer pushes, the head moves, a re-review produces a fresh
// verdict, and only then can another attempt fire. A fixer that stops pushing
// stops the loop, because the head stops moving and the claim is already taken.
const gateAutofixName = "forge-gate-autofix"

// startGateAutofix attaches the lane to the event spine, alongside the gate
// reconciler and on the same bus — queue-group delivery in cloud, so exactly one
// replica reacts to a given run outcome.
func (s *Server) startGateAutofix() {
	if s == nil || s.forgePublishTokens == nil || s.forgeConnections == nil || s.forgeIntegrations == nil {
		return
	}
	bus := s.cfg.EventsBus
	if bus == nil && s.triggerCoord != nil {
		bus = s.triggerCoord.Bus()
	}
	if bus == nil {
		return
	}
	cancel, err := s.attachGateAutofix(bus)
	if err != nil {
		s.logWarn("server: gate auto-fix subscribe failed: %v — repos that opted in will not get an automatic fix pass", err)
		return
	}
	s.gateAutofixCancel = cancel
	if s.logger != nil {
		s.logger.Info("server: gate auto-fix attached (opt-in per repo; a red merge gate launches the repo's fixer once per head)")
	}
}

func (s *Server) attachGateAutofix(bus eventbus.Bus) (func(), error) {
	if s == nil || bus == nil {
		return func() {}, nil
	}
	return bus.Subscribe(gateAutofixName, trigger.Matcher{
		Sources: []trigger.Source{trigger.SourceRun},
		Kinds:   []string{trigger.KindRunFinished, trigger.KindRunFailed},
	}, s.autofixForRun)
}

// autofixForRun is the eventbus handler. Every refusal below is silent by
// design: the overwhelming majority of runs are not gating runs at all.
func (s *Server) autofixForRun(ctx context.Context, ev trigger.Event) error {
	runID := strings.TrimSpace(ev.Subject.ID)
	if runID == "" || s.cfg.Store == nil || s.forgePublishTokens == nil || s.forgeIntegrations == nil {
		return nil
	}
	run, err := s.cfg.Store.LoadRun(store.WithoutTenantFilter(ctx), runID)
	if err != nil || run == nil {
		return nil
	}
	// A run that will resume is still expected to post its own verdict; acting
	// on the interim state would fire on a gate that is about to change.
	if run.Status == store.RunStatusFailedResumable ||
		run.Status == store.RunStatusPausedWaitingHuman || run.Status == store.RunStatusPausedOperator {
		return nil
	}

	token := runInputString(run, forgePublishVarToken)
	prURL := runInputString(run, "pr_url")
	gateCtx := runInputString(run, "gate_context")
	reviewed := runInputString(run, "head_sha")
	// The same anchor the reconciler uses: holding a publish grant is not
	// gating, and a repo that never pinned its gate context has no check for
	// this lane to read.
	if token == "" || prURL == "" || gateCtx == "" || reviewed == "" {
		return nil
	}
	grant, ok := s.forgePublishTokens.lookup(token)
	if !ok {
		return nil
	}
	host, repo, number, err := forge.ParsePullURL(prURL)
	if err != nil {
		return nil
	}
	// pr_url is a caller-chosen launch var, so the grant's repo scope is
	// re-enforced here exactly as the publish endpoint enforces it. Launching a
	// code-mutating bot is a far larger blast radius than the red status the
	// reconciler posts, and the grant is what bounds it.
	if !strings.EqualFold(strings.TrimSpace(repo), strings.TrimSpace(grant.Repo)) {
		s.logWarn("gate auto-fix: run %s carries a grant for %s but a pr_url on %s — refusing", runID, grant.Repo, repo)
		return nil
	}

	integration, err := s.forgeIntegrations.GetByConnRepo(store.WithoutTenantFilter(ctx), grant.TeamID, grant.ConnectionID, repo)
	if err != nil || !integration.AutoFixOnGateFailure {
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
		return nil
	}
	pr, err := gc.GetPullRequest(ctx, repo, number)
	if err != nil || strings.TrimSpace(pr.HeadSHA) == "" {
		return nil
	}
	// Only the revision this run judged. The head moves constantly, and a
	// verdict about an older commit says nothing about the current one.
	if !strings.EqualFold(reviewed, pr.HeadSHA) {
		return nil
	}
	if pr.State != "" && pr.State != "open" {
		return nil
	}

	// The forge is the authority on the verdict — never our own bookkeeping,
	// which a second replica would not share and a restart would lose. No read
	// capability means abstain: launching a fixer on a gate we cannot see would
	// be acting on a guess.
	state, known, err := gateStateOn(ctx, gc, repo, pr.HeadSHA, gateCtx)
	if err != nil || !known || state != forge.CommitStateFailure {
		return nil
	}

	fixer := s.reviewFixerFor(integration)
	if fixer == "" {
		s.logWarn("gate auto-fix: %s opted in but no enabled bot on it consumes a review — nothing to launch", repo)
		return nil
	}
	// A fixer's own red verdict must not relaunch the fixer. The per-head claim
	// below would catch it on the second pass, but a bot re-triggering itself on
	// its own output should be refused on its face, not on a race.
	if strings.EqualFold(run.BotID, fixer) {
		return nil
	}

	cfg, err := s.webhookConfigs.Get(store.WithoutTenantFilter(ctx), integration.WebhookID)
	if err != nil {
		return nil
	}
	if !cfg.AllowsBot(fixer) {
		return nil
	}
	// The hold label is the operator's per-PR pause on ALL automation, and a
	// lane that ignored it would be the one the escape hatch does not reach.
	// Read live: the run that triggered us may predate the label being applied.
	if len(cfg.HoldLabels) > 0 {
		if held := s.pullRequestHoldLabel(ctx, conn, repo, number, cfg.HoldLabels); held != "" {
			return nil
		}
	}

	// One attempt per head sha. The fixer pushes → the head moves → a re-review
	// produces a new verdict → a new claim becomes available. A fixer that
	// pushes nothing leaves the head where it is and the loop ends here.
	idem := knowledge.ChecksumHex([]byte(fmt.Sprintf("autofix|%s|%s|%d|%s", grant.TeamID, repo, number, pr.HeadSHA)))
	if _, err := s.webhookDeliveries.GetByIdempotencyKey(store.WithoutTenantFilter(ctx), idem); err == nil {
		return nil // this head already had its attempt
	}

	// Metered like any other launch. The bus handler carries no request
	// identity, so the team is taken from the grant — the same shape the retry
	// sweeper uses to gate a launch it makes on a tenant's behalf.
	gateCtxWithID := auth.WithIdentity(ctx, auth.Identity{TeamID: grant.TeamID})
	if _, denial := s.gateLaunch(gateCtxWithID); denial != nil {
		s.logWarn("gate auto-fix: %s#%d not launched: %s", repo, number, denial.reason)
		return nil
	}

	vars := applyWebhookVarLayers(fixerPRVars(
		pr.TargetBranch, pr.SourceBranch, prURL,
		fmt.Sprintf("The merge gate %q is red on this pull request. Address the findings of the review that set it, then push.", gateCtx),
		cfg.BranchImproveAsPR, nil), cfg)
	vars["head_sha"] = pr.HeadSHA
	s.stampHandoffs(ctx, cfg, fixer, vars, handoffQuery{PRURL: prURL, HeadSHA: pr.HeadSHA})

	delivery := webhooks.Delivery{
		ID: uuid.NewString(), TenantID: grant.TeamID, WebhookID: cfg.ID,
		IdempotencyKey: idem, Status: webhooks.StatusLaunched,
		EventKind: "gate_autofix", ProjectPath: repo, BotID: fixer,
	}
	if err := s.webhookDeliveries.Insert(store.WithoutTenantFilter(ctx), delivery); err != nil {
		return nil // lost the claim to a concurrent replica
	}

	launch := s.webhookLaunchBot
	if launch == nil {
		launch = s.webhookLauncherFor(cfg)
	}
	runIDNew, lerr := launch(gateCtxWithID, fixer, vars, forgeCloneURL(conn, repo), pr.SourceBranch, repo, cfg.KeyOverrides, cfg.SecretOverrides)
	if lerr != nil {
		delivery.Status = webhooks.StatusLaunchError
		delivery.Error = lerr.Error()
		_ = s.webhookDeliveries.Update(store.WithoutTenantFilter(ctx), delivery)
		s.logWarn("gate auto-fix: launching %s on %s#%d failed: %v", fixer, repo, number, lerr)
		return nil
	}
	delivery.RunID = runIDNew
	_ = s.webhookDeliveries.Update(store.WithoutTenantFilter(ctx), delivery)
	if s.logger != nil {
		s.logger.Info("gate auto-fix: %s red on %s#%d@%s → launched %s (run %s)",
			gateCtx, repo, number, pr.HeadSHA[:7], fixer, runIDNew)
	}
	return nil
}

// reviewFixerFor picks the bot this repo has enabled that declares it CONSUMES a
// review. That declaration already means "I start from a review and act on it",
// which is exactly the bot a red gate needs — so the lane names no bot, and a
// repo that enables a different fixer gets that one.
func (s *Server) reviewFixerFor(integration forge.RepoIntegration) string {
	entries, err := botregistry.List(s.botListOptions())
	if err != nil {
		s.logWarn("gate auto-fix: cannot read the bot catalog: %v", err)
		return ""
	}
	for _, want := range integration.BotIDs {
		for _, e := range entries {
			if e.Name != want {
				continue
			}
			for _, c := range e.Consumes {
				if c.Kind == bundle.HandoffKindReview {
					return e.Name
				}
			}
		}
	}
	return ""
}

// gateStateOn reads back the state of one commit status. known is false when the
// provider cannot list statuses or the context is absent — the caller must then
// abstain rather than assume.
func gateStateOn(ctx context.Context, gc forgeGateClient, repo, sha, ctxName string) (forge.CommitState, bool, error) {
	lister, ok := gc.(forge.CommitStatusLister)
	if !ok {
		return "", false, nil
	}
	sts, err := lister.ListCommitStatuses(ctx, repo, sha)
	if err != nil {
		return "", false, err
	}
	for _, st := range sts {
		if strings.EqualFold(strings.TrimSpace(st.Context), ctxName) {
			return st.State, true, nil
		}
	}
	return "", false, nil
}

// pullRequestHoldLabel reports the hold label present on the PR, if any. Best
// effort: a provider with no issue-read capability, or a failed call, returns
// "" — the veto is opt-in and must not become a reason a launch silently stops.
func (s *Server) pullRequestHoldLabel(ctx context.Context, conn forge.Connection, repo string, number int, holds []string) string {
	admin, err := s.forgeAdminFor(ctx, conn)
	if err != nil || admin == nil {
		return ""
	}
	ic, ok := admin.(forge.IssueClient)
	if !ok {
		return ""
	}
	iss, err := ic.GetIssue(ctx, repo, number)
	if err != nil {
		return ""
	}
	return webhooks.HeldByLabel(holds, iss.Labels)
}

// forgeCloneURL derives the repo's clone URL from the connection's own host, so
// the lane never takes a URL from run input.
func forgeCloneURL(conn forge.Connection, repo string) string {
	base := strings.TrimRight(strings.TrimSpace(conn.BaseURL()), "/")
	if base == "" || repo == "" {
		return ""
	}
	return base + "/" + repo + ".git"
}
