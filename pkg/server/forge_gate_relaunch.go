package server

import (
	"fmt"
	"strings"
	"time"

	"context"

	"github.com/SocialGouv/iterion/pkg/auth"
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
//
// A relaunch that fails to START is a different case from one that dies: the
// claim key is not spent by a launch_error row, and the sweep re-offers the
// dead run every minute for an hour. That retry is wanted (a deploy window, a
// queue blip) but bounded — see forge_gate_launch_budget.go: a few attempts on
// a backoff, then the same escalation, then silence.
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

// gateRelaunchBotID names the bot the dead run owed the gate for.
func gateRelaunchBotID(d deadGateRun) string {
	botID := strings.TrimSpace(d.run.BotID)
	if botID == "" {
		botID = strings.TrimSpace(d.grant.Bot)
	}
	return botID
}

// gateRelaunchIdemKey is the relaunch claim: one per (team, repo, PR, head,
// BOT) — EVER, once a launch succeeds under it. A second death of the same
// bot on the same head replays as a duplicate of this key, which is the
// signal that automation is out of moves for this revision. The bot id is
// part of the key because a repo's gate context is shared and the publish
// grant is minted for ANY bot launched with a pr_url: two different gating
// bots dying on one head are two independent recoveries, and folding them
// onto one key would deny the second its relaunch while filing a board card
// that names the wrong run.
func gateRelaunchIdemKey(d deadGateRun, botID string) string {
	return knowledge.ChecksumHex([]byte(fmt.Sprintf("gaterelaunch|%s|%s|%d|%s|%s", d.grant.TeamID, d.repo, d.number, d.pr.HeadSHA, botID)))
}

// gateRelaunchRetryPending reports whether the head's relaunch claim is a
// launch that FAILED TO START and still has the lane's attention — a retry
// due, or a spent budget whose escalation must be (re)filed. It is the one
// case the reconciler re-enters the relaunch tail for from behind this run's
// own synthetic marker. One store read; a settled claim, a fresh key, or a
// failure still inside its backoff all answer no, which keeps the own-marker
// offer the cheap exit it has to be at one offer per minute per dead run.
func (s *Server) gateRelaunchRetryPending(ctx context.Context, d deadGateRun) bool {
	if d.run == nil || s == nil || s.webhookDeliveries == nil {
		return false
	}
	botID := gateRelaunchBotID(d)
	if botID == "" {
		return false
	}
	_, _, verdict, _ := s.unattendedLaunchVerdict(ctx, gateRelaunchIdemKey(d, botID))
	return verdict == launchRetryDue || verdict == launchRetryExhausted
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
	botID := gateRelaunchBotID(d)
	if botID == "" {
		return // cannot name the bot to re-run
	}
	idem := gateRelaunchIdemKey(d, botID)

	// The failure budget, BEFORE any forge round trip: the sweep offers this
	// dead run every minute, and the hold-label read below is a forge call
	// against the same App quota the reconciler lives on.
	prior, _, verdict, wait := s.unattendedLaunchVerdict(ctx, idem)
	switch verdict {
	case launchRetryWait:
		if s.logger != nil {
			s.logger.Debug("gate relaunch: %s on %s#%d@%s failed to start %d time(s) — next attempt in %s",
				botID, d.repo, d.number, shortSHA(d.pr.HeadSHA), prior.Attempts, wait.Round(time.Second))
		}
		return
	case launchRetryExhausted:
		s.escalateExhaustedRelaunch(ctx, d, botID, prior.Attempts, prior.Error)
		return
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
			s.logger.Info("gate relaunch: %s died on %s#%d@%s without a verdict → relaunched %s (run %s, launch attempt %d)",
				d.gateCtx, d.repo, d.number, shortSHA(d.pr.HeadSHA), botID, res.RunID, max(res.attempts, 1))
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
		// An admission denial (org quota, cost cap, concurrency, suspension)
		// names a horizon the sweep window will not outlast: escalate now,
		// as before. A launch that failed to START is retried on the budget
		// — the claim row counts the attempts — and escalates when it is
		// spent; the sweep's next offers carry the retries.
		if res.denial != nil {
			if s.escalateDeadGateToBoard(ctx, d, "", "the automatic relaunch could not start: "+why) {
				s.logWarn("gate relaunch: could not relaunch %s on %s#%d@%s (%s) — escalated to the board",
					botID, d.repo, d.number, shortSHA(d.pr.HeadSHA), why)
			} else if s.logger != nil {
				s.logger.Debug("gate relaunch: could not relaunch %s on %s#%d@%s (%s) — already escalated",
					botID, d.repo, d.number, shortSHA(d.pr.HeadSHA), why)
			}
			return
		}
		switch {
		case res.attempts >= maxUnattendedLaunchAttempts:
			s.escalateExhaustedRelaunch(ctx, d, botID, res.attempts, why)
		case res.attempts == 0:
			// Nothing was recorded (the delivery store itself failed), so
			// nothing was spent: the next offer retries on a fresh read.
			s.logWarn("gate relaunch: %s on %s#%d@%s failed to start (%s) — the next offer retries",
				botID, d.repo, d.number, shortSHA(d.pr.HeadSHA), why)
		default:
			s.logWarn("gate relaunch: %s on %s#%d@%s failed to start (attempt %d/%d: %s) — retrying in %s",
				botID, d.repo, d.number, shortSHA(d.pr.HeadSHA), res.attempts, maxUnattendedLaunchAttempts, why,
				unattendedLaunchBackoff(res.attempts))
		}
	}
}

// escalateExhaustedRelaunch files the out-of-moves card once the relaunch's
// failure budget is spent. Deduped per (PR, head) by the card id, so the
// sweep's later offers cost one board read and add nothing.
func (s *Server) escalateExhaustedRelaunch(ctx context.Context, d deadGateRun, botID string, attempts int, lastErr string) {
	why := fmt.Sprintf("the automatic relaunch of %s failed to start %d times (last: %s)", botID, attempts, orNoError(lastErr))
	if s.escalateDeadGateToBoard(ctx, d, "", why) {
		s.logWarn("gate relaunch: %s on %s#%d@%s failed to start %d times — no further attempt on this head; escalated to the board",
			botID, d.repo, d.number, shortSHA(d.pr.HeadSHA), attempts)
	} else if s.logger != nil {
		s.logger.Debug("gate relaunch: %s on %s#%d@%s is out of launch attempts and already escalated",
			botID, d.repo, d.number, shortSHA(d.pr.HeadSHA))
	}
}

// escalateDeadGateToBoard leaves a board card when the relaunch lane is out of
// moves: the gate died, the automatic re-run either died too or could not
// start within its budget, and from here only a human can unstick the PR.
// One card per (PR, head): a closed card means a human already dealt with
// this revision, and a fresh death on the SAME head adds nothing they don't
// know. The card is also posted as a PR comment (fileGateEscalation). Returns
// whether THIS call filed the card.
func (s *Server) escalateDeadGateToBoard(ctx context.Context, d deadGateRun, priorRelaunchRunID, why string) bool {
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

	return s.fileGateEscalation(ctx, gateEscalation{
		teamID:  d.grant.TeamID,
		conn:    d.conn,
		repo:    d.repo,
		number:  d.number,
		headSHA: d.pr.HeadSHA,
		lane:    "gate-dead",
		label:   gateRelaunchLabel,
		title:   fmt.Sprintf("Merge gate %s keeps dying on %s#%d", d.gateCtx, d.repo, d.number),
		body:    body,
	})
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
