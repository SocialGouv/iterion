package runview

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/portsactivation"
	"github.com/SocialGouv/iterion/pkg/store"
)

const launchPortsBot = `dsl: 2
contract Root:
  display_name: "Echo"
  responsibility: "Return the input"
  inputs:
    value: string
  outputs:
    result: string
contract Echo:
  display_name: "Echo value"
  responsibility: "Copy value"
  inputs:
    value: string
  outputs:
    result: string
compute echo_impl:
  expr:
    result: "input.value"
workflow echo:
  runtime_semantics: "ports-v1"
  worktree: none
  contract: Root
  graph:
    nodes:
      echo:
        implementation: echo_impl
        contract: Echo
    bindings:
      input.value -> echo.value
    exports:
      result: echo.result
    products: ["result"]
`

func TestLaunchNativeDefaultOffAndActivatedLocal(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "echo.bot")
	if err := os.WriteFile(path, []byte(launchPortsBot), 0o600); err != nil {
		t.Fatal(err)
	}
	svc, err := NewService(dir, WithLogger(iterlog.Nop()), WithWorkDir(dir), WithSandboxDefault("none"))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := svc.Launch(ctx, LaunchSpec{FilePath: path, Vars: map[string]string{"value": "hello"}}); !errors.Is(err, store.ErrPortActivation) {
		t.Fatalf("default-off launch: %v", err)
	}
	ids, err := svc.store.ListRuns(ctx)
	if err != nil || len(ids) != 0 {
		t.Fatalf("refused launch persisted a run: %v %v", ids, err)
	}
	proof, err := portsactivation.ProbeLocal(ctx, svc.store, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := portsactivation.ActivateLocal(ctx, svc.store, *proof); err != nil {
		t.Fatal(err)
	}
	result, err := svc.Launch(ctx, LaunchSpec{FilePath: path, Vars: map[string]string{"value": "hello"}})
	if err != nil {
		t.Fatal(err)
	}
	if !store.IsNativeRunID(result.RunID) {
		t.Fatalf("native launch received legacy ID %q", result.RunID)
	}
	select {
	case <-result.Done:
	case <-time.After(15 * time.Second):
		t.Fatal("native run did not finish")
	}
	run, err := svc.store.LoadRun(ctx, result.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if run.RuntimeSemantics != store.RuntimeSemanticsPortsV1 || run.Status != store.RunStatusFinished {
		t.Fatalf("native launch ended with %+v", run)
	}
}
