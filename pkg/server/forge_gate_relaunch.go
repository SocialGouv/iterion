package server

import (
	"fmt"
	"strings"

	"context"

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
	vars := make(map[string]string, len(d.run.Inputs))
	for k, v := range d.run.Inputs {
		if k == forgePublishVarToken || k == forgePublishVarURL {
			continue
		}
		if sv, ok := v.(string); ok {
			vars[k] = sv
		}
	}

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
		s.logWarn("gate relaunch: %s on %s#%d@%s already got its one relaunch (run %s) and died again — escalating to the board",
			d.gateCtx, d.repo, d.number, shortSHA(d.pr.HeadSHA), res.RunID)
		s.escalateDeadGateToBoard(d, res.RunID, "the automatic relaunch died too")
	default:
		why := strings.TrimSpace(res.Error)
		if why == "" {
			why = res.Status
		}
		s.logWarn("gate relaunch: could not relaunch %s on %s#%d@%s (%s) — escalating to the board",
			botID, d.repo, d.number, shortSHA(d.pr.HeadSHA), why)
		s.escalateDeadGateToBoard(d, "", "the automatic relaunch could not start: "+why)
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
func (s *Server) escalateDeadGateToBoard(d deadGateRun, priorRelaunchRunID, why string) {
	if s.cfg.CloudBoardFor == nil {
		s.logWarn("gate relaunch: no board on this deployment — %s#%d stays red with no card; the failure status carries the remedy", d.repo, d.number)
		return
	}
	board := s.cfg.CloudBoardFor(d.grant.TeamID)
	if board == nil {
		s.logWarn("gate relaunch: no board for team %s — %s#%d stays red with no card", d.grant.TeamID, d.repo, d.number)
		return
	}
	dedup := fmt.Sprintf("gate-dead:%s#%d@%s", d.repo, d.number, shortSHA(d.pr.HeadSHA))
	if existing, err := board.List(native.ListFilter{Labels: []string{dedup}}); err == nil && len(existing) > 0 {
		return
	}

	reason := strings.TrimSpace(d.run.Error)
	if reason == "" {
		reason = "(no error recorded on the run)"
	}
	base := strings.TrimRight(strings.TrimSpace(s.cfg.PublicURL), "/")
	body := fmt.Sprintf(
		"The merge gate `%s` on %s#%d died without posting a verdict, and the automatic recovery is exhausted: %s.\n\n"+
			"- Pull request: %s\n"+
			"- Head audited: `%s`\n"+
			"- Dead run: %s — `%s`\n",
		d.gateCtx, d.repo, d.number, why,
		d.prURL,
		d.pr.HeadSHA,
		gateRunURL(base, d.run.ID), reason)
	if priorRelaunchRunID != "" {
		body += fmt.Sprintf("- Relaunched run (also dead): %s\n", gateRunURL(base, priorRelaunchRunID))
	}
	body += "\nA required check dying twice on one revision usually means a structural problem — " +
		"a run budget too short for this workload, a recurring provider quota, or a bot defect. " +
		"Fix the cause, then push or comment the bot's command to re-run; the synthetic `failure` " +
		"status on the PR is overwritten by the next real verdict."

	if _, err := board.Create(native.Issue{
		Title:  truncate(fmt.Sprintf("Merge gate %s keeps dying on %s#%d", d.gateCtx, d.repo, d.number), 120),
		Body:   body,
		Labels: []string{gateRelaunchLabel, dedup},
	}); err != nil {
		s.logWarn("gate relaunch: board card create failed for %s#%d: %v", d.repo, d.number, err)
		return
	}
	if s.logger != nil {
		s.logger.Info("gate relaunch: board card filed for %s on %s#%d@%s", d.gateCtx, d.repo, d.number, shortSHA(d.pr.HeadSHA))
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
