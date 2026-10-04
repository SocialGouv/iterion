package runtime

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/sandbox"
	"github.com/SocialGouv/iterion/pkg/store"
)

// #1773 — the PR2b admission floor: a run whose workspace holds an
// OUTSIDER's code (trust=fork) is admitted only when its sandbox resolves
// ACTIVE — per-run isolation that does not carry the runner's secrets. An
// ITERION_SANDBOX_OVERRIDE=none deployment does not qualify to run forks:
// the FORK run is refused, typed, at admission, before any node executes;
// the override keeps meaning what it means for trusted runs.

// forkLaneWorkflow is the fork lane's program: its sandbox block says auto —
// the shape the ticket measured — so the deployment's override, not the
// declaration, decides whether a container hosts the run.
func forkLaneWorkflow() *ir.Workflow {
	return &ir.Workflow{
		Name:    "fork_lane",
		Entry:   "done",
		Nodes:   map[string]ir.Node{"done": &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}}},
		Schemas: map[string]*ir.Schema{}, Prompts: map[string]*ir.Prompt{},
		Vars: map[string]*ir.Var{}, Loops: map[string]*ir.Loop{},
		Sandbox: &ir.SandboxSpec{Mode: "auto"},
	}
}

func workingHostRegistry(t *testing.T) map[string]sandbox.DriverConstructor {
	t.Helper()
	return map[string]sandbox.DriverConstructor{
		"docker": func() (sandbox.Driver, error) { return &podDriver{root: t.TempDir()}, nil },
	}
}

// TestAdmissionRefusesUntrustedRunWithoutSandbox is the ticket's witness for
// acceptance (1): a fork run under ITERION_SANDBOX_OVERRIDE=none is refused
// BEFORE any node executes, typed, naming the reason and the way out; the
// decision is persisted on the run record.
func TestAdmissionRefusesUntrustedRunWithoutSandbox(t *testing.T) {
	t.Setenv("ITERION_MODE", "local") // pin the factory's preference order (CI pods prefer kubernetes,noop)
	ctx := context.Background()
	s := tmpStore(t)
	run, err := s.CreateRun(ctx, "fork-no-sandbox", "fork_lane", nil)
	if err != nil {
		t.Fatal(err)
	}
	run.Trust = store.RunTrustFork
	if err := s.SaveRun(ctx, run); err != nil {
		t.Fatal(err)
	}

	eng := New(forkLaneWorkflow(), s, newStubExecutor(), WithWorkDir(t.TempDir()), WithSandboxOverride("none"), WithSandboxDrivers(driverlessHostRegistry()))
	runErr := eng.Run(ctx, run.ID, nil)
	var rtErr *RuntimeError
	if !errors.As(runErr, &rtErr) || rtErr.Code != store.FailureLaunchFailed {
		t.Fatalf("run = %v, want the typed launch refusal", runErr)
	}
	for _, want := range []string{"trust=fork", "per-run sandbox", "ITERION_SANDBOX_OVERRIDE"} {
		if !strings.Contains(runErr.Error(), want) {
			t.Errorf("refusal %q does not carry %q — an operator reading it needs the reason and the way out", runErr, want)
		}
	}
	got, err := s.LoadRun(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Admission == nil || got.Admission.Decision != "denied" || got.Admission.Code != "untrusted_without_sandbox" {
		t.Fatalf("admission = %+v, want a durable untrusted_without_sandbox denial", got.Admission)
	}
	if got.Status != store.RunStatusFailed {
		t.Fatalf("status = %s, want failed — refused before any node executed", got.Status)
	}
}

// TestAdmissionUntrustedRunWithActiveSandboxIsAdmitted is the same run under
// a deployment whose sandbox resolves active: admitted.
func TestAdmissionUntrustedRunWithActiveSandboxIsAdmitted(t *testing.T) {
	t.Setenv("ITERION_MODE", "local")
	ctx := context.Background()
	s := tmpStore(t)
	run, err := s.CreateRun(ctx, "fork-sandboxed", "fork_lane", nil)
	if err != nil {
		t.Fatal(err)
	}
	run.Trust = store.RunTrustFork
	if err := s.SaveRun(ctx, run); err != nil {
		t.Fatal(err)
	}

	eng := New(forkLaneWorkflow(), s, newStubExecutor(), WithWorkDir(t.TempDir()), WithSandboxDrivers(workingHostRegistry(t)))
	if err := eng.admitRun(ctx, run.ID, run); err != nil {
		t.Fatalf("admission refused a fork run whose sandbox resolves active: %v", err)
	}
}

// TestAdmissionUntrustedResumeRefusedWithoutSandbox extends the floor to the
// resume boundary: a fork child (or a fork-lane run parked resumable) is
// refused on THIS delivery too, and the run keeps its resumable status.
func TestAdmissionUntrustedResumeRefusedWithoutSandbox(t *testing.T) {
	t.Setenv("ITERION_MODE", "local")
	ctx := context.Background()
	s := tmpStore(t)
	run, err := s.CreateRun(ctx, "fork-resume-no-sandbox", "fork_lane", nil)
	if err != nil {
		t.Fatal(err)
	}
	run.Trust = store.RunTrustFork
	run.Status = store.RunStatusFailedResumable
	if err := s.SaveRun(ctx, run); err != nil {
		t.Fatal(err)
	}

	eng := New(forkLaneWorkflow(), s, newStubExecutor(), WithWorkDir(t.TempDir()), WithSandboxOverride("none"), WithSandboxDrivers(driverlessHostRegistry()))
	if err := eng.Resume(ctx, run.ID, nil); err == nil {
		t.Fatal("resume admitted an untrusted run without a per-run sandbox")
	}
	got, err := s.LoadRun(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != store.RunStatusFailedResumable {
		t.Fatalf("status = %s, want failed_resumable — a refused resume leaves the run as it was", got.Status)
	}
}

// TestAdmissionTrustedRunWithoutSandboxUnchanged is witness (e): the
// override keeps meaning what it means for trusted runs — a normal
// unsandboxed run is untouched by the floor.
func TestAdmissionTrustedRunWithoutSandboxUnchanged(t *testing.T) {
	t.Setenv("ITERION_MODE", "local")
	ctx := context.Background()
	s := tmpStore(t)
	run, err := s.CreateRun(ctx, "trusted-no-sandbox", "fork_lane", nil)
	if err != nil {
		t.Fatal(err)
	}

	eng := New(forkLaneWorkflow(), s, newStubExecutor(), WithWorkDir(t.TempDir()), WithSandboxOverride("none"), WithSandboxDrivers(driverlessHostRegistry()))
	if err := eng.Run(ctx, run.ID, nil); err != nil {
		t.Fatalf("a trusted run under the override was refused: %v", err)
	}
	got, err := s.LoadRun(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != store.RunStatusFinished {
		t.Fatalf("status = %s, want finished", got.Status)
	}
	if got.Admission == nil || got.Admission.Code == "untrusted_without_sandbox" {
		t.Fatalf("admission = %+v, want the ordinary context decision", got.Admission)
	}
}

// TestAdmissionUnknownTrustIsFlooredToo pins the predicate's direction: the
// floor reads Trusted(), which admits ONE value — an unknown trust class
// (a newer replica's marker mid-rollout, a hand-edited row) is untrusted and
// needs the sandbox, exactly as fork does.
func TestAdmissionUnknownTrustIsFlooredToo(t *testing.T) {
	t.Setenv("ITERION_MODE", "local")
	ctx := context.Background()
	s := tmpStore(t)
	run, err := s.CreateRun(ctx, "fork-unknown-trust", "fork_lane", nil)
	if err != nil {
		t.Fatal(err)
	}
	run.Trust = store.RunTrust("vendored")
	if err := s.SaveRun(ctx, run); err != nil {
		t.Fatal(err)
	}

	eng := New(forkLaneWorkflow(), s, newStubExecutor(), WithWorkDir(t.TempDir()), WithSandboxOverride("none"), WithSandboxDrivers(driverlessHostRegistry()))
	err = eng.Run(ctx, run.ID, nil)
	var rtErr *RuntimeError
	if !errors.As(err, &rtErr) || rtErr.Code != store.FailureLaunchFailed {
		t.Fatalf("run = %v, want the typed launch refusal for the unknown trust class too", err)
	}
}
