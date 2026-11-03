package runtime

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
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
	if dir.CapBytes != sandbox.ReadBackCapBytes {
		t.Errorf("drain cap = %d, want the %d default carried on the entry — the drain meters on the manifest's figure", dir.CapBytes, sandbox.ReadBackCapBytes)
	}
}

// TestTheDrainManifestCarriesTheServingMountsCap: the figure a dir is
// metered against is the serving mount's to name (sandbox.ReadBackCapFor).
// A bot that serves the run-files path with its own sized emptyDir chose
// the figure; the manifest carries it, and the drain never cuts the
// author's explicit choice down to the default. The deployment's override
// stands in where the mount names none.
func TestTheDrainManifestCarriesTheServingMountsCap(t *testing.T) {
	t.Run("the bot's own sized emptyDir names the figure", func(t *testing.T) {
		t.Setenv(sandbox.ReadBackCapEnv, "1Gi")
		p := canaryParams(t)
		p.RunFilesHostDir = t.TempDir()
		p.Workflow.Sandbox.Mounts = []string{"type=emptydir,target=/iterion/artifact-files,size=2Gi"}
		info := buildRunInfoFor(t, p)
		if len(info.ReadBackDirs) != 1 {
			t.Fatalf("RunInfo carries %d read-back dirs, want 1: %#v", len(info.ReadBackDirs), info.ReadBackDirs)
		}
		if got := info.ReadBackDirs[0].CapBytes; got != 2<<30 {
			t.Errorf("drain cap = %d, want the bot's own 2 GiB — the author's explicit figure, over the override and the default", got)
		}
	})
	t.Run("the override stands in where the mount names none", func(t *testing.T) {
		t.Setenv(sandbox.ReadBackCapEnv, "1Gi")
		p := canaryParams(t)
		p.RunFilesHostDir = t.TempDir()
		info := buildRunInfoFor(t, p)
		if len(info.ReadBackDirs) != 1 {
			t.Fatalf("RunInfo carries %d read-back dirs, want 1: %#v", len(info.ReadBackDirs), info.ReadBackDirs)
		}
		if got := info.ReadBackDirs[0].CapBytes; got != 1<<30 {
			t.Errorf("drain cap = %d, want the 1 GiB override — the runtime's own emptyDir names no size, so the deployment's figure applies", got)
		}
	})
	// The two guards on WHICH mount names the figure, one scenario each.
	t.Run("nested mounts: the nearest one's volume holds the files", func(t *testing.T) {
		t.Setenv(sandbox.ReadBackCapEnv, "")
		p := canaryParams(t)
		p.RunFilesHostDir = t.TempDir()
		p.Workflow.Sandbox.Mounts = []string{
			"type=emptydir,target=/iterion,size=1Gi",
			"type=emptydir,target=/iterion/artifact-files,size=2Gi",
		}
		info := buildRunInfoFor(t, p)
		if len(info.ReadBackDirs) != 1 {
			t.Fatalf("RunInfo carries %d read-back dirs, want 1: %#v", len(info.ReadBackDirs), info.ReadBackDirs)
		}
		if got := info.ReadBackDirs[0].CapBytes; got != 2<<30 {
			t.Errorf("drain cap = %d, want the 2 GiB of the run-files volume itself — not the 1 GiB of the parent it shadows", got)
		}
	})
	t.Run("a host bind the driver drops names no figure", func(t *testing.T) {
		t.Setenv(sandbox.ReadBackCapEnv, "")
		p := canaryParams(t)
		p.RunFilesHostDir = t.TempDir()
		// The bind is how the bot serves the path on a host — listed first,
		// so a figure asked of every mount meets it before the emptyDir; on
		// the pod it is dropped, and the sized emptyDir at the same path
		// holds the files.
		p.Workflow.Sandbox.Mounts = []string{
			"type=bind,source=/host/persist,target=/iterion/artifact-files",
			"type=emptydir,target=/iterion/artifact-files,size=2Gi",
		}
		info := buildRunInfoFor(t, p)
		if len(info.ReadBackDirs) != 1 {
			t.Fatalf("RunInfo carries %d read-back dirs, want 1: %#v", len(info.ReadBackDirs), info.ReadBackDirs)
		}
		if got := info.ReadBackDirs[0].CapBytes; got != 2<<30 {
			t.Errorf("drain cap = %d, want the 2 GiB of the emptyDir the pod keeps — a dropped bind serves nothing, so it names no figure", got)
		}
	})
	t.Run("a mount above the path names no figure: the runtime's own emptyDir holds the files", func(t *testing.T) {
		t.Setenv(sandbox.ReadBackCapEnv, "")
		p := canaryParams(t)
		p.RunFilesHostDir = t.TempDir()
		p.Workflow.Sandbox.Mounts = []string{"type=emptydir,target=/iterion,size=2Gi"}
		info := buildRunInfoFor(t, p)
		if len(info.ReadBackDirs) != 1 {
			t.Fatalf("RunInfo carries %d read-back dirs, want 1: %#v", len(info.ReadBackDirs), info.ReadBackDirs)
		}
		if got := info.ReadBackDirs[0].CapBytes; got != sandbox.ReadBackCapBytes {
			t.Errorf("drain cap = %d, want the %d default — the files live in the runtime's own emptyDir at the path, which names no size, not in the parent's 2 GiB volume", got, sandbox.ReadBackCapBytes)
		}
	})
}

// TestThePodRunLaysItsOwnStorageBeneathAMountAboveThePath: a mount above
// the run-files path holds the path in its tree, never as a directory it
// promises — the path may not exist (the drain's in-pod tar -C then fails
// on every run that wrote nothing: a false loss), a configmap or a secret
// parent is read-only, and a pvc parent would drain every earlier run's
// files into this run's set. The runtime lays its own emptyDir AT the path
// beneath it: a directory that exists, starts empty and takes writes.
func TestThePodRunLaysItsOwnStorageBeneathAMountAboveThePath(t *testing.T) {
	for _, parent := range []string{
		"type=pvc,source=persist,target=/iterion",
		"type=emptydir,target=/iterion",
		// The kubelet creates the mount points nested in a configmap or a
		// secret volume before it mounts it read-only.
		"type=configmap,source=cfg,target=/iterion",
		"type=secret,source=sec,target=/iterion",
	} {
		t.Run(parent, func(t *testing.T) {
			p := canaryParams(t)
			p.Workflow.Sandbox.Mounts = []string{parent}
			spec := buildSandboxSpecFor(t, false, p)
			serving := mountServing(spec.Mounts, "/iterion/artifact-files")
			if path.Clean(mountTarget(serving)) != "/iterion/artifact-files" || !strings.Contains(serving, "type=emptydir") {
				t.Fatalf("the nearest mount serving the path is %q, want the runtime's emptyDir AT it — a parent's tree is no directory anyone promised (mounts: %v)", serving, spec.Mounts)
			}
			if spec.Env[runFilesEnvVar] != "/iterion/artifact-files" {
				t.Errorf("spec.Env[%s] = %q, want the path the emptyDir serves", runFilesEnvVar, spec.Env[runFilesEnvVar])
			}
			if len(spec.ReadBackPaths) != 1 || spec.ReadBackPaths[0] != "/iterion/artifact-files" {
				t.Errorf("spec.ReadBackPaths = %v, want the declared path", spec.ReadBackPaths)
			}
		})
	}
}

// TestAReadOnlyMountLeavesTheRunFilesPathUnserved: a read-only mount AT the
// path — or a read-only pvc or emptyDir above it, where no container
// runtime can create a mount point — leaves no storage a tool could write.
// Nothing is declared, named or drained: the variable would name a
// directory no tool can fill (#815's defect), and the drain would collect
// the mount's own content as the run's files.
func TestAReadOnlyMountLeavesTheRunFilesPathUnserved(t *testing.T) {
	for _, mount := range []string{
		"type=configmap,source=cfg,target=/iterion/artifact-files",
		"type=secret,source=sec,target=/iterion/artifact-files",
		"type=pvc,source=shared,target=/iterion/artifact-files,readonly",
		"type=pvc,source=shared,target=/iterion,ro=true",
		"type=emptydir,target=/iterion,readonly",
	} {
		t.Run(mount, func(t *testing.T) {
			p := canaryParams(t)
			p.Workflow.Sandbox.Mounts = []string{mount}
			spec := buildSandboxSpecFor(t, false, p)
			if v, ok := spec.Env[runFilesEnvVar]; ok {
				t.Errorf("spec.Env[%s] = %q, want it unset — no tool can write the path it names", runFilesEnvVar, v)
			}
			if len(spec.ReadBackPaths) != 0 {
				t.Errorf("spec.ReadBackPaths = %v, want none — nothing the run wrote can be there to drain", spec.ReadBackPaths)
			}
			for _, m := range spec.Mounts {
				if m != mount && strings.Contains(m, "type=emptydir") {
					t.Errorf("the runtime laid %q under a read-only mount — a mount point no container runtime can create there (mounts: %v)", m, spec.Mounts)
				}
			}
			if info := buildRunInfoFor(t, p); len(info.ReadBackDirs) != 0 {
				t.Errorf("RunInfo carries %d read-back dirs, want none: %#v", len(info.ReadBackDirs), info.ReadBackDirs)
			}
		})
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
		// A sizeLimit is a kill switch, not a meter: the kubelet evicts
		// the WHOLE pod when an emptyDir's usage passes it — no drain, no
		// workspace export. The cap is the drain's meter on the host
		// (ADR-143), which drops the member over the figure and keeps
		// the rest; the pod side names no figure of its own.
		if strings.Contains(serving, "size=") {
			t.Errorf("the runtime's emptyDir carries a sizeLimit: %q — past it the kubelet evicts the run instead of the drain dropping a member", serving)
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
		if events[0].Data["reason"] != string(sandbox.ReadBackUnknown) {
			t.Errorf("event reason = %v, want %q — a failure the driver typed nothing on is said to be untyped, never guessed", events[0].Data["reason"], sandbox.ReadBackUnknown)
		}
	})
	t.Run("a typed loss carries its reason as a stable token", func(t *testing.T) {
		events = nil
		run := &drainFakeRun{exportErr: &sandbox.ReadBackLoss{Reason: sandbox.ReadBackOverCap, Err: errors.New("the run's read-back set is over the 256 MiB cap (ADR-143) at big.bin")}}
		drainRunFilesOnCleanup(run, nil, emit)
		if len(events) != 1 || events[0].Type != store.EventSandboxRunFilesDrainFailed {
			t.Fatalf("events = %+v, want one sandbox_runfiles_drain_failed", events)
		}
		if events[0].Data["reason"] != string(sandbox.ReadBackOverCap) {
			t.Errorf("event reason = %v, want %q — a reader tells a cap reached from a tool still writing without parsing the prose", events[0].Data["reason"], sandbox.ReadBackOverCap)
		}
	})
	t.Run("the members the host declined are named on the event", func(t *testing.T) {
		events = nil
		run := &drainFakeRun{exportErr: &sandbox.ReadBackLoss{
			Reason: sandbox.ReadBackDeclined,
			Err:    errors.New("kubernetes: drain /iterion/artifact-files: the host declined 2 member(s)"),
			Declined: []sandbox.ReadBackDeclinedMember{
				{Dir: "/iterion/artifact-files", Name: "pipe", Why: sandbox.DeclinedKind},
				{Dir: "/iterion/artifact-files", Name: "latest.html", Why: sandbox.DeclinedHardlinkTarget},
			},
		}}
		drainRunFilesOnCleanup(run, nil, emit)
		if len(events) != 1 || events[0].Type != store.EventSandboxRunFilesDrainFailed {
			t.Fatalf("events = %+v, want one sandbox_runfiles_drain_failed — a set short of a member is a loss on the run, not a log line", events)
		}
		data := events[0].Data
		if data["reason"] != string(sandbox.ReadBackDeclined) {
			t.Errorf("event reason = %v, want %q", data["reason"], sandbox.ReadBackDeclined)
		}
		want := []map[string]any{
			{"dir": "/iterion/artifact-files", "name": "latest.html", "why": string(sandbox.DeclinedHardlinkTarget)},
			{"dir": "/iterion/artifact-files", "name": "pipe", "why": string(sandbox.DeclinedKind)},
		}
		if !reflect.DeepEqual(data["declined"], want) {
			t.Errorf("event declined = %#v, want %#v — every member, by name, in path order", data["declined"], want)
		}
		if data["declined_count"] != 2 {
			t.Errorf("event declined_count = %v, want 2", data["declined_count"])
		}
		if data["stale"] != false {
			t.Errorf("event stale = %v, want false sent — a reader cannot tell a false from an absent field", data["stale"])
		}
	})
	t.Run("a set that may hold a raced attempt's leftovers says so", func(t *testing.T) {
		events = nil
		run := &drainFakeRun{exportErr: &sandbox.ReadBackLoss{Reason: sandbox.ReadBackOverCap, Err: errors.New("over the cap, after a raced attempt"), Stale: true}}
		drainRunFilesOnCleanup(run, nil, emit)
		if len(events) != 1 || events[0].Data["stale"] != true {
			t.Fatalf("events = %+v, want one event with stale=true — over_cap alone reads as the failing attempt's partial set", events)
		}
		if _, listed := events[0].Data["declined"]; listed {
			t.Errorf("event declined = %v on a drain that declined nothing, want the field absent", events[0].Data["declined"])
		}
	})
	t.Run("the declined list is bounded, its count is not", func(t *testing.T) {
		events = nil
		var many []sandbox.ReadBackDeclinedMember
		for i := runFilesDeclinedListed + 50; i > 0; i-- {
			many = append(many, sandbox.ReadBackDeclinedMember{Dir: "/iterion/artifact-files", Name: fmt.Sprintf("fifo-%04d", i), Why: sandbox.DeclinedKind})
		}
		run := &drainFakeRun{exportErr: &sandbox.ReadBackLoss{Reason: sandbox.ReadBackDeclined, Err: errors.New("declined"), Declined: many}}
		drainRunFilesOnCleanup(run, nil, emit)
		if len(events) != 1 {
			t.Fatalf("events = %+v, want one", events)
		}
		listed, _ := events[0].Data["declined"].([]map[string]any)
		if len(listed) != runFilesDeclinedListed {
			t.Fatalf("the event names %d members, want the first %d", len(listed), runFilesDeclinedListed)
		}
		if listed[0]["name"] != "fifo-0001" {
			t.Errorf("the first listed member is %v, want fifo-0001 — the bound keeps the first in path order, not the archive's", listed[0]["name"])
		}
		if events[0].Data["declined_count"] != runFilesDeclinedListed+50 {
			t.Errorf("event declined_count = %v, want every declined member counted (%d)", events[0].Data["declined_count"], runFilesDeclinedListed+50)
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

// TestAFailedDrainWarnsThePartialUploadNotAnEmptyPanel: a PARTIAL drain
// leaves what it landed in the store's scratch dir, and the post-run
// upload carries every file it finds — the panel reads partial, not
// empty. The warn that says "empty" sends the operator to a panel state
// that does not exist.
func TestAFailedDrainWarnsThePartialUploadNotAnEmptyPanel(t *testing.T) {
	var buf bytes.Buffer
	logger := iterlog.New(iterlog.LevelWarn, &buf)
	run := &drainFakeRun{exportErr: errors.New("kubectl exec failed")}
	drainRunFilesOnCleanup(run, logger, func(store.EventType, map[string]any) error { return nil })

	out := buf.String()
	if out == "" {
		t.Fatal("a failed drain logged nothing — the failure must stay loud")
	}
	if !strings.Contains(out, "partial") {
		t.Errorf("warn = %q, want the partial-upload truth: what landed before the failure still uploads", out)
	}
	if strings.Contains(out, "panel reads empty") {
		t.Errorf("warn = %q, want no empty-panel claim — a partial drain leaves the panel partial", out)
	}
}

// TestTheSandboxTeardownDrainsTheRunFilesBeforeTheSandboxGoes: the drain
// is wired at ONE site — the cleanup startSandbox returns — and the
// witnesses above call drainRunFilesOnCleanup by hand, so the call itself
// had none: with that line removed from the cleanup, every test in this
// package stayed green while a pod's run files were never collected
// (#2668 reopened in silence). This drives the production cleanup on a
// pod-shaped driver and counts the drain on the run the driver started —
// once, and before the sandbox's own Cleanup, because the bytes only exist
// in the pod until then.
func TestTheSandboxTeardownDrainsTheRunFilesBeforeTheSandboxGoes(t *testing.T) {
	t.Setenv("ITERION_MODE", "local") // pin the factory's preference order to docker,podman,noop
	st := tmpStore(t)
	ctx := context.Background()
	const runID = "run-drain-wiring"
	if _, err := st.CreateRun(ctx, runID, "drain", nil); err != nil {
		t.Fatal(err)
	}
	fake := &drainFakeRun{}
	wf := &ir.Workflow{
		Name:    "drain",
		Sandbox: &ir.SandboxSpec{Mode: "inline", Image: "example.invalid/iterion-sandbox:test", HostState: "none"},
	}
	e := New(wf, st, newStubExecutor(),
		WithLogger(iterlog.Nop()),
		WithWorkDir(t.TempDir()),
		WithSandboxDrivers(map[string]sandbox.DriverConstructor{
			"docker": func() (sandbox.Driver, error) { return &drainingDriver{run: fake}, nil },
		}),
	)
	cleanup, err := e.startSandbox(ctx, runID, e.workDir, "", nil)
	if err != nil {
		t.Fatalf("startSandbox: %v", err)
	}
	if fake.drained != 0 {
		t.Fatalf("drained = %d before the teardown, want 0 — the drain is the teardown's, not the start's", fake.drained)
	}
	cleanup()
	if fake.drained != 1 {
		t.Fatalf("drained = %d after the teardown, want exactly 1 — the cleanup did not drain the pod's run files (trace %v)", fake.drained, fake.trace)
	}
	drain, shutdown := slices.Index(fake.trace, "drain"), slices.Index(fake.trace, "cleanup")
	if shutdown < 0 {
		t.Fatalf("trace = %v, want the sandbox's own Cleanup in it — the teardown did not run, so the order proves nothing", fake.trace)
	}
	if drain > shutdown {
		t.Fatalf("trace = %v, want the drain BEFORE the sandbox's Cleanup — after it, the pod and its bytes are gone", fake.trace)
	}
}

// drainingDriver is a pod-shaped driver — no host filesystem — whose Start
// hands the runtime a run carrying the export seam: the teardown's drain
// call on that run is what the wiring witness counts.
type drainingDriver struct {
	run *drainFakeRun
}

func (d *drainingDriver) Name() string { return "docker" }

func (d *drainingDriver) Capabilities() sandbox.Capabilities {
	return sandbox.Capabilities{SupportsImage: true, SupportsMounts: true}
}

func (d *drainingDriver) Prepare(_ context.Context, spec sandbox.Spec) (sandbox.PreparedSpec, error) {
	return stallingPrepared{spec: spec}, nil
}

func (d *drainingDriver) Start(context.Context, sandbox.PreparedSpec, sandbox.RunInfo) (sandbox.Run, error) {
	return d.run, nil
}

// drainFakeRun implements the widened export seam and counts the drain
// call, so the wiring (not the driver) is what the assertions measure;
// trace records the drain and the sandbox's Cleanup in the order the
// teardown made them.
type drainFakeRun struct {
	exportErr error
	drained   int
	trace     []string
}

func (r *drainFakeRun) Driver() string { return "kubernetes" }

func (r *drainFakeRun) ExportWorkspace(context.Context) error { return nil }

func (r *drainFakeRun) ExportRunFiles(context.Context) error {
	r.drained++
	r.trace = append(r.trace, "drain")
	return r.exportErr
}

func (r *drainFakeRun) Command(context.Context, []string, sandbox.ExecOpts) *exec.Cmd {
	return nil
}

func (r *drainFakeRun) Exec(context.Context, []string, sandbox.ExecOpts) (sandbox.ExecResult, error) {
	return sandbox.ExecResult{}, nil
}

func (r *drainFakeRun) Cleanup(context.Context) error {
	r.trace = append(r.trace, "cleanup")
	return nil
}

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
