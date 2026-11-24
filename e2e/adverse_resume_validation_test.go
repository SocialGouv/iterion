package e2e

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"testing"

	"github.com/SocialGouv/iterion/pkg/cli"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/store"
)

func TestRVA_RunAndResumeEnforceStrictValidation(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("TMPDIR", t.TempDir())
	t.Setenv(runtime.EnvOutputValidation, "true")
	bot := filepath.Join("testdata", "rva_validation_resume.bot")
	p := &cli.Printer{W: io.Discard, Format: cli.OutputJSON}

	// Control: on `run`, an invalid `second` fails SCHEMA_VALIDATION.
	ctrl := newScenarioExecutor()
	ctrl.on("first", func(map[string]any) (map[string]any, error) { return map[string]any{"value": "a"}, nil })
	ctrl.on("second", func(map[string]any) (map[string]any, error) { return map[string]any{"other": 1}, nil })
	err := cli.RunRun(context.Background(), cli.RunOptions{File: bot, StoreDir: t.TempDir(), RunID: "rva-ctrl", Executor: ctrl, NoInteractive: true, MergeInto: "none"}, p)
	var re *runtime.RuntimeError
	if !errors.As(err, &re) || re.Code != runtime.ErrCodeSchemaValidation {
		t.Fatalf("strict run accepted invalid second: %v", err)
	}

	// Same run shape, but `second` first fails resumably, then is resumed
	// with the same invalid output.
	storeDir := t.TempDir()
	first := newScenarioExecutor()
	first.on("first", func(map[string]any) (map[string]any, error) { return map[string]any{"value": "a"}, nil })
	first.on("second", func(map[string]any) (map[string]any, error) {
		return nil, errors.New("boom: transient backend failure")
	})
	err = cli.RunRun(context.Background(), cli.RunOptions{File: bot, StoreDir: storeDir, RunID: "rva-resume", Executor: first, NoInteractive: true, MergeInto: "none"}, p)
	run := loadRun(t, storeDir, "rva-resume")
	t.Logf("launch: err=%v status=%s", err, run.Status)

	second := newScenarioExecutor()
	second.on("second", func(map[string]any) (map[string]any, error) { return map[string]any{"other": 1}, nil })
	err = cli.RunResumeWithFile(context.Background(), bot, cli.ResumeOptions{RunID: "rva-resume", StoreDir: storeDir, Executor: second}, p)
	run = loadRun(t, storeDir, "rva-resume")
	t.Logf("resume with invalid second: err=%v status=%s", err, run.Status)
	if !errors.As(err, &re) || re.Code != runtime.ErrCodeSchemaValidation || run.Status != store.RunStatusFailedResumable {
		t.Errorf("strict resume accepted invalid second: err=%v status=%s", err, run.Status)
	}
}
