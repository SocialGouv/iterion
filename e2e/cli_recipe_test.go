package e2e

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/recipe"
	"github.com/SocialGouv/iterion/pkg/cli"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/store"
)

// `iterion run --recipe <file>` runs a workflow through a recipe: a
// first-class overlay carrying preset vars, prompt overrides and a
// budget, so the same `.bot` can be launched as a named, comparable
// configuration. recipe.Apply is unit-covered in pkg/backend/recipe;
// the CLI WIRING — that the overlay is loaded, applied to the compiled
// workflow, and reaches the executor before it snapshots budget and
// prompts — had no test at all.
//
// Both readouts are things an operator observes: the values a node
// resolved (published artifact) and whether the run converged under the
// recipe's budget (run status).
//
// Mutation check: skip spec.Apply and the preset-var assertions fall
// back to the recognisable "default-*" values; apply the recipe AFTER
// the CLI vars and the precedence assertion fails; drop the recipe
// budget and the raise case stops converging; skip the workflow-name
// check and the mismatch case stops erroring.

// writeRecipe marshals a spec to a temp JSON file, the way an operator
// hands one to --recipe.
func writeRecipe(t *testing.T, spec recipe.RecipeSpec) string {
	t.Helper()
	data, err := json.MarshalIndent(spec, "", "  ")
	if err != nil {
		t.Fatalf("marshal recipe: %v", err)
	}
	path := filepath.Join(t.TempDir(), "recipe.json")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write recipe: %v", err)
	}
	return path
}

// runWithRecipe launches through the real CLI entry point and returns the
// store dir it wrote to plus the engine error.
func runWithRecipe(t *testing.T, runID, recipePath, file string, vars map[string]string) (string, error) {
	t.Helper()
	storeDir := t.TempDir()
	err := cli.RunRun(context.Background(), cli.RunOptions{
		File:          file,
		Recipe:        recipePath,
		StoreDir:      storeDir,
		RunID:         runID,
		Vars:          vars,
		Executor:      newScenarioExecutor(),
		NoInteractive: true,
		MergeInto:     "none",
	}, &cli.Printer{W: io.Discard, Format: cli.OutputJSON})
	return storeDir, err
}

func loadEchoArtifact(t *testing.T, storeDir, runID string) map[string]any {
	t.Helper()
	s, err := store.New(storeDir)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	art, err := s.LoadLatestArtifact(context.Background(), runID, "echo")
	if err != nil {
		t.Fatalf("load echo artifact: %v", err)
	}
	return art.Data
}

func presetMiniRecipe(vars recipe.PresetVars) recipe.RecipeSpec {
	return recipe.RecipeSpec{
		Name:        "staging-overlay",
		WorkflowRef: recipe.WorkflowRef{Name: "preset_mini", Path: filepath.Join("testdata", "preset_mini.bot")},
		PresetVars:  vars,
	}
}

func TestRunRecipeAppliesPresetVarsAndVarStillWins(t *testing.T) {
	botFile := filepath.Join("testdata", "preset_mini.bot")

	t.Run("the recipe's preset vars reach the run", func(t *testing.T) {
		path := writeRecipe(t, presetMiniRecipe(recipe.PresetVars{
			"target": "recipe.example.com",
			"mode":   "thorough",
		}))
		storeDir, err := runWithRecipe(t, "recipe-vars", path, botFile, nil)
		if err != nil {
			t.Fatalf("run --recipe: %v", err)
		}
		got := loadEchoArtifact(t, storeDir, "recipe-vars")
		if got["target"] != "recipe.example.com" {
			t.Errorf("target = %v, want the recipe preset value (the recipe never reached the run)", got["target"])
		}
		if got["mode"] != "thorough" {
			t.Errorf("mode = %v, want the recipe preset value", got["mode"])
		}
	})

	t.Run("--var wins over the recipe", func(t *testing.T) {
		path := writeRecipe(t, presetMiniRecipe(recipe.PresetVars{
			"target": "recipe.example.com",
			"mode":   "thorough",
		}))
		storeDir, err := runWithRecipe(t, "recipe-var-wins", path, botFile, map[string]string{"mode": "fast"})
		if err != nil {
			t.Fatalf("run --recipe --var: %v", err)
		}
		got := loadEchoArtifact(t, storeDir, "recipe-var-wins")
		if got["mode"] != "fast" {
			t.Errorf("mode = %v, want the --var override to win over the recipe", got["mode"])
		}
		if got["target"] != "recipe.example.com" {
			t.Errorf("target = %v, want the recipe value to survive an unrelated --var", got["target"])
		}
	})

	t.Run("the recipe supplies the workflow path when --file is omitted", func(t *testing.T) {
		path := writeRecipe(t, presetMiniRecipe(recipe.PresetVars{"target": "from-the-recipe-ref"}))
		storeDir, err := runWithRecipe(t, "recipe-path", path, "", nil)
		if err != nil {
			t.Fatalf("run --recipe with no --file: %v", err)
		}
		got := loadEchoArtifact(t, storeDir, "recipe-path")
		if got["target"] != "from-the-recipe-ref" {
			t.Errorf("target = %v — workflow_ref.path did not resolve the .bot", got["target"])
		}
	})
}

// The recipe's budget is the reason a recipe is a comparable unit: two
// recipes over the same bot differ by what they may spend. The fixture's
// own budget cannot finish the run, so the outcome is a direct readout of
// whether the recipe budget was applied.
func TestRunRecipeBudgetOverridesTheWorkflowBudget(t *testing.T) {
	botFile := filepath.Join("testdata", "budget_override_mini.bot")

	base := recipe.RecipeSpec{
		Name:        "budgeted",
		WorkflowRef: recipe.WorkflowRef{Name: "budget_override_mini", Path: botFile},
	}

	t.Run("no recipe budget keeps the bot's too-small one", func(t *testing.T) {
		path := writeRecipe(t, base)
		storeDir, err := runWithRecipe(t, "recipe-budget-baseline", path, botFile, nil)
		if err == nil {
			t.Fatal("the run finished under the bot's 25-token budget; the DSL budget was not honoured")
		}
		if !errors.Is(err, runtime.ErrBudgetExceeded) {
			t.Fatalf("err = %v, want ErrBudgetExceeded", err)
		}
		if got := loadRun(t, storeDir, "recipe-budget-baseline").Status; got != store.RunStatusFailedResumable {
			t.Errorf("status = %q, want %q", got, store.RunStatusFailedResumable)
		}
	})

	t.Run("a recipe budget raises the cap and the run converges", func(t *testing.T) {
		raised := base
		raised.Budget = &recipe.BudgetOverride{MaxTokens: 500}
		path := writeRecipe(t, raised)
		storeDir, err := runWithRecipe(t, "recipe-budget-raised", path, botFile, nil)
		if err != nil {
			t.Fatalf("run under the raised recipe budget: %v", err)
		}
		if got := loadRun(t, storeDir, "recipe-budget-raised").Status; got != store.RunStatusFinished {
			t.Errorf("status = %q, want %q — the recipe budget never reached the engine", got, store.RunStatusFinished)
		}
	})
}

// A recipe pointing at a different workflow than the one being run would
// silently apply the wrong presets. The command must refuse.
func TestRunRecipeRefusesAMismatchedWorkflow(t *testing.T) {
	path := writeRecipe(t, recipe.RecipeSpec{
		Name:        "wrong-target",
		WorkflowRef: recipe.WorkflowRef{Name: "some_other_workflow"},
	})
	_, err := runWithRecipe(t, "recipe-mismatch", path, filepath.Join("testdata", "preset_mini.bot"), nil)
	if err == nil {
		t.Fatal("a recipe referencing another workflow was accepted")
	}
	if !strings.Contains(err.Error(), "some_other_workflow") || !strings.Contains(err.Error(), "preset_mini") {
		t.Errorf("error = %v, want it to name both the referenced and the actual workflow", err)
	}
}

// A malformed or missing recipe must fail loudly at launch, not degrade
// into an un-overlaid run that looks like it worked.
func TestRunRecipeRefusesAnUnloadableFile(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope.json")
	if _, err := runWithRecipe(t, "recipe-missing", missing, filepath.Join("testdata", "preset_mini.bot"), nil); err == nil {
		t.Fatal("a missing --recipe file was accepted")
	}

	broken := filepath.Join(t.TempDir(), "broken.json")
	if err := os.WriteFile(broken, []byte("{not json"), 0o644); err != nil {
		t.Fatalf("write broken recipe: %v", err)
	}
	err := func() error {
		_, e := runWithRecipe(t, "recipe-broken", broken, filepath.Join("testdata", "preset_mini.bot"), nil)
		return e
	}()
	if err == nil {
		t.Fatal("a malformed --recipe file was accepted")
	}
	if !strings.Contains(err.Error(), "recipe") {
		t.Errorf("error = %v, want it to name the recipe as the cause", err)
	}
}
