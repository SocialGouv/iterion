//go:build live

package e2e

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/benchmark"
	"github.com/SocialGouv/iterion/pkg/benchmark/quality"
	"github.com/SocialGouv/iterion/pkg/store"
)

// ---------------------------------------------------------------------------
// assessQuality — subjective quality + value-for-money snapshot layer
// ---------------------------------------------------------------------------
//
// After a live run's reliability invariants pass, assessQuality grades the
// REAL work product with a cross-family LLM judge panel, persists a
// snapshot into the committed per-target history, and compares it against
// the previous snapshot to attest improvement/regression. It is
// report-only by default (it never fails the test on a subjective quality
// dip); set ITERION_LIVE_QUALITY_GATE=1 to turn a clear regression into a
// test failure. Set ITERION_LIVE_QUALITY=off to skip the panel entirely
// while iterating.

// qualityInput is what a per-bot/per-feature test supplies; assessQuality
// fills in the metrics + outcome from the run itself.
type qualityInput struct {
	kind          string // "bot" | "feature"
	name          string // target name, e.g. "review-pr"
	persona       string // optional, e.g. "Revi"
	primaryFamily string // family the run primarily used ("anthropic"/"openai"); default "anthropic"
	task          string // what the run was asked to do (scenario + key vars)
	workProduct   string // the REAL artifact to grade (caller gathers it)
}

// maxWorkProductChars bounds the artifact fed to the judges so a huge diff
// doesn't balloon judge cost. 20k chars (~5-6k tokens) is plenty of signal.
const maxWorkProductChars = 20000

// assessQuality runs the judge panel and records a snapshot. Best-effort by
// design: panel unavailability (no judge credential) is logged, not fatal.
func assessQuality(t *testing.T, res liveResult, qi qualityInput) {
	t.Helper()
	if strings.EqualFold(os.Getenv("ITERION_LIVE_QUALITY"), "off") {
		t.Logf("[quality] ITERION_LIVE_QUALITY=off — skipping panel for %s", qi.name)
		return
	}
	if qi.primaryFamily == "" {
		qi.primaryFamily = "anthropic"
	}

	ctx := context.Background()

	// Price side: aggregate cost/tokens/duration/iterations from the run.
	rm, err := benchmark.CollectMetrics(ctx, res.store, res.runID, qi.name, "")
	if err != nil {
		t.Logf("[quality] CollectMetrics failed (%v) — using zero metrics", err)
		rm = &benchmark.RunMetrics{}
	}
	metrics := quality.Metrics{
		CostUSD:    rm.TotalCostUSD,
		Tokens:     rm.TotalTokens,
		DurationMS: res.elapsed.Milliseconds(),
		Iterations: rm.Iterations,
		ModelCalls: rm.ModelCalls,
		Retries:    rm.Retries,
	}

	ev := quality.Evidence{
		Kind:          qi.kind,
		Name:          qi.name,
		Persona:       qi.persona,
		PrimaryFamily: qi.primaryFamily,
		Task:          qi.task,
		WorkProduct:   truncate(qi.workProduct, maxWorkProductChars),
		Outcome:       runOutcome(res),
		Metrics:       metrics,
	}

	st := quality.NewSnapshotStore(qualitySnapshotRoot())
	prev, hasPrev, err := st.Last(qi.name)
	if err != nil {
		t.Logf("[quality] loading previous snapshot failed: %v", err)
	}
	if !hasPrev {
		prev = nil
	}

	reg := model.NewRegistry()
	models := quality.DefaultJudgeModels()
	agg, err := quality.RunPanel(ctx, reg, models, ev, prev)
	if err != nil {
		t.Logf("[quality] panel unavailable (%v) — no snapshot written", err)
		return
	}

	snap := &quality.Snapshot{
		Kind:           qi.kind,
		Name:           qi.name,
		Persona:        qi.persona,
		RunID:          res.runID,
		At:             time.Now().UTC(),
		IterionSHA:     iterionSHA(),
		Task:           qi.task,
		Metrics:        metrics,
		Aggregate:      agg,
		EvidenceDigest: truncate(ev.WorkProduct, 1200),
	}
	delta := quality.Compare(prev, snap)
	snap.Comparison = delta
	if prev != nil {
		snap.PrevRunID = prev.RunID
	}

	path, werr := st.Write(snap)
	if werr != nil {
		t.Logf("[quality] writing snapshot failed: %v", werr)
	}

	logQuality(t, qi.name, snap, delta, path, models)
	appendQualityToReport(res, snap, delta)
	maybeQualityGate(t, snap, delta)
}

// assessQualityRaw is an adapter for tests that hold the raw run vars
// (runID, store, events…) rather than a liveResult — notably the three
// pre-existing bot tests relocated into live_bot_*_test.go. It rebuilds a
// liveResult (loading the run) and delegates to assessQuality.
func assessQualityRaw(t *testing.T, name, persona, task, runID, workspaceDir, storeDir string, s store.RunStore, events []*store.Event, elapsed time.Duration, reason, workProduct string) {
	t.Helper()
	run, _ := s.LoadRun(context.Background(), runID)
	res := liveResult{
		runID:        runID,
		workspaceDir: workspaceDir,
		storeDir:     storeDir,
		store:        s,
		events:       events,
		run:          run,
		elapsed:      elapsed,
		reason:       reason,
	}
	assessQuality(t, res, qualityInput{kind: "bot", name: name, persona: persona, task: task, workProduct: workProduct})
}

// runOutcome summarises the reliability side for the judges.
func runOutcome(res liveResult) string {
	var b strings.Builder
	status := "unknown"
	if res.run != nil {
		status = string(res.run.Status)
	}
	fmt.Fprintf(&b, "status: %s\nacceptable_reason: %s\nelapsed: %s\n", status, res.reason, res.elapsed.Round(time.Second))
	finished := eventNodeIDs(res.events, store.EventNodeFinished)
	if len(finished) > 0 {
		// De-dup preserving order for a compact node-progress summary.
		seen := map[string]bool{}
		var uniq []string
		for _, id := range finished {
			if !seen[id] {
				seen[id] = true
				uniq = append(uniq, id)
			}
		}
		fmt.Fprintf(&b, "nodes_finished: %s\n", strings.Join(uniq, ", "))
	}
	return b.String()
}

// logQuality prints the assessment + comparison to the test log.
func logQuality(t *testing.T, name string, snap *quality.Snapshot, delta *quality.Delta, path string, models []string) {
	t.Helper()
	var b strings.Builder
	fmt.Fprintf(&b, "\n========== QUALITY ASSESSMENT: %s ==========\n", name)
	fmt.Fprintf(&b, "judges: %s\n", strings.Join(models, ", "))
	fmt.Fprintf(&b, "cost=$%.4f tokens=%d duration=%s iterations=%d\n",
		snap.Metrics.CostUSD, snap.Metrics.Tokens, time.Duration(snap.Metrics.DurationMS)*time.Millisecond, snap.Metrics.Iterations)
	fmt.Fprintf(&b, "mean scores:\n")
	for _, d := range quality.Dimensions {
		if s, ok := snap.Aggregate.MeanScores[d]; ok {
			fmt.Fprintf(&b, "  %-16s %.2f", d, s)
			if sp, ok := snap.Aggregate.Disagreement[d]; ok && sp > 0 {
				fmt.Fprintf(&b, "  (panel spread %.2f)", sp)
			}
			b.WriteByte('\n')
		}
	}
	if delta != nil {
		fmt.Fprintf(&b, "vs previous (%s): overall %+.3f, value %+.3f → %s\n", delta.PrevRunID, delta.OverallDelta, delta.ValueDelta, strings.ToUpper(string(delta.Verdict)))
	} else {
		fmt.Fprintf(&b, "vs previous: (first snapshot — no baseline)\n")
	}
	for _, v := range snap.Aggregate.Verdicts {
		flag := ""
		if v.SameFamilyAsBot {
			flag = " [same-family-as-bot]"
		}
		fmt.Fprintf(&b, "- judge %s%s (conf %.2f): %s\n", v.Model, flag, v.Confidence, oneLine(v.Narrative))
		if v.RelativeNarrative != "" {
			fmt.Fprintf(&b, "    vs prev: %s\n", oneLine(v.RelativeNarrative))
		}
	}
	if snap.Aggregate.Note != "" {
		fmt.Fprintf(&b, "note: %s\n", snap.Aggregate.Note)
	}
	if path != "" {
		fmt.Fprintf(&b, "snapshot: %s\n", path)
	}
	b.WriteString("====================================================\n")
	t.Log(b.String())
}

// appendQualityToReport appends a quality section to the run's report.md
// (best-effort; report.md is written by writeLiveTestReport just before).
func appendQualityToReport(res liveResult, snap *quality.Snapshot, delta *quality.Delta) {
	reportPath := filepath.Join(res.storeDir, "runs", res.runID, "report.md")
	f, err := os.OpenFile(reportPath, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	var b strings.Builder
	b.WriteString("\n## Quality assessment (subjective, cross-family panel)\n\n")
	fmt.Fprintf(&b, "- cost: $%.4f · tokens: %d · iterations: %d\n", snap.Metrics.CostUSD, snap.Metrics.Tokens, snap.Metrics.Iterations)
	for _, d := range quality.Dimensions {
		if s, ok := snap.Aggregate.MeanScores[d]; ok {
			fmt.Fprintf(&b, "- %s: %.2f\n", d, s)
		}
	}
	if delta != nil {
		fmt.Fprintf(&b, "- vs previous (%s): overall %+.3f → **%s**\n", delta.PrevRunID, delta.OverallDelta, strings.ToUpper(string(delta.Verdict)))
	}
	_, _ = f.WriteString(b.String())
}

// maybeQualityGate fails the test only when the opt-in gate is enabled AND
// a clear regression is detected.
func maybeQualityGate(t *testing.T, snap *quality.Snapshot, delta *quality.Delta) {
	t.Helper()
	if !truthyEnv("ITERION_LIVE_QUALITY_GATE") {
		return
	}
	regressed, reasons := delta.IsRegression(0)
	if regressed {
		t.Errorf("[quality] ITERION_LIVE_QUALITY_GATE: regression detected: %s", strings.Join(reasons, "; "))
	}
}

// ---------------------------------------------------------------------------
// Evidence gatherers
// ---------------------------------------------------------------------------

// gitArtifactEvidence returns the bot's cumulative work as a bounded text
// bundle: the commit log plus the full diff from the seed (root) commit to
// HEAD. For code-mutating bots this IS the work product to grade.
func gitArtifactEvidence(t *testing.T, workspaceDir string) string {
	t.Helper()
	root := strings.TrimSpace(gitOut(workspaceDir, "rev-list", "--max-parents=0", "HEAD"))
	var b strings.Builder
	b.WriteString("## git log (oneline)\n")
	b.WriteString(gitOut(workspaceDir, "log", "--oneline", "--no-decorate"))
	b.WriteString("\n## diff (seed..HEAD)\n")
	if root != "" {
		b.WriteString(gitOut(workspaceDir, "--no-pager", "diff", root+"..HEAD"))
	} else {
		b.WriteString(gitOut(workspaceDir, "--no-pager", "diff", "HEAD"))
	}
	// Untracked files the bot created but never `git add`ed.
	if unt := strings.TrimSpace(gitOut(workspaceDir, "ls-files", "--others", "--exclude-standard")); unt != "" {
		b.WriteString("\n## untracked files\n")
		b.WriteString(unt)
	}
	return b.String()
}

// gitOut runs a git command in dir and returns combined output (best-effort).
func gitOut(dir string, args ...string) string {
	full := append([]string{"-C", dir}, args...)
	out, _ := exec.Command("git", full...).CombinedOutput()
	return string(out)
}

// iterionSHA returns the short HEAD sha of the iterion repo (for snapshot
// provenance). Repo root is the e2e parent directory.
func iterionSHA() string {
	out, err := exec.Command("git", "-C", "..", "rev-parse", "--short", "HEAD").CombinedOutput()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// qualitySnapshotRoot resolves the committed snapshot history root. Default
// e2e/testdata/live/quality (cwd is the e2e package dir under `go test`);
// override with ITERION_LIVE_QUALITY_DIR.
func qualitySnapshotRoot() string {
	if v := strings.TrimSpace(os.Getenv("ITERION_LIVE_QUALITY_DIR")); v != "" {
		return v
	}
	return filepath.Join("testdata", "live", "quality")
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "\n…[truncated]…"
}

func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	return strings.TrimSpace(s)
}

// sprintAny renders any decoded-JSON value for inclusion in a work-product
// blob handed to the quality judges.
func sprintAny(v interface{}) string { return fmt.Sprintf("%v", v) }

func truthyEnv(name string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}
