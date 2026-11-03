package runtime

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/sandbox"
	"github.com/SocialGouv/iterion/pkg/store"
)

// The read-back wiring (ADR-143): the run-files path is DECLARED, served
// by storage the driver keeps, and carried to the drain as data. The
// canary beside this file pins the named ⇔ served rule; these tests pin
// the flip itself, so reverting the wiring reddens here and not only in
// an operator's empty artifact-files panel.

// runInfoCapturingDriver records the RunInfo handed to Start and then
// refuses: the test reads what the runtime would have started, and
// nothing runs.
type runInfoCapturingDriver struct {
	captured sandbox.RunInfo
}

func (d *runInfoCapturingDriver) Name() string { return "docker" }

func (d *runInfoCapturingDriver) Capabilities() sandbox.Capabilities {
	return sandbox.Capabilities{
		SupportsImage:          true,
		SupportsMounts:         true,
		SupportsHostBindMounts: false, // the pod case: no host filesystem
		SupportsPostCreate:     true,
	}
}

func (d *runInfoCapturingDriver) Prepare(context.Context, sandbox.Spec) (sandbox.PreparedSpec, error) {
	return preparedCanary{}, nil
}

func (d *runInfoCapturingDriver) Start(_ context.Context, _ sandbox.PreparedSpec, info sandbox.RunInfo) (sandbox.Run, error) {
	d.captured = info
	return nil, errCanaryStop
}

type preparedCanary struct{}

func (preparedCanary) DriverName() string { return "docker" }

func buildRunInfoFor(t *testing.T, p SandboxParams) sandbox.RunInfo {
	t.Helper()
	t.Setenv("ITERION_MODE", "local")
	d := &runInfoCapturingDriver{}
	p.Drivers = map[string]sandbox.DriverConstructor{
		"docker": func() (sandbox.Driver, error) { return d, nil },
	}
	p.Logger = nil
	p.EmitEvent = func(store.EventType, map[string]any) error { return nil }
	if _, err := resolveAndStartSandbox(context.Background(), p); !errors.Is(err, errCanaryStop) {
		t.Fatalf("resolveAndStartSandbox = %v, want the capturing driver's refusal — the wiring did not reach Start, so this test proves nothing", err)
	}
	return d.captured
}

// TestThePodRunCarriesTheDrainManifest: on a driver with no host
// filesystem the declared read-back path reaches the Run as a drain
// manifest entry — container path and the run file store's host dir —
// because the driver at the other end of that list is the one that
// streams the bytes back at teardown. A declaration that never reaches
// the driver drains nothing and the panel reads empty: the defect this
// pins (#2668).
func TestThePodRunCarriesTheDrainManifest(t *testing.T) {
	hostScratch := t.TempDir()
	p := canaryParams(t)
	p.RunFilesHostDir = hostScratch
	info := buildRunInfoFor(t, p)

	if len(info.ReadBackDirs) != 1 {
		t.Fatalf("RunInfo carries %d read-back dirs, want 1: %#v", len(info.ReadBackDirs), info.ReadBackDirs)
	}
	dir := info.ReadBackDirs[0]
	if dir.ContainerPath != "/iterion/artifact-files" {
		t.Errorf("drain container path = %q, want /iterion/artifact-files", dir.ContainerPath)
	}
	if dir.HostDir != hostScratch {
		t.Errorf("drain host dir = %q, want the run file store's dir %s", dir.HostDir, hostScratch)
	}
}

// TestTheRunFilesDeclarationFollowsTheStorage: the spec declares the
// read-back path exactly when the runtime arranged storage for it — the
// emptyDir on the pod, the bind on a host — and names it in no form when
// the store offered no directory at all. The admission walk in
// Spec.Validate refuses a declaration no mount serves; these cases pin
// the three shapes the runtime may produce.
func TestTheRunFilesDeclarationFollowsTheStorage(t *testing.T) {
	t.Run("a pod: an emptyDir serves the declared path", func(t *testing.T) {
		spec := buildSandboxSpecFor(t, false, canaryParams(t))
		if spec.Env[runFilesEnvVar] != "/iterion/artifact-files" {
			t.Fatalf("spec.Env[%s] = %q, want the declared container path", runFilesEnvVar, spec.Env[runFilesEnvVar])
		}
		serving := mountServing(spec.Mounts, "/iterion/artifact-files")
		if serving == "" {
			t.Fatalf("no kept mount serves %s (mounts: %v)", "/iterion/artifact-files", spec.Mounts)
		}
		if !strings.Contains(serving, "type=emptydir") {
			t.Errorf("the serving mount is %q, want driver-owned storage (type=emptydir)", serving)
		}
		if !strings.Contains(serving, "size=") {
			t.Errorf("the emptyDir carries no size cap: %q — the read-back set is named and metered (ADR-143)", serving)
		}
		if len(spec.ReadBackPaths) != 1 || spec.ReadBackPaths[0] != "/iterion/artifact-files" {
			t.Errorf("spec.ReadBackPaths = %v, want the declared path", spec.ReadBackPaths)
		}
	})
	t.Run("a host: the bind serves it", func(t *testing.T) {
		spec := buildSandboxSpecFor(t, true, canaryParams(t))
		if spec.Env[runFilesEnvVar] != "/iterion/artifact-files" {
			t.Fatalf("spec.Env[%s] = %q, want the declared container path", runFilesEnvVar, spec.Env[runFilesEnvVar])
		}
		serving := mountServing(spec.Mounts, "/iterion/artifact-files")
		if serving == "" || !strings.Contains(serving, "type=bind") {
			t.Errorf("the serving mount is %q, want the host bind", serving)
		}
		if len(spec.ReadBackPaths) != 1 || spec.ReadBackPaths[0] != "/iterion/artifact-files" {
			t.Errorf("spec.ReadBackPaths = %v, want the declared path", spec.ReadBackPaths)
		}
	})
	t.Run("no store dir: nothing is named", func(t *testing.T) {
		p := canaryParams(t)
		p.RunFilesHostDir = ""
		spec := buildSandboxSpecFor(t, false, p)
		if spec.Env[runFilesEnvVar] != "" {
			t.Errorf("spec.Env[%s] = %q with no dir to serve — a promise the runtime never arranged", runFilesEnvVar, spec.Env[runFilesEnvVar])
		}
		if len(spec.ReadBackPaths) != 0 {
			t.Errorf("spec.ReadBackPaths = %v, want none", spec.ReadBackPaths)
		}
	})
}

// TestThePodRunSkipsItsOwnStorageWhenABotMountServesTheTarget: the
// runtime's emptyDir is the run-files path's storage only when nothing
// else keeps it. A bot's own kept mount at the target (a PVC mounted for
// persistence) already serves the read-back path, and a second volumeMount
// at the same mountPath is a pod the API server refuses — the runtime adds
// nothing, and the declaration and the variable stand (the drain reads
// whatever mount serves the path). A mount the driver will drop (a host
// bind) serves nothing: the emptyDir is added.

func TestThePodRunSkipsItsOwnStorageWhenABotMountServesTheTarget(t *testing.T) {
	t.Run("a kept mount already serves the target: no second storage", func(t *testing.T) {
		p := canaryParams(t)
		p.Workflow.Sandbox.Mounts = []string{"type=pvc,source=persist,target=/iterion/artifact-files"}
		spec := buildSandboxSpecFor(t, false, p)
		for _, m := range spec.Mounts {
			if strings.Contains(m, "type=emptydir") && strings.Contains(m, "target=/iterion/artifact-files") {
				t.Errorf("the spec carries the runtime emptyDir %q beside the bot's own mount at the target — two volumeMounts at one mountPath build a pod the API server refuses (mounts: %v)", m, spec.Mounts)
			}
		}
		if len(spec.ReadBackPaths) != 1 || spec.ReadBackPaths[0] != "/iterion/artifact-files" {
			t.Errorf("spec.ReadBackPaths = %v, want the declared path — the bot's mount serves it, so the declaration stands", spec.ReadBackPaths)
		}
		if spec.Env[runFilesEnvVar] != "/iterion/artifact-files" {
			t.Errorf("spec.Env[%s] = %q, want the variable naming the bot-served path", runFilesEnvVar, spec.Env[runFilesEnvVar])
		}
	})
	t.Run("a host bind serves nothing here: the emptyDir stands", func(t *testing.T) {
		p := canaryParams(t)
		p.Workflow.Sandbox.Mounts = []string{"source=/host/persist,target=/iterion/artifact-files,type=bind"}
		spec := buildSandboxSpecFor(t, false, p)
		serving := mountServing(spec.Mounts, "/iterion/artifact-files")
		if !strings.Contains(serving, "type=emptydir") {
			t.Errorf("the serving mount is %q, want the runtime's emptyDir — a bind this driver drops serves nothing (mounts: %v)", serving, spec.Mounts)
		}
	})
}

// TestDrainRunFilesOnCleanupSurfacesAFailedDrain: the drain is the clause
// that makes the variable legal on a pod, and a failed drain must be an
// event — never an empty set (ADR-143 §4). A reader of the run has to
// tell "the drain failed" from "the tool wrote nothing"; the empty panel
// reads the same for both.
func TestDrainRunFilesOnCleanupSurfacesAFailedDrain(t *testing.T) {
	var events []store.Event
	emit := func(t store.EventType, data map[string]any) error {
		events = append(events, store.Event{Type: t, Data: data})
		return nil
	}
	t.Run("a failing drain says so on the run", func(t *testing.T) {
		events = nil
		run := &drainFakeRun{exportErr: errors.New("kubectl exec failed")}
		drainRunFilesOnCleanup(run, nil, emit)
		if len(events) != 1 || events[0].Type != store.EventSandboxRunFilesDrainFailed {
			t.Fatalf("events = %+v, want one sandbox_runfiles_drain_failed", events)
		}
		if events[0].Data["driver"] != "kubernetes" {
			t.Errorf("event driver = %v, want kubernetes", events[0].Data["driver"])
		}
		if errText, _ := events[0].Data["error"].(string); !strings.Contains(errText, "kubectl exec failed") {
			t.Errorf("event error = %v, want the drain's own cause", events[0].Data["error"])
		}
	})
	t.Run("a clean drain stays silent", func(t *testing.T) {
		events = nil
		run := &drainFakeRun{}
		drainRunFilesOnCleanup(run, nil, emit)
		if len(events) != 0 {
			t.Fatalf("events = %+v, want none — success is the log's, not the stream's", events)
		}
	})
	t.Run("a run without the export seam is not asked", func(t *testing.T) {
		events = nil
		drainRunFilesOnCleanup(&plainFakeRun{}, nil, emit)
		if len(events) != 0 {
			t.Fatalf("events = %+v, want none", events)
		}
	})
}

// drainFakeRun implements the widened export seam and counts the drain
// call, so the wiring (not the driver) is what the assertions measure.
type drainFakeRun struct {
	exportErr error
	drained   int
}

func (r *drainFakeRun) Driver() string { return "kubernetes" }

func (r *drainFakeRun) ExportWorkspace(context.Context) error { return nil }

func (r *drainFakeRun) ExportRunFiles(context.Context) error {
	r.drained++
	return r.exportErr
}

func (r *drainFakeRun) Command(context.Context, []string, sandbox.ExecOpts) *exec.Cmd {
	return nil
}

func (r *drainFakeRun) Exec(context.Context, []string, sandbox.ExecOpts) (sandbox.ExecResult, error) {
	return sandbox.ExecResult{}, nil
}

func (r *drainFakeRun) Cleanup(context.Context) error { return nil }

// plainFakeRun is a bind-mount driver's run: no export seam at all.
type plainFakeRun struct{}

func (plainFakeRun) Driver() string { return "docker" }

func (plainFakeRun) Command(context.Context, []string, sandbox.ExecOpts) *exec.Cmd {
	return nil
}

func (plainFakeRun) Exec(context.Context, []string, sandbox.ExecOpts) (sandbox.ExecResult, error) {
	return sandbox.ExecResult{}, nil
}

func (plainFakeRun) Cleanup(context.Context) error { return nil }
