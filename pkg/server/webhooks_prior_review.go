package server

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/webhooks"
)

// reviewBotID is the read-only cross-family reviewer (Revi) whose findings a
// `/billy` invocation on the same PR picks up as its starting point.
const reviewBotID = defaultWebhookBotReviewPR

// priorReviewVar is the launch var Billy (branch-improve-loop) reads to start
// from Revi's review instead of re-deriving it. Declared on the bot's main.bot.
const priorReviewVar = "prior_review"

// maxPriorReviewRunsScanned bounds the reverse scan for the most recent Revi run
// on a PR. Run ids are UUIDv7 and ListRuns returns them created-at ascending on
// both backends, so scanning from the newest end and stopping at the first match
// finds the latest review after loading only a few runs in the common case; the
// cap keeps a pathological store (a PR never reviewed) from a full-table scan on
// this manual, best-effort path.
const maxPriorReviewRunsScanned = 400

// maxPriorReviewChars caps the rendered prior-review text injected into Billy's
// prompt (~a few thousand tokens): enough for Revi's finding set, bounded so a
// huge review can't blow the launch var / prompt budget.
const maxPriorReviewChars = 12000

// findPriorReview resolves the most recent Revi (review-pr) run for the PR and
// renders its findings as the `prior_review` seed text for a `/billy` launch.
// Best-effort: any miss (no run service, no matching run, unreadable artifact)
// returns "" and Billy simply reviews the PR from scratch. Routed through the
// webhookPriorReview seam so handler tests need no run store.
func (s *Server) findPriorReview(ctx context.Context, cfg webhooks.Config, prURL, projectPath string, prNumber int) string {
	fn := s.webhookPriorReview
	if fn == nil {
		fn = s.realWebhookPriorReview
	}
	return fn(ctx, cfg, prURL, projectPath, prNumber)
}

// stampPriorReview seeds a `/billy` (branch-improve-loop) launch with Revi's
// most recent review of the PR under the `prior_review` var, so Billy starts
// from that review instead of re-deriving it. No-op for any other bot, when the
// var is already pinned (operator LaunchVars / ContextVars win), or when no
// prior review exists. Best-effort: never blocks or fails the launch.
func (s *Server) stampPriorReview(ctx context.Context, cfg webhooks.Config, botID string, vars map[string]string, prURL, projectPath string, prNumber int) {
	if botID != branchImproveBotID || vars == nil {
		return
	}
	if _, pinned := vars[priorReviewVar]; pinned {
		return
	}
	if pr := s.findPriorReview(ctx, cfg, prURL, projectPath, prNumber); pr != "" {
		vars[priorReviewVar] = pr
	}
}

// realWebhookPriorReview is the production findPriorReview: scan the run store
// (newest first) for the latest terminal review-pr run whose pr_url input
// matches this PR, load its merged-findings artifact, and render it.
func (s *Server) realWebhookPriorReview(ctx context.Context, cfg webhooks.Config, prURL, projectPath string, prNumber int) string {
	if s.runs == nil || strings.TrimSpace(prURL) == "" {
		return ""
	}
	rs := s.runs.RunStore()
	if rs == nil {
		return ""
	}
	ctx = store.WithTenant(ctx, cfg.TenantID)
	ids, err := rs.ListRuns(ctx)
	if err != nil || len(ids) == 0 {
		return ""
	}
	// ListRuns is created-at ascending on both backends; walk from the newest
	// end and stop at the first review-pr run for this PR (the latest review).
	scanned := 0
	for i := len(ids) - 1; i >= 0 && scanned < maxPriorReviewRunsScanned; i-- {
		scanned++
		run, lerr := rs.LoadRun(ctx, ids[i])
		if lerr != nil {
			continue
		}
		if run.BotID != reviewBotID {
			continue
		}
		if !runTargetsPR(run, prURL) {
			continue
		}
		return renderPriorReview(ctx, rs, run.ID, prURL)
	}
	return ""
}

// runTargetsPR reports whether the run's launch inputs pin the given PR url.
func runTargetsPR(run *store.Run, prURL string) bool {
	if run == nil {
		return false
	}
	v, ok := run.Inputs["pr_url"]
	if !ok {
		return false
	}
	got, ok := v.(string)
	return ok && strings.EqualFold(strings.TrimSpace(got), strings.TrimSpace(prURL))
}

// reviewConvergeNode is the review-pr node whose output artifact carries the
// merged, de-duplicated finding set (schema emit_output: findings + counts).
const reviewConvergeNode = "converge"

// renderPriorReview loads the review run's merged-findings artifact and renders
// a compact, bounded markdown digest suitable for injection into Billy's prompt.
// Returns "" on any read failure.
func renderPriorReview(ctx context.Context, rs store.RunStore, runID, prURL string) string {
	art, err := rs.LoadLatestArtifact(ctx, runID, reviewConvergeNode)
	if err != nil || art == nil {
		return ""
	}
	findings := decodeFindings(art.Data["findings"])
	var b strings.Builder
	fmt.Fprintf(&b, "Prior review of this PR by Revi (review-pr, run %s):\n", runID)
	if len(findings) == 0 {
		b.WriteString("Revi reviewed the PR and reported no findings. Confirm the diff is clean and improve anything it missed.\n")
		return truncate(b.String(), maxPriorReviewChars)
	}
	fmt.Fprintf(&b, "%d finding(s). Address each, then continue reviewing the diff for anything Revi missed:\n\n", len(findings))
	for _, f := range findings {
		writeFinding(&b, f)
		if b.Len() >= maxPriorReviewChars {
			break
		}
	}
	return truncate(b.String(), maxPriorReviewChars)
}

// decodeFindings normalises the artifact's `findings` value — persisted either
// as a decoded []any (filesystem JSON) or re-marshalled string (defensive) —
// into a slice of finding maps.
func decodeFindings(raw any) []map[string]any {
	switch v := raw.(type) {
	case []any:
		out := make([]map[string]any, 0, len(v))
		for _, e := range v {
			if m, ok := e.(map[string]any); ok {
				out = append(out, m)
			}
		}
		return out
	case string:
		var arr []map[string]any
		if json.Unmarshal([]byte(v), &arr) == nil {
			return arr
		}
	}
	return nil
}

// writeFinding renders one finding as a bounded markdown bullet.
func writeFinding(b *strings.Builder, f map[string]any) {
	sev := strings.TrimSpace(fmt.Sprint(firstNonEmpty(f["severity"], "")))
	cat := strings.TrimSpace(fmt.Sprint(firstNonEmpty(f["category"], "")))
	title := strings.TrimSpace(fmt.Sprint(firstNonEmpty(f["title"], "")))
	file := strings.TrimSpace(fmt.Sprint(firstNonEmpty(f["file"], "")))
	line := strings.TrimSpace(fmt.Sprint(firstNonEmpty(f["line"], "")))
	detail := strings.TrimSpace(fmt.Sprint(firstNonEmpty(f["detail"], "")))

	head := "-"
	if sev != "" || cat != "" {
		head += " [" + strings.TrimSpace(sev+"/"+cat) + "]"
	}
	if title != "" {
		head += " " + title
	}
	if file != "" {
		loc := file
		if line != "" && line != "0" {
			loc += ":" + line
		}
		head += " (" + loc + ")"
	}
	b.WriteString(head + "\n")
	if detail != "" {
		b.WriteString("  " + truncate(detail, 600) + "\n")
	}
}

func firstNonEmpty(v any, fallback string) any {
	if v == nil {
		return fallback
	}
	if s, ok := v.(string); ok && strings.TrimSpace(s) == "" {
		return fallback
	}
	return v
}
