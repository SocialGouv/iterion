package runview

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/internal/proc"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

// mustCompileFold compiles the fixture source the way the entries do, so the
// parity assertions can run the derivation both folded lists feed.
func mustCompileFold(t *testing.T, source string) *ir.Workflow {
	t.Helper()
	wf, _, _, err := compileForLaunch("fold.bot", source, "")
	if err != nil {
		t.Fatalf("compile fixture: %v", err)
	}
	return wf
}

// stubPolicyFoldPublisher records the spec the launch/preview entries hand
// the publisher, so a test can assert what the folded list the launch
// carries onward actually contains — and that the preview and the launch
// carry the SAME list (ADR-129 §6, the parity doctrine).
type stubPolicyFoldPublisher struct {
	lastSpec    *LaunchSpec
	lastPreview *CredentialPreviewSpec
	lastResume  *ResumeSpec
}

func (p *stubPolicyFoldPublisher) SubmitLaunch(_ context.Context, _ string, spec LaunchSpec, _ *ir.Workflow, _ *CompiledSource) (int, error) {
	cp := spec
	p.lastSpec = &cp
	return 1, nil
}
func (p *stubPolicyFoldPublisher) CancelRun(context.Context, string) error { return nil }
func (p *stubPolicyFoldPublisher) CancelRunWithReason(context.Context, string, store.RunEndReason) error {
	return nil
}
func (p *stubPolicyFoldPublisher) SubmitResume(_ context.Context, spec ResumeSpec, _ *ir.Workflow, _ *CompiledSource) error {
	cp := spec
	p.lastResume = &cp
	return nil
}
func (p *stubPolicyFoldPublisher) PreviewCredentials(_ context.Context, spec CredentialPreviewSpec, _ *ir.Workflow) (CredentialPreview, error) {
	cp := spec
	p.lastPreview = &cp
	return CredentialPreview{Context: spec.Context}, nil
}

// ---------------------------------------------------------------------------
// The launch fold: the policy's override cells reach the permission screen,
// the run doc and the preview, under the launch's own rows (ADR-129 §6).
// ---------------------------------------------------------------------------

const foldBotSource = `
agent work:
  backend: "claude_code"
  model: "claude-opus-5"
  system: p
  tools: [read_file]

prompt p:
  Do the work.

workflow gated:
  entry: work
  work -> done
`

// A policy override pinning an ungated backend on a permission-gated node is
// REFUSED at launch: the fold runs BEFORE the permission screen, so the
// screen sees the cell the launch itself never typed. This is the safety
// witness — a fold moved after the screen would let the cell reach the
// executor past a guard the launcher's own override cannot.
func TestLaunchFold_RunsBeforePermissionValidation(t *testing.T) {
	const source = foldBotSource + `
  permission: deny
`
	pub := &stubPolicyFoldPublisher{}
	srv, err := NewService(t.TempDir(), WithLogger(iterlog.Nop()), WithLaunchPublisher(pub))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	_, err = srv.Launch(context.Background(), LaunchSpec{
		FilePath: "fold.bot",
		Source:   source,
		LLMRoutePolicy: &store.RunLLMRoutePolicy{
			Overrides: []store.RunLLMRouteOverride{
				{Selector: "agent", Backend: "codex"},
				{Selector: "work", Backend: "codex"},
			},
		},
	})
	if err == nil || !strings.Contains(err.Error(), "UNGATED") {
		t.Fatalf("launch error = %v, want the permission screen refusing the policy's codex cell", err)
	}
	if pub.lastSpec != nil {
		t.Fatal("the launch reached the publisher past the screen")
	}
}

// The preview is a second launch-shaped reader: it must fold the SAME cells
// the launch folds, or the operator is shown candidates, pool wants and
// warnings for a launch shape that is not the launch's (O3). Both entries
// must carry the same folded list onward.
func TestPreviewAndLaunchFoldTheSameRows(t *testing.T) {
	pub := &stubPolicyFoldPublisher{}
	srv, err := NewService(t.TempDir(), WithLogger(iterlog.Nop()), WithLaunchPublisher(pub))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	pol := &store.RunLLMRoutePolicy{
		Overrides: []store.RunLLMRouteOverride{
			{Selector: "work", Backend: "claw", Model: "anthropic/claude-opus-4-8", Provider: "anthropic", Effort: "low"},
		},
	}
	if _, err := srv.Launch(context.Background(), LaunchSpec{
		FilePath:       "fold.bot",
		Source:         foldBotSource,
		LLMRoutePolicy: pol,
	}); err != nil {
		t.Fatalf("launch: %v", err)
	}
	// The server's production shape: the resolved policy rides the spec's
	// TOP-LEVEL field and the embedded Launch.LLMRoutePolicy stays nil —
	// the handler never writes it (pkg/server/credential_preview.go). The
	// preview entry must fold exactly that shape.
	if _, err := srv.PreviewCredentials(context.Background(), CredentialPreviewSpec{
		Context: CredentialPreviewContext{TeamID: "t1", BotID: "b1"},
		Launch: LaunchSpec{
			FilePath: "fold.bot",
			Source:   foldBotSource,
		},
		LLMRoutePolicy: pol,
	}); err != nil {
		t.Fatalf("preview: %v", err)
	}
	if pub.lastSpec == nil || pub.lastPreview == nil {
		t.Fatalf("captured spec=%v preview=%v, want both entries driven", pub.lastSpec, pub.lastPreview)
	}
	want := []ModelOverrideEntry{{Selector: "work", Backend: "claw", Model: "anthropic/claude-opus-4-8", Provider: "anthropic", Effort: "low"}}
	if !reflect.DeepEqual(pub.lastSpec.ModelOverrides, want) {
		t.Errorf("launch folded rows = %+v, want the policy's cell under the launch's (empty) rows", pub.lastSpec.ModelOverrides)
	}
	if !reflect.DeepEqual(pub.lastPreview.Launch.ModelOverrides, want) {
		t.Errorf("preview folded rows = %+v, want the SAME folded list the launch carries", pub.lastPreview.Launch.ModelOverrides)
	}
	// The fold's point: the derivation both entries feed agrees, because
	// the rows are the same list.
	launchRes := model.EffectiveProviders(
		model.RouteView{
			Workflow:  mustCompileFold(t, foldBotSource),
			Overrides: toModelOverrides(pub.lastSpec.ModelOverrides),
		},
		map[string]bool{"anthropic": true},
	)
	previewRes := model.EffectiveProviders(
		model.RouteView{
			Workflow:  mustCompileFold(t, foldBotSource),
			Overrides: toModelOverrides(pub.lastPreview.Launch.ModelOverrides),
		},
		map[string]bool{"anthropic": true},
	)
	if !reflect.DeepEqual(launchRes.Providers, previewRes.Providers) {
		t.Errorf("providers disagree: launch=%v preview=%v", launchRes.Providers, previewRes.Providers)
	}
	if !launchRes.NarrowSafe {
		t.Errorf("the pinned walk should be narrow-safe: %+v", launchRes)
	}
}

// The embedded shape is honored when the top level is nil: a producer that
// resolved the policy onto Launch.LLMRoutePolicy (the LaunchSpec contract's
// own field) must get the SAME fold, never a silent identity — an unfolded
// set would drive the publisher's pinned/envFunded/wants derivation.
func TestPreviewFoldsAnEmbeddedOnlyPolicy(t *testing.T) {
	pub := &stubPolicyFoldPublisher{}
	srv, err := NewService(t.TempDir(), WithLogger(iterlog.Nop()), WithLaunchPublisher(pub))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	embedded := &store.RunLLMRoutePolicy{
		Overrides: []store.RunLLMRouteOverride{
			{Selector: "work", Backend: "codex"},
		},
	}
	top := &store.RunLLMRoutePolicy{
		Overrides: []store.RunLLMRouteOverride{
			{Selector: "work", Backend: "claw"},
		},
	}
	if _, err := srv.PreviewCredentials(context.Background(), CredentialPreviewSpec{
		Context: CredentialPreviewContext{TeamID: "t1", BotID: "b1"},
		Launch: LaunchSpec{
			FilePath:       "fold.bot",
			Source:         foldBotSource,
			LLMRoutePolicy: embedded,
		},
	}); err != nil {
		t.Fatalf("preview: %v", err)
	}
	if pub.lastPreview == nil {
		t.Fatal("the preview never reached the publisher")
	}
	want := []ModelOverrideEntry{{Selector: "work", Backend: "codex"}}
	if !reflect.DeepEqual(pub.lastPreview.Launch.ModelOverrides, want) {
		t.Fatalf("preview folded rows = %+v, want the embedded policy's cell folded (the top level is nil)", pub.lastPreview.Launch.ModelOverrides)
	}
	// The publisher's slot whitelist and fallback door read the TOP-LEVEL
	// field: an embedded-only producer must reach them with the same
	// policy the fold folded, or the preview derives candidates over an
	// unnarrowed whitelist and an unarmed door while its launch narrows
	// both (the launch publisher reads the launch shape).
	if !reflect.DeepEqual(pub.lastPreview.LLMRoutePolicy, embedded) {
		t.Fatalf("preview publisher policy = %+v, want the embedded policy back-filled (the whitelist and door read this field)", pub.lastPreview.LLMRoutePolicy)
	}

	// When both are set, the TOP level wins: the server's resolved policy
	// answers for the launch, and the embedded value never overrides it.
	if _, err := srv.PreviewCredentials(context.Background(), CredentialPreviewSpec{
		Context:        CredentialPreviewContext{TeamID: "t1", BotID: "b1"},
		Launch:         LaunchSpec{FilePath: "fold.bot", Source: foldBotSource, LLMRoutePolicy: embedded},
		LLMRoutePolicy: top,
	}); err != nil {
		t.Fatalf("preview both-set: %v", err)
	}
	wantTop := []ModelOverrideEntry{{Selector: "work", Backend: "claw"}}
	if !reflect.DeepEqual(pub.lastPreview.Launch.ModelOverrides, wantTop) {
		t.Fatalf("preview folded rows = %+v, want the TOP level's cell (claw; the embedded value never wins)", pub.lastPreview.Launch.ModelOverrides)
	}
	// The publisher's consumers read the top level: with both set, the
	// back-fill never lets the embedded value win there either.
	if !reflect.DeepEqual(pub.lastPreview.LLMRoutePolicy, top) {
		t.Fatalf("preview publisher policy = %+v, want the TOP level (the embedded value never wins at the publisher either)", pub.lastPreview.LLMRoutePolicy)
	}
}

// The folded list is what the launch carries onward to the publisher, which
// converts it verbatim into the run doc's model_overrides and the queue
// message's rows — the resume paths' replay source. Never a re-derivation
// from a policy record that may have changed since the launch.
func TestRunDoc_StampsTheFoldedRows(t *testing.T) {
	pub := &stubPolicyFoldPublisher{}
	srv, err := NewService(t.TempDir(), WithLogger(iterlog.Nop()), WithLaunchPublisher(pub))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	pol := &store.RunLLMRoutePolicy{
		Overrides: []store.RunLLMRouteOverride{
			{Selector: "work", Backend: "claw"},
		},
	}
	if _, err := srv.Launch(context.Background(), LaunchSpec{
		FilePath:       "fold.bot",
		Source:         foldBotSource,
		LLMRoutePolicy: pol,
	}); err != nil {
		t.Fatalf("launch: %v", err)
	}
	if pub.lastSpec == nil {
		t.Fatal("the launch never reached the publisher")
	}
	want := []ModelOverrideEntry{{Selector: "work", Backend: "claw"}}
	if !reflect.DeepEqual(pub.lastSpec.ModelOverrides, want) {
		t.Fatalf("spec rows = %+v, want the folded list (the publisher stamps this verbatim)", pub.lastSpec.ModelOverrides)
	}
	// And a resume of such a run replays the persisted rows: the fresh
	// flags layer on top, the folded winners stay the winners.
	persisted := RunModelOverrides(pub.lastSpec.ModelOverrides)
	var flags model.ModelOverrides
	flags.SetBackend("*", "codex")
	resolved := flags.MergeOver(ModelOverridesFromRun(persisted)).ForNode("work", ir.NodeAgent)
	if resolved.Backend != "codex" {
		t.Errorf("resume flags backend = %q, want the fresh flag to outrank the persisted cell", resolved.Backend)
	}
	if resolved.Model != "" || resolved.Provider != "" {
		t.Errorf("resume resolved %+v, want nothing else pinned (the folded cell only pinned backend)", resolved)
	}
}

// The replay source of a resume is the run doc's model_overrides — the
// folded list the launch stamped — never a re-derivation from the policy
// snapshot beside it, which a level may have changed since the launch.
// The doc carries BOTH: the folded rows (work→claw) and a policy
// snapshot whose cell now names codex, as a level's edit after the
// launch would leave it. The resume must republish the doc's rows
// untouched.
func TestResume_ReplaysTheFoldedRowsNotThePolicy(t *testing.T) {
	t.Setenv("ITERION_RUNS_DETACHED", "0")
	pub := &stubPolicyFoldPublisher{}
	srv, err := NewService(t.TempDir(), WithLogger(iterlog.Nop()), WithLaunchPublisher(pub))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	dir := t.TempDir()
	botPath := filepath.Join(dir, "fold.bot")
	if err := os.WriteFile(botPath, []byte(foldBotSource), 0o644); err != nil {
		t.Fatal(err)
	}
	const runID = "run-foldreplay"
	_, hash, err := CompileWorkflowWithHash(botPath)
	if err != nil {
		t.Fatalf("compile fixture: %v", err)
	}
	ctx := context.Background()
	if _, err := srv.store.CreateRun(ctx, runID, "fold", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	r, err := srv.store.LoadRun(ctx, runID)
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	r.WorkflowHash = hash
	r.ModelOverrides = RunModelOverrides([]ModelOverrideEntry{{Selector: "work", Backend: "claw"}})
	r.LLMRoutePolicy = &store.RunLLMRoutePolicy{
		Overrides: []store.RunLLMRouteOverride{{Selector: "work", Backend: "codex"}},
	}
	if err := srv.store.SaveRun(ctx, r); err != nil {
		t.Fatalf("SaveRun: %v", err)
	}
	if err := srv.store.SaveCheckpoint(ctx, runID, &store.Checkpoint{NodeID: "done"}); err != nil {
		t.Fatalf("SaveCheckpoint: %v", err)
	}
	if err := srv.store.UpdateRunStatus(ctx, runID, store.RunStatusPausedOperator, "paused by operator"); err != nil {
		t.Fatalf("UpdateRunStatus: %v", err)
	}
	if _, err := srv.Resume(ctx, ResumeSpec{RunID: runID, FilePath: botPath}); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if pub.lastResume == nil {
		t.Fatal("the resume never reached the publisher")
	}
	after, err := srv.store.LoadRun(ctx, runID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	want := []store.RunModelOverride{{Selector: "work", Backend: "claw"}}
	if !reflect.DeepEqual(after.ModelOverrides, want) {
		t.Fatalf("doc rows after resume = %+v, want the launch's folded rows untouched (the policy snapshot beside them names codex)", after.ModelOverrides)
	}
}

// The detached runner carries neither the launch's model_overrides nor
// the resolved routing policy — no fold, no permission screen, no run-doc
// stamp — so a launch that typed either gesture is TOLD so, the way a
// dropped backend override is.
func TestLaunchDetached_NamesTheDroppedOverrides(t *testing.T) {
	t.Setenv("ITERION_RUNS_DETACHED", "1")
	t.Setenv("PATH", t.TempDir()) // no runner binary: the spawn fails after the warnings
	t.Setenv("ITERION_BIN", "")   // the env tier: a host override would spawn a real binary
	t.Setenv("HOME", t.TempDir()) // the install tier reads UserHomeDir
	restore := proc.SetInstallPathCandidates(func() []string { return nil })
	t.Cleanup(restore) // the fixed install paths: the resolver must refuse on any host
	var buf bytes.Buffer
	srv, err := NewService(t.TempDir(), WithLogger(iterlog.New(iterlog.LevelWarn, &buf)))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	_, err = srv.Launch(context.Background(), LaunchSpec{
		FilePath:       "fold.bot",
		Source:         foldBotSource,
		Backend:        "claude_code",
		ModelOverrides: []ModelOverrideEntry{{Selector: "work", Backend: "claw"}},
		LLMRoutePolicy: &store.RunLLMRoutePolicy{
			Overrides: []store.RunLLMRouteOverride{{Selector: "work", Backend: "claw"}},
		},
	})
	if err == nil {
		t.Fatal("the detached spawn was expected to fail with no runner binary on PATH")
	}
	for _, want := range []string{"model_overrides and routing-policy overrides are ignored in detached mode", "backend override"} {
		if !strings.Contains(buf.String(), want) {
			t.Fatalf("log = %q, want the warning naming %q", buf.String(), want)
		}
	}
}
