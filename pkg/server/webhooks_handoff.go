package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/SocialGouv/iterion/pkg/botregistry"
	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/eventbus"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/webhooks"
)

// maxHandoffRunsScanned bounds the reverse scan for the most recent review run
// on a PR. Run ids are UUIDv7 and ListRuns returns them created-at ascending on
// both backends, so scanning from the newest end and stopping at the first match
// finds the latest review after loading only a few runs in the common case; the
// cap keeps a pathological store (a PR never reviewed) from a full-table scan on
// this manual, best-effort path.
const maxHandoffRunsScanned = 400

// maxHandoffChars caps the rendered prior-review text injected into the
// consumer's prompt: enough for a full finding set INCLUDING each finding's
// ready-made replacement (the whole point of the hand-off — see
// renderReviewDigest), bounded so a huge review can't blow the launch var.
const maxHandoffChars = 16000

// maxFindingDetail / maxFindingReplacement bound one finding's share, so
// a single verbose finding cannot crowd out every other one.
const (
	maxFindingDetail      = 900
	maxFindingReplacement = 900
)

// handoffQuery identifies the pull request a prior review is sought for.
// HeadSHA is the PR's CURRENT head when the caller knows it (empty otherwise):
// a review anchors to the tree it read, so a review of an older head must be
// handed over LABELLED as such, never presented as current.
type handoffQuery struct {
	PRURL   string
	HeadSHA string
}

// findHandoff resolves the most recent run PRODUCING the given kind for this PR
// and renders it as the text the consumer starts from. Best-effort: any miss
// (no run service, no producer, no matching run, unreadable artifact) returns ""
// and the consumer proceeds without it. Routed through the webhookPriorReview
// seam so handler tests need no run store.
func (s *Server) findHandoff(ctx context.Context, cfg webhooks.Config, kind bundle.HandoffKind, q handoffQuery) string {
	fn := s.webhookHandoff
	if fn == nil {
		fn = s.realWebhookHandoff
	}
	return fn(ctx, cfg, kind, q)
}

// stampHandoffs seeds a launch with what earlier runs on the same PR left
// behind, so the launched bot starts from it instead of re-deriving it.
//
// Which bot receives what, into which var, and from whose artifact are all
// DECLARED, not hardcoded: the launched bot's manifest asks (`consumes:`) and
// any bot whose manifest offers the same kind (`produces:`) supplies it. That is
// what lets a reviewer and a fixer cooperate — in BOTH directions, the review
// forward and the ledger back — without the engine, or either manifest, naming
// the other bot. An operator-pinned var always wins; a miss is silent by design.
func (s *Server) stampHandoffs(ctx context.Context, cfg webhooks.Config, botID string, vars map[string]string, q handoffQuery) {
	// Every hand-off is PR-scoped, so a lane without one has nothing to resolve —
	// checked before the catalog is read, or every push event and every bot that
	// consumes nothing would pay a full bundle scan to learn it has no work.
	if vars == nil || strings.TrimSpace(q.PRURL) == "" {
		return
	}
	for _, want := range s.handoffConsumersFor(botID) {
		if _, pinned := vars[want.Var]; pinned {
			continue
		}
		// Always claim the key, even on a miss. Two lanes call this (the command
		// dispatch and the launch tail, since neither covers every path), and a
		// miss leaves a full reverse scan of the run store to be repeated by the
		// second. An empty value is what the bot would have defaulted to anyway.
		vars[want.Var] = s.findHandoff(ctx, cfg, want.Kind, q)
	}
}

// handoffConsumersFor returns the bot's declared PR-scoped consumption entries.
func (s *Server) handoffConsumersFor(botID string) []bundle.ConsumedArtifact {
	if strings.TrimSpace(botID) == "" {
		return nil
	}
	entry, ok, err := botregistry.FindByName(s.botListOptions(), botID)
	if err != nil {
		s.logWarn("handoff: cannot read the bot catalog, %s will be launched without its declared seeds: %v", botID, err)
		return nil
	}
	if !ok {
		return nil
	}
	var out []bundle.ConsumedArtifact
	for _, c := range entry.Consumes {
		if c.EffectiveScope() == bundle.HandoffScopePR {
			out = append(out, c)
		}
	}
	return out
}

// realWebhookHandoff is the production findHandoff: scan the run store (newest
// first) for the latest run of a bot PRODUCING this kind whose pr_url input
// matches this PR and whose payload is readable, then render it through that
// producer's own declared node names.
func (s *Server) realWebhookHandoff(ctx context.Context, cfg webhooks.Config, kind bundle.HandoffKind, q handoffQuery) string {
	if s.runs == nil || strings.TrimSpace(q.PRURL) == "" {
		return ""
	}
	producers := s.handoffProducers(kind)
	if len(producers) == 0 {
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
	// end and stop at the first matching run for this PR that RENDERS.
	//
	// "Renders" is the usability test, deliberately in place of a run-status
	// filter: the artifact is the truth. A review still in flight has not
	// written its merged set yet, and returning on the mere id match would hand
	// the consumer an empty seed while a complete older review sat one step
	// further down the list — the freshest COMPLETE review is what saves a round.
	scanned := 0
	for i := len(ids) - 1; i >= 0 && scanned < maxHandoffRunsScanned; i-- {
		scanned++
		run, lerr := rs.LoadRun(ctx, ids[i])
		if lerr != nil {
			continue
		}
		spec, produces := producers[run.BotID]
		if !produces {
			continue
		}
		if !runTargetsPR(run, q.PRURL) {
			continue
		}
		if rendered := renderHandoff(ctx, rs, run, kind, spec, q); rendered != "" {
			return rendered
		}
	}
	return ""
}

// handoffProducers maps each discovered bot that declares it produces this kind
// to the node layout to read it from.
//
// Resolved against the BAKED CATALOG only, not the tenant-merged set: a webhook
// delivery carries no active-team context to read team-authored bundles with.
// A team that forks a reviewer in the cloud editor therefore does not
// participate in the hand-off — a real boundary, stated here because a miss is
// silent and would otherwise read as "nothing reviewed this PR".
func (s *Server) handoffProducers(kind bundle.HandoffKind) map[string]bundle.ProducedArtifact {
	entries, err := botregistry.List(s.botListOptions())
	if err != nil {
		// Swallowing this disables every hand-off, and a missing seed is
		// indistinguishable by design from "nothing reviewed this PR" — so the
		// only way an operator ever learns is if we say it here.
		s.logWarn("handoff: cannot read the bot catalog, no %s producer will be found: %v", kind, err)
		return nil
	}
	out := make(map[string]bundle.ProducedArtifact, 2)
	for _, e := range entries {
		for _, p := range e.Produces {
			if p.Kind == kind {
				out[e.Name] = p
				break
			}
		}
	}
	return out
}

// renderHandoff dispatches to the renderer for the kind. The engine knows the
// SHAPE of each role's payload; the producing bot's declaration says where in
// its own graph to find it.
func renderHandoff(ctx context.Context, rs store.RunStore, run *store.Run, kind bundle.HandoffKind, spec bundle.ProducedArtifact, q handoffQuery) string {
	switch kind {
	case bundle.HandoffKindReview:
		return renderReviewDigest(ctx, rs, run, spec, q)
	case bundle.HandoffKindReviewLedger:
		return renderReviewLedger(ctx, rs, run.ID, spec)
	}
	return ""
}

// runTargetsPR reports whether the run's launch inputs pin the given PR url.
func runTargetsPR(run *store.Run, prURL string) bool {
	got := runInputString(run, "pr_url")
	return got != "" && strings.EqualFold(got, strings.TrimSpace(prURL))
}

// renderReviewDigest loads the review run's findings and renders the digest the
// downstream fixer starts from. Returns "" when nothing readable was found, so
// the caller keeps scanning for an older, complete review.
//
// What it renders is the whole value of the hand-off. A review carries a stable
// id, a severity, a confidence, a cross-family confirmation flag, an anchor
// span, a prose detail, a remediation sketch AND — when the fix is local and
// high-confidence — a literal `replacement` ready to apply. A digest that drops
// those last ones asks the fixer to re-derive a patch that already exists,
// which is exactly the round this hand-off is supposed to save.
//
// `spec` is the producing bot's own declaration of where those live in its
// graph, so this function knows the SHAPE of a review and nothing about the bot
// that produced it.
func renderReviewDigest(ctx context.Context, rs store.RunStore, run *store.Run, spec bundle.ProducedArtifact, q handoffQuery) string {
	runID := run.ID
	findings, questions, degraded, ok := loadReviewFindings(ctx, rs, run, spec)
	if !ok {
		return ""
	}

	var b strings.Builder
	if run.Status == store.RunStatusFinished {
		fmt.Fprintf(&b, "Prior review of this PR (run %s).\n", runID)
	} else {
		// A run that did not finish still has a real merged set if it wrote one,
		// but the consumer must be able to tell that from a clean review — a bare
		// "the set was unreadable" reads identically to the producer's own prose
		// degradation, which is a different problem with a different remedy.
		fmt.Fprintf(&b, "Prior review of this PR (run %s) — that run ended %s, so it may have stopped before reviewing everything.\n", runID, run.Status)
	}
	if note := reviewAnchorNote(ctx, rs, runID, spec.AnchorNode, q.HeadSHA); note != "" {
		b.WriteString(note + "\n")
	}
	if degraded {
		b.WriteString("NOTE: the merged finding set was unreadable, so these come straight from the reviewers, de-duplicated by anchor. Severity threshold, cap and cross-confirmation were not applied.\n")
	}

	tail := renderReviewQuestions(questions)
	if len(findings) == 0 {
		b.WriteString("\nThe review reported no findings. Confirm the diff is clean and improve anything it missed.\n")
		return truncate(b.String()+tail, maxHandoffChars)
	}

	fmt.Fprintf(&b, "\n%d finding(s), each with a STABLE id. Verify every one against the current diff — the reviewer can be wrong, and the branch may have moved. Report per id what you did with it.\n\n", len(findings))

	budget := maxHandoffChars - b.Len() - len(tail)
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
	return truncate(b.String()+tail, maxHandoffChars)
}

// loadReviewFindings resolves the run's finding set, mirroring the recovery the
// producing bot's own publish step applies: prefer the merged array, fall back to
// the two reviewers' raw arrays de-duplicated by anchor when it is unreadable.
// Without the fallback, a review that degraded but still published N findings
// onto the PR hands the fixer an empty seed.
//
// ok distinguishes a review that HAPPENED from one that has nothing to say yet:
// "0 findings" is a verdict worth handing over ("the diff was read and is
// clean"), an absent artifact is a review still in flight and the caller must
// keep looking for an older, complete one.
func loadReviewFindings(ctx context.Context, rs store.RunStore, run *store.Run, spec bundle.ProducedArtifact) (findings []map[string]any, questions string, degraded, ok bool) {
	runID := run.ID
	if art, err := rs.LoadLatestArtifact(ctx, runID, spec.Node); err == nil && art != nil {
		findings = decodeFindings(art.Data["findings"])
		questions, _ = art.Data["questions"].(string)
		// A readable array — including a genuinely empty one, which the producer
		// confirms via total_findings — is the verdict. total_findings > 0 with
		// no readable array is the prose degradation the fallback exists for.
		if len(findings) > 0 || asInt(art.Data["total_findings"]) == 0 {
			return findings, questions, false, true
		}
	}
	// The fallback exists for a merged set that came back as PROSE — not for a
	// run that never got that far. Consulting it on an unfinished run makes a
	// CRASHED review render, and since the scan takes the first run that
	// renders, it then shadows the last complete review: the fixer is handed
	// raw, un-thresholded reviewer output while a merged `[high]` finding sits
	// one step further down the list, invisible.
	//
	// (That only became reachable when the fallback node started publishing —
	// before that it rendered nothing and the scan walked past.)
	if run.Status != store.RunStatusFinished {
		return nil, questions, false, false
	}
	// The fallback nodes carry finding arrays under their own field names, which
	// the producer alone knows — so every json-array field is taken, unioned and
	// de-duplicated by anchor. An un-merged set beats no set at all.
	var union []map[string]any
	for _, node := range spec.FallbackNodes {
		art, err := rs.LoadLatestArtifact(ctx, runID, node)
		if err != nil || art == nil {
			continue
		}
		keys := make([]string, 0, len(art.Data))
		for k := range art.Data {
			keys = append(keys, k)
		}
		sort.Strings(keys) // map order would vary the order, and the budget the SUBSET
		for _, k := range keys {
			union = append(union, decodeFindings(art.Data[k])...)
		}
	}
	seen := make(map[string]bool, len(union))
	deduped := make([]map[string]any, 0, len(union))
	for _, f := range union {
		if strField(f, "title") == "" {
			continue // not a finding: some other json field on the same node
		}
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
func reviewAnchorNote(ctx context.Context, rs store.RunStore, runID, anchorNode, headSHA string) string {
	if strings.TrimSpace(anchorNode) == "" {
		return ""
	}
	art, err := rs.LoadLatestArtifact(ctx, runID, anchorNode)
	if err != nil || art == nil {
		return ""
	}
	reviewed := strField(art.Data, "reviewed_sha")
	if reviewed == "" {
		return ""
	}
	head := strings.TrimSpace(headSHA)
	if head == "" {
		// The caller could not tell us the PR's head — the merge-queue auto-heal
		// lane is one, and it fires precisely when the base moved. Asserting
		// currency here is the stale-as-current framing this function exists to
		// prevent, so say what is known and let the consumer check.
		return "Anchored to " + shortSHA(reviewed) + " — verify it is still the PR head before trusting a line anchor."
	}
	if strings.EqualFold(head, reviewed) {
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
		b.WriteString(truncate(detail, maxFindingDetail) + "\n")
	}
	if sketch := strField(f, "suggestion"); sketch != "" {
		b.WriteString("Fix sketch: " + truncate(sketch, 400) + "\n")
	}
	if repl := strField(f, "replacement"); repl != "" {
		b.WriteString("Replacement the reviewer already wrote for that anchor span (apply it only after checking it still fits the current code):\n```\n" +
			truncate(repl, maxFindingReplacement) + "\n```\n")
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
// round-trip it exists for. A producing bot derives the same id in its own
// publish step; bots/review_pr_finding_id_test.go pins the two against each
// other, since a drift between the two would be silent.
// findingIDHexLen: 24 bits. At 4 the birthday odds reached ~2% over a 50-finding
// review, and a collision makes one ledger entry silently resolve two findings.
const findingIDHexLen = 6

func findingID(file, title string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(file) + "\n" + normalizeFindingTitle(title)))
	return "R" + hex.EncodeToString(sum[:])[:findingIDHexLen]
}

// normalizeFindingTitle collapses a title to the form both id derivations hash:
// trimmed, lower-cased, inner whitespace collapsed, capped at 80 chars.
func normalizeFindingTitle(title string) string {
	t := strings.ToLower(strings.Join(strings.Fields(title), " "))
	// RUNES, not bytes: the producing bot slices a python str, which counts
	// characters. Cutting at 80 bytes made every accented or CJK title hash
	// differently on the two sides — and split a rune, hashing invalid UTF-8.
	if r := []rune(t); len(r) > 80 {
		t = string(r[:80])
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
	switch v := m[key].(type) {
	case nil:
		return ""
	case string:
		return strings.TrimSpace(v)
	case float64:
		if v == float64(int64(v)) {
			return fmt.Sprint(int64(v))
		}
		return fmt.Sprint(v)
	default:
		return strings.TrimSpace(fmt.Sprint(v))
	}
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

// maxLedgerNote bounds one ledger entry's note — a refusal argues its case, and
// the argument is the point, but it must not crowd out the other entries.
const maxLedgerNote = 400

// The outcomes a ledger entry can carry. The producing bot writes these strings.
const (
	ledgerFixed    = "fixed"
	ledgerRefused  = "refused"
	ledgerDeferred = "deferred"
)

// renderReviewLedger renders the REPLY half of the hand-off: what an earlier
// fixer did with each finding of a review on this PR.
//
// Its job is anti-oscillation. Without it a reviewer re-raises, on every pass,
// a finding the fixer already contested — the relay ADR-058 removed from the
// catalog precisely because it never settles. The refusals are therefore
// rendered WITH their argument and with an explicit instruction: re-raise only
// against new evidence that answers it. Deliberately NOT a licence to drop a
// finding — a reviewer that still disagrees says what the argument misses, and
// the merge gate stays red on an unfixed finding regardless.
func renderReviewLedger(ctx context.Context, rs store.RunStore, runID string, spec bundle.ProducedArtifact) string {
	art, err := rs.LoadLatestArtifact(ctx, runID, spec.Node)
	if err != nil || art == nil {
		return ""
	}
	// Bucket on the KNOWN statuses only. Keying on whatever the producer wrote
	// would let an unrecognised one ("wontfix") count as content and emit the
	// preamble with no section under it.
	sections := []struct {
		status, header string
		entries        []map[string]any
	}{
		{ledgerRefused, "\nCONTESTED — it judged these not to be real problems, with its argument. Do NOT re-raise one unless you have NEW evidence that answers the argument; if you still disagree, raise it and say precisely what the argument misses:\n", nil},
		{ledgerFixed, "\nREPORTED FIXED — verify against the current diff; re-raise only if the fix is wrong, incomplete, or broke something else:\n", nil},
		{ledgerDeferred, "\nDEFERRED — still open, raise them again:\n", nil},
	}
	total := 0
	for _, e := range decodeFindings(art.Data["finding_ledger"]) {
		if strField(e, "id") == "" {
			continue
		}
		status := strings.ToLower(strField(e, "status"))
		for i := range sections {
			if sections[i].status == status {
				sections[i].entries = append(sections[i].entries, e)
				total++
			}
		}
	}
	if total == 0 {
		return ""
	}

	var b strings.Builder
	fmt.Fprintf(&b, "A fixer already answered an earlier review of this PR (run %s), finding by finding.\n", runID)
	for _, sec := range sections {
		if len(sec.entries) == 0 {
			continue
		}
		b.WriteString(sec.header)
		for _, e := range sec.entries {
			b.WriteString("- " + strField(e, "id"))
			if c := strField(e, "commit"); c != "" {
				b.WriteString(" (" + shortSHA(c) + ")")
			}
			if n := strField(e, "note"); n != "" {
				b.WriteString(": " + truncate(n, maxLedgerNote))
			}
			b.WriteString("\n")
		}
	}
	return b.String()
}

// logWarn is nil-safe: several of these paths run with no logger in tests.
func (s *Server) logWarn(format string, args ...any) {
	if s.logger != nil {
		s.logger.Warn(format, args...)
	}
}

// eventsBus resolves the event spine this server publishes on: the configured
// bus, else the trigger coordinator's. Four call sites had their own copy of
// this precedence, which is three too many for a rule that will change.
func (s *Server) eventsBus() eventbus.Bus {
	if s == nil {
		return nil
	}
	if s.cfg.EventsBus != nil {
		return s.cfg.EventsBus
	}
	if s.triggerCoord != nil {
		return s.triggerCoord.Bus()
	}
	return nil
}
