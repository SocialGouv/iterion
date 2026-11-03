package cli_test

import (
	"context"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/detecttest"
	"github.com/SocialGouv/iterion/internal/gittest"
	"github.com/SocialGouv/iterion/pkg/cli"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
)

// `iterion resume` judges the mono_family pin its run FROZE at launch
// against THIS host's credentials — the resume-side twin of `iterion run`'s
// injection, at the one other moment that can still refuse (#1221). Without
// it the CLI lane replayed a dead pin and the run died at its first review
// node, which is the failure the launch refusal #2258 closed for new runs
// only.
//
// Both halves judge one staged host (internal/detecttest): an Anthropic key
// and the claude_code CLI the claude slot's routes pin (#1221), never the
// machine's own credentials or CLI.
//
// Mutants: the RefuseFrozenPin call removed from RunResumeWithFile → the
// refusal half reds (the resume proceeds into the engine); the route
// judgement blind to the installed CLI (InventoryFromReport reading
// Available alone) → the served half reds.

// monoFamilyCLIBot declares the opt-in var the topology reads, so the
// judgement applies to this bot at all.
const monoFamilyCLIBot = `
vars:
  review_mode: string = "mono"
  mono_family: string = "claude"

workflow frozen_pin:
  entry: done
`

// seedFrozenPinCLIRun parks a run whose inputs carry the family its launch
// resolved, the way a launch leaves them on the document.
func seedFrozenPinCLIRun(t *testing.T, family string) (bot, storeDir, runID string, frozen map[string]any, s store.RunStore) {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	bot = filepath.Join(dir, "frozen_pin.bot")
	if err := os.WriteFile(bot, []byte(monoFamilyCLIBot), 0o644); err != nil {
		t.Fatal(err)
	}
	_, hash, err := runview.CompileWorkflowWithHash(bot, "")
	if err != nil {
		t.Fatal(err)
	}
	storeDir = filepath.Join(dir, "store")
	st, err := store.New(storeDir)
	if err != nil {
		t.Fatal(err)
	}
	r, err := st.CreateRun(ctx, "run-cli-frozen-pin", "frozen_pin", map[string]any{"review_mode": "mono", "mono_family": family})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SaveCheckpoint(ctx, r.ID, &store.Checkpoint{NodeID: "done"}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateRunStatus(ctx, r.ID, store.RunStatusPausedOperator, "paused by operator"); err != nil {
		t.Fatal(err)
	}
	if r, err = st.LoadRun(ctx, r.ID); err != nil {
		t.Fatal(err)
	}
	r.WorkflowHash = hash
	r.FilePath = bot
	if err := st.SaveRun(ctx, r); err != nil {
		t.Fatal(err)
	}
	return bot, storeDir, r.ID, maps.Clone(r.Inputs), st
}

// A CLI resume whose frozen family this host no longer serves is refused,
// the run does not move, and its inputs stay byte-identical.
func TestResume_CLIRefusesAPinThisHostCannotServe(t *testing.T) {
	hermeticSandbox(t)
	t.Chdir(gittest.SourceRepo(t))
	detecttest.StageClaudeCodeHost(t, "ANTHROPIC_API_KEY")
	bot, storeDir, runID, frozen, st := seedFrozenPinCLIRun(t, "gpt")

	ctx := context.Background()
	p, _ := newTestPrinter(cli.OutputHuman)
	err := cli.RunResumeWithFile(ctx, bot, cli.ResumeOptions{RunID: runID, StoreDir: storeDir}, p)
	if err == nil {
		t.Fatal("a CLI resume pinned to a family this host cannot serve started — it dies at its first review node instead")
	}
	for _, want := range []string{"refusing to resume", `"gpt"`, "claude", "launch a fresh run"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not carry %q: %v", want, err)
		}
	}
	r, lerr := st.LoadRun(ctx, runID)
	if lerr != nil {
		t.Fatalf("LoadRun after the refusal: %v", lerr)
	}
	if r.Status != store.RunStatusPausedOperator {
		t.Errorf("the refused resume moved the run to %s, want paused_operator", r.Status)
	}
	if !reflect.DeepEqual(r.Inputs, frozen) {
		t.Errorf("the refused resume rewrote the frozen inputs: %+v, want %+v", r.Inputs, frozen)
	}
}

// The same lane resumes normally when the host still serves the frozen
// family — a credential to spend and the claude_code CLI its routes run on:
// this refuses a dead configuration, not resumes.
func TestResume_CLIAcceptsAPinThisHostServes(t *testing.T) {
	hermeticSandbox(t)
	t.Chdir(gittest.SourceRepo(t))
	detecttest.StageClaudeCodeHost(t, "ANTHROPIC_API_KEY")
	bot, storeDir, runID, _, st := seedFrozenPinCLIRun(t, "claude")

	ctx := context.Background()
	p, _ := newTestPrinter(cli.OutputHuman)
	if err := cli.RunResumeWithFile(ctx, bot, cli.ResumeOptions{RunID: runID, StoreDir: storeDir}, p); err != nil {
		t.Fatalf("a CLI resume whose frozen family this host serves: %v, want it to run", err)
	}
	if r, err := st.LoadRun(ctx, runID); err != nil || r.Status != store.RunStatusFinished {
		t.Fatalf("the accepted resume left the run %v (%v), want finished", r.Status, err)
	}
}
