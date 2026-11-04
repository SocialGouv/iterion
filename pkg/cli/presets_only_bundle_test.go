package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/store"
)

const presetsOnlyWorkflow = `vars:
  answer: string = "missing"

schema checked:
  configured: bool

compute check:
  output: checked
  expr:
    configured: "vars.answer == 'from-preset'"

fail missing:
  code: PRESET_MISSING
  message: "the file preset did not reach the run"

workflow probe:
  worktree: none
  sandbox: none
  entry: check
  check -> done when configured
  check -> missing when not configured
`

type noPresetModel struct{}

func (noPresetModel) Execute(_ context.Context, node ir.Node, _ map[string]any) (map[string]any, error) {
	return nil, fmt.Errorf("unexpected model execution at %s", node.NodeID())
}

// The directory and main.bot spellings must resolve the same file preset,
// including validate's executable path and the actual CLI run.
func TestAPresetsOnlyDirectoryIsABundleOnRunAndValidate(t *testing.T) {
	inTempWorkspace(t)
	writeFile(t, "bnd/main.bot", presetsOnlyWorkflow)
	writeFile(t, "bnd/presets/sre.md", "---\nname: sre\nvars:\n  answer: from-preset\n---\nA file preset.\n")
	for i, path := range []string{"bnd", "bnd/main.bot"} {
		t.Run(path, func(t *testing.T) {
			jp, out := jsonPrinter()
			if err := RunValidateWith(path, jp, ValidateOptions{Preset: "sre"}); err != nil {
				t.Fatalf("validate --preset sre: %v\n%s", err, out.String())
			}
			var res ValidateResult
			if err := json.Unmarshal(out.Bytes(), &res); err != nil {
				t.Fatal(err)
			}
			if res.ExecError != "" || res.Exec == nil || len(res.Exec.Passes) == 0 {
				t.Fatalf("file preset did not reach validation: %+v", res)
			}
			if contains(res.Exec.Passes[0].Nodes, "missing") {
				t.Fatalf("validation took the no-preset path: %+v", res.Exec)
			}
			runID := fmt.Sprintf("preset-%d", i)
			storeDir := filepath.Join(t.TempDir(), "store")
			err := RunRun(context.Background(), RunOptions{
				File: path, Preset: "sre", RunID: runID, StoreDir: storeDir,
				Executor: noPresetModel{}, Sandbox: "none", NoInteractive: true,
			}, &Printer{W: io.Discard, Format: OutputJSON})
			if err != nil {
				t.Fatalf("run --preset sre: %v", err)
			}
			s, err := store.New(storeDir)
			if err != nil {
				t.Fatal(err)
			}
			run, err := s.LoadRun(context.Background(), runID)
			if err != nil {
				t.Fatal(err)
			}
			if run.Status != store.RunStatusFinished {
				t.Fatalf("preset run finished as %s", run.Status)
			}
		})
	}
}

// Validation must read a preset file even when its workflow is named as a
// loose-looking main.bot. A malformed preset must not disappear silently:
// the bundle warning names the file with its reason, and the dry run
// refuses to run over it (the refusal counts it, the warning names it).
func TestValidateNamesABrokenPresetBesideAnUnmanifestedMain(t *testing.T) {
	inTempWorkspace(t)
	writeFile(t, "bnd/main.bot", presetsOnlyWorkflow)
	writeFile(t, "bnd/presets/broken.md", "---\nnam: typo\n---\nBroken preset.\n")
	jp, out := jsonPrinter()
	err := RunValidateWith("bnd/main.bot", jp, ValidateOptions{Exec: true})
	var res ValidateResult
	if decodeErr := json.Unmarshal(out.Bytes(), &res); decodeErr != nil {
		t.Fatal(decodeErr)
	}
	named := false
	for _, d := range res.Diagnostics {
		if d.Source == "bundle" && strings.Contains(d.Message, "broken.md") {
			named = true
		}
	}
	if err == nil || res.ExecError == "" || !named {
		t.Fatalf("validation lost the broken file preset: err=%v result=%+v", err, res)
	}
}
