package runview

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/testfs"
	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/workflowfile"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/store"
)

const provenanceSource = "schema out:\n  ok: bool\n\ntool work:\n  command: `true`\n  output: out\n\nworkflow provenance:\n  entry: work\n  work -> done\n"

type provenanceExecutor struct {
	execute func(ir.Node) (map[string]any, error)
}

func (e provenanceExecutor) Execute(_ context.Context, node ir.Node, _ map[string]any) (map[string]any, error) {
	return e.execute(node)
}

// #2150 / F18. Mutations: remove recordAttempt at run_resumed, read disk in
// buildWireWorkflow, or reuse the first attempt's budget. Each loses a
// separately asserted fact below. The executor inspects the ACTIVE graph.
func TestAttemptProvenanceSurvivesResumeAndSourceDeletion(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	path := filepath.Join(root, "main.bot")
	write := func(src string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(provenanceSource)
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	wf, hash, err := CompileWorkflowWithHash(path, "")
	if err != nil {
		t.Fatal(err)
	}
	wf.Budget = &ir.Budget{MaxTokens: 100}
	fail := provenanceExecutor{execute: func(ir.Node) (map[string]any, error) { return nil, errors.New("transient") }}
	opts := []runtime.EngineOption{runtime.WithWorkDir(t.TempDir()), runtime.WithSandboxOverride("none"), runtime.WithFilePath(path), runtime.WithWorkflowHash(hash)}
	if err := runtime.New(wf, st, fail, opts...).Run(ctx, "provenance", nil); err == nil {
		t.Fatal("first attempt must fail")
	}
	first, err := st.LoadRun(ctx, "provenance")
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Attempts) != 1 {
		t.Fatalf("launch attempts: %+v", first.Attempts)
	}
	before, err := st.LoadAttempt(ctx, first.ID, first.Attempts[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	write(strings.Replace(provenanceSource, "workflow provenance:", "tool added:\n  command: `true`\n  output: out\n\nworkflow provenance:", 1))
	edited, _ := os.ReadFile(path)
	write(strings.Replace(string(edited), "work -> done", "work -> added\n  added -> done", 1))
	wf, hash, err = CompileWorkflowWithHash(path, "")
	if err != nil {
		t.Fatal(err)
	}
	wf.Budget = &ir.Budget{MaxTokens: 200}
	seen := false
	succeed := provenanceExecutor{execute: func(node ir.Node) (map[string]any, error) {
		graph, err := BuildWireWorkflowFromStore(ctx, st, first.ID)
		if err != nil {
			t.Fatal(err)
		}
		if graph.Attempt == nil || graph.Attempt.Budget.MaxTokens != 200 {
			t.Fatalf("active budget: %+v", graph.Attempt)
		}
		for _, n := range graph.Nodes {
			if n.ID == "added" {
				seen = true
			}
		}
		if !seen {
			t.Fatalf("active node absent: %+v", graph.Nodes)
		}
		return map[string]any{"ok": true}, nil
	}}
	opts = append(opts, runtime.WithWorkflowHash(hash), runtime.WithForceResume(true))
	if err := runtime.New(wf, st, succeed, opts...).Resume(ctx, first.ID, nil); err != nil {
		t.Fatal(err)
	}
	if !seen {
		t.Fatal("resume did not execute")
	}
	testfs.RemoveAll(t, root, path)
	after, err := st.LoadRun(ctx, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(after.Attempts) != 2 || after.Budget.MaxTokens != 200 {
		t.Fatalf("resumed run: attempts=%v budget=%+v", after.Attempts, after.Budget)
	}
	latest, err := BuildWireWorkflowFromStore(ctx, st, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if latest.Attempt.ID == first.Attempts[0].ID || latest.StaleHash {
		t.Fatalf("current attempt: %+v", latest)
	}
	old, err := BuildWireWorkflowAttemptFromStore(ctx, st, first.ID, first.Attempts[0].ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range old.Nodes {
		if n.ID == "added" {
			t.Fatal("history changed with resume")
		}
	}
	if old.Attempt.Budget.MaxTokens != 100 {
		t.Fatal("historical budget changed")
	}
	unchanged, err := st.LoadAttempt(ctx, first.ID, first.Attempts[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if unchanged.Source.Files[unchanged.Source.Main] != before.Source.Files[before.Source.Main] {
		t.Fatal("snapshot was overwritten")
	}
}

// F18: removing the captured fragments/prompts/includes makes this fail after
// the entire source directory disappears; reading a current bundle cannot pass.
func TestAttemptCapturesUnitAndBundleResources(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{
		"main.bot":           "import \"lib/gate.bot\"\n\nworkflow w:\n  entry: gate\n  gate -> done\n",
		"lib/gate.bot":       "schema out:\n  ok: bool\n\nhuman gate:\n  instructions: rules\n  output: out\n  interaction: human\n",
		"manifest.yaml":      "name: provenance\nversion: 1.0.0\n",
		"prompts/rules.md":   "{{include \"details.md\"}}",
		"prompts/details.md": "Approve the immutable plan.",
		"presets/default.md": "Frozen preset.",
	}
	for rel, body := range files {
		path := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	wf, hash, _, err := CompileWorkflowPath(filepath.Join(root, "main.bot"), "")
	if err != nil {
		t.Fatal(err)
	}
	testfs.RemoveAll(t, root, root)
	back, gotHash, err := compileSourceSnapshot(wf.SourceSnapshot)
	if err != nil {
		t.Fatal(err)
	}
	if gotHash != hash || back.Prompts["rules"].Body != "Approve the immutable plan." {
		t.Fatalf("recompiled resources differ: hash=%s want=%s prompt=%+v", gotHash, hash, back.Prompts["rules"])
	}
}

// #2696: bypassing cache.get returns distinct projections; ignoring content
// changes returns the stale node for a new source under an old legacy hash.
func TestWorkflowHashCacheSeparatesSourceRevisions(t *testing.T) {
	cache := &wireWorkflowCache{}
	source := &workflowfile.Snapshot{Main: "main.bot", Files: map[string]string{"main.bot": provenanceSource}}
	a, err := cachedSourceWorkflow(source, "team", "hash", cache)
	if err != nil {
		t.Fatal(err)
	}
	b, err := cachedSourceWorkflow(source, "team", "hash", cache)
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Fatal("identical workflow hash recompiles")
	}
	source.Files["main.bot"] = strings.NewReplacer("tool work:", "tool changed:", "entry: work", "entry: changed", "work ->", "changed ->").Replace(provenanceSource)
	c, err := cachedSourceWorkflow(source, "team", "hash", cache)
	if err != nil {
		t.Fatal(err)
	}
	if c.Entry != "changed" || c == a {
		t.Fatal("edited legacy source aliased the old graph")
	}
}

// F18: deleting an authoritative blob must never activate the legacy resolver.
func TestMissingAttemptSnapshotDoesNotFallBack(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	st, err := store.New(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateRun(ctx, "missing", "w", nil); err != nil {
		t.Fatal(err)
	}
	a, err := st.RecordAttempt(ctx, "missing", &store.AttemptSnapshot{Source: &workflowfile.Snapshot{Main: "main.bot", Files: map[string]string{"main.bot": provenanceSource}}})
	if err != nil {
		t.Fatal(err)
	}
	testfs.RemoveAll(t, root, filepath.Join(root, "runs", "missing", "attempts", a.ID+".json"))
	_, err = BuildWireWorkflowAttemptFromStore(ctx, st, "missing", "", func(context.Context, *store.Run) (*workflowfile.Snapshot, error) {
		t.Fatal("called legacy resolver")
		return nil, fmt.Errorf("unexpected")
	})
	var missing *WorkflowUnavailable
	if !errors.As(err, &missing) || missing.Code != "attempt_snapshot_missing" {
		t.Fatalf("missing snapshot: %v", err)
	}
}

// Mutation: run MCP preparation during projection. The graph then depends on
// a config file outside the captured compilation resources and fails here.
func TestAttemptProjectionDoesNotRebindHostMCP(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "main.bot")
	source := strings.Replace(provenanceSource, "workflow provenance:", "workflow provenance:\n  mcp:\n    servers: [testsrv]", 1)
	if err := os.WriteFile(path, []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".mcp.json"), []byte(`{"mcpServers":{"testsrv":{"command":"true"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	wf, _, err := CompileWorkflowWithHash(path, "")
	if err != nil {
		t.Fatal(err)
	}
	testfs.RemoveAll(t, root, root)
	back, _, err := compileSourceSnapshot(wf.SourceSnapshot)
	if err != nil {
		t.Fatal(err)
	}
	if back.Entry != "work" {
		t.Fatalf("captured graph: %+v", back)
	}
}

// #2150 legacy migration: prefer captured run text even when the live file is
// both valid and different. Mutation: start buildLegacyWireWorkflow at disk.
func TestLegacyRecordedSourceOutranksDisk(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "main.bot")
	edited := strings.Replace(provenanceSource, "entry: work", "entry: done", 1)
	if err := os.WriteFile(path, []byte(edited), 0o600); err != nil {
		t.Fatal(err)
	}
	graph, err := buildWireWorkflowFromRun(&store.Run{FilePath: path, WorkflowSource: provenanceSource}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if graph.Entry != "work" || graph.Provenance != "recorded_source" {
		t.Fatalf("recorded source lost: %+v", graph)
	}
}

// F18: promoting recordedSources() into a new snapshot lies about a caller's
// already-compiled IR. Mutation: restore that runtime compatibility fallback.
func TestAttemptWithoutCompilerCaptureRefusesMutableDisk(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	path := filepath.Join(root, "main.bot")
	if err := os.WriteFile(path, []byte(provenanceSource), 0o600); err != nil {
		t.Fatal(err)
	}
	wf, hash, err := CompileWorkflowWithHash(path, "")
	if err != nil {
		t.Fatal(err)
	}
	wf.SourceSnapshot = nil // Public callers can provide a programmatically built IR.
	if err := os.WriteFile(path, []byte(strings.Replace(provenanceSource, "entry: work", "entry: done", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	executed := false
	executor := provenanceExecutor{execute: func(node ir.Node) (map[string]any, error) { executed = true; return map[string]any{"ok": true}, nil }}
	if err := runtime.New(wf, st, executor, runtime.WithWorkDir(t.TempDir()), runtime.WithSandboxOverride("none"), runtime.WithFilePath(path), runtime.WithWorkflowHash(hash)).Run(ctx, "raw-ir", nil); err != nil {
		t.Fatal(err)
	}
	if !executed {
		t.Fatal("old compiled IR did not execute")
	}
	_, err = BuildWireWorkflowFromStore(ctx, st, "raw-ir")
	var missing *WorkflowUnavailable
	if !errors.As(err, &missing) || missing.Code != "attempt_snapshot_missing" {
		t.Fatalf("mutable source became authoritative: %v", err)
	}
}

// F18: keep export identity independent of the projection's temporary root.
// Mutation: name bundle includes relative to unit.Root instead of bundle.Dir.
func TestAttemptBundleExportIdentitySurvivesMaterialization(t *testing.T) {
	root := t.TempDir()
	for rel, body := range map[string]string{
		"workflows/check.bot": "schema out:\n  ok: bool\n\nhuman gate:\n  instructions: rules\n  output: out\n  interaction: human\n\nworkflow w:\n  entry: gate\n  gate -> done\n",
		"prompts/rules.md":    "{{include \"details.md\"}}", "prompts/details.md": "Frozen body.",
	} {
		path := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(root, "workflows/check.bot")
	wf, hash, err := CompileBundleWorkflow(path, &bundle.Bundle{Dir: root, IterPath: path, PromptsDir: filepath.Join(root, "prompts")}, "")
	if err != nil {
		t.Fatal(err)
	}
	testfs.RemoveAll(t, root, root)
	for range 2 {
		_, actual, err := compileSourceSnapshot(wf.SourceSnapshot)
		if err != nil {
			t.Fatal(err)
		}
		if actual != hash {
			t.Fatalf("identity changes across materialization: %s != %s", actual, hash)
		}
	}
}

// Mutation: omit the subbot_attached append before consumption. Clearing the
// operational pointer then erases the reused child from this attempt's view.
func TestAttemptKeepsConsumedSubbotAttachment(t *testing.T) {
	ctx := context.Background()
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"parent", "child"} {
		if _, err := st.CreateRun(ctx, id, "w", nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.UpdateRunStatus(ctx, "child", store.RunStatusFinished, ""); err != nil {
		t.Fatal(err)
	}
	if err := st.SetSubbotChild(ctx, "parent", "sub", "child"); err != nil {
		t.Fatal(err)
	}
	_, err, handled := ReattachSubbotChild(ctx, st, runtime.SubbotRequest{ParentRunID: "parent", NodeID: "sub", ReattachKey: "sub"}, nil)
	if err != nil || !handled {
		t.Fatalf("reattach: %v handled=%v", err, handled)
	}
	parent, err := st.LoadRun(ctx, "parent")
	if err != nil {
		t.Fatal(err)
	}
	if len(parent.SubbotChildren) != 0 {
		t.Fatal("operational pointer is not consumed")
	}
	events, err := st.LoadEvents(ctx, "parent")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range events {
		if e.Type == store.EventSubbotAttached && e.Data["child_run_id"] == "child" {
			found = true
		}
	}
	if !found {
		t.Fatal("consumed child's attempt relationship is lost")
	}
}

// Mutation: append the attachment directly to the store, bypassing the
// engine's observer. Durable reload then works but the open console loses it.
func TestAttemptAttachmentUsesEngineObserver(t *testing.T) {
	ctx := context.Background()
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateRun(ctx, "child", "w", nil); err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateRunStatus(ctx, "child", store.RunStatusFinished, ""); err != nil {
		t.Fatal(err)
	}
	wf := &ir.Workflow{Name: "parent", Entry: "sub", Nodes: map[string]ir.Node{"sub": &ir.SubbotNode{BaseNode: ir.BaseNode{ID: "sub"}, Source: "child.bot"}, "done": &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}}}, Edges: []*ir.Edge{{From: "sub", To: "done"}}}
	seen := false
	engine := runtime.New(wf, st, provenanceExecutor{}, runtime.WithWorkDir(t.TempDir()), runtime.WithSandboxOverride("none"), runtime.WithEventObserver(func(e store.Event) {
		if e.Type == store.EventSubbotAttached {
			seen = true
		}
	}), runtime.WithSubbotRunner(func(ctx context.Context, req runtime.SubbotRequest) (map[string]any, error) {
		if err := st.SetSubbotChild(ctx, req.ParentRunID, req.ReattachKey, "child"); err != nil {
			return nil, err
		}
		out, err, _ := ReattachSubbotChild(ctx, st, req, nil)
		return out, err
	}))
	if err := engine.Run(ctx, "parent", nil); err != nil {
		t.Fatal(err)
	}
	if !seen {
		t.Fatal("live engine observer never sees the child attachment")
	}
}
