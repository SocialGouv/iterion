package server

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/SocialGouv/iterion/pkg/store"
)

// A gating run that pauses for a human keeps the in-flight claim it posted at
// launch, and that claim reads "review in progress" for as long as the pause
// lasts: a required check pending for days, a pull request nobody can merge,
// and nothing anywhere saying a person is expected to act. Measured on
// SocialGouv/iterion from 2026-10-12 to 2026-11-26: 25 Vetty runs paused (23
// on a node recovery pause after AUTH_FAILED, 2 at a human node), each left
// the required check pending until its pull request closed, and the operator
// read the silence as "the reviewer does not review" (#3768).
//
// So once the pause has lasted gatePauseAnswerAfter, the reconciler answers
// the claim the way it answers a dead run's — a synthetic `failure` on the
// head, through the same verdict-order authority — with a description that
// names the pause and its cure, and the pull request gets ONE comment saying
// the same. Unlike a dead run's repair it relaunches nothing: the run is
// alive, it waits for a human, and a second run would duplicate it. When the
// run resumes and publishes, its verdict — decided after the pause began —
// supersedes the synthetic failure.
//
// Only paused_waiting_human is answered (a human node, an await, and the
// recovery dispatcher's pause for a failed node all land there). An
// operator's own pause (paused_operator) is left alone: the person who paused
// it is the one who knows, and the sweep's enumeration — the shared
// ListNotifiableRuns query the notification and ops-alert sweeps also read —
// does not list it.

// EnvGatePauseAnswerAfter overrides how long a gating run may stay paused for
// a human before its unanswered claim is answered with a synthetic failure.
// A Go duration; `0`, a negative duration or `off` disables the answer (a
// paused run then holds its claim for as long as it stays paused).
const EnvGatePauseAnswerAfter = "ITERION_GATE_PAUSE_ANSWER_AFTER"

// defaultGatePauseAnswerAfter is one hour: long enough that an operator who
// answers or fixes the credential within minutes never sees the pull request
// turn red, short enough that a required check does not sit pending through a
// working day. Paused runs reach the sweep on every deep pass (every
// gateDeepSweepEvery passes, half an hour at the default cadence) and on
// every fast pass whose page is not full, so the answer lands between one
// hour and about ninety minutes after the pause began. Far inside the
// publish grant's life (forgePublishDefaultTTL), which the answer needs.
const defaultGatePauseAnswerAfter = time.Hour

// gatePauseAnswerSetting is the resolved override. configured=false means
// unset: the default applies. A configured value of zero or less is off.
type gatePauseAnswerSetting struct {
	after      time.Duration
	configured bool
}

// gatePauseAnswerFromEnv reads EnvGatePauseAnswerAfter. A value that does not
// parse keeps the default and warns naming the variable; a value at least as
// long as the publish grant's life is kept, with a warning that the grant may
// be gone by then — the operator's explicit choice is never replaced in
// silence.
func gatePauseAnswerFromEnv(getenv func(string) string, warnf func(format string, args ...any)) gatePauseAnswerSetting {
	raw := strings.TrimSpace(getenv(EnvGatePauseAnswerAfter))
	if raw == "" {
		return gatePauseAnswerSetting{}
	}
	if strings.EqualFold(raw, "off") {
		warnf("merge-gate reconciler: %s=off — a gating run paused for a human holds its required check pending for as long as the pause lasts", EnvGatePauseAnswerAfter)
		return gatePauseAnswerSetting{after: 0, configured: true}
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		warnf("merge-gate reconciler: %s=%q is neither off nor a Go duration — keeping the default %s", EnvGatePauseAnswerAfter, raw, defaultGatePauseAnswerAfter)
		return gatePauseAnswerSetting{}
	}
	if d <= 0 {
		warnf("merge-gate reconciler: %s=%s disables the answer — a gating run paused for a human holds its required check pending for as long as the pause lasts", EnvGatePauseAnswerAfter, raw)
		return gatePauseAnswerSetting{after: 0, configured: true}
	}
	if d >= forgePublishDefaultTTL {
		warnf("merge-gate reconciler: %s=%s reaches the %s life of a publish grant — a run paused that long may have no grant left to answer its check with", EnvGatePauseAnswerAfter, d, forgePublishDefaultTTL)
	}
	return gatePauseAnswerSetting{after: d, configured: true}
}

// gatePauseAnswerAfter is the bound in force; zero or less means off.
func (s *Server) gatePauseAnswerAfter() time.Duration {
	if s != nil && s.gatePauseAnswer.configured {
		return s.gatePauseAnswer.after
	}
	return defaultGatePauseAnswerAfter
}

// gatePauseIsDue reports whether a paused run has waited long enough for its
// claim to be answered. The pause is dated by updated_at, which the status
// transition into the pause stamps; a later write on the paused document only
// moves the instant later, never earlier.
func (s *Server) gatePauseIsDue(run *store.Run) bool {
	after := s.gatePauseAnswerAfter()
	if after <= 0 || run == nil || run.UpdatedAt.IsZero() {
		return false
	}
	return s.gateNow().Sub(run.UpdatedAt) >= after
}

// gatePauseFirstOffers reports whether the sweep is among its first offers of
// a paused run past the bound — the window an abstain speaks in at Warn. A
// paused run is offered on every pass for as long as it stays paused, so a
// Warn on each would repeat forever; two deep intervals after the bound is
// when the first deep pass has certainly reached it.
func (s *Server) gatePauseFirstOffers(run *store.Run) bool {
	after := s.gatePauseAnswerAfter()
	if after <= 0 || run == nil || run.UpdatedAt.IsZero() {
		return false
	}
	_, deepEvery := s.gateSweepCadence()
	return s.gateNow().Sub(run.UpdatedAt) < after+2*time.Duration(deepEvery)*s.gateSweepEvery()
}

// gatePausedDescriptionPrefix opens the synthetic status a paused run's claim
// is answered with. isSyntheticGateInterruption recognises it, so the
// auto-fix lane reads no findings into it and a re-assert never puts it back
// over a real verdict.
const gatePausedDescriptionPrefix = "review paused ("

// The two remedies: what resumes the run, which the status's target URL
// links. A recovery pause re-executes its failed node once the cause is fixed
// and the recovery prompt answered; a human pause resumes on the answer.
const (
	gatePausedRecoveryRemedy = ") — fix the cause, then answer the run's recovery prompt to resume it"
	gatePausedHumanRemedy    = ") — answer the run's question to resume it"
)

// gatePausedDescription renders the synthetic status for a paused run. The
// remedy keeps priority over the reason within the 140 runes a forge allows.
func gatePausedDescription(run *store.Run) string {
	reason, remedy := gatePausedReason(run)
	budget := 140 - utf8.RuneCountInString(gatePausedDescriptionPrefix) - utf8.RuneCountInString(remedy)
	return gatePausedDescriptionPrefix + boundedRunes(reason, budget) + remedy
}

// gatePausedReason names the pause from the run's checkpoint: the failure a
// recovery pause was written for, or the node waiting for a human.
func gatePausedReason(run *store.Run) (reason, remedy string) {
	var cp *store.Checkpoint
	if run != nil {
		cp = run.Checkpoint
	}
	node := strings.TrimSpace(cp.PausedNodeID())
	if cp != nil && cp.RecoveryPause {
		code := strings.TrimSpace(cp.RecoveryCode)
		if code == "" {
			code = "a node failed"
		}
		if node != "" {
			return code + " at " + node, gatePausedRecoveryRemedy
		}
		return code, gatePausedRecoveryRemedy
	}
	if node != "" {
		return "waiting for a human at " + node, gatePausedHumanRemedy
	}
	return "waiting for a human", gatePausedHumanRemedy
}

// gatePausedNoticeMarker tags the comment, distinct from the quota pause's:
// that one promises an automatic resume, this one says a human must act.
const gatePausedNoticeMarker = "<!-- iterion:gate-paused-for-human -->"

// noticeGatePausedForHuman posts the one comment that goes with the synthetic
// failure. Once per pause: the reconciler calls it only after it posted the
// failure itself, and the next offer finds that failure on the head (or the
// settle mark) and goes no further. Best-effort like the other gate notices:
// the check is already answered, so a comment that cannot land is logged and
// dropped.
func (s *Server) noticeGatePausedForHuman(ctx context.Context, run *store.Run, gateCtx string) {
	if s == nil || run == nil {
		return
	}
	target, ok := s.gateNoticeTarget(ctx, run, "paused-for-human notice")
	if !ok {
		return
	}
	runURL := gateRunURL(strings.TrimRight(strings.TrimSpace(s.cfg.PublicURL), "/"), run.ID)
	body := gatePausedNoticeBody(run, gateCtx, runURL, s.gateNow())
	if _, err := target.commenter.CommentIssue(ctx, target.repo, target.number, body); err != nil {
		s.gateNoticeDebug(run, "paused-for-human notice", "%v", err)
		return
	}
	if s.logger != nil {
		s.logger.Info("forge gate: run %s is paused for a human — notice posted on %s", run.ID, target.prURL)
	}
}

// gatePausedNoticeBody renders the comment: what is paused and since when,
// why the check turned red, what resumes the run, and that its own verdict
// replaces the failure.
func gatePausedNoticeBody(run *store.Run, gateCtx, runURL string, now time.Time) string {
	var cp *store.Checkpoint
	if run != nil {
		cp = run.Checkpoint
	}
	recovery := cp != nil && cp.RecoveryPause
	node := strings.TrimSpace(cp.PausedNodeID())
	var b strings.Builder
	b.WriteString(gatePausedNoticeMarker)
	b.WriteString("\n⏸️ **Run paused — it waits for a human.** The check it owes is marked failed until it resumes.\n\n")
	b.WriteString("The run")
	if node != "" {
		fmt.Fprintf(&b, " stopped at node `%s`", node)
	}
	if recovery {
		code := strings.TrimSpace(cp.RecoveryCode)
		if code == "" {
			code = "a failure"
		}
		fmt.Fprintf(&b, " after `%s` and paused for a human", code)
	} else {
		b.WriteString(" and asked a question only a human can answer")
	}
	if run != nil && !run.UpdatedAt.IsZero() {
		fmt.Fprintf(&b, " at %s (%s ago)", run.UpdatedAt.UTC().Format("15:04 UTC on 2006-01-02"), now.Sub(run.UpdatedAt).Round(time.Minute))
	}
	b.WriteString(". ")
	fmt.Fprintf(&b, "The required check `%s` it claimed would otherwise stay pending for as long as the pause lasts, so iterion marked it **failed** on this head. ", gateCtx)
	b.WriteString("The run is not cancelled and not relaunched.\n\n")
	if recovery {
		b.WriteString("**To continue:** fix the cause (for an `AUTH_FAILED`, renew or replace the credential the bot uses), then answer the run's recovery prompt to resume it")
	} else {
		b.WriteString("**To continue:** answer the run's question to resume it")
	}
	if runURL != "" {
		fmt.Fprintf(&b, ": %s", runURL)
	}
	b.WriteString(". When the run resumes and publishes, its verdict replaces this failure.\n")
	if run != nil {
		if cause := gatePauseCause(run); cause != "" {
			fmt.Fprintf(&b, "\n> %s\n", cause)
		}
	}
	return b.String()
}
