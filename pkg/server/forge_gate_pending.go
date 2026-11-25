package server

import (
	"context"
	"strings"

	"github.com/SocialGouv/iterion/pkg/forge"
	"github.com/SocialGouv/iterion/pkg/store"
)

// A required check has three states an operator can act on — green, red, and
// "running". A review only ever posted the first two, at the very END of a run
// that takes minutes. Between the push and the verdict the context is simply
// ABSENT, which the forge renders as "Expected — waiting for status to be
// reported": the same rendering as a review that was never launched, a bot
// that crashed on boot, and a webhook that never fired.
//
// The reconciler next door already names this exact ambiguity ("an absent
// required check is indistinguishable from one still running") and closes it
// from one side only, once the run is dead. This closes it from the other: the
// launch itself claims the context, so the absence of a status once again
// means what it says — nothing is reviewing this revision.
//
// The marker is deliberately a real `pending`, not a comment: it carries the
// run URL, so the operator lands on the live console instead of guessing which
// run owes the check.
const gateInFlightDescription = "review in progress — the verdict will replace this"

// gateAdvisoryBotVar rides the launch vars of a run whose bot declares no
// `statuses` token scope — the provisioner's gating posture
// (forge.BotGatesMerges). Such a bot is not relied on to answer the repo's
// gate, so it neither claims it nor owes it a verdict. The server writes the
// name at launch (stampGateAdvisoryBot) and both gate readers — the in-flight
// claim here and runOwesGateVerdict over the stored inputs — honour it, so a
// launch of such a bot leaves the review's own claim, repair and relaunch
// untouched. The publish endpoint does not read the declared scope: a bot
// that still posts a gate verdict through its publish node is honoured, but
// gets no claim and no repair.
const gateAdvisoryBotVar = store.GateAdvisoryBotVar

// stampGateAdvisoryBot marks a PR-targeted launch whose bot declares no
// `statuses` token scope, so neither the claim nor the repair machinery reads
// it as a verdict owed. The server owns the name: a stale, forked or caller-sent
// value is deleted first, so a gating bot never carries it (and a bot
// flipping to gating loses a stale mark on its next launch).
//
// Best-effort, same posture as markGateInFlight: no gate vars on the launch,
// no bot named, no resolver wired, or a bot that does not resolve leaves the
// gate armed — today's behaviour — because the claim is only an absence of
// status made readable, never a harm a missing stamp can cause. Only a
// resolution FAILURE is warned about: it is a fact about the store or the
// manifest read, not about the bot, and silencing it would hide why a
// converse launch still claims.
func (s *Server) stampGateAdvisoryBot(ctx context.Context, teamID, botID string, vars map[string]string) {
	if s == nil || vars == nil {
		return
	}
	delete(vars, gateAdvisoryBotVar)
	if strings.TrimSpace(vars["gate_context"]) == "" || strings.TrimSpace(vars["pr_url"]) == "" {
		return
	}
	a, ok := s.botGatesMergesAnswer(ctx, teamID, botID)
	if !ok {
		return
	}
	if a.err != nil {
		if s.logger != nil {
			s.logger.Warn("forge gate: team %s: bot %q does not resolve, so the launch keeps the gate armed: %v", teamID, botID, a.err)
		}
		return
	}
	if a.gates {
		return
	}
	vars[gateAdvisoryBotVar] = "true"
	if s.logger != nil {
		s.logger.Info("forge gate: bot %q declares no statuses scope, so its launch on %s carries %s=true — it claims no gate and owes no verdict",
			botID, vars["gate_context"], gateAdvisoryBotVar)
	}
}

// isGateInFlight reports whether a status is this server's own in-flight
// marker rather than a verdict a bot posted.
//
// Every consumer that asks "does this head already have an answer?" MUST route
// through here. The marker is a `pending` with a non-empty description, so a
// guard written as `state != "" → someone already answered` reads it as a
// verdict and stands down — which would make posting the marker actively
// harmful: it would silence the very reconciler that exists to un-stick the
// PR. The predicate keeps "claimed" and "answered" distinct.
func isGateInFlight(st forge.CommitStatus) bool {
	return st.State == forge.CommitStatePending &&
		strings.TrimSpace(st.Description) == gateInFlightDescription
}

// markGateInFlight claims the repo's gate context on the revision a freshly
// launched run is about to review.
//
// Best-effort and deliberately silent about launches that owe no gate: the
// vast majority carry no gate_context at all. It only ever writes over NOTHING
// or over a previous in-flight marker — never over a verdict. That guard is
// what makes it safe under a gate context SHARED by several bots (the repo
// pins one context precisely so a required check can span them): a second bot
// launching on a head another bot already judged must not blank that judgment
// back to "running".
// The run URL is built from cfg.PublicURL rather than the request, so a claim
// and the reconciler's later failure name the run identically — the target URL
// is what tells "already answered" from "must escalate" over there.
//
// A launch whose bot declares no `statuses` token scope (marked
// gate_advisory_bot by stampGateAdvisoryBot) claims nothing: it is not relied
// on to replace its own claim with a verdict, and its claim would overwrite
// the live review's own — leaving a review that dies under it unrepaired.
func (s *Server) markGateInFlight(ctx context.Context, teamID, botID string, vars map[string]string, runID string) {
	if s == nil || s.forgeConnections == nil || vars == nil {
		return
	}
	gateCtx := strings.TrimSpace(vars["gate_context"])
	prURL := strings.TrimSpace(vars["pr_url"])
	if gateCtx == "" || prURL == "" {
		return
	}
	// An explicit gate_enabled pin that leaves the bot advisory-only means
	// no verdict will ever replace a claim — posting one would park a
	// pending status on the head forever. Same classification as the
	// provisioning derivation and the reconciler (forge.GateValueDisables).
	if v, ok := vars["gate_enabled"]; ok && forge.GateValueDisables(v) {
		return
	}
	// Evaluation mode (publish_mode=off): the bot posts no verdict at all, so
	// the claim would be the only status the run leaves — on a pull request
	// it was launched not to touch. Same predicate as runGateDisabled.
	if forge.PublishModeWithholds(vars[forge.PublishModeVar]) {
		return
	}
	// A bot that declares no `statuses` scope is not relied on to post the
	// verdict that would replace the claim, and the repo's gate context is
	// shared: its
	// claim would overwrite the live review's own pending marker (target URL
	// and all) and stand the reconciler down on a review that then dies. The
	// stamp rides the same vars map, so the claim reads what the stamp wrote.
	if strings.TrimSpace(vars[gateAdvisoryBotVar]) == "true" {
		return
	}
	// The revision, not the PR. A status is posted on a sha, and the head can
	// move between this launch and the verdict; claiming the head the run was
	// handed keeps the marker and the verdict on the same commit.
	sha := strings.TrimSpace(vars["head_sha"])
	if sha == "" {
		return
	}
	// An UNATTRIBUTABLE claim must never exist. Ownership of a status is read
	// off its target URL (gateRunTarget.speaksFor), so a claim posted without one
	// cannot be told from another run's — and the reconciler would have to
	// choose between painting "review died" over a live review and leaving a
	// PR stuck on a pending nothing resolves. Neither is acceptable, so with
	// no PublicURL configured the launch simply does not claim: the check
	// behaves exactly as it did before this feature existed.
	runURL := gateRunURL(strings.TrimRight(strings.TrimSpace(s.cfg.PublicURL), "/"), runID)
	if runURL == "" {
		if s.logger != nil {
			s.logger.Warn("forge gate: not claiming %s for run %s — PublicURL is unset, so the claim could not name the run and nothing could later tell it from another run's",
				gateCtx, runID)
		}
		return
	}
	host, repo, _, err := forge.ParsePullURL(prURL)
	if err != nil {
		return
	}
	conn, ok := s.forgeConnectionForPR(ctx, teamID, "", host, repo)
	if !ok {
		return
	}
	gc, err := s.gateClientFor(ctx, conn, repo)
	if err != nil || gc == nil {
		if s.logger != nil && err != nil {
			s.logger.Warn("forge gate: cannot claim %s on %s@%s for run %s: %v — the check stays absent while the review runs",
				gateCtx, repo, shortSHA(sha), runID, err)
		}
		return
	}
	// Read before write: an unreadable status list means we cannot tell a
	// verdict from an absence, and blanking a real verdict is worse than
	// leaving the in-flight window unlabelled.
	cur, readable, err := gateStatusOn(ctx, gc, repo, sha, gateCtx)
	if err != nil || !readable {
		return
	}
	if cur.State != "" && !isGateInFlight(cur) && !isSyntheticGateInterruption(cur.Description) {
		return
	}
	st := forge.CommitStatus{
		State:       forge.CommitStatePending,
		Context:     gateCtx,
		Description: gateInFlightDescription,
		TargetURL:   runURL,
	}
	if err := gc.SetCommitStatus(ctx, repo, sha, st); err != nil {
		if s.logger != nil {
			s.logger.Warn("forge gate: run %s could not claim %s on %s@%s: %v — the check stays absent while the review runs",
				runID, gateCtx, repo, shortSHA(sha), err)
		}
		return
	}
	if s.logger != nil {
		s.logger.Info("forge gate: run %s claimed %s=pending on %s@%s (bot %s)", runID, gateCtx, repo, shortSHA(sha), botID)
	}
}
