package server

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/eventbus"
	"github.com/SocialGouv/iterion/pkg/forge"
	"github.com/SocialGouv/iterion/pkg/identity"
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
// What bounds the loop is at the claim below.
const gateAutofixName = "forge-gate-autofix"

// autofixEventKind marks this lane's rows in the delivery audit, so an operator
// can see every unattended launch it made and the per-PR ceiling can count them.
const autofixEventKind = "gate_autofix"

// gateAutofixLabel marks the board cards this lane files when a fixer launch
// keeps failing to start, so an operator can find them in one query.
const gateAutofixLabel = "source:gate-autofix"

// maxAutofixAttemptsPerPR is the ceiling the per-head claim cannot provide.
//
// One attempt per head sha bounds a fixer that STOPS pushing. It does not bound
// one that keeps pushing without converging: each push moves the head, a
// re-review produces a fresh verdict, and a fresh claim becomes available — a
// loop that is making progress by its own measure and none by anyone else's, at
// roughly two runs a cycle. The org cost cap is the other backstop, and it
// defaults to unlimited, so a deployment that never configured one would have
// no bound at all.
const maxAutofixAttemptsPerPR = 5

// startGateAutofix attaches the lane to the event spine, alongside the gate
// reconciler and on the same bus — queue-group delivery in cloud, so exactly one
// replica reacts to a given run outcome.
func (s *Server) startGateAutofix() {
	if s == nil || s.forgePublishTokens == nil || s.forgeConnections == nil || s.forgeIntegrations == nil {
		return
	}
	bus := s.eventsBus()
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

// autofixOffer re-offers a run to the lane by id — the sweep net's entry
// point (the bus event only carries the run id anyway). Idempotent end to
// end: the guards below exit on a local field read for the overwhelming
// majority, and a genuine launch is deduped by the per-head idempotency key
// plus the launch tail's atomic claim, so a double offer costs one read.
func (s *Server) autofixOffer(ctx context.Context, runID string) {
	_ = s.autofixForRunID(ctx, runID, gateTriggerSweep)
}

// autofixForRun is the eventbus handler.
func (s *Server) autofixForRun(ctx context.Context, ev trigger.Event) error {
	return s.autofixForRunID(ctx, strings.TrimSpace(ev.Subject.ID), gateTriggerEvent)
}

// autofixForRunID is the lane itself, reachable from either of its two
// triggers: the run-outcome event (immediate) and the periodic sweep (the net
// under it). `via` names which one — the same discriminator the reconciler
// carries, and for the same reason: the sweep re-offers every terminal run in
// its lookback ONCE A MINUTE, per process, so anything here that is not
// idempotent must be event-only. The launch path is idempotent by its
// per-head claim; a COMMENT is not, and has no claim to hide behind.
//
// Every refusal below is silent by design: the overwhelming majority of runs
// are not gating runs at all.
func (s *Server) autofixForRunID(ctx context.Context, runID, via string) error {
	runID = strings.TrimSpace(runID)
	if runID == "" || s.cfg.Store == nil || s.forgePublishTokens == nil || s.forgeIntegrations == nil ||
		s.webhookConfigs == nil || s.webhookDeliveries == nil {
		// The last two are dereferenced below, and this runs in a bus goroutine
		// whose panic has no recover above it.
		return nil
	}
	run, err := s.cfg.Store.LoadRun(store.WithoutTenantFilter(ctx), runID)
	if err != nil || run == nil {
		return nil
	}
	// The sweep net offers every run in the notifiable window, cancelled and
	// paused rows included (the event path pre-filters by kind; the sweep
	// cannot). One explicit status gate serves both paths: a cancelled run is
	// an operator's stop — its verdict, if any, is not an invitation to push
	// code — and a paused one is still expected to post its own.
	if run.Status == store.RunStatusCancelled ||
		run.Status == store.RunStatusPausedWaitingHuman || run.Status == store.RunStatusPausedOperator {
		return nil
	}
	// A run that will resume is still expected to post its own verdict; acting
	// on the interim state would fire on a gate that is about to change. Only
	// an ARMED retry makes that true (same distinction as the reconciler): a
	// failed_resumable with nothing coming back for it is final, and a red
	// verdict it did post before dying deserves its fix pass.
	if run.Status == store.RunStatusFailedResumable &&
		run.RetryState != nil && run.RetryState.RetryAfter != nil {
		return nil
	}
	// A DECLINED run is an ANSWER, not a failure to repair (#706): the bot
	// read its task and refused it on the merits, changing nothing. Nothing
	// downstream may act on it — the head has not moved, so a launch here
	// re-derives the same refusal for as long as the trigger keeps firing —
	// and the reason has to reach the author, because a run that changed
	// nothing leaves no other trace on the pull request. Keyed on the typed
	// code alone: any bot may decline, and the engine never learns which.
	//
	// The notice is posted on the EVENT path only. A declined run is terminal
	// and its updated_at never moves, so the sweep re-offers it every minute
	// for the whole lookback — dozens of identical comments on one pull
	// request, per replica. Same guard, for the same reason, as the pause and
	// DLQ notices next door (R69a603).
	if run.FailureCode == declinedFailureCode {
		if via == gateTriggerEvent {
			s.noticeFixerDeclined(ctx, run)
		}
		return nil
	}

	token := runInputString(run, forgePublishVarToken)
	prURL := runInputString(run, "pr_url")
	gateCtx := runInputString(run, "gate_context")
	reviewed := runInputString(run, "head_sha")
	// The same anchor the reconciler uses: holding a publish grant is not
	// gating, and a repo that never pinned its gate context has no check for
	// this lane to read — and a run whose launch pinned the gate OFF owes no
	// verdict this lane could fix (see runGateDisabled).
	if token == "" || prURL == "" || gateCtx == "" || reviewed == "" || runGateDisabled(run) {
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

	// The per-head claim probe comes BEFORE every forge round-trip: the
	// sweep re-offers each gating run ~once a minute for an hour, on every
	// replica, and without this exit each offer costs GetPullRequest +
	// ListCommitStatuses (+ GetIssue with hold labels) against the same App
	// quota the merge-gate reconciler lives on — the net would starve the
	// gate it backs. `reviewed` is the only sha a launch is possible for
	// (the head must still equal it), so the key needs nothing from the
	// forge. A launch_error row does not settle the head: the launch may
	// legitimately retry — on the budget the same row carries, then never
	// again on this head (see forge_gate_launch_budget.go).
	idem := autofixIdemKey(grant.TeamID, repo, number, reviewed)
	prior, found, verdict, wait := s.unattendedLaunchVerdict(ctx, idem)
	if found && prior.Status != webhooks.StatusLaunchError {
		return nil // this head already had its pass — zero forge traffic
	}
	switch verdict {
	case launchRetryWait:
		if s.logger != nil {
			s.logger.Debug("gate auto-fix: the fixer on %s#%d@%s failed to start %d time(s) — next attempt in %s",
				repo, number, shortSHA(reviewed), prior.Attempts, wait.Round(time.Second))
		}
		return nil
	case launchRetryExhausted:
		// Escalated at the failure that spent the budget; re-filing is a
		// deduped no-op that only exists so a board outage at that moment
		// does not lose the notice.
		if conn, cerr := s.forgeConnections.Get(store.WithoutTenantFilter(ctx), grant.ConnectionID); cerr == nil && conn.TenantID == grant.TeamID {
			s.escalateExhaustedAutofix(ctx, conn, grant.TeamID, repo, number, prURL, reviewed, gateCtx, prior.BotID, prior.Attempts, prior.Error)
		}
		return nil
	}

	integration, err := s.forgeIntegrations.GetByConnRepo(store.WithoutTenantFilter(ctx), grant.TeamID, grant.ConnectionID, repo)
	if err != nil || !integration.AutoFixOnGateFailure {
		return nil
	}
	// The gate context arrives from the RUN's inputs, which whoever launched it
	// chose. Unchecked, ANY red status on that head whose name some run had used
	// — a failing CI build, a coverage bot — would launch a code-pushing agent.
	// The repo's pinned context is the authority; a repo that pinned none has
	// not made a check required, so there is no gate for this lane to react to.
	pinned := strings.TrimSpace(integration.LaunchVars[gateContextVar])
	if pinned == "" || !strings.EqualFold(pinned, gateCtx) {
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
	// Fork guard, fail-CLOSED. Autofix pushes commits (fixer.RepoURL is the
	// BASE repo's CloneURL and RepoRef is the PR's SourceBranch), so a fork
	// PR — or one whose head repo cannot be verified — would push LLM
	// commits to a branch on the BASE repo. SameRepoAs returns false on
	// empty HeadRepoFullName (deleted-fork payloads), so refuse both.
	if !pr.SameRepoAs(repo) {
		if s.logger != nil {
			s.logger.Warn("gate auto-fix: refusing %s#%d — fork PR or unverifiable head repo (head=%q base=%q)", repo, number, pr.HeadRepoFullName, repo)
		}
		return nil
	}

	// The forge is the authority on the verdict — never our own bookkeeping,
	// which a second replica would not share and a restart would lose. No read
	// capability means abstain: launching a fixer on a gate we cannot see would
	// be acting on a guess.
	gate, readable, err := gateStatusOn(ctx, gc, repo, pr.HeadSHA, gateCtx)
	if err != nil || !readable || gate.State != forge.CommitStateFailure {
		return nil
	}
	// A SYNTHETIC failure — the reconciler's "the review died without a
	// verdict" marker — is not a verdict either. There are no findings behind
	// it for a fixer to address; recovery for a review that never happened is
	// the relaunch lane's job (re-run the REVIEWER), and launching the fixer
	// here would push code against an instruction to fix nothing.
	if isSyntheticGateInterruption(gate.Description) {
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
	// The hold label is the operator's per-PR pause on ALL automation, and it is
	// the only brake on this lane — every other automatic launch also has a
	// human trigger. Read LIVE: the run that triggered us may predate the label.
	//
	// Fails CLOSED. Elsewhere the veto is best-effort because a missed pause
	// costs a review; here it costs an unattended push. A launch skipped
	// because the forge was briefly unreachable is recovered by the next
	// re-review; a push past a pause the operator set is not recoverable at all.
	if len(cfg.HoldLabels) > 0 {
		held, readErr := s.pullRequestHoldLabel(ctx, conn, repo, number, cfg.HoldLabels)
		if readErr != nil {
			s.logWarn("gate auto-fix: cannot read labels on %s#%d (%v) — not launching, since the hold label could not be ruled out", repo, number, readErr)
			return nil
		}
		if held != "" {
			return nil
		}
	}

	// Metered like any other launch, and stamped with BOTH identities the
	// inbound-webhook middleware puts on a request: the auth identity the
	// admission gate reads, and the store identity every tenant-scoped query
	// asserts on. A bus handler is not an HTTP request and carries neither, and
	// the store half is not optional — without it the launch reaches Mongo with
	// no tenant and the tenancy tripwire fires inside SaveRun, taking the
	// process down rather than failing the launch.
	actor := "autofix:" + cfg.ID
	launchCtx := auth.WithIdentity(ctx, auth.Identity{
		UserID: actor,
		TeamID: grant.TeamID,
		Role:   identity.RoleMember,
		Kind:   auth.KindWebhook,
	})
	launchCtx = store.WithIdentity(launchCtx, grant.TeamID, actor)

	vars := applyWebhookVarLayers(fixerPRVars(
		pr.TargetBranch, pr.SourceBranch, prURL,
		fmt.Sprintf("The merge gate %q is red on this pull request. Address the findings of the review that set it, then push.", gateCtx),
		cfg.BranchImproveAsPR, nil), cfg)
	vars["head_sha"] = pr.HeadSHA

	// The shared launch tail, not a copy of it. It owns the claim (atomically,
	// so two replicas reacting to the same outcome cannot both launch), the
	// quota metering AND its rollback when the claim is lost, the per-run forge
	// publish grant — without which the fixer could not post the verdict this
	// lane exists to produce — the metrics, and the trigger-spine emit.
	//
	// The claim key is the bound: one attempt per (PR, head sha). The fixer
	// pushes → the head moves → a re-review produces a fresh verdict → a new
	// key becomes available. A fixer that pushes nothing leaves the head where
	// it is, and that key is already spent.
	subject := fmt.Sprintf("pr:%d", number)
	if spent, capped := s.autofixAttemptsSpent(ctx, cfg, repo, subject); capped {
		s.logWarn("gate auto-fix: %s#%d has had %d unattended fix passes without converging — stopping; a human decides from here", repo, number, spent)
		return nil
	}
	meta := webhookEventMeta{
		Kind:        autofixEventKind,
		Action:      "gate_failure",
		ProjectPath: repo,
		SubjectID:   subject,
		SubjectURL:  prURL,
		SubjectSHA:  pr.HeadSHA,
	}
	res := s.launchWebhookTarget(launchCtx, nil, cfg, meta, forgeLaunchTarget{
		BotID:   fixer,
		IdemKey: idem,
		Vars:    vars,
		RepoURL: forge.CloneURLFor(conn.BaseURL(), repo),
		RepoRef: pr.SourceBranch,
	}, "", "")
	if res.RunID == "" {
		// Replayed, denied, or failed — the tail recorded why. A launch that
		// failed to START is retried on the budget the claim row carries and
		// escalated when it is spent; the sweep's next offers carry the
		// retries. A denial keeps its own recovery (the forge is told, the
		// org quota resets on its own horizon).
		if res.Status == webhooks.StatusLaunchError && res.denial == nil {
			why := strings.TrimSpace(res.Error)
			switch {
			case res.attempts >= maxUnattendedLaunchAttempts:
				s.escalateExhaustedAutofix(ctx, conn, grant.TeamID, repo, number, prURL, pr.HeadSHA, gateCtx, fixer, res.attempts, why)
			case res.attempts == 0:
				s.logWarn("gate auto-fix: %s on %s#%d@%s failed to start (%s) — the next offer retries",
					fixer, repo, number, shortSHA(pr.HeadSHA), why)
			default:
				s.logWarn("gate auto-fix: %s on %s#%d@%s failed to start (attempt %d/%d: %s) — retrying in %s",
					fixer, repo, number, shortSHA(pr.HeadSHA), res.attempts, maxUnattendedLaunchAttempts, why,
					unattendedLaunchBackoff(res.attempts))
			}
		}
		return nil
	}
	if s.logger != nil {
		s.logger.Info("gate auto-fix: %s red on %s#%d@%s → launched %s (run %s)",
			gateCtx, repo, number, pr.HeadSHA[:7], fixer, res.RunID)
	}
	return nil
}

// escalateExhaustedAutofix files the out-of-moves card when the fixer could
// not be STARTED within its budget on this head — a different notice from a
// fixer that ran and did not converge (which the per-PR ceiling logs). Deduped
// per (PR, head) by the card id; also posted on the PR.
func (s *Server) escalateExhaustedAutofix(ctx context.Context, conn forge.Connection, teamID, repo string, number int, prURL, headSHA, gateCtx, fixer string, attempts int, lastErr string) {
	if fixer == "" {
		fixer = "the repo's fixer"
	}
	body := fmt.Sprintf(
		"The merge gate `%s` on %s#%d is red and the automatic fix pass could not start: the launch of `%s` failed %d times — last: `%s`.\n\n"+
			"- Pull request: %s\n"+
			"- Head: `%s`\n\n"+
			"The lane has stopped retrying this revision. Fix the launch cause — a broken plugin source, a queue outage, "+
			"a bot that no longer resolves — then push, or run the fixer with its command; a new head gets a fresh attempt.",
		gateCtx, repo, number, fixer, attempts, orNoError(lastErr),
		prURL,
		headSHA)
	filed := s.fileGateEscalation(ctx, gateEscalation{
		teamID:  teamID,
		conn:    conn,
		repo:    repo,
		number:  number,
		headSHA: headSHA,
		lane:    "gate-autofix",
		label:   gateAutofixLabel,
		title:   fmt.Sprintf("Auto-fix of %s#%d cannot start", repo, number),
		body:    body,
	})
	if filed {
		s.logWarn("gate auto-fix: %s on %s#%d@%s failed to start %d times — no further attempt on this head; escalated to the board",
			fixer, repo, number, shortSHA(headSHA), attempts)
	} else if s.logger != nil {
		s.logger.Debug("gate auto-fix: %s on %s#%d@%s is out of launch attempts and already escalated",
			fixer, repo, number, shortSHA(headSHA))
	}
}

// autofixIdemKey derives the per-(PR, head) claim key. The sha is lowercased
// so the early probe (fed by the run's head_sha input) and the launch (fed by
// the forge's pr.HeadSHA) always derive the SAME key — the two are only ever
// compared case-insensitively.
func autofixIdemKey(teamID, repo string, number int, sha string) string {
	return knowledge.ChecksumHex([]byte(fmt.Sprintf("autofix|%s|%s|%d|%s", teamID, repo, number, strings.ToLower(sha))))
}

// reviewFixerFor picks the bot this repo has enabled that declares it CONSUMES a
// review. That declaration already means "I start from a review and act on it",
// which is exactly the bot a red gate needs — so the lane names no bot, and a
// repo that enables a different fixer gets that one.
func (s *Server) reviewFixerFor(integration forge.RepoIntegration) string {
	for _, want := range integration.BotIDs {
		for _, c := range s.handoffConsumersFor(want) {
			if c.Kind == bundle.HandoffKindReview {
				return want
			}
		}
	}
	return ""
}

// gateStatusOn reports the status of ctxName on sha; an ABSENT context is the
// zero status. readable=false means the provider cannot list statuses at ALL —
// deliberately distinct from "absent", because the two callers assume opposite
// things from it: the reconciler treats unreadable as "already posted" (never
// overwrite a verdict it cannot see), this lane treats it as "abstain" (never
// launch on a gate it cannot see).
func gateStatusOn(ctx context.Context, gc forgeGateClient, repo, sha, ctxName string) (st forge.CommitStatus, readable bool, err error) {
	lister, ok := gc.(forge.CommitStatusLister)
	if !ok {
		return forge.CommitStatus{}, false, nil
	}
	sts, err := lister.ListCommitStatuses(ctx, repo, sha)
	if err != nil {
		return forge.CommitStatus{}, false, err
	}
	for _, s := range sts {
		if strings.EqualFold(strings.TrimSpace(s.Context), ctxName) {
			return s, true, nil
		}
	}
	return forge.CommitStatus{}, true, nil
}

// pullRequestHoldLabel reports the hold label present on the PR, if any. An
// error means the labels could not be READ — distinct from "none present", and
// the caller must not collapse the two: this is a veto, and a veto that cannot
// be evaluated has not been cleared.
func (s *Server) pullRequestHoldLabel(ctx context.Context, conn forge.Connection, repo string, number int, holds []string) (string, error) {
	admin, err := s.forgeAdminFor(ctx, conn)
	if err != nil {
		return "", err
	}
	ic, ok := admin.(forge.IssueClient)
	if !ok {
		return "", fmt.Errorf("cannot read issue labels: %w", forgeCapabilityErr(conn, admin, "IssueClient"))
	}
	iss, err := ic.GetIssue(ctx, repo, number)
	if err != nil {
		return "", err
	}
	return webhooks.HeldByLabel(holds, iss.Labels), nil
}

// autofixAttemptsSpent counts the unattended passes this lane has already made
// on one pull request, from the delivery audit it writes them to. The audit is
// the only place they are recorded, and it is what an operator reads to see
// them, so counting there keeps one source of truth rather than a second ledger.
func (s *Server) autofixAttemptsSpent(ctx context.Context, cfg webhooks.Config, repo, subject string) (int, bool) {
	// Exact, whole-audit count — a recent-window scan is not a ceiling: 200
	// unrelated deliveries on a busy webhook would push this lane's rows out
	// of the page and silently re-arm the bound.
	spent, err := s.webhookDeliveries.CountLaunched(store.WithoutTenantFilter(ctx), cfg.TenantID, cfg.ID, autofixEventKind, repo, subject)
	if err != nil {
		// Unreadable audit means the ceiling cannot be evaluated. This lane
		// pushes code with nobody watching, so an unevaluable bound is not a
		// cleared one — the same rule the hold label follows.
		s.logWarn("gate auto-fix: cannot count the delivery audit for %s (%v) — not launching, since the per-PR ceiling could not be checked", repo, err)
		return 0, true
	}
	return spent, spent >= maxAutofixAttemptsPerPR
}
