package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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

// maxPriorReviewChars caps the rendered prior-review text injected into the
// consumer's prompt: enough for a full finding set INCLUDING each finding's
// ready-made replacement (the whole point of the hand-off — see
// renderPriorReview), bounded so a huge review can't blow the launch var.
const maxPriorReviewChars = 16000

// maxPriorReviewDetail / maxPriorReviewReplacement bound one finding's share, so
// a single verbose finding cannot crowd out every other one.
const (
	maxPriorReviewDetail      = 900
	maxPriorReviewReplacement = 900
)

// priorReviewQuery identifies the pull request a prior review is sought for.
// HeadSHA is the PR's CURRENT head when the caller knows it (empty otherwise):
// a review anchors to the tree it read, so a review of an older head must be
// handed over LABELLED as such, never presented as current.
type priorReviewQuery struct {
	PRURL       string
	ProjectPath string
	PRNumber    int
	HeadSHA     string
}

// findPriorReview resolves the most recent Revi (review-pr) run for the PR and
// renders its findings as the `prior_review` seed text for a `/billy` launch.
// Best-effort: any miss (no run service, no matching run, unreadable artifact)
// returns "" and Billy simply reviews the PR from scratch. Routed through the
// webhookPriorReview seam so handler tests need no run store.
func (s *Server) findPriorReview(ctx context.Context, cfg webhooks.Config, q priorReviewQuery) string {
	fn := s.webhookPriorReview
	if fn == nil {
		fn = s.realWebhookPriorReview
	}
	return fn(ctx, cfg, q)
}

// stampPriorReview seeds a `/billy` (branch-improve-loop) launch with Revi's
// most recent review of the PR under the `prior_review` var, so Billy starts
// from that review instead of re-deriving it. No-op for any other bot, when the
// var is already pinned (operator LaunchVars / ContextVars win), or when no
// prior review exists. Best-effort: never blocks or fails the launch.
func (s *Server) stampPriorReview(ctx context.Context, cfg webhooks.Config, botID string, vars map[string]string, q priorReviewQuery) {
	if botID != branchImproveBotID || vars == nil {
		return
	}
	if _, pinned := vars[priorReviewVar]; pinned {
		return
	}
	if pr := s.findPriorReview(ctx, cfg, q); pr != "" {
		vars[priorReviewVar] = pr
	}
}

// realWebhookPriorReview is the production findPriorReview: scan the run store
// (newest first) for the latest review-pr run whose pr_url input matches this
// PR and whose finding set is readable, then render it.
func (s *Server) realWebhookPriorReview(ctx context.Context, cfg webhooks.Config, q priorReviewQuery) string {
	if s.runs == nil || strings.TrimSpace(q.PRURL) == "" {
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
	// end and stop at the first review-pr run for this PR that RENDERS.
	//
	// "Renders" is the usability test, deliberately in place of a run-status
	// filter: the artifact is the truth. A review still in flight has not
	// written `converge` yet, and returning on the mere id match would hand the
	// consumer an empty seed while a complete older review sat one step further
	// down the list — the freshest COMPLETE review is what saves a round.
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
		if !runTargetsPR(run, q.PRURL) {
			continue
		}
		if rendered := renderPriorReview(ctx, rs, run.ID, q); rendered != "" {
			return rendered
		}
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

const (
	// reviewConvergeNode is the review-pr node whose output artifact carries the
	// merged, de-duplicated finding set (schema emit_output: findings +
	// questions).
	reviewConvergeNode = "converge"
	// reviewMergeNode carries the two families' RAW finding arrays, untouched
	// past converge — the deterministic fallback when the merged set is prose.
	reviewMergeNode = "merge_reviews"
	// reviewPrecheckNode carries `reviewed_sha`: the tree the findings anchor to.
	reviewPrecheckNode = "diff_precheck"
)

// renderPriorReview loads the review run's findings and renders the digest the
// downstream fixer starts from. Returns "" when nothing readable was found, so
// the caller keeps scanning for an older, complete review.
//
// What it renders is the whole value of the hand-off. Revi produces a stable
// id, a severity, a confidence, a cross-family confirmation flag, an anchor
// span, a prose detail, a remediation sketch AND — when the fix is local and
// high-confidence — a literal `replacement` ready to apply. A digest that drops
// those last ones asks the fixer to re-derive a patch that already exists,
// which is exactly the round this hand-off is supposed to save.
func renderPriorReview(ctx context.Context, rs store.RunStore, runID string, q priorReviewQuery) string {
	findings, questions, degraded, ok := loadReviewFindings(ctx, rs, runID)
	if !ok {
		return ""
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Prior review of this PR by Revi (review-pr, run %s).\n", runID)
	if note := reviewAnchorNote(ctx, rs, runID, q.HeadSHA); note != "" {
		b.WriteString(note + "\n")
	}
	if degraded {
		b.WriteString("NOTE: the merged finding set was unreadable, so these come straight from the reviewers, de-duplicated by anchor. Severity threshold, cap and cross-confirmation were not applied.\n")
	}

	tail := renderReviewQuestions(questions)
	if len(findings) == 0 {
		b.WriteString("\nRevi reported no findings. Confirm the diff is clean and improve anything it missed.\n")
		return truncate(b.String()+tail, maxPriorReviewChars)
	}

	fmt.Fprintf(&b, "\n%d finding(s), each with a STABLE id. Verify every one against the current diff — Revi can be wrong, and the branch may have moved. Report per id what you did with it.\n\n", len(findings))

	budget := maxPriorReviewChars - b.Len() - len(tail)
	written := 0
	for _, f := range findings {
		entry := renderFinding(f)
		// Room for this finding AND the "omitted" line the next miss would need.
		if written > 0 && len(entry)+80 > budget {
			break
		}
		b.WriteString(entry)
		budget -= len(entry)
		written++
	}
	if written < len(findings) {
		fmt.Fprintf(&b, "(%d further finding(s) omitted for size — read the full review on the PR.)\n", len(findings)-written)
	}
	return truncate(b.String()+tail, maxPriorReviewChars)
}

// loadReviewFindings resolves the run's finding set, mirroring the recovery the
// bot's own publish step applies: prefer converge's merged array, fall back to
// the two reviewers' raw arrays de-duplicated by anchor when it is unreadable.
// Without the fallback, a review that degraded but still published N findings
// onto the PR hands the fixer an empty seed.
//
// ok distinguishes a review that HAPPENED from one that has nothing to say yet:
// "0 findings" is a verdict worth handing over ("the diff was read and is
// clean"), an absent artifact is a review still in flight and the caller must
// keep looking for an older, complete one.
func loadReviewFindings(ctx context.Context, rs store.RunStore, runID string) (findings []map[string]any, questions string, degraded, ok bool) {
	if art, err := rs.LoadLatestArtifact(ctx, runID, reviewConvergeNode); err == nil && art != nil {
		findings = decodeFindings(art.Data["findings"])
		questions, _ = art.Data["questions"].(string)
		// A readable array — including a genuinely empty one, which converge
		// confirms via total_findings — is the verdict. total_findings > 0 with
		// no readable array is the prose degradation the fallback exists for.
		if len(findings) > 0 || asInt(art.Data["total_findings"]) == 0 {
			return findings, questions, false, true
		}
	}
	art, err := rs.LoadLatestArtifact(ctx, runID, reviewMergeNode)
	if err != nil || art == nil {
		return nil, questions, false, false
	}
	union := append(decodeFindings(art.Data["claude_findings"]), decodeFindings(art.Data["gpt_findings"])...)
	seen := make(map[string]bool, len(union))
	deduped := make([]map[string]any, 0, len(union))
	for _, f := range union {
		key := strField(f, "file") + "|" + strField(f, "line") + "|" + normalizeFindingTitle(strField(f, "title"))
		if seen[key] {
			continue
		}
		seen[key] = true
		deduped = append(deduped, f)
	}
	if len(deduped) == 0 {
		return nil, questions, false, false
	}
	return deduped, questions, true, true
}

// reviewAnchorNote reports the revision the findings anchor to, and says so
// loudly when the PR head has moved since. A stale review presented as current
// makes the fixer "fix" what is already fixed — the exact opposite of the round
// this hand-off saves.
func reviewAnchorNote(ctx context.Context, rs store.RunStore, runID, headSHA string) string {
	art, err := rs.LoadLatestArtifact(ctx, runID, reviewPrecheckNode)
	if err != nil || art == nil {
		return ""
	}
	reviewed := strField(art.Data, "reviewed_sha")
	if reviewed == "" {
		return ""
	}
	head := strings.TrimSpace(headSHA)
	if head == "" || strings.EqualFold(head, reviewed) {
		return "Anchored to " + shortSHA(reviewed) + " (the current head)."
	}
	return "Anchored to " + shortSHA(reviewed) + ", but the PR head is now " + shortSHA(head) +
		" — the branch moved after this review. Diff " + shortSHA(reviewed) + "..HEAD first: a finding may already be fixed, and every line anchor may have shifted."
}

// renderReviewQuestions renders Revi's non-blocking falsifiability channel. They
// are not findings and must never be treated as such — but they are where the
// reviewers say what they could NOT verify, which is precisely where a second
// pass should look.
func renderReviewQuestions(questions string) string {
	lines := make([]string, 0, 8)
	for _, q := range strings.Split(questions, "\n") {
		if q = strings.TrimSpace(q); q != "" {
			lines = append(lines, "- "+q)
		}
	}
	if len(lines) == 0 {
		return ""
	}
	return "\nOpen questions from the reviewers — NOT findings, never fix-and-close them; they mark where the residual risk is:\n" +
		strings.Join(lines, "\n") + "\n"
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

// renderFinding renders one finding as a bounded markdown block, id first.
func renderFinding(f map[string]any) string {
	var b strings.Builder
	sev := strField(f, "severity")
	cat := strField(f, "category")
	title := strField(f, "title")

	b.WriteString("### " + findingID(strField(f, "file"), title))
	if label := strings.Trim(strings.TrimSpace(sev+"/"+cat), "/"); label != "" {
		b.WriteString(" [" + label + "]")
	}
	if title != "" {
		b.WriteString(" " + title)
	}
	if loc := findingAnchor(f); loc != "" {
		b.WriteString(" (" + loc + ")")
	}
	b.WriteString("\n")

	// Confidence and cross-family confirmation are how the fixer decides how
	// hard to verify a finding before acting on it. Dropping them forces it to
	// re-verify everything at equal weight.
	tags := make([]string, 0, 2)
	if c := strField(f, "confidence"); c != "" {
		tags = append(tags, "confidence: "+c)
	}
	if strField(f, "reviewers") == "both" {
		tags = append(tags, "cross-confirmed by both model families")
	}
	if len(tags) > 0 {
		b.WriteString(strings.Join(tags, " · ") + "\n")
	}
	if detail := strField(f, "detail"); detail != "" {
		b.WriteString(truncate(detail, maxPriorReviewDetail) + "\n")
	}
	if sketch := strField(f, "suggestion"); sketch != "" {
		b.WriteString("Fix sketch: " + truncate(sketch, 400) + "\n")
	}
	if repl := strField(f, "replacement"); repl != "" {
		b.WriteString("Replacement Revi already wrote for that anchor span (apply it only after checking it still fits the current code):\n```\n" +
			truncate(repl, maxPriorReviewReplacement) + "\n```\n")
	}
	b.WriteString("\n")
	return b.String()
}

// findingAnchor renders "file:line" or "file:line-line_end".
func findingAnchor(f map[string]any) string {
	file := strField(f, "file")
	if file == "" {
		return ""
	}
	line, end := strField(f, "line"), strField(f, "line_end")
	if line == "" || line == "0" {
		return file
	}
	if end != "" && end != "0" && end != line {
		return file + ":" + line + "-" + end
	}
	return file + ":" + line
}

// findingID derives a short, stable identifier for a review finding. It is the
// shared handle between Revi's inline PR comment, the operator's arbitration
// ("skip R7a3f, it's a false positive") and the fixer's report of what it did
// with each one.
//
// Keyed on (file, normalized title) and deliberately NOT on the line: the line
// moves whenever code above it changes, which is exactly what happens between a
// review and the fix — an id that changed there could not survive the one
// round-trip it exists for. The same derivation runs in review-pr's publish
// step; bots/review_pr_finding_id_test.go pins the two against each other.
func findingID(file, title string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(file) + "\n" + normalizeFindingTitle(title)))
	return "R" + hex.EncodeToString(sum[:])[:4]
}

// normalizeFindingTitle collapses a title to the form both id derivations hash:
// trimmed, lower-cased, inner whitespace collapsed, capped at 80 chars.
func normalizeFindingTitle(title string) string {
	t := strings.ToLower(strings.Join(strings.Fields(title), " "))
	if len(t) > 80 {
		t = t[:80]
	}
	return t
}

func shortSHA(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

// strField reads a map field as a trimmed string, whatever JSON type it decoded
// to (a line number arrives as float64 through encoding/json).
func strField(m map[string]any, key string) string {
	v, ok := m[key]
	if !ok || v == nil {
		return ""
	}
	if n, ok := v.(float64); ok && n == float64(int64(n)) {
		return fmt.Sprint(int64(n))
	}
	return strings.TrimSpace(fmt.Sprint(v))
}

func asInt(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case int64:
		return int(n)
	}
	return 0
}
