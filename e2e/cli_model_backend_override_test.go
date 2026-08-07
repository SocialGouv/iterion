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

// `iterion run --model <sel>=<spec>` / `--backend <sel>=<name>` re-target the
// bot's LLM nodes at launch, ABOVE the node's own DSL `backend:`/`model:`, so
// an operator can run someone else's bot on their own provider without editing
// the `.bot`. The flags are consumed deep inside the real ClawExecutor
// (resolveBackendName / buildTask), which is why a stub executor can say
// nothing about them — these tests therefore run the REAL executor and read the
// resolved value back out of the failure the unresolvable target produces.
// Nothing here needs a credential or a network: an unknown backend dies at
// delegate-registry lookup, an unknown provider dies inside claw's spec parse.
//
// Mutation check: delete the `if ov := e.modelOverrides.ForNode(...)` branch in
// resolveBackendName and the "--backend re-targets" case reads the DSL name;
// delete the one in buildTask and the "--model re-targets" case reads the DSL
// model spec. Both fail loudly.

// runOverrideFixture launches a fixture through the real CLI entry point with
// NO stub executor and returns the error text plus the persisted run status.
func runOverrideFixture(t *testing.T, fixture, runID string, models, backends []string) (string, store.RunStatus) {
	t.Helper()
	storeDir := t.TempDir()
	err := cli.RunRun(context.Background(), cli.RunOptions{
		File:          filepath.Join("testdata", fixture),
		StoreDir:      storeDir,
		RunID:         runID,
		ModelFor:      models,
		BackendFor:    backends,
		NoInteractive: true,
		MergeInto:     "none",
		Sandbox:       "none",
	}, &cli.Printer{W: io.Discard, Format: cli.OutputJSON})
	if err == nil {
		t.Fatalf("run %s: expected the unresolvable target to fail the run, got success", runID)
	}
	s, serr := store.New(storeDir)
	if serr != nil {
		t.Fatalf("open store: %v", serr)
	}
	r, lerr := s.LoadRun(context.Background(), runID)
	if lerr != nil {
		t.Fatalf("load run %s: %v", runID, lerr)
	}
	return err.Error(), r.Status
}

func TestRunBackendOverrideRetargetsNodes(t *testing.T) {
	t.Run("no override: the node's DSL backend is what executes", func(t *testing.T) {
		msg, status := runOverrideFixture(t, "model_backend_override_mini.bot", "backend-ov-none", nil, nil)
		if !strings.Contains(msg, "dsl_backend_absent") {
			t.Fatalf("error %q does not name the DSL backend — the fixture no longer proves anything", msg)
		}
		if status != store.RunStatusFailedResumable {
			t.Errorf("status = %q, want failed_resumable (a backend-resolution failure keeps a checkpoint)", status)
		}
	})

	t.Run("--backend selector=value wins over the DSL backend", func(t *testing.T) {
		msg, _ := runOverrideFixture(t, "model_backend_override_mini.bot", "backend-ov-node",
			nil, []string{"survey=cli_backend_absent"})
		if !strings.Contains(msg, "cli_backend_absent") {
			t.Errorf("error %q does not name the --backend value: the launch override never reached the executor", msg)
		}
		if strings.Contains(msg, "dsl_backend_absent") {
			t.Errorf("error %q still names the DSL backend: the override did not win the resolution chain", msg)
		}
	})

	t.Run("a bare --backend value targets every LLM node", func(t *testing.T) {
		msg, _ := runOverrideFixture(t, "model_backend_override_mini.bot", "backend-ov-bare",
			nil, []string{"star_backend_absent"})
		if !strings.Contains(msg, "star_backend_absent") {
			t.Errorf("error %q does not name the bare --backend value: the implicit '*' selector did not apply", msg)
		}
	})

	t.Run("a selector that matches another node leaves this one alone", func(t *testing.T) {
		msg, _ := runOverrideFixture(t, "model_backend_override_mini.bot", "backend-ov-other",
			nil, []string{"reviewer=other_backend_absent"})
		if !strings.Contains(msg, "dsl_backend_absent") {
			t.Errorf("error %q does not name the DSL backend: a selector for reviewer leaked onto survey", msg)
		}
		if strings.Contains(msg, "other_backend_absent") {
			t.Errorf("error %q names reviewer's override: the selector matched the wrong node", msg)
		}
	})
}

func TestRunModelOverrideRetargetsNodes(t *testing.T) {
	t.Run("no override: the node's DSL model spec is what executes", func(t *testing.T) {
		msg, status := runOverrideFixture(t, "model_override_claw_mini.bot", "model-ov-none", nil, nil)
		if !strings.Contains(msg, "dslprovider") {
			t.Fatalf("error %q does not quote the DSL model spec — the fixture no longer proves anything", msg)
		}
		if status != store.RunStatusFailedResumable {
			t.Errorf("status = %q, want failed_resumable", status)
		}
	})

	t.Run("--model selector=spec wins over the DSL model", func(t *testing.T) {
		msg, _ := runOverrideFixture(t, "model_override_claw_mini.bot", "model-ov-node",
			[]string{"gen=cliprovider/cli-model"}, nil)
		if !strings.Contains(msg, "cliprovider/cli-model") {
			t.Errorf("error %q does not quote the --model spec: the launch override never reached the task", msg)
		}
		if strings.Contains(msg, "dslprovider") {
			t.Errorf("error %q still quotes the DSL model: the override did not win the resolution chain", msg)
		}
	})

	t.Run("--model and --backend compose on the same node", func(t *testing.T) {
		// The node keeps claw (so the model spec is still what fails) while the
		// model comes from the flag — per-FIELD resolution, not last-flag-wins.
		msg, _ := runOverrideFixture(t, "model_override_claw_mini.bot", "model-ov-compose",
			[]string{"gen=cliprovider/cli-model"}, []string{"agent=claw"})
		if !strings.Contains(msg, "cliprovider/cli-model") {
			t.Errorf("error %q does not quote the --model spec: --backend clobbered the model directive", msg)
		}
	})

	t.Run("a malformed override fails the launch instead of being ignored", func(t *testing.T) {
		storeDir := t.TempDir()
		err := cli.RunRun(context.Background(), cli.RunOptions{
			File:          filepath.Join("testdata", "model_override_claw_mini.bot"),
			StoreDir:      storeDir,
			RunID:         "model-ov-malformed",
			ModelFor:      []string{"gen="},
			NoInteractive: true,
			MergeInto:     "none",
			Sandbox:       "none",
		}, &cli.Printer{W: io.Discard, Format: cli.OutputJSON})
		if err == nil {
			t.Fatalf("expected an empty --model value to be rejected, got success")
		}
		if !strings.Contains(err.Error(), "--model") {
			t.Errorf("error %q does not attribute the failure to --model", err)
		}
	})
}
