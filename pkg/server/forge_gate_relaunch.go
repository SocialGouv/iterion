package server

import (
	"fmt"
	"strings"

	"context"

	"github.com/google/uuid"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	"github.com/SocialGouv/iterion/pkg/forge"
	"github.com/SocialGouv/iterion/pkg/identity"
	"github.com/SocialGouv/iterion/pkg/knowledge"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/webhooks"
)

// Relaunching a dead gating run closes the recovery loop the reconciler only
// half-closes: the synthetic failure status makes the interruption VISIBLE,
// but somebody still has to re-run the bot, and on an automated dependency
// lane there is no somebody. So when a gating run dies without a verdict, the
// same bot is relaunched on the same pull request — once per head, ever.
//
// This is crash-recovery of a launch that was already decided, not a new
// authority: the webhook admitted this bot on this PR when the event arrived,
// and the relaunch re-runs exactly that decision through exactly the same
// tail (idempotency claim, quota metering, publish grant, hold-label veto).
// The once-per-head bound is the idempotency key itself — if the relaunched
// run dies on the same head too, the claim is already spent, the synthetic
// failure stays, and the problem graduates to the board (below) for a human.
const gateRelaunchEventKind = "gate_relaunch"

// gateRelaunchLabel marks the board cards this lane creates, so an operator
// (or Nexie) can find every gate that died past recovery in one query.
const gateRelaunchLabel = "source:gate-reconcile"

// gateRelaunchOfVar rides the relaunch's launch vars and names the dead run it
// replaces. The workflow itself drops undeclared vars, but run.Inputs keeps
// them (same mechanism as the publish token), which is what lets the SECOND
// death name the ORIGINAL run in its escalation: the escalating pass is the
// relaunched run's own, and without this stamp the card cited one run twice
// while the first death stayed unfindable.
const gateRelaunchOfVar = "gate_relaunch_of"

// deadGateRun carries what reconcileGateForRun already resolved about the dead
// run, so the relaunch re-validates nothing it does not have to.
type deadGateRun struct {
	run     *store.Run
	grant   ForgePublishGrant
	conn    forge.Connection
	gc      forgeGateClient
	repo    string
	number  int
	pr      forge.PullRef
	gateCtx string
	prURL   string
}

// relaunchDeadGateRun relaunches the bot that owed the (now synthetically
// failed) gate, bounded to one attempt per (PR, head sha). Every refusal is
// deliberate and most are silent: this runs on a bus goroutine after the
// visible failure status has already landed, so nothing here can make the PR
// blind — only less automatic.
func (s *Server) relaunchDeadGateRun(ctx context.Context, d deadGateRun) {
	if s == nil || d.run == nil || s.forgeIntegrations == nil || s.webhookConfigs == nil || s.webhookDeliveries == nil {
		return // local mode: no integrations/webhook spine to relaunch through
	}
	if d.pr.State != "" && d.pr.State != "open" {
		return
	}
	// Fork guard, fail-CLOSED. The relaunch pair — CloneURLFor(base) +
	// d.pr.SourceBranch — is the same fork-unsafe pair the auto-launch and
	// autofix lanes guard against: on a fork PR the source branch lives in
	// the head repo, so the checkout misses (or hits a same-named branch on
	// the base). SameRepoAs is false on empty HeadRepoFullName too, refusing
	// deleted-fork payloads.
	if !d.pr.SameRepoAs(d.repo) {
		if s.logger != nil {
			s.logger.Warn("gate relaunch: refusing %s#%d — fork PR or unverifiable head repo (head=%q base=%q)", d.repo, d.number, d.pr.HeadRepoFullName, d.repo)
		}
		return
	}
	botID := strings.TrimSpace(d.run.BotID)
	if botID == "" {
		botID = strings.TrimSpace(d.grant.Bot)
	}
	if botID == "" {
		return // cannot name the bot to re-run
	}

	integration, err := s.forgeIntegrations.GetByConnRepo(store.WithoutTenantFilter(ctx), d.grant.TeamID, d.grant.ConnectionID, d.repo)
	if err != nil {
		return
	}
	// The repo must still have this bot enabled: an operator who removed it
	// since the original launch has revoked exactly the decision this lane
	// replays.
	enabled := false
	for _, id := range integration.BotIDs {
		if strings.EqualFold(id, botID) {
			enabled = true
			break
		}
	}
	if !enabled {
		return
	}
	cfg, err := s.webhookConfigs.Get(store.WithoutTenantFilter(ctx), integration.WebhookID)
	if err != nil || !cfg.AllowsBot(botID) {
		return
	}
	// The hold label is the operator's per-PR pause on ALL automation. Fails
	// CLOSED, like the auto-fix lane and unlike the best-effort veto on human
	// commands: an unattended launch skipped on a briefly unreachable forge is
	// recovered by the next push; a launch past a pause the operator set is not.
	if len(cfg.HoldLabels) > 0 {
		held, readErr := s.pullRequestHoldLabel(ctx, d.conn, d.repo, d.number, cfg.HoldLabels)
		if readErr != nil {
			s.logWarn("gate relaunch: cannot read labels on %s#%d (%v) — not relaunching, since the hold label could not be ruled out", d.repo, d.number, readErr)
			return
		}
		if held != "" {
			return
		}
	}

	// The original run's inputs ARE the launch decision being replayed — the
	// PR facts, the operator's pinned gate_context/arm_automerge, the bot's
	// own vars. Only the per-run publish grant is dropped: the launch tail
	// mints a fresh one (and would overwrite a stale copy anyway).
	vars := make(map[string]string, len(d.run.Inputs)+1)
	for k, v := range d.run.Inputs {
		if k == forgePublishVarToken || k == forgePublishVarURL {
			continue
		}
		if sv, ok := v.(string); ok {
			vars[k] = sv
		}
	}
	vars[gateRelaunchOfVar] = d.run.ID

	actor := "gaterelaunch:" + cfg.ID
	launchCtx := auth.WithIdentity(ctx, auth.Identity{
		UserID: actor,
		TeamID: d.grant.TeamID,
		Role:   identity.RoleMember,
		Kind:   auth.KindWebhook,
	})
	launchCtx = store.WithIdentity(launchCtx, d.grant.TeamID, actor)

	meta := webhookEventMeta{
		Kind:        gateRelaunchEventKind,
		Action:      "review_died",
		ProjectPath: d.repo,
		SubjectID:   fmt.Sprintf("pr:%d", d.number),
		SubjectURL:  d.prURL,
		SubjectSHA:  d.pr.HeadSHA,
	}
	// One relaunch per (team, repo, PR, head, BOT) — EVER. A second death of
	// the same bot on the same head replays as a duplicate of this key, which
	// is the signal that automation is out of moves for this revision. The
	// bot id is part of the key because a repo's gate context is shared and
	// the publish grant is minted for ANY bot launched with a pr_url: two
	// different gating bots dying on one head are two independent recoveries,
	// and folding them onto one key would deny the second its relaunch while
	// filing a board card that names the wrong run.
	idem := knowledge.ChecksumHex([]byte(fmt.Sprintf("gaterelaunch|%s|%s|%d|%s|%s", d.grant.TeamID, d.repo, d.number, d.pr.HeadSHA, botID)))
	res := s.launchWebhookTarget(launchCtx, nil, cfg, meta, forgeLaunchTarget{
		BotID:   botID,
		IdemKey: idem,
		Vars:    vars,
		RepoURL: forge.CloneURLFor(d.conn.BaseURL(), d.repo),
		RepoRef: d.pr.SourceBranch,
	}, "", "")

	switch res.Status {
	case webhooks.StatusLaunched:
		if s.logger != nil {
			s.logger.Info("gate relaunch: %s died on %s#%d@%s without a verdict → relaunched %s (run %s)",
				d.gateCtx, d.repo, d.number, shortSHA(d.pr.HeadSHA), botID, res.RunID)
		}
	case webhooks.StatusDuplicate:
		// The one attempt this head gets was already spent (res.RunID names
		// it). The failure status stays; a human decides from here — and the
		// board card is how they find out a gate is dying REPEATEDLY, which
		// is a deeper problem (a structurally short budget, a recurring
		// provider quota, a bot defect) than any single death.
		//
		// But "duplicate" alone does not mean the replacement died: the
		// idempotency claim is a read-then-insert, and the gate sweep runs
		// unelected on every replica, so two passes landing on one dead run
		// give one StatusLaunched and one StatusDuplicate — for a relaunch
		// that just SUCCEEDED. Escalating on that files a card telling a human
		// the automation is out of moves while the replacement is alive and
		// reviewing. The card is worth filing only once the named run has
		// itself stopped without answering.
		if alive, why := s.relaunchStillRunning(launchCtx, res.RunID); alive {
			if s.logger != nil {
				s.logger.Debug("gate relaunch: %s on %s#%d@%s already has a relaunch in flight (run %s, %s) — not escalating",
					d.gateCtx, d.repo, d.number, shortSHA(d.pr.HeadSHA), res.RunID, why)
			}
			return
		}
		// Log AFTER the dedup decision: the sweep re-offers a dead run every
		// minute for its whole lookback, and a Warn claiming "escalating"
		// on every already-escalated pass is 60 false lines an hour.
		if s.escalateDeadGateToBoard(ctx, d, res.RunID, "the automatic relaunch died too") {
			s.logWarn("gate relaunch: %s on %s#%d@%s already got its one relaunch (run %s) and died again — escalated to the board",
				d.gateCtx, d.repo, d.number, shortSHA(d.pr.HeadSHA), res.RunID)
		} else if s.logger != nil {
			s.logger.Debug("gate relaunch: %s on %s#%d@%s is already escalated — nothing new to file",
				d.gateCtx, d.repo, d.number, shortSHA(d.pr.HeadSHA))
		}
	default:
		why := strings.TrimSpace(res.Error)
		if why == "" {
			why = res.Status
		}
		if s.escalateDeadGateToBoard(ctx, d, "", "the automatic relaunch could not start: "+why) {
			s.logWarn("gate relaunch: could not relaunch %s on %s#%d@%s (%s) — escalated to the board",
				botID, d.repo, d.number, shortSHA(d.pr.HeadSHA), why)
		} else if s.logger != nil {
			s.logger.Debug("gate relaunch: could not relaunch %s on %s#%d@%s (%s) — already escalated",
				botID, d.repo, d.number, shortSHA(d.pr.HeadSHA), why)
		}
	}
}

// escalateDeadGateToBoard leaves a board card when the relaunch lane is out of
// moves: the gate died, the one automatic re-run either died too or could not
// start, and from here only a human can unstick the PR. Best-effort by
// construction — the synthetic failure status is already on the PR, so a board
// that cannot be written costs visibility, never correctness.
//
// One card per (PR, head): a closed card means a human already dealt with this
// revision, and a fresh death on the SAME head adds nothing they don't know.
// A freshly filed card is ALSO posted as a PR comment — the board is the
// operator's queue, but the PR is where the people waiting on the merge look
// (a card alone sat unseen for 7 days while a security PR stayed blocked).
//
// Returns whether THIS call filed the card. The dedup is a deterministic card
// id (UUIDv5 of the (repo, PR, head) key), not just the List pre-check: the
// gate sweep runs unelected on every replica, and two replicas racing past
// the List would otherwise each create a card AND each post the PR comment —
// the store's unique-id insert is the only primitive here that serialises
// cross-replica. The label stays on the card for querying.
func (s *Server) escalateDeadGateToBoard(ctx context.Context, d deadGateRun, priorRelaunchRunID, why string) bool {
	if s.cfg.CloudBoardFor == nil {
		s.logWarn("gate relaunch: no board on this deployment — %s#%d stays red with no card; the failure status carries the remedy", d.repo, d.number)
		return false
	}
	board := s.cfg.CloudBoardFor(d.grant.TeamID)
	if board == nil {
		s.logWarn("gate relaunch: no board for team %s — %s#%d stays red with no card", d.grant.TeamID, d.repo, d.number)
		return false
	}
	dedup := fmt.Sprintf("gate-dead:%s#%d@%s", d.repo, d.number, shortSHA(d.pr.HeadSHA))
	if existing, err := board.List(native.ListFilter{Labels: []string{dedup}}); err == nil && len(existing) > 0 {
		return false
	}
	cardID := "native:" + uuid.NewSHA1(uuid.NameSpaceURL, []byte("iterion://gate-dead/"+d.grant.TeamID+"/"+dedup)).String()

	// Name the two runs that died — not one of them twice. The escalating
	// pass is usually the RELAUNCHED run's own death: the idempotency claim
	// then names d.run itself, and the card used to cite that one URL as both
	// "dead run" and "relaunched run" while the original death stayed
	// unfindable. The original's id travels on the relaunch's launch vars.
	originalID, originalErr := d.run.ID, strings.TrimSpace(d.run.Error)
	relaunchID, relaunchErr := strings.TrimSpace(priorRelaunchRunID), ""
	if relaunchID != "" && strings.EqualFold(relaunchID, d.run.ID) {
		relaunchID, relaunchErr = d.run.ID, originalErr
		originalID, originalErr = runInputString(d.run, gateRelaunchOfVar), ""
	}
	// The two ids do NOT have the same provenance, so they are not trusted
	// the same way. relaunchID comes from iterion's own idempotency claim —
	// the delivery record proves this team launched it — so an unreadable run
	// only costs its error text. originalID comes from a LAUNCH VAR, which an
	// operator can pin on a webhook: it is a claim, not proof, and one this
	// lane is about to publish on a pull request.
	if relaunchID != "" && relaunchErr == "" {
		if e, ok := s.gateRunError(ctx, d.grant.TeamID, relaunchID); ok {
			relaunchErr = e
		}
	}
	if originalID != "" && originalErr == "" {
		if e, ok := s.gateRunError(ctx, d.grant.TeamID, originalID); ok {
			originalErr = e
		} else {
			originalID = "" // unreadable, or another team's — do not cite it
		}
	}

	base := strings.TrimRight(strings.TrimSpace(s.cfg.PublicURL), "/")
	body := fmt.Sprintf(
		"The merge gate `%s` on %s#%d died without posting a verdict, and the automatic recovery is exhausted: %s.\n\n"+
			"- Pull request: %s\n"+
			"- Head audited: `%s`\n",
		d.gateCtx, d.repo, d.number, why,
		d.prURL,
		d.pr.HeadSHA)
	if originalID != "" {
		body += fmt.Sprintf("- Dead run: %s — `%s`\n", gateRunRef(base, originalID), orNoError(originalErr))
	} else {
		body += "- Dead run: unknown (the relaunch was launched before its parent run was stamped)\n"
	}
	if relaunchID != "" && !strings.EqualFold(relaunchID, originalID) {
		body += fmt.Sprintf("- Relaunched run (also dead): %s — `%s`\n", gateRunRef(base, relaunchID), orNoError(relaunchErr))
	}
	body += "\nA required check dying twice on one revision usually means a structural problem — " +
		"a run budget too short for this workload, a recurring provider quota, or a bot defect. " +
		"Fix the cause, then push or comment the bot's command to re-run; the synthetic `failure` " +
		"status on the PR is overwritten by the next real verdict."

	if _, err := board.Create(native.Issue{
		ID:     cardID,
		Title:  truncate(fmt.Sprintf("Merge gate %s keeps dying on %s#%d", d.gateCtx, d.repo, d.number), 120),
		Body:   body,
		Labels: []string{gateRelaunchLabel, dedup},
	}); err != nil {
		// A create refused on the deterministic id means another replica won
		// the race a moment ago — the escalation exists, nothing to add. Any
		// other failure is a real board problem: say so, the PR keeps its
		// synthetic status either way.
		if existing, lerr := board.List(native.ListFilter{Labels: []string{dedup}}); lerr == nil && len(existing) > 0 {
			return false
		}
		s.logWarn("gate relaunch: board card create failed for %s#%d: %v", d.repo, d.number, err)
		return false
	}
	if s.logger != nil {
		s.logger.Info("gate relaunch: board card filed for %s on %s#%d@%s", d.gateCtx, d.repo, d.number, shortSHA(d.pr.HeadSHA))
	}
	s.commentDeadGateOnPR(ctx, d, body)
	return true
}

// gateRunRef names a run in the escalation body. A deployment with no
// PublicURL cannot build a link (gateRunURL returns ""), and this text is now
// published on the pull request — where "- Dead run:  — `budget exceeded`"
// tells the reader nothing they can look up. The id alone is enough to find
// the run through the CLI or the API, so it is what stands in.
func gateRunRef(base, runID string) string {
	if u := gateRunURL(base, runID); u != "" {
		return u
	}
	return "`" + runID + "` (no PublicURL configured on this deployment — no link)"
}

// gateRunError reads another run's failure reason for the escalation, and is
// the TENANT BOUNDARY of this lane. `originalID` reaches it from a launch var
// (`gate_relaunch_of`), which an operator can pin on a webhook — so the id can
// name a run this team does not own, whose error and URL are about to be
// published on a pull request and a board card. The untenanted load is
// deliberate (the reconciler runs on a bus goroutine with no request tenant),
// which is exactly why the ownership check has to be explicit here.
//
// Reports ok=false when the run is unreadable, missing, or another team's —
// the caller then drops the id entirely rather than citing a run it cannot
// vouch for.
func (s *Server) gateRunError(ctx context.Context, teamID, runID string) (string, bool) {
	if s.cfg.Store == nil {
		return "", false
	}
	r, err := s.cfg.Store.LoadRun(store.WithoutTenantFilter(ctx), runID)
	if err != nil || r == nil {
		return "", false
	}
	if r.TenantID != "" && teamID != "" && !strings.EqualFold(r.TenantID, teamID) {
		s.logWarn("gate relaunch: refusing to cite run %s (team %s) in team %s's escalation — the id came from a launch var",
			runID, r.TenantID, teamID)
		return "", false
	}
	return strings.TrimSpace(r.Error), true
}

// orNoError renders a run error for embedding in a markdown `code span`: an
// empty error becomes an explicit placeholder, and everything that can END
// the span is neutralised, so forge-controlled error prose (which quotes a
// hostile ref name and a remote's message verbatim) cannot smuggle active
// markdown — @mentions, headings — into a comment posted by iterion's own
// forge identity.
//
// A NEWLINE ends a code span just as surely as a backtick does, and run
// errors are routinely multi-line: the runner builds them from git's full
// CombinedOutput. Collapsing first is what makes the backtick escaping mean
// anything.
func orNoError(err string) string {
	if err == "" {
		return "no error recorded"
	}
	flat := strings.Join(strings.Fields(err), " ")
	return strings.ReplaceAll(flat, "`", "'")
}

// commentDeadGateOnPR posts the escalation on the pull request itself — the
// one surface the PR's audience is guaranteed to see. The commit status only
// offers 140 characters and the board card lives on the integration's team
// board, which the people waiting on THIS merge may never open. Best-effort,
// and deliberately gated on the board card having just been created: the card
// dedup is what bounds this to one comment per (PR, head) — a deployment with
// no board gets no comment rather than one per sweep pass.
func (s *Server) commentDeadGateOnPR(ctx context.Context, d deadGateRun, body string) {
	rc, err := s.reviewClientFor(ctx, d.conn)
	if err != nil {
		s.logWarn("gate relaunch: no review client for %s (%v) — escalation stays board+status only", d.repo, err)
		return
	}
	if rc == nil {
		s.logWarn("gate relaunch: provider %s cannot post PR comments — escalation stays board+status only", d.conn.Provider)
		return
	}
	if _, err := rc.CreatePullReview(ctx, d.repo, d.number, forge.NewReview{Body: body}); err != nil {
		s.logWarn("gate relaunch: escalation comment on %s#%d failed: %v", d.repo, d.number, err)
		return
	}
	if s.logger != nil {
		s.logger.Info("gate relaunch: escalation comment posted on %s#%d", d.repo, d.number)
	}
}

// relaunchStillRunning reports whether the run an idempotency claim named is
// still working, and a short phrase saying how it was decided.
//
// The claim is a read-then-insert, and the gate sweep runs unelected on every
// replica, so a StatusDuplicate does NOT by itself mean the replacement died:
// two passes on one dead run give one launch and one duplicate for the SAME,
// live, replacement. Escalation is a message to a human ("automation is out of
// moves"), so it must be false only in the direction that stays quiet: an
// unknown run — never launched, already pruned, unreadable store — is reported
// as NOT running, which preserves the escalation this branch exists for.
func (s *Server) relaunchStillRunning(ctx context.Context, runID string) (bool, string) {
	runID = strings.TrimSpace(runID)
	if runID == "" || s.cfg.Store == nil {
		return false, "no run named"
	}
	run, err := s.cfg.Store.LoadRun(store.WithoutTenantFilter(ctx), runID)
	if err != nil || run == nil {
		return false, "run unreadable"
	}
	switch run.Status {
	case store.RunStatusQueued, store.RunStatusRunning,
		store.RunStatusPausedWaitingHuman, store.RunStatusPausedOperator:
		return true, string(run.Status)
	}
	return false, string(run.Status)
}
