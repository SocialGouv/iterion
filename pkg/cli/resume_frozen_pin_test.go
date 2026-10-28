package cli_test

import (
	"context"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

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
// Mutant: the RefuseFrozenPin call removed from RunResumeWithFile → the
// refusal half reds (the resume proceeds into the engine).

// monoFamilyCLIBot declares the opt-in var the topology reads, so the
// judgement applies to this bot at all.
const monoFamilyCLIBot = `
vars:
  review_mode: string = "mono"
  mono_family: string = "claude"

workflow frozen_pin:
  entry: done
`

// isolateHostFamiliesCLI pins what detect.Detect can see to exactly the
// provider keys named: a scratch HOME and empty credential homes so no
// forfait, codex login or OAuth bridge is found, and every family-bearing
// variable cleared before the wanted ones are set. Without it this witness
// would read the developer's own keys and be green or red by accident.
// Values are literals written here; no host value is ever read or printed.
func isolateHostFamiliesCLI(t *testing.T, available ...string) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("CODEX_HOME", t.TempDir())
	t.Setenv("ITERION_BACKEND_PREFERENCE", "")
	for _, name := range []string{
		"ANTHROPIC_API_KEY", "ANTHROPIC_BASE_URL", "ANTHROPIC_AUTH_TOKEN",
		"ZAI_API_KEY", "OPENAI_API_KEY", "OPENAI_BASE_URL",
	} {
		t.Setenv(name, "")
	}
	for _, name := range available {
		t.Setenv(name, "sk-witness-not-a-real-key")
	}
}

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
	isolateHostFamiliesCLI(t, "ANTHROPIC_API_KEY")
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

// The same lane resumes normally when a credential still serves the frozen
// family: this refuses a dead configuration, not resumes.
func TestResume_CLIAcceptsAPinThisHostServes(t *testing.T) {
	hermeticSandbox(t)
	t.Chdir(gittest.SourceRepo(t))
	isolateHostFamiliesCLI(t, "ANTHROPIC_API_KEY")
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
