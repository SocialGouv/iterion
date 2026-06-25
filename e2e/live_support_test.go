//go:build live

package e2e

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
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
	vars         map[string]interface{}
	inputs       map[string]interface{}
	timeout      time.Duration
	// withWorkDir passes runtime.WithWorkDir(workspaceDir) — required for
	// sandbox-backed and worktree:auto bots so the engine mounts/operates
	// on the seeded fixture instead of the iterion repo root.
	withWorkDir bool
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

	wf := compileLiveWorkflow(t, spec)

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
	eng := runtime.New(wf, s, executor, engOpts...)

	ctx, cancel := context.WithTimeout(context.Background(), spec.timeout)
	defer cancel()

	t.Logf("[live] starting %s (run %s, timeout %s)…", spec.runIDBase, runID, spec.timeout)
	start := time.Now()
	runErr := eng.Run(ctx, runID, spec.inputs)
	elapsed := time.Since(start)
	executor.Close()
	t.Logf("[live] %s finished in %s", spec.runIDBase, elapsed.Round(time.Second))

	acceptable, reason := liveRunResultAcceptable(runErr)
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

// compileLiveWorkflow compiles either a plain .bot fixture or a bundle dir.
func compileLiveWorkflow(t *testing.T, spec liveSpec) *ir.Workflow {
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
		return wf
	}
	if spec.botFile == "" {
		t.Fatalf("runBotLive: one of botFile or bundleDir must be set")
	}
	return compileFixture(t, spec.botFile)
}

// ---------------------------------------------------------------------------
// Invariant assertion helpers (reliability layer)
// ---------------------------------------------------------------------------

// lastNodeOutput returns the output map of the LAST EventNodeFinished for
// nodeID (loops fire it once per iteration; the last is the settled one).
func lastNodeOutput(events []*store.Event, nodeID string) (map[string]interface{}, bool) {
	var out map[string]interface{}
	var found bool
	for _, e := range events {
		if e.Type == store.EventNodeFinished && e.NodeID == nodeID {
			if m, ok := e.Data["output"].(map[string]interface{}); ok {
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
