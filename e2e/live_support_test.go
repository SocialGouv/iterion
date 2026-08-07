//go:build live

package e2e

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/mcp"
	"github.com/SocialGouv/iterion/pkg/botreplay"
	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
)

// ---------------------------------------------------------------------------
// runBotLive — shared live-run orchestrator
// ---------------------------------------------------------------------------
//
// The per-bot / per-feature live tests are deliberately explicit: each
// seeds its own fixture and writes its own invariants. runBotLive removes
// only the boilerplate every one of them shares — compile, prepare MCP,
// build the real executor, run under a timeout with a tee'd run.log,
// assert the run ended acceptably, load the run + events, and write the
// diagnostic report. It returns everything a caller needs to assert on top
// (and to feed assessQuality).

// liveSpec configures one live run. The caller must have already created +
// seeded workspaceDir. Provide exactly one of botFile (a compileFixture
// path like "review-pr/main.bot") or bundleDir (a bundle directory like
// "../bots/secured-renovacy", compiled via bundle.OpenDir so skills +
// recipes merge as `iterion run` would).
type liveSpec struct {
	runIDBase    string
	botFile      string
	bundleDir    string
	workspaceDir string
	vars         map[string]any
	inputs       map[string]any
	timeout      time.Duration
	// withWorkDir passes runtime.WithWorkDir(workspaceDir) — required for
	// sandbox-backed and worktree:auto bots so the engine mounts/operates
	// on the seeded fixture instead of the iterion repo root.
	withWorkDir bool
	// autoResume drives interactive bots headlessly: when a `human` node
	// pauses the run, the harness synthesizes an answer from that node's
	// output schema (bool→true, enum→a termination-favoring value, …) and
	// resumes, up to maxResumes times. Lets a test reach an emit/terminal
	// node past human gates without a real operator.
	autoResume bool
	maxResumes int // default 12 when autoResume is set
	// acceptPause treats a final ErrRunPaused as an acceptable terminal
	// state (instead of fatal) — for feature tests that EXPECT a pause,
	// e.g. the permission ask-mode gate. The caller then asserts on
	// res.run.Status == paused_waiting_human.
	acceptPause bool
}

// liveResult is the loaded outcome of a runBotLive call.
type liveResult struct {
	runID        string
	workspaceDir string
	storeDir     string
	store        store.RunStore
	wf           *ir.Workflow
	run          *store.Run
	events       []*store.Event
	runErr       error
	elapsed      time.Duration
	reason       string
}

// runBotLive executes spec end-to-end and returns the loaded result. It
// t.Fatal's on setup failures and on an unacceptable run error (anything
// outside the budget-exceeded / loop-exhausted / context-cancel graceful
// set), so callers can assume a usable result on return.
func runBotLive(t *testing.T, spec liveSpec) liveResult {
	t.Helper()
	if spec.workspaceDir == "" {
		t.Fatalf("runBotLive: workspaceDir must be set (seed it before calling)")
	}
	if spec.timeout <= 0 {
		spec.timeout = 30 * time.Minute
	}

	wf, bnd := compileLiveWorkflow(t, spec)

	storeDir := resolveLiveStoreDir(t, spec.workspaceDir)
	s, err := store.New(storeDir)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	runID := uniqueRunID(spec.runIDBase)

	if err := mcp.PrepareWorkflow(wf, spec.workspaceDir); err != nil {
		t.Fatalf("mcp.PrepareWorkflow: %v", err)
	}

	logger := prepareLiveRunLog(t, storeDir, runID)
	executor := newLiveExecutor(t, wf, s, runID, spec.workspaceDir, withLiveLogger(logger))
	if len(spec.vars) > 0 {
		executor.SetVars(spec.vars)
	}

	engOpts := []runtime.EngineOption{runtime.WithLogger(logger)}
	if spec.withWorkDir {
		engOpts = append(engOpts, runtime.WithWorkDir(spec.workspaceDir))
	}
	if bnd != nil {
		// Mirror the bundle's skills into <workspace>/.claude/skills so the
		// bot's agents read them exactly as `iterion run <bundle>` would.
		engOpts = append(engOpts, runtime.WithBundle(bnd))
	}
	eng := runtime.New(wf, s, executor, engOpts...)

	ctx, cancel := context.WithTimeout(context.Background(), spec.timeout)
	defer cancel()

	t.Logf("[live] starting %s (run %s, timeout %s)…", spec.runIDBase, runID, spec.timeout)
	start := time.Now()
	runErr := eng.Run(ctx, runID, spec.inputs)
	if spec.autoResume {
		runErr = driveAutoResume(t, ctx, eng, s, wf, runID, runErr, spec.maxResumes)
	}
	elapsed := time.Since(start)
	executor.Close()
	t.Logf("[live] %s finished in %s", spec.runIDBase, elapsed.Round(time.Second))

	// A dead credential is a MISSING PREREQUISITE, not a failing feature:
	// skip, exactly as the requireX guards do when a credential is absent.
	// Those guards check that a credential is PRESENT (a file, an env var)
	// — they cannot tell it expired, so the run reaches the provider and
	// fails on its 401.
	//
	// This runs BEFORE the acceptable-error boundary on purpose: that
	// boundary maps ExecutionFailed to "likely context deadline" and
	// ACCEPTS it, so an auth failure would sail past it and redden a
	// downstream assertion instead — which is how six tests reported as
	// feature failures when the only thing wrong was an expired token.
	// Skipping (never accepting) keeps it honest: the test does not claim
	// to have verified anything.
	if why := deadCredentialReason(runErr); why != "" {
		t.Skipf("live prerequisite unavailable — %s (re-authenticate, then re-run)", why)
	}

	acceptable, reason := liveRunResultAcceptable(runErr)
	if spec.acceptPause && errors.Is(runErr, runtime.ErrRunPaused) {
		acceptable, reason = true, "run paused (expected by acceptPause)"
	}
	if !acceptable {
		t.Fatalf("unacceptable run error: %v", runErr)
	}
	t.Logf("[live] result: %s", reason)

	run, err := s.LoadRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	events, err := s.LoadEvents(context.Background(), runID)
	if err != nil {
		t.Fatalf("LoadEvents: %v", err)
	}

	writeLiveTestReport(t, runID, spec.workspaceDir, storeDir, s, events)

	return liveResult{
		runID:        runID,
		workspaceDir: spec.workspaceDir,
		storeDir:     storeDir,
		store:        s,
		wf:           wf,
		run:          run,
		events:       events,
		runErr:       runErr,
		elapsed:      elapsed,
		reason:       reason,
	}
}

// deadCredentialReason reports why a run failed on an unusable credential,
// or "" when the failure is something else.
//
// The patterns are deliberately narrow — an expired/invalid/revoked token,
// or a provider refusing to authenticate. A plain 403, a quota refusal or a
// permission error is NOT here: those are real signals a live test should
// keep reporting as failures. A usage/quota WALL is also excluded on
// purpose: it means the credential works and the account ran out, which the
// budget-and-retry paths are meant to surface, not hide.
func deadCredentialReason(err error) string {
	if err == nil {
		return ""
	}
	msg := strings.ToLower(err.Error())
	for _, p := range []struct{ needle, why string }{
		{"authentication token is expired", "the provider's OAuth token has expired"},
		{"please try signing in again", "the provider asked for a fresh sign-in"},
		{"oauth token has expired", "the provider's OAuth token has expired"},
		{"invalid_api_key", "the API key was rejected as invalid"},
		{"incorrect api key", "the API key was rejected as invalid"},
		{"invalid bearer token", "the bearer token was rejected"},
		{"401 unauthorized", "the provider refused to authenticate the request"},
		{"error 401", "the provider refused to authenticate the request"},
	} {
		if strings.Contains(msg, p.needle) {
			return p.why
		}
	}
	return ""
}

// compileLiveWorkflow compiles either a plain .bot fixture or a bundle dir.
// For a bundle it also returns the *bundle.Bundle so the engine can mirror
// its skills (runtime.WithBundle); the fixture path returns a nil bundle.
func compileLiveWorkflow(t *testing.T, spec liveSpec) (*ir.Workflow, *bundle.Bundle) {
	t.Helper()
	if spec.bundleDir != "" {
		bDir, err := filepath.Abs(spec.bundleDir)
		if err != nil {
			t.Fatalf("abs bundle dir %q: %v", spec.bundleDir, err)
		}
		b, err := bundle.OpenDir(bDir)
		if err != nil {
			t.Fatalf("bundle.OpenDir %q: %v", bDir, err)
		}
		wf, _, err := runview.CompileBundleWorkflow(b.IterPath, b)
		if err != nil {
			t.Fatalf("CompileBundleWorkflow %q: %v", bDir, err)
		}
		return wf, b
	}
	if spec.botFile == "" {
		t.Fatalf("runBotLive: one of botFile or bundleDir must be set")
	}
	return compileFixture(t, spec.botFile), nil
}

// ---------------------------------------------------------------------------
// Invariant assertion helpers (reliability layer)
// ---------------------------------------------------------------------------

// lastNodeOutput returns the output map of the LAST EventNodeFinished for
// nodeID (loops fire it once per iteration; the last is the settled one).
func lastNodeOutput(events []*store.Event, nodeID string) (map[string]any, bool) {
	var out map[string]any
	var found bool
	for _, e := range events {
		if e.Type == store.EventNodeFinished && e.NodeID == nodeID {
			if m, ok := e.Data["output"].(map[string]any); ok {
				out, found = m, true
			}
		}
	}
	return out, found
}

// countFinished returns how many times nodeID finished — the convergence /
// streak signal for loop bots.
func countFinished(events []*store.Event, nodeID string) int {
	n := 0
	for _, id := range eventNodeIDs(events, store.EventNodeFinished) {
		if id == nodeID {
			n++
		}
	}
	return n
}

// hasEventType reports whether any event has the given type.
func hasEventType(events []*store.Event, typ store.EventType) bool {
	for _, e := range events {
		if e.Type == typ {
			return true
		}
	}
	return false
}

// eventDataMentions reports whether any event (optionally restricted to the
// given types) carries a string value, at any depth in Data, containing
// substr (case-insensitive). Used to assert e.g. a board tool was called or
// a tool was refused by the permission gate, without binding to exact keys.
func eventDataMentions(events []*store.Event, substr string, types ...store.EventType) bool {
	want := map[store.EventType]bool{}
	for _, ty := range types {
		want[ty] = true
	}
	low := strings.ToLower(substr)
	for _, e := range events {
		if len(types) > 0 && !want[e.Type] {
			continue
		}
		if valueContainsFold(e.Data, low) {
			return true
		}
	}
	return false
}

func valueContainsFold(v any, low string) bool {
	switch t := v.(type) {
	case string:
		return strings.Contains(strings.ToLower(t), low)
	case map[string]any:
		for _, c := range t {
			if valueContainsFold(c, low) {
				return true
			}
		}
	case []any:
		for _, c := range t {
			if valueContainsFold(c, low) {
				return true
			}
		}
	}
	return false
}

// assertNodesFinished fails the test if any of the named nodes never
// emitted an EventNodeFinished (i.e. the run aborted before reaching them).
func assertNodesFinished(t *testing.T, events []*store.Event, ids ...string) {
	t.Helper()
	finished := map[string]bool{}
	for _, id := range eventNodeIDs(events, store.EventNodeFinished) {
		finished[id] = true
	}
	for _, id := range ids {
		if !finished[id] {
			t.Errorf("expected node %q to finish — run may have aborted before it", id)
		}
	}
}

// assertCommitsBeyond fails if HEAD is not strictly ahead of seedCount —
// i.e. the bot produced no commit. Returns the observed count.
func assertCommitsBeyond(t *testing.T, workspaceDir string, seedCount int) int {
	t.Helper()
	n := workspaceCommitCount(t, workspaceDir)
	if n <= seedCount {
		t.Errorf("expected >%d commits (a bot commit beyond seed), got %d — work likely never committed", seedCount, n)
	}
	return n
}

// assertSchemaValid validates a node's recorded output against its declared
// IR output schema, reusing the same model.ValidateOutput the runtime uses.
func assertSchemaValid(t *testing.T, wf *ir.Workflow, events []*store.Event, nodeID string) {
	t.Helper()
	out, ok := lastNodeOutput(events, nodeID)
	if !ok {
		t.Errorf("assertSchemaValid: node %q produced no output", nodeID)
		return
	}
	f := &botreplay.Fixture{Node: nodeID, Output: out}
	if err := botreplay.VerifySchema(f, wf); err != nil {
		t.Errorf("node %q output fails its schema: %v", nodeID, err)
	}
}

// assertOutputFieldsNonEmpty asserts each named field of a node's recorded
// output is present AND non-empty (reusing botreplay's semantic-presence
// check) — e.g. created_issues / findings must not be an empty array.
func assertOutputFieldsNonEmpty(t *testing.T, events []*store.Event, nodeID string, fields ...string) {
	t.Helper()
	out, ok := lastNodeOutput(events, nodeID)
	if !ok {
		t.Errorf("assertOutputFieldsNonEmpty: node %q produced no output", nodeID)
		return
	}
	f := &botreplay.Fixture{Node: nodeID, Output: out}
	if err := botreplay.VerifyRequiredNonEmpty(f, fields); err != nil {
		t.Errorf("node %q: %v", nodeID, err)
	}
}

// assertNoHallucinatedAssignees scans a node's recorded output for
// assignee/bot fields that don't resolve to a real catalog bot — reusing
// botreplay's verifier so the rule stays identical to the golden layer.
func assertNoHallucinatedAssignees(t *testing.T, events []*store.Event, nodeID string) {
	t.Helper()
	out, ok := lastNodeOutput(events, nodeID)
	if !ok {
		t.Errorf("assertNoHallucinatedAssignees: node %q produced no output", nodeID)
		return
	}
	valid, err := botreplay.ValidBots("..")
	if err != nil {
		t.Fatalf("ValidBots: %v", err)
	}
	f := &botreplay.Fixture{Node: nodeID, Output: out}
	if err := botreplay.VerifyNoHallucinatedAssignees(f, valid); err != nil {
		t.Errorf("node %q: %v", nodeID, err)
	}
}

// ---------------------------------------------------------------------------
// Workspace seeders
// ---------------------------------------------------------------------------

// writeWorkspaceFiles writes each path→content under dir (creating parent
// directories), failing the test on error.
func writeWorkspaceFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for rel, content := range files {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("mkdir for %s: %v", rel, err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}
}

// gitCommitAll stages everything and commits with msg.
func gitCommitAll(t *testing.T, dir, msg string) {
	t.Helper()
	runCmd(t, dir, "git", "add", "-A")
	runCmd(t, dir, "git", "commit", "-m", msg)
}

// seedGoModuleFixture creates a fresh git repo with a go.mod (module
// `iterion-live-fixture`) plus files, committed as the seed. Returns the
// commit count after seeding (the baseline for assertCommitsBeyond).
func seedGoModuleFixture(t *testing.T, dir string, files map[string]string) int {
	t.Helper()
	seedGitRepo(t, dir)
	if _, ok := files["go.mod"]; !ok {
		files["go.mod"] = "module iterion-live-fixture\n\ngo 1.25\n"
	}
	writeWorkspaceFiles(t, dir, files)
	gitCommitAll(t, dir, "chore: seed live fixture")
	return workspaceCommitCount(t, dir)
}

// seedBranchDiffFixture commits baseFiles on baseRef, branches to branch,
// applies branchFiles, and commits them — producing a base..branch diff a
// review/branch bot can inspect via `git diff baseRef`.
func seedBranchDiffFixture(t *testing.T, dir, baseRef string, baseFiles, branchFiles map[string]string, branch string) {
	t.Helper()
	seedGitRepo(t, dir)
	// Normalise the base branch name (seedGitRepo's initial branch may be
	// master or main depending on git config).
	runCmd(t, dir, "git", "checkout", "-B", baseRef)
	writeWorkspaceFiles(t, dir, baseFiles)
	gitCommitAll(t, dir, "chore: base on "+baseRef)
	runCmd(t, dir, "git", "checkout", "-B", branch)
	writeWorkspaceFiles(t, dir, branchFiles)
	gitCommitAll(t, dir, "feat: change under review")
}

// ---------------------------------------------------------------------------
// Skip guards (new)
// ---------------------------------------------------------------------------

// requireDockerImage skips the test unless the named local docker image
// exists. Used by the sec bots, which need iterion-sandbox-sec on PATH.
func requireDockerImage(t *testing.T, ref string) {
	t.Helper()
	requireBinaryInPath(t, "docker")
	if err := exec.Command("docker", "image", "inspect", ref).Run(); err != nil {
		t.Skipf("docker image %q not present locally — skipping (build/tag it first)", ref)
	}
}

// requireOpus48 skips the test unless an Anthropic credential is available
// (env key or Claude Code OAuth via the claude CLI), since claude-opus-4-8
// is Anthropic-only and the ultracode orchestration half is 4.8-only.
func requireOpus48(t *testing.T) {
	t.Helper()
	if os.Getenv("ANTHROPIC_API_KEY") != "" {
		return
	}
	if _, err := exec.LookPath("claude"); err == nil {
		return
	}
	t.Skip("no Anthropic credential (ANTHROPIC_API_KEY or claude CLI) — skipping claude-opus-4-8 test")
}

// ---------------------------------------------------------------------------
// Auto-resume driver for interactive (human-gated) bots
// ---------------------------------------------------------------------------

// liveAutoAnswerText is the free-text every synthesized string answer
// carries — neutral guidance so an auto-answered gate keeps the run moving.
const liveAutoAnswerText = "Auto-answer (live e2e, no operator). Prioritise correctness and developer tooling; use your best judgment and proceed."

// driveAutoResume loops while the run is paused at a `human` node: it
// synthesizes an answer from that node's output schema and resumes, up to
// maxResumes times. Returns the final run error (nil / acceptable / the
// last resume error). Non-pause errors short-circuit immediately.
func driveAutoResume(t *testing.T, ctx context.Context, eng *runtime.Engine, s store.RunStore, wf *ir.Workflow, runID string, runErr error, maxResumes int) error {
	t.Helper()
	if maxResumes <= 0 {
		maxResumes = 12
	}
	for i := 0; errors.Is(runErr, runtime.ErrRunPaused) && i < maxResumes; i++ {
		run, err := s.LoadRun(ctx, runID)
		if err != nil || run.Checkpoint == nil {
			t.Logf("[live] auto-resume: cannot load checkpoint (%v) — stopping", err)
			break
		}
		nodeID := run.Checkpoint.NodeID
		answers := synthesizeHumanAnswers(wf, nodeID)
		t.Logf("[live] auto-resume #%d: answering human node %q with %v", i+1, nodeID, answers)
		runErr = eng.Resume(ctx, runID, answers)
	}
	if errors.Is(runErr, runtime.ErrRunPaused) {
		t.Logf("[live] auto-resume: still paused after %d resumes (treating pause as terminal)", maxResumes)
	}
	return runErr
}

// synthesizeHumanAnswers builds an answer map for a paused human node from
// its declared output schema. Falls back to a permissive catch-all when the
// node has no schema.
func synthesizeHumanAnswers(wf *ir.Workflow, nodeID string) map[string]any {
	node, ok := wf.Nodes[nodeID]
	if !ok {
		return map[string]any{"approved": true, "action": "close", "response": liveAutoAnswerText}
	}
	schemaName := ir.NodeOutputSchema(node)
	sch, ok := wf.Schemas[schemaName]
	if schemaName == "" || !ok || len(sch.Fields) == 0 {
		return map[string]any{"approved": true, "action": "close", "response": liveAutoAnswerText}
	}
	ans := make(map[string]any, len(sch.Fields))
	for _, f := range sch.Fields {
		ans[f.Name] = defaultForField(f)
	}
	return ans
}

// defaultForField picks a sensible auto-answer value for one schema field.
// Booleans approve, enums prefer a termination/approval value, strings get
// the neutral guidance text, lists are empty (select nothing).
func defaultForField(f *ir.SchemaField) any {
	switch f.Type {
	case ir.FieldTypeBool:
		return true
	case ir.FieldTypeInt:
		return 1
	case ir.FieldTypeFloat:
		return 1.0
	case ir.FieldTypeStringArray:
		return []string{}
	case ir.FieldTypeJSON:
		return map[string]any{}
	default: // FieldTypeString
		if len(f.EnumValues) > 0 {
			return pickTerminatingEnum(f.EnumValues)
		}
		return liveAutoAnswerText
	}
}

// pickTerminatingEnum prefers an enum value that advances a gate toward a
// terminal/approved state (so e.g. a "what next?" loop chooses "close"),
// falling back to the first declared value.
func pickTerminatingEnum(vals []string) string {
	pref := []string{"close", "done", "approve", "approved", "accept", "yes", "continue", "proceed", "finish", "standby"}
	for _, p := range pref {
		for _, v := range vals {
			if strings.EqualFold(strings.TrimSpace(v), p) {
				return v
			}
		}
	}
	return vals[0]
}
