package e2e

import (
	"context"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/cli"
	"github.com/SocialGouv/iterion/pkg/store"
)

// `iterion run --preset <name>` applies an in-source named preset before
// the `--var` overrides. The precedence — vars defaults < preset < --var
// — is what an operator relies on to launch the same bot against a named
// environment and still tweak one knob. buildRunInputs implements it and
// had no test.
//
// Mutation check: drop the preset and the run falls back to the
// recognisable "default-*" values; apply the preset AFTER --var and the
// tweak assertion fails; accept an unknown preset silently and the error
// case fails.

// runPresetFixture launches preset_mini.bot through the real CLI entry
// point and returns the values the compute node actually resolved.
func runPresetFixture(t *testing.T, runID, preset string, vars map[string]string) (map[string]any, error) {
	t.Helper()
	storeDir := t.TempDir()
	err := cli.RunRun(context.Background(), cli.RunOptions{
		File:          filepath.Join("testdata", "preset_mini.bot"),
		StoreDir:      storeDir,
		RunID:         runID,
		Preset:        preset,
		Vars:          vars,
		Executor:      newScenarioExecutor(),
		NoInteractive: true,
		MergeInto:     "none",
	}, &cli.Printer{W: io.Discard, Format: cli.OutputJSON})
	if err != nil {
		return nil, err
	}
	s, serr := store.New(storeDir)
	if serr != nil {
		t.Fatalf("open store: %v", serr)
	}
	art, aerr := s.LoadLatestArtifact(context.Background(), runID, "echo")
	if aerr != nil {
		t.Fatalf("load echo artifact: %v", aerr)
	}
	return art.Data, nil
}

func TestRunPresetAppliesValuesAndVarWins(t *testing.T) {
	t.Run("no preset falls back to the declared defaults", func(t *testing.T) {
		got, err := runPresetFixture(t, "preset-none", "", nil)
		if err != nil {
			t.Fatalf("run: %v", err)
		}
		if got["target"] != "default-target" || got["mode"] != "default-mode" {
			t.Fatalf("resolved %v, want the vars: defaults", got)
		}
	})

	t.Run("preset supplies both values", func(t *testing.T) {
		got, err := runPresetFixture(t, "preset-applied", "staging", nil)
		if err != nil {
			t.Fatalf("run: %v", err)
		}
		if got["target"] != "staging.example.com" {
			t.Errorf("target = %v, want the preset value (the preset never reached the run)", got["target"])
		}
		if got["mode"] != "careful" {
			t.Errorf("mode = %v, want the preset value", got["mode"])
		}
	})

	t.Run("--var wins over the preset", func(t *testing.T) {
		got, err := runPresetFixture(t, "preset-overridden", "staging", map[string]string{"mode": "fast"})
		if err != nil {
			t.Fatalf("run: %v", err)
		}
		if got["mode"] != "fast" {
			t.Errorf("mode = %v, want the --var override to win over the preset", got["mode"])
		}
		if got["target"] != "staging.example.com" {
			t.Errorf("target = %v, want the preset value to survive an unrelated --var", got["target"])
		}
	})

	t.Run("unknown preset is a clear user error", func(t *testing.T) {
		_, err := runPresetFixture(t, "preset-unknown", "does-not-exist", nil)
		if err == nil {
			t.Fatal("an unknown --preset was accepted, want an error naming the available presets")
		}
		if !strings.Contains(err.Error(), "staging") {
			t.Errorf("error = %v, want it to list the available preset names", err)
		}
	})
}
