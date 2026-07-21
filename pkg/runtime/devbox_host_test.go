package runtime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

// hostDevboxRecorder captures what the (stubbed) devbox install seam was
// asked to do, plus the staged-content checks that must happen at install
// time — the engine removes the per-run staging dir at run end, so the
// only moment its contents are observable is inside the install call.
type hostDevboxRecorder struct {
	installs      []string // project dirs passed to `devbox install -c`
	stagedConfigs []string // install dirs that carried devbox.json at install time
	stagedLocks   []string // install dirs that carried devbox.lock at install time
	installErr    error    // returned from every install when non-nil
}

// stubHostDevbox replaces the host-devbox test seams for the duration of
// the test. lookErr simulates a host without a devbox binary.
func stubHostDevbox(t *testing.T, lookErr error) *hostDevboxRecorder {
	t.Helper()
	rec := &hostDevboxRecorder{}
	origLook, origRun := hostDevboxLookPath, runHostDevboxInstall
	hostDevboxLookPath = func() (string, error) {
		if lookErr != nil {
			return "", lookErr
		}
		return "/fake/devbox", nil
	}
	runHostDevboxInstall = func(_ context.Context, _, projectDir string, _ *iterlog.Logger) error {
		rec.installs = append(rec.installs, projectDir)
		if _, err := os.Stat(filepath.Join(projectDir, devboxConfigName)); err == nil {
			rec.stagedConfigs = append(rec.stagedConfigs, projectDir)
		}
		if _, err := os.Stat(filepath.Join(projectDir, devboxLockName)); err == nil {
			rec.stagedLocks = append(rec.stagedLocks, projectDir)
		}
		return rec.installErr
	}
	t.Cleanup(func() { hostDevboxLookPath, runHostDevboxInstall = origLook, origRun })
	return rec
}

// envRecordingExecutor is the stub executor plus the SetRunExtraEnv seam
// the engine pushes host-provisioned env through.
type envRecordingExecutor struct {
	*stubExecutor
	runExtraEnv []string
	setCalls    int
}

func (e *envRecordingExecutor) SetRunExtraEnv(env []string) {
	e.runExtraEnv = env
	e.setCalls++
}

// writeBotBundleDir lays out a directory shaped like a catalog bot on a
// runner image (/opt/iterion/bots/<bot>): main.bot + devbox.json (+ lock),
// and opens it the way the cloud runner does (bundle.OpenDir on the
// resolved bot's directory).
func writeBotBundleDir(t *testing.T, withLock bool) (*bundle.Bundle, string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "main.bot"), []byte("workflow placeholder {}\n"), 0o644); err != nil {
		t.Fatalf("write main.bot: %v", err)
	}
	writeDevboxConfig(t, dir, "go-containerregistry@0.21.6", "yq-go@4.53.3")
	if withLock {
		if err := os.WriteFile(filepath.Join(dir, devboxLockName), []byte(`{"lockfile_version":"1"}`), 0o644); err != nil {
			t.Fatalf("write devbox.lock: %v", err)
		}
	}
	b, err := bundle.OpenDir(dir)
	if err != nil {
		t.Fatalf("bundle.OpenDir(%s): %v", dir, err)
	}
	return b, dir
}

// devboxTestWorkflow is a minimal workflow carrying the app-dev shape's
// inline sandbox block: on the cloud runner that block is neutralized by
// ITERION_SANDBOX_OVERRIDE=none and the run executes in the pod.
func devboxTestWorkflow() *ir.Workflow {
	return &ir.Workflow{
		Name:  "devbox-host",
		Entry: "start",
		Nodes: map[string]ir.Node{
			"start": &ir.ToolNode{BaseNode: ir.BaseNode{ID: "start"}, Command: "true"},
			"done":  &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
			"fail":  &ir.FailNode{BaseNode: ir.BaseNode{ID: "fail"}},
		},
		Edges:    []*ir.Edge{{From: "start", To: "done"}},
		Worktree: "none",
		Sandbox:  &ir.SandboxSpec{Mode: "inline", Image: "ghcr.io/example/iterion-sandbox-full:edge"},
	}
}

// devboxEvent returns the run's sandbox_devbox_provisioned event data, or
// nil when none was emitted.
func devboxEvent(t *testing.T, s store.RunStore, runID string) map[string]any {
	t.Helper()
	events, err := s.LoadEvents(context.Background(), runID)
	if err != nil {
		t.Fatalf("load events: %v", err)
	}
	for _, ev := range events {
		if ev.Type == store.EventSandboxDevboxProvisioned {
			return ev.Data
		}
	}
	return nil
}

// stringsFromAny converts a JSON-round-tripped []any back to []string.
func stringsFromAny(v any) []string {
	items, ok := v.([]any)
	if !ok {
		if ss, ok := v.([]string); ok {
			return ss
		}
		return nil
	}
	out := make([]string, 0, len(items))
	for _, it := range items {
		if s, ok := it.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// TestEngineRun_CloudShape_ProvisionsBotDevboxWithoutSandbox is the
// regression lock for the CLOUD wiring specifically. It reproduces the
// exact shape prod runs take (observed on run 019f847b, zero devbox
// events across the whole run):
//
//   - the bot is resolved from the catalog and attached via WithBundle
//     (bundle.OpenDir on the bot's directory — Dir set, devbox.json
//     beside main.bot), with NO WithFilePath (the queue message carries
//     no workflow path);
//   - the workflow declares an inline sandbox block (the app-dev shape);
//   - ITERION_SANDBOX_OVERRIDE=none (the chart's posture: the runner pod
//     IS the isolation boundary) neutralizes that block, so NO sandbox
//     ever starts.
//
// The devbox feature has now been broken twice by logic asserted
// downstream of a precondition that is never true on the real path (the
// bundle at first, an active sandbox now) — so this test asserts the
// PRECONDITION: on a run where no sandbox starts, provisioning must
// still fire from the engine's own path, install the bot's staged
// config, expose the profile bin dir on the run's PATH, and emit the
// observable event.
func TestEngineRun_CloudShape_ProvisionsBotDevboxWithoutSandbox(t *testing.T) {
	rec := stubHostDevbox(t, nil)
	b, botDir := writeBotBundleDir(t, true)

	s := tmpStore(t)
	execRec := &envRecordingExecutor{stubExecutor: newStubExecutor()}
	workDir := t.TempDir() // target workspace without a devbox.json of its own

	eng := New(devboxTestWorkflow(), s, execRec,
		WithWorkDir(workDir),
		WithBundle(b),
		WithSandboxOverride("none"), // the cloud runner's ITERION_SANDBOX_OVERRIDE
		WithLogger(iterlog.Nop()),
	)

	runID := "run-devbox-cloud-shape"
	if err := eng.Run(context.Background(), runID, nil); err != nil {
		t.Fatalf("engine.Run: %v", err)
	}

	// Install fired, against a STAGED copy (the bundle dir is read-only
	// on a runner image), carrying both the config and the lock.
	if len(rec.installs) != 1 {
		t.Fatalf("devbox installs = %v, want exactly one (the staged bot project)", rec.installs)
	}
	staged := rec.installs[0]
	if staged == botDir {
		t.Errorf("install targeted the bundle dir %s directly, want a staged writable copy", botDir)
	}
	if !strings.Contains(staged, runID) {
		t.Errorf("staged dir %s does not embed the run id — concurrent runs in one pod would collide", staged)
	}
	if len(rec.stagedConfigs) != 1 {
		t.Errorf("staged dir carried no %s at install time (stagedConfigs=%v)", devboxConfigName, rec.stagedConfigs)
	}
	if len(rec.stagedLocks) != 1 {
		t.Errorf("staged dir carried no %s at install time (stagedLocks=%v)", devboxLockName, rec.stagedLocks)
	}

	// The profile bin dir reached the executor's run-level env: this is
	// what puts the bot's tools on PATH for every node.
	if execRec.setCalls != 1 {
		t.Fatalf("SetRunExtraEnv calls = %d, want 1", execRec.setCalls)
	}
	wantBin := filepath.Join(staged, filepath.FromSlash(devboxProfileBin))
	if len(execRec.runExtraEnv) != 1 || !strings.HasPrefix(execRec.runExtraEnv[0], "PATH=") {
		t.Fatalf("runExtraEnv = %v, want a single PATH= entry", execRec.runExtraEnv)
	}
	if !strings.HasPrefix(execRec.runExtraEnv[0], "PATH="+wantBin+":") {
		t.Errorf("run PATH %q does not start with the bot profile bin %q", execRec.runExtraEnv[0], wantBin)
	}

	// The fact is observable: the event landed with the host target.
	data := devboxEvent(t, s, runID)
	if data == nil {
		t.Fatal("no sandbox_devbox_provisioned event emitted — the feature is invisible")
	}
	if got := data["target"]; got != "host" {
		t.Errorf("event target = %v, want \"host\"", got)
	}
	if got := stringsFromAny(data["sources"]); len(got) != 1 || got[0] != "bot" {
		t.Errorf("event sources = %v, want [bot]", got)
	}
	if _, hasErrs := data["errors"]; hasErrs {
		t.Errorf("event carries errors %v, want none", data["errors"])
	}

	// The per-run staging area is removed at run end.
	if _, err := os.Stat(filepath.Join(os.TempDir(), "iterion-devbox", runID)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("staging root survived run end (stat err=%v), want removed", err)
	}
}

// TestEngineRun_HostDevbox_RepoProjectInstallsInPlace covers the second
// devbox source on the no-sandbox path: the TARGET REPO's devbox.json at
// the workspace root installs in place (the workspace is writable and
// its config may reference sibling paths).
func TestEngineRun_HostDevbox_RepoProjectInstallsInPlace(t *testing.T) {
	rec := stubHostDevbox(t, nil)

	s := tmpStore(t)
	execRec := &envRecordingExecutor{stubExecutor: newStubExecutor()}
	workDir := writeDevboxConfig(t, t.TempDir(), "go@1.26")

	eng := New(devboxTestWorkflow(), s, execRec,
		WithWorkDir(workDir),
		WithSandboxOverride("none"),
		WithLogger(iterlog.Nop()),
	)

	runID := "run-devbox-repo-host"
	if err := eng.Run(context.Background(), runID, nil); err != nil {
		t.Fatalf("engine.Run: %v", err)
	}

	if len(rec.installs) != 1 || rec.installs[0] != workDir {
		t.Fatalf("devbox installs = %v, want exactly [%s] (in place)", rec.installs, workDir)
	}
	wantBin := filepath.Join(workDir, filepath.FromSlash(devboxProfileBin))
	if execRec.setCalls != 1 || len(execRec.runExtraEnv) != 1 ||
		!strings.HasPrefix(execRec.runExtraEnv[0], "PATH="+wantBin+":") {
		t.Fatalf("runExtraEnv = %v (setCalls=%d), want PATH starting with %s", execRec.runExtraEnv, execRec.setCalls, wantBin)
	}
	data := devboxEvent(t, s, runID)
	if data == nil {
		t.Fatal("no sandbox_devbox_provisioned event emitted")
	}
	if got := stringsFromAny(data["sources"]); len(got) != 1 || got[0] != "repo" {
		t.Errorf("event sources = %v, want [repo]", got)
	}
}

// TestEngineRun_HostDevbox_RepoBeforeBotOnPATH locks the composition
// order when BOTH sources exist: the repo's toolchain stays authoritative
// for building itself (its bin dir first), the bot's packages fill in the
// rest — the same precedence the sandbox path applies.
func TestEngineRun_HostDevbox_RepoBeforeBotOnPATH(t *testing.T) {
	rec := stubHostDevbox(t, nil)
	b, _ := writeBotBundleDir(t, false)

	s := tmpStore(t)
	execRec := &envRecordingExecutor{stubExecutor: newStubExecutor()}
	workDir := writeDevboxConfig(t, t.TempDir(), "go@1.26")

	eng := New(devboxTestWorkflow(), s, execRec,
		WithWorkDir(workDir),
		WithBundle(b),
		WithSandboxOverride("none"),
		WithLogger(iterlog.Nop()),
	)

	runID := "run-devbox-both-host"
	if err := eng.Run(context.Background(), runID, nil); err != nil {
		t.Fatalf("engine.Run: %v", err)
	}

	if len(rec.installs) != 2 {
		t.Fatalf("devbox installs = %v, want two (repo in place + staged bot)", rec.installs)
	}
	if rec.installs[0] != workDir {
		t.Errorf("first install = %s, want the repo workspace %s", rec.installs[0], workDir)
	}
	repoBin := filepath.Join(workDir, filepath.FromSlash(devboxProfileBin))
	botBin := filepath.Join(rec.installs[1], filepath.FromSlash(devboxProfileBin))
	if len(execRec.runExtraEnv) != 1 ||
		!strings.HasPrefix(execRec.runExtraEnv[0], "PATH="+repoBin+":"+botBin+":") {
		t.Fatalf("run PATH = %v, want %s before %s", execRec.runExtraEnv, repoBin, botBin)
	}
	data := devboxEvent(t, s, runID)
	if got := stringsFromAny(data["sources"]); len(got) != 2 || got[0] != "repo" || got[1] != "bot" {
		t.Errorf("event sources = %v, want [repo bot]", got)
	}
}

// TestEngineRun_HostDevbox_MissingBinaryIsLoudNotSilent covers the base
// runner image (no devbox binary): the run proceeds, nothing lands on
// PATH, and the gap is OBSERVABLE — the event carries the error naming
// the ignored config. A declared-but-unprovisionable toolchain must
// never read as an agent bug.
func TestEngineRun_HostDevbox_MissingBinaryIsLoudNotSilent(t *testing.T) {
	rec := stubHostDevbox(t, errors.New("exec: \"devbox\": executable file not found in $PATH"))
	b, _ := writeBotBundleDir(t, true)

	s := tmpStore(t)
	execRec := &envRecordingExecutor{stubExecutor: newStubExecutor()}

	eng := New(devboxTestWorkflow(), s, execRec,
		WithWorkDir(t.TempDir()),
		WithBundle(b),
		WithSandboxOverride("none"),
		WithLogger(iterlog.Nop()),
	)

	runID := "run-devbox-nobinary"
	if err := eng.Run(context.Background(), runID, nil); err != nil {
		t.Fatalf("engine.Run: %v", err)
	}

	if len(rec.installs) != 0 {
		t.Errorf("devbox installs = %v, want none (binary absent)", rec.installs)
	}
	if execRec.setCalls != 0 {
		t.Errorf("SetRunExtraEnv called %d times, want 0", execRec.setCalls)
	}
	data := devboxEvent(t, s, runID)
	if data == nil {
		t.Fatal("no sandbox_devbox_provisioned event emitted — the missing-binary gap is invisible")
	}
	errs := stringsFromAny(data["errors"])
	if len(errs) != 1 || !strings.Contains(errs[0], "devbox is not on PATH") {
		t.Errorf("event errors = %v, want one naming the missing devbox binary", errs)
	}
}

// TestEngineRun_HostDevbox_NoConfigIsNoOp keeps the opt-in contract: a
// run whose bot and workspace declare no devbox.json pays nothing — no
// install, no PATH change, no event.
func TestEngineRun_HostDevbox_NoConfigIsNoOp(t *testing.T) {
	rec := stubHostDevbox(t, nil)

	s := tmpStore(t)
	execRec := &envRecordingExecutor{stubExecutor: newStubExecutor()}

	eng := New(devboxTestWorkflow(), s, execRec,
		WithWorkDir(t.TempDir()),
		WithSandboxOverride("none"),
		WithLogger(iterlog.Nop()),
	)

	runID := "run-devbox-optout"
	if err := eng.Run(context.Background(), runID, nil); err != nil {
		t.Fatalf("engine.Run: %v", err)
	}

	if len(rec.installs) != 0 || execRec.setCalls != 0 {
		t.Errorf("installs=%v setCalls=%d, want no provisioning activity", rec.installs, execRec.setCalls)
	}
	if data := devboxEvent(t, s, runID); data != nil {
		t.Errorf("unexpected sandbox_devbox_provisioned event: %v", data)
	}
}

// TestEngineRun_HostDevbox_InstallFailureStillExposesPATHAndErrors locks
// the best-effort-but-loud contract: a failed install keeps the run
// alive, still exposes the bin dirs (successful siblings must load), and
// records the failure in the event.
func TestEngineRun_HostDevbox_InstallFailureStillExposesPATHAndErrors(t *testing.T) {
	rec := stubHostDevbox(t, nil)
	rec.installErr = errors.New("nix realise failed")
	b, _ := writeBotBundleDir(t, false)

	s := tmpStore(t)
	execRec := &envRecordingExecutor{stubExecutor: newStubExecutor()}

	eng := New(devboxTestWorkflow(), s, execRec,
		WithWorkDir(t.TempDir()),
		WithBundle(b),
		WithSandboxOverride("none"),
		WithLogger(iterlog.Nop()),
	)

	runID := "run-devbox-installfail"
	if err := eng.Run(context.Background(), runID, nil); err != nil {
		t.Fatalf("engine.Run: %v (a failed devbox install must not fail the run)", err)
	}

	if execRec.setCalls != 1 {
		t.Errorf("SetRunExtraEnv calls = %d, want 1 (PATH still exposed)", execRec.setCalls)
	}
	data := devboxEvent(t, s, runID)
	if data == nil {
		t.Fatal("no sandbox_devbox_provisioned event emitted")
	}
	errs := stringsFromAny(data["errors"])
	if len(errs) != 1 || !strings.Contains(errs[0], "install failed") {
		t.Errorf("event errors = %v, want one install failure", errs)
	}
}
