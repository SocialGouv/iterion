package runtime

import (
	"bytes"
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
	origLook, origRun, origEngineDir := hostDevboxLookPath, runHostDevboxInstall, hostEngineBinPath
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
	// Pin the engine-binary-dir seam empty by default so the devbox
	// PATH assertions of the existing tests stay independent of what
	// iterion sits at /usr/bin on the test host. Tests that assert
	// the #1384 prepend behaviour override this seam themselves.
	hostEngineBinPath = func() string { return "" }
	t.Cleanup(func() {
		hostDevboxLookPath, runHostDevboxInstall, hostEngineBinPath = origLook, origRun, origEngineDir
	})
	return rec
}

// envRecordingExecutor is the stub executor plus the two layers of the
// run-level env seam, kept apart exactly as ClawExecutor keeps them: the
// LAUNCH-SURFACE layer (SetRunExtraEnv — what runview / the dispatcher
// push before the engine starts; what the engine reads back through
// GetRunExtraEnvValue) and the ENGINE layer (SetEngineExtraEnv — what
// provisionHostDevbox composes and pushes; read back by nobody).
type envRecordingExecutor struct {
	*stubExecutor
	launchEnv []string // the launch surface's layer, merged by key
	engineEnv []string // the engine's layer, merged by key
	setCalls  int      // SetEngineExtraEnv calls
}

// SetRunExtraEnv is the launch surface's push: merged by key, the newest
// value wins, as on ClawExecutor.
func (e *envRecordingExecutor) SetRunExtraEnv(env []string) {
	e.launchEnv = mergeEnvByKey(e.launchEnv, env)
}

// SetEngineExtraEnv is the engine's push: merged by key into its own
// layer, never into the launch surface's.
func (e *envRecordingExecutor) SetEngineExtraEnv(env []string) {
	e.engineEnv = mergeEnvByKey(e.engineEnv, env)
	e.setCalls++
}

// GetRunExtraEnvValue reads the LAUNCH-SURFACE layer only, as
// ClawExecutor does: the engine composes on the base it was launched
// with, never on its own previous composition.
func (e *envRecordingExecutor) GetRunExtraEnvValue(key string) string {
	prefix := key + "="
	for _, entry := range e.launchEnv {
		if strings.HasPrefix(entry, prefix) {
			return entry[len(prefix):]
		}
	}
	return ""
}

// mergeEnvByKey overlays KEY=value entries by key, the overlay winning,
// in first-seen key order.
func mergeEnvByKey(base, overlay []string) []string {
	var order []string
	values := map[string]string{}
	for _, entry := range append(append([]string(nil), base...), overlay...) {
		key, _, ok := strings.Cut(entry, "=")
		if !ok || key == "" {
			continue
		}
		if _, seen := values[key]; !seen {
			order = append(order, key)
		}
		values[key] = entry
	}
	out := make([]string, 0, len(order))
	for _, key := range order {
		out = append(out, values[key])
	}
	return out
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
		t.Fatalf("SetEngineExtraEnv calls = %d, want 1", execRec.setCalls)
	}
	wantBin := filepath.Join(staged, filepath.FromSlash(devboxProfileBin))
	if len(execRec.engineEnv) != 1 || !strings.HasPrefix(execRec.engineEnv[0], "PATH=") {
		t.Fatalf("engineEnv = %v, want a single PATH= entry", execRec.engineEnv)
	}
	if !strings.HasPrefix(execRec.engineEnv[0], "PATH="+wantBin+":") {
		t.Errorf("run PATH %q does not start with the bot profile bin %q", execRec.engineEnv[0], wantBin)
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

	// The per-run staging area — a private directory directly under
	// os.TempDir(), never a predictable `iterion-devbox/<runID>` path a
	// foreign party could pre-create — is removed at run end.
	stageRoot := filepath.Dir(staged)
	if filepath.Dir(stageRoot) != filepath.Clean(os.TempDir()) {
		t.Errorf("staging root %q is not directly under the temp dir %q", stageRoot, os.TempDir())
	}
	if predictable := filepath.Join(os.TempDir(), "iterion-devbox") + string(os.PathSeparator); strings.HasPrefix(staged, predictable) {
		t.Errorf("staged dir %q sits under the predictable parent %q", staged, predictable)
	}
	if _, err := os.Stat(stageRoot); !errors.Is(err, os.ErrNotExist) {
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
	if execRec.setCalls != 1 || len(execRec.engineEnv) != 1 ||
		!strings.HasPrefix(execRec.engineEnv[0], "PATH="+wantBin+":") {
		t.Fatalf("engineEnv = %v (setCalls=%d), want PATH starting with %s", execRec.engineEnv, execRec.setCalls, wantBin)
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
	if len(execRec.engineEnv) != 1 ||
		!strings.HasPrefix(execRec.engineEnv[0], "PATH="+repoBin+":"+botBin+":") {
		t.Fatalf("run PATH = %v, want %s before %s", execRec.engineEnv, repoBin, botBin)
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
		t.Errorf("SetEngineExtraEnv calls = %d, want 1 (PATH still exposed)", execRec.setCalls)
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

// TestEngineRun_HostDevbox_EngineBinDirPrependsPATH_Issue1384 pins the
// promise of #1384: a bot's shell tools (a tool node's command:, claw's
// diagnostic_shell, a claude_code Bash call) resolve `iterion` to THIS
// engine's binary, not to whatever `iterion` sits earlier on the
// operator's ambient PATH. The chokepoint is the run env the engine
// assembles from devbox_host.go's provisionHostDevbox — the same PATH
// entry every host-spawned command inherits (executor.engineEnv).
//
// The forbidden alternative in this test: a decoy `iterion` sitting
// EARLIER on the operator's PATH than the engine dir. The test asserts
// the engine's dir wins — i.e. PATH starts with it, ahead of both the
// bot's devbox profile bin AND the operator's decoy dir.
//
// Mutation: drop the engine-bin-dir prepend from applyRunPath (or set
// hostEngineBinPath to return "") and this reddens: PATH starts with
// the bot's devbox bin (no engine dir first).
func TestEngineRun_HostDevbox_EngineBinDirPrependsPATH_Issue1384(t *testing.T) {
	rec := stubHostDevbox(t, nil)
	b, _ := writeBotBundleDir(t, false)

	// A stub engine binary at a controlled path — the analog of THIS
	// engine's iterion. It sits in a dir that ALSO holds decoys of
	// devbox-provisioned tools (node, git). The fix must route the
	// prepend through a SHIM DIR holding only `iterion` so the
	// engine dir does not shadow node/git — that would reintroduce
	// #1384 one level up (finding Rff1076).
	engineOwnerDir := t.TempDir()
	engineBin := filepath.Join(engineOwnerDir, "iterion")
	if err := os.WriteFile(engineBin, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	// The forbidden alternative: decoys of common devbox tools next
	// to iterion. If the whole engineOwnerDir was prepended, these
	// would win over the devbox-provisioned versions.
	for _, name := range []string{"node", "git"} {
		if err := os.WriteFile(filepath.Join(engineOwnerDir, name), []byte("#!/bin/sh\necho engine-decoy\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	origEngineBin := hostEngineBinPath
	hostEngineBinPath = func() string { return engineBin }
	t.Cleanup(func() { hostEngineBinPath = origEngineBin })

	// A decoy `iterion` earlier on the operator's ambient PATH — the
	// wave-4 dogfood shape where ~/.local/bin/iterion (v3.112.2)
	// shadowed the branch build's iterion. This test asserts the
	// engine shim wins over any such decoy.
	decoyDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(decoyDir, "iterion"), []byte("#!/bin/sh\necho decoy\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", decoyDir+":/usr/bin")

	s := tmpStore(t)
	execRec := &envRecordingExecutor{stubExecutor: newStubExecutor()}

	eng := New(devboxTestWorkflow(), s, execRec,
		WithWorkDir(t.TempDir()),
		WithBundle(b),
		WithSandboxOverride("none"),
		WithLogger(iterlog.Nop()),
	)

	runID := "run-devbox-1384"
	if err := eng.Run(context.Background(), runID, nil); err != nil {
		t.Fatalf("engine.Run: %v", err)
	}

	if len(rec.installs) != 1 {
		t.Fatalf("devbox installs = %v, want the bot install", rec.installs)
	}
	if execRec.setCalls != 1 || len(execRec.engineEnv) != 1 {
		t.Fatalf("engineEnv = %v (setCalls=%d), want exactly one PATH entry",
			execRec.engineEnv, execRec.setCalls)
	}
	path := execRec.engineEnv[0]

	// The engine owner dir MUST NOT appear on PATH — a whole-dir
	// prepend would put node/git decoys next to iterion ahead of
	// the devbox toolchain (Rff1076).
	if strings.Contains(path, engineOwnerDir) {
		t.Fatalf("engine owner dir %q leaked into PATH — the whole-dir prepend regressed Rff1076\n  PATH: %s", engineOwnerDir, path)
	}

	// Instead, the FIRST entry is this run's shim: a private directory
	// directly under os.TempDir(), named after the run.
	shimDir := engineShimDirOf(t, path, runID)
	// The shim dir must be a per-run subdir, distinct from
	// engineOwnerDir. This is what scopes the prepend to ONE binary
	// and prevents Rff1076. (The shim's file layout is asserted in
	// TestStageEngineShim_HoldsOnlyIterionSymlink.)
	if shimDir == engineOwnerDir {
		t.Fatalf("shim path collapsed to engineOwnerDir — the shim is missing")
	}

	// The bot's devbox bin is SECOND — the previous contract held.
	wantBotBin := filepath.Join(rec.installs[0], filepath.FromSlash(devboxProfileBin))
	wantOrder := "PATH=" + shimDir + ":" + wantBotBin + ":"
	if !strings.HasPrefix(path, wantOrder) {
		t.Fatalf("engine shim dir must lead the bot devbox bin\n  got:  %s\n  want prefix: %s", path, wantOrder)
	}
	// And the decoy comes AFTER both — a `command -v iterion` inside
	// the bot resolves to the engine's stub, not the decoy.
	if !strings.Contains(path, ":"+decoyDir) {
		t.Errorf("decoy should still appear (from the inherited PATH), but not before the engine shim: %s", path)
	}
	shimIdx := strings.Index(path, shimDir)
	decoyIdx := strings.Index(path, decoyDir)
	if shimIdx == -1 || decoyIdx == -1 || shimIdx >= decoyIdx {
		t.Errorf("shim dir must appear BEFORE the decoy in PATH (shimIdx=%d decoyIdx=%d)", shimIdx, decoyIdx)
	}
}

// TestEngineRun_HostDevbox_EngineBinDirFiresEvenWithoutDevbox pins the
// second half of #1384's promise: a run with NO devbox source still
// gets the engine binary dir prepended to PATH — the fix is not
// gated on devbox provisioning, because the operator-vs-engine
// `iterion` mismatch has nothing to do with devbox.
func TestEngineRun_HostDevbox_EngineBinDirFiresEvenWithoutDevbox(t *testing.T) {
	// No devbox stubbing needed: with no devbox.json anywhere the
	// devbox lookup never fires. What we assert is that PATH still
	// gets pushed, carrying the engine binary shim.
	engineOwnerDir := t.TempDir()
	engineBin := filepath.Join(engineOwnerDir, "iterion")
	if err := os.WriteFile(engineBin, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	origEngineBin := hostEngineBinPath
	hostEngineBinPath = func() string { return engineBin }
	t.Cleanup(func() { hostEngineBinPath = origEngineBin })

	s := tmpStore(t)
	execRec := &envRecordingExecutor{stubExecutor: newStubExecutor()}

	eng := New(devboxTestWorkflow(), s, execRec,
		WithWorkDir(t.TempDir()),
		WithSandboxOverride("none"),
		WithLogger(iterlog.Nop()),
	)

	runID := "run-devbox-1384-nodevbox"
	if err := eng.Run(context.Background(), runID, nil); err != nil {
		t.Fatalf("engine.Run: %v", err)
	}

	if execRec.setCalls != 1 || len(execRec.engineEnv) != 1 {
		t.Fatalf("engineEnv = %v (setCalls=%d), want exactly one PATH entry — the fix must fire without devbox",
			execRec.engineEnv, execRec.setCalls)
	}
	_ = engineShimDirOf(t, execRec.engineEnv[0], runID)
}

// TestEngineRun_HostDevbox_PreservesEarlierPATHOverride pins the M1
// property found by the #1384 adversarial round: an EARLIER writer's
// PATH override (e.g. projectenv.Snapshot pushed by
// pkg/runview/service_launch.go via `executor.SetRunExtraEnv(s.runEnv)`)
// MUST survive as the tail of the run's PATH after provisionHostDevbox
// prepends the engine shim + devbox bin dirs. `applyRunPath` reads the
// launch surface's PATH via GetRunExtraEnvValue instead of
// `os.Getenv("PATH")`, so a `.env` PATH addition (or any other earlier
// SetRunExtraEnv PATH push) is not silently discarded.
//
// Mutation: revert applyRunPath's tail source to `os.Getenv("PATH")`
// unconditionally (drop the getter branch) and this test reddens: the
// projectenv PATH is missing from the merged value.
func TestEngineRun_HostDevbox_PreservesEarlierPATHOverride(t *testing.T) {
	// The forbidden alternative: a projectenv PATH override that the
	// fix would silently discard. If we ever fall back to
	// os.Getenv("PATH") wholesale, this ordering flips.
	projectEnvPath := "/opt/project/bin:/opt/other-project/bin"

	engineOwnerDir := t.TempDir()
	engineBin := filepath.Join(engineOwnerDir, "iterion")
	if err := os.WriteFile(engineBin, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	origEngineBin := hostEngineBinPath
	hostEngineBinPath = func() string { return engineBin }
	t.Cleanup(func() { hostEngineBinPath = origEngineBin })

	// Ensure os.Getenv("PATH") is DIFFERENT from projectEnvPath so a
	// bug that falls back to os.Getenv wouldn't accidentally pick up
	// the same value.
	t.Setenv("PATH", "/some/host/bin")

	s := tmpStore(t)
	execRec := &envRecordingExecutor{stubExecutor: newStubExecutor()}
	// The launch surface's push, as runview.Service's
	// executor.SetRunExtraEnv(s.runEnv) makes it with a
	// projectenv-derived PATH.
	execRec.SetRunExtraEnv([]string{"PATH=" + projectEnvPath})

	eng := New(devboxTestWorkflow(), s, execRec,
		WithWorkDir(t.TempDir()),
		WithSandboxOverride("none"),
		WithLogger(iterlog.Nop()),
	)

	runID := "run-devbox-preserve-projectenv"
	if err := eng.Run(context.Background(), runID, nil); err != nil {
		t.Fatalf("engine.Run: %v", err)
	}

	if len(execRec.engineEnv) != 1 {
		t.Fatalf("engineEnv = %v, want exactly one PATH entry after provisioning", execRec.engineEnv)
	}
	got := execRec.engineEnv[0]
	_ = engineShimDirOf(t, got, runID)
	// The projectenv PATH must appear as the tail — the whole reason
	// applyRunPath reads GetRunExtraEnvValue instead of os.Getenv.
	if !strings.Contains(got, projectEnvPath) {
		t.Fatalf("projectenv PATH override was discarded (got %q, wanted the tail to contain %q)", got, projectEnvPath)
	}
	// And the os.Getenv PATH must NOT be picked up — the fix reads
	// through the executor seam when it's available.
	if strings.Contains(got, "/some/host/bin") {
		t.Fatalf("fallback os.Getenv PATH leaked in when projectenv was set (got %q)", got)
	}
}

// TestEngineRun_HostDevbox_NoDevbox_NonSetter_EmitsSkippedRepoEvent
// pins the answer to R (question) of #1487's revi verdict: a run with
// a DECLINED repo devbox source (repo_devbox=off) AND a non-setter
// executor must still emit the sandbox_devbox_provisioned skipped_repo
// event — moving the setter check above the no-devbox short circuit
// swallowed the event silently. The current code checks the setter
// AFTER the short circuit, so this case exits through the short
// circuit that still emits.
//
// Mutation: move the setter check above the no-devbox short circuit →
// the event is not emitted → red.
func TestEngineRun_HostDevbox_NoDevbox_NonSetter_EmitsSkippedRepoEvent(t *testing.T) {
	rec := stubHostDevbox(t, nil)
	_ = rec // no installs will fire but stubHostDevbox pins hostEngineBinPath empty
	// The workspace declares a devbox.json, but repo_devbox is off.
	// The bot declares none. The setter is NOT the ClawExecutor —
	// stubExecutor (the plain test stub) does not implement the seam.
	workDir := writeDevboxConfig(t, t.TempDir(), "go@1.26")

	s := tmpStore(t)
	// The non-setter executor: plain stubExecutor, no
	// SetRunExtraEnv method. envRecordingExecutor implements it, so
	// we use the bare stub here.
	baseStub := newStubExecutor()

	eng := New(devboxTestWorkflow(), s, baseStub,
		WithWorkDir(workDir),
		WithSandboxOverride("none"),
		WithLogger(iterlog.Nop()),
		WithRepoDevbox("off"), // decline the repo devbox
	)

	runID := "run-devbox-nonsetter-skippedrepo"
	if err := eng.Run(context.Background(), runID, nil); err != nil {
		t.Fatalf("engine.Run: %v", err)
	}
	// The event must land — it is the ONLY signal the operator
	// gets about the declined repo.
	data := devboxEvent(t, s, runID)
	if data == nil {
		t.Fatal("no sandbox_devbox_provisioned event emitted — the setter-check move regressed the declined-repo path")
	}
	if data["reason"] != "repo_devbox off" {
		t.Errorf("event reason = %v, want %q", data["reason"], "repo_devbox off")
	}
	if got := stringsFromAny(data["skipped_sources"]); len(got) != 1 || got[0] != "repo" {
		t.Errorf("event skipped_sources = %v, want [repo]", got)
	}
}

// TestEngineRun_HostDevbox_BotDevbox_RepoOff_NonSetter_EmitsSkippedRepoEvent
// closes the sibling of the "question" answer at the SECOND
// non-setter return path: a run with BOTH a declined repo devbox
// (skippedRepo != "") AND a bot devbox (botCfg != "") AND a non-setter
// executor was pre-existingly dropping the sandbox_devbox_provisioned
// skipped_repo event — the no-devbox short-circuit does not fire
// (botCfg is non-empty), the setter check returns without emitting.
// Round-2 adversarial found this as a pre-existing LOW; folded here.
//
// Mutation: drop the emit block inside the non-setter branch → the
// event is missing → red.
func TestEngineRun_HostDevbox_BotDevbox_RepoOff_NonSetter_EmitsSkippedRepoEvent(t *testing.T) {
	_ = stubHostDevbox(t, nil) // pins hostEngineBinPath empty; keeps devbox seams inert
	// Bot devbox present (bot bundle carries a devbox.json).
	b, _ := writeBotBundleDir(t, false)
	// Repo devbox declared but declined.
	workDir := writeDevboxConfig(t, t.TempDir(), "go@1.26")

	s := tmpStore(t)
	// Non-setter executor — the plain stubExecutor does not implement
	// SetRunExtraEnv.
	baseStub := newStubExecutor()

	eng := New(devboxTestWorkflow(), s, baseStub,
		WithWorkDir(workDir),
		WithBundle(b),
		WithSandboxOverride("none"),
		WithLogger(iterlog.Nop()),
		WithRepoDevbox("off"),
	)

	runID := "run-devbox-bot-repooff-nonsetter"
	if err := eng.Run(context.Background(), runID, nil); err != nil {
		t.Fatalf("engine.Run: %v", err)
	}
	data := devboxEvent(t, s, runID)
	if data == nil {
		t.Fatal("no sandbox_devbox_provisioned event emitted — the non-setter branch is losing the declined-repo signal")
	}
	if data["reason"] != "repo_devbox off" {
		t.Errorf("event reason = %v, want %q", data["reason"], "repo_devbox off")
	}
	if got := stringsFromAny(data["skipped_sources"]); len(got) != 1 || got[0] != "repo" {
		t.Errorf("event skipped_sources = %v, want [repo]", got)
	}
}

// TestStageEngineShim_HoldsOnlyIterionSymlink pins the round-2 fix
// for Rff1076: the per-run shim staged by provisionHostDevbox
// contains EXACTLY one entry (`iterion`), a symlink to the engine's
// binary path. A whole-dir prepend of LocateIterionBinary's containing
// dir would put every one of its neighbours (node/go/git/devbox) on
// the run's PATH ahead of the devbox-provisioned toolchain — the
// #1384-shape defect one level up.
//
// Mutation: replace the symlink with a copy of engineOwnerDir's
// contents → this reddens on both the entry count and the symlink
// target check.
func TestStageEngineShim_HoldsOnlyIterionSymlink(t *testing.T) {
	// The staging surface: an owner dir with iterion PLUS decoys of
	// other tools. If the shim shipped the whole dir, node/git here
	// would reach the run's PATH.
	engineOwnerDir := t.TempDir()
	engineBin := filepath.Join(engineOwnerDir, "iterion")
	if err := os.WriteFile(engineBin, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"node", "git", "python", "devbox"} {
		if err := os.WriteFile(filepath.Join(engineOwnerDir, name), []byte("#!/bin/sh\necho decoy\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	runID := "test-run-stage-shim"
	shimDir, cleanup, err := stageEngineShim(runID, engineBin)
	if err != nil {
		t.Fatalf("stageEngineShim: %v", err)
	}
	t.Cleanup(cleanup)

	// The shim must live under os.TempDir(), NOT next to the engine.
	if shimDir == engineOwnerDir || strings.HasPrefix(shimDir, engineOwnerDir) {
		t.Errorf("shim dir must not be a child of engineOwnerDir (got %q under %q)", shimDir, engineOwnerDir)
	}
	// Directly under os.TempDir() — no intermediate parent a foreign
	// party could own — and private to this user: it leads the PATH of
	// every host-spawned command of the run.
	if filepath.Dir(shimDir) != filepath.Clean(os.TempDir()) {
		t.Errorf("shim dir %q is not directly under the temp dir %q", shimDir, os.TempDir())
	}
	if fi, err := os.Stat(shimDir); err != nil {
		t.Fatalf("stat shim dir: %v", err)
	} else if fi.Mode().Perm() != 0o700 {
		t.Errorf("shim dir mode = %o, want 0700", fi.Mode().Perm())
	}

	// Exactly one entry: iterion.
	entries, err := os.ReadDir(shimDir)
	if err != nil {
		t.Fatalf("ReadDir shim: %v", err)
	}
	if len(entries) != 1 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("shim dir must contain exactly ONE entry, got %d: %v (a whole-dir prepend would reintroduce Rff1076)", len(entries), names)
	}
	if entries[0].Name() != "iterion" {
		t.Errorf("shim entry name = %q, want %q", entries[0].Name(), "iterion")
	}
	// Symlink pointing at engineBin — not a copy, so the shim never
	// drifts against the running binary.
	target, err := os.Readlink(filepath.Join(shimDir, "iterion"))
	if err != nil {
		t.Fatalf("shim iterion must be a symlink: %v", err)
	}
	if target != engineBin {
		t.Errorf("shim symlink target = %q, want %q", target, engineBin)
	}

	// Cleanup removes the whole shim dir.
	cleanup()
	if _, err := os.Stat(shimDir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("cleanup did not remove shim dir (stat err=%v)", err)
	}
}

// engineShimDirOf returns the first entry of a `PATH=…` value after
// asserting it is this run's engine shim: a private directory DIRECTLY
// under os.TempDir() — no fixed intermediate parent another local user
// could own — named after the run.
func engineShimDirOf(t *testing.T, pathEnv, runID string) string {
	t.Helper()
	first, _, _ := strings.Cut(strings.TrimPrefix(pathEnv, "PATH="), ":")
	if filepath.Dir(first) != filepath.Clean(os.TempDir()) {
		t.Fatalf("engine shim %q is not DIRECTLY under the temp dir %q — an intermediate parent is a foreign party's to own\n  PATH: %s", first, os.TempDir(), pathEnv)
	}
	if want := "iterion-engine-shim-" + runID + "-"; !strings.HasPrefix(filepath.Base(first), want) {
		t.Fatalf("engine shim %q is not named after the run (want base prefix %q)\n  PATH: %s", first, want, pathEnv)
	}
	return first
}

// stubEngineBinary writes a stand-in for THIS engine's iterion binary in
// a fresh dir and pins the hostEngineBinPath seam to it for the test.
func stubEngineBinary(t *testing.T) string {
	t.Helper()
	engineBin := filepath.Join(t.TempDir(), "iterion")
	if err := os.WriteFile(engineBin, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	orig := hostEngineBinPath
	hostEngineBinPath = func() string { return engineBin }
	t.Cleanup(func() { hostEngineBinPath = orig })
	return engineBin
}

// TestStageEngineShim_NeverLandsInAForeignPreCreatedDir stages the shim
// while a foreign party owns the predictable location a fixed-parent
// scheme would use — `$TMPDIR/iterion-engine-shim/<runID>` as a real
// directory carrying an `iterion` of theirs, then that parent SYMLINKED
// into a directory of theirs. The shim must land directly under the temp
// dir, under a name of its own, and write nothing into (nor remove
// anything from) the foreign party's directories: what leads the run's
// PATH is a directory only this run created.
//
// Mutation seen red: stage under `filepath.Join(os.TempDir(),
// "iterion-engine-shim", runID)` with RemoveAll + MkdirAll — the shim
// wipes and reuses the foreign directory, or lands inside the symlinked
// target.
func TestStageEngineShim_NeverLandsInAForeignPreCreatedDir(t *testing.T) {
	engineBin := stubEngineBinary(t)
	for _, shape := range []string{"foreign directory", "symlinked parent"} {
		t.Run(shape, func(t *testing.T) {
			// A private temp root: the foreign party acts inside it.
			t.Setenv("TMPDIR", t.TempDir())
			tmp := filepath.Clean(os.TempDir())
			runID := "run-foreign-shim"
			parent := filepath.Join(tmp, "iterion-engine-shim")
			foreignDir := filepath.Join(parent, runID)
			theirs := filepath.Join(t.TempDir(), "theirs")
			if err := os.MkdirAll(theirs, 0o755); err != nil {
				t.Fatal(err)
			}
			switch shape {
			case "foreign directory":
				if err := os.MkdirAll(foreignDir, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(foreignDir, "iterion"), []byte("#!/bin/sh\necho foreign\n"), 0o755); err != nil {
					t.Fatal(err)
				}
			case "symlinked parent":
				if err := os.Symlink(theirs, parent); err != nil {
					t.Fatal(err)
				}
			}

			shimDir, cleanup, err := stageEngineShim(runID, engineBin)
			if err != nil {
				t.Fatalf("stageEngineShim: %v", err)
			}
			t.Cleanup(cleanup)

			if filepath.Dir(shimDir) != tmp {
				t.Errorf("shim dir %q is not directly under the temp dir %q", shimDir, tmp)
			}
			if shimDir == foreignDir || strings.HasPrefix(shimDir, parent+string(os.PathSeparator)) {
				t.Errorf("shim dir %q landed under the foreign party's predictable parent %q", shimDir, parent)
			}
			if fi, err := os.Stat(shimDir); err != nil {
				t.Fatalf("stat shim dir: %v", err)
			} else if fi.Mode().Perm() != 0o700 {
				t.Errorf("shim dir mode = %o, want 0700", fi.Mode().Perm())
			}
			if target, err := os.Readlink(filepath.Join(shimDir, "iterion")); err != nil || target != engineBin {
				t.Errorf("shim iterion -> %q (err=%v), want %q", target, err, engineBin)
			}
			// The foreign party's directories are untouched: nothing was
			// written there, nothing of theirs was removed.
			switch shape {
			case "foreign directory":
				if got, err := os.ReadFile(filepath.Join(foreignDir, "iterion")); err != nil || !strings.Contains(string(got), "foreign") {
					t.Errorf("the foreign directory's iterion was touched (content %q, err=%v)", got, err)
				}
			case "symlinked parent":
				if entries, err := os.ReadDir(theirs); err != nil || len(entries) != 0 {
					t.Errorf("the foreign party's symlink target received entries %v (err=%v), want none", entries, err)
				}
			}
		})
	}
}

// TestEngineRun_HostDevbox_StagedBotProjectNeverLandsInAForeignPreCreatedDir
// is the same guarantee for the other per-run directory provisioning
// creates: the staged copy of the bot's devbox.json, which `devbox
// install` runs against and whose profile bin dir goes on the run's PATH.
// A foreign party pre-creates `$TMPDIR/iterion-devbox/<runID>/bot` with a
// devbox.json of theirs; the run must stage elsewhere — directly under
// the temp dir, in a directory of its own — and leave theirs untouched.
//
// Mutation seen red: stage under `filepath.Join(os.TempDir(),
// "iterion-devbox", runID)` — the install runs in the foreign directory
// and its devbox.json is overwritten.
func TestEngineRun_HostDevbox_StagedBotProjectNeverLandsInAForeignPreCreatedDir(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	tmp := filepath.Clean(os.TempDir())
	rec := stubHostDevbox(t, nil)
	b, _ := writeBotBundleDir(t, false)
	runID := "run-foreign-devbox-stage"

	foreignBot := filepath.Join(tmp, "iterion-devbox", runID, "bot")
	if err := os.MkdirAll(foreignBot, 0o755); err != nil {
		t.Fatal(err)
	}
	const theirs = `{"packages":["theirs"]}`
	if err := os.WriteFile(filepath.Join(foreignBot, devboxConfigName), []byte(theirs), 0o644); err != nil {
		t.Fatal(err)
	}

	s := tmpStore(t)
	execRec := &envRecordingExecutor{stubExecutor: newStubExecutor()}
	eng := New(devboxTestWorkflow(), s, execRec,
		WithWorkDir(t.TempDir()),
		WithBundle(b),
		WithSandboxOverride("none"),
		WithLogger(iterlog.Nop()),
	)
	if err := eng.Run(context.Background(), runID, nil); err != nil {
		t.Fatalf("engine.Run: %v", err)
	}
	if len(rec.installs) != 1 {
		t.Fatalf("devbox installs = %v, want the staged bot project", rec.installs)
	}
	staged := rec.installs[0]
	if staged == foreignBot || strings.HasPrefix(staged, filepath.Join(tmp, "iterion-devbox")+string(os.PathSeparator)) {
		t.Fatalf("the bot project was staged in the foreign party's predictable directory %q", staged)
	}
	if stageRoot := filepath.Dir(staged); filepath.Dir(stageRoot) != tmp {
		t.Errorf("staging root %q is not directly under the temp dir %q", stageRoot, tmp)
	}
	if got, err := os.ReadFile(filepath.Join(foreignBot, devboxConfigName)); err != nil || string(got) != theirs {
		t.Errorf("the foreign party's devbox.json was touched (content %q, err=%v)", got, err)
	}
}

// TestEngineRun_HostDevbox_SecondProvisioningOfOneExecutorComposesFromTheLaunchLayer
// provisions ONE executor twice — two engines, two runs, the executor
// shared, the shape a future surface reusing an executor would have —
// and requires each composition to be exactly `<this run's shim>:<the
// launch surface's PATH>`: the engine pushes its PATH into the executor's
// engine layer and composes on the launch-surface layer it reads back,
// so nothing of the first run's composition (its removed shim) is in
// front of the second's.
//
// Mutation seen red: push the composed PATH through SetRunExtraEnv (the
// launch-surface layer) instead of SetEngineExtraEnv — the second run
// then composes `<shim2>:<shim1>:<launch PATH>`.
func TestEngineRun_HostDevbox_SecondProvisioningOfOneExecutorComposesFromTheLaunchLayer(t *testing.T) {
	_ = stubHostDevbox(t, nil)
	_ = stubEngineBinary(t)
	// The process PATH differs from the launch surface's so a
	// composition falling back to os.Getenv would show.
	t.Setenv("PATH", "/some/host/bin")

	s := tmpStore(t)
	execRec := &envRecordingExecutor{stubExecutor: newStubExecutor()}
	// The launch surface's push, made once when the executor is built.
	execRec.SetRunExtraEnv([]string{"PATH=/opt/project/bin"})

	var shims []string
	for _, runID := range []string{"run-shared-executor-first", "run-shared-executor-second"} {
		eng := New(devboxTestWorkflow(), s, execRec,
			WithWorkDir(t.TempDir()),
			WithSandboxOverride("none"),
			WithLogger(iterlog.Nop()),
		)
		if err := eng.Run(context.Background(), runID, nil); err != nil {
			t.Fatalf("engine.Run(%s): %v", runID, err)
		}
		if len(execRec.engineEnv) != 1 {
			t.Fatalf("engineEnv = %v after %s, want exactly one PATH entry", execRec.engineEnv, runID)
		}
		shim := engineShimDirOf(t, execRec.engineEnv[0], runID)
		if got, want := execRec.engineEnv[0], "PATH="+shim+":/opt/project/bin"; got != want {
			t.Fatalf("%s composed %q, want exactly %q — this run's shim, then the launch surface's PATH, nothing of a previous composition", runID, got, want)
		}
		shims = append(shims, shim)
	}
	if shims[0] == shims[1] {
		t.Errorf("both runs staged the same shim dir %q, want one per run", shims[0])
	}
	if got := execRec.GetRunExtraEnvValue("PATH"); got != "/opt/project/bin" {
		t.Errorf("the launch surface's PATH reads back as %q after two provisionings, want it untouched", got)
	}
}

// TestRunScopedTempDir_KeepsARunIDInsideTheTempDir: whatever an explicit
// --run-id carries — separators, `..`, spaces, non-ASCII, an unbounded
// length — the per-run directory is a single private entry directly
// under os.TempDir().

// TestRunScopedTempDir_RelativeTMPDIRComesBackAbsolute: a relative TMPDIR
// must not put a cwd-relative entry at the head of the run's PATH — tool
// nodes run with cmd.Dir set to the run's workDir, where the relative
// entry resolves nowhere and the shim (and the devbox profile dirs)
// silently drop.
//
// Mutation seen red: drop runScopedTempDir's filepath.Abs tail.
func TestRunScopedTempDir_RelativeTMPDIRComesBackAbsolute(t *testing.T) {
	base := t.TempDir()
	if err := os.MkdirAll(filepath.Join(base, "rel-scratch"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(base)
	t.Setenv("TMPDIR", "rel-scratch") // relative, EXISTS: MkdirTemp's job

	dir, err := runScopedTempDir("iterion-probe", "run-rel")
	if err != nil {
		t.Fatalf("runScopedTempDir: %v", err)
	}
	if !filepath.IsAbs(dir) {
		t.Fatalf("runScopedTempDir with a relative TMPDIR = %q, want an absolute path", dir)
	}
	if want := filepath.Join(base, "rel-scratch"); filepath.Dir(dir) != want {
		t.Errorf("runScopedTempDir = %q, want it under %q", dir, want)
	}
	_ = os.RemoveAll(dir)
}
func TestRunScopedTempDir_KeepsARunIDInsideTheTempDir(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	tmp := filepath.Clean(os.TempDir())
	for _, runID := range []string{"../../escape", "a/b/c", "", strings.Repeat("x", 500), "id with spaces & ünïcode"} {
		dir, err := runScopedTempDir("iterion-probe", runID)
		if err != nil {
			t.Fatalf("runScopedTempDir(%q): %v", runID, err)
		}
		if filepath.Dir(dir) != tmp {
			t.Errorf("runScopedTempDir(%q) = %q, not directly under %q", runID, dir, tmp)
		}
		if fi, err := os.Stat(dir); err != nil || fi.Mode().Perm() != 0o700 {
			t.Errorf("runScopedTempDir(%q) = %q: mode %v (err=%v), want 0700", runID, dir, fi, err)
		}
		_ = os.RemoveAll(dir)
	}
	if got, want := tempNameHint("../../escape"), ".._.._escape"; got != want {
		t.Errorf("tempNameHint(../../escape) = %q, want %q", got, want)
	}
	if got := tempNameHint(strings.Repeat("y", 500)); len(got) != 64 {
		t.Errorf("tempNameHint(500 bytes) has %d bytes, want 64", len(got))
	}
}

// TestStageEngineShim_DanglingTargetFailsLoudAndLeavesNothing: a shim
// whose `iterion` resolves to nothing (the engine binary path went stale
// between the locate and the link, or was never absolute) would silently
// send every bot shell to the ambient PATH's iterion — the #1384
// disagreement — with the staging itself reporting success. The shim
// refuses to exist in that shape: loud error, nothing left on disk.
//
// Mutation seen red: drop the post-symlink Stat belt in stageEngineShim —
// staging returns nil and the dangling directory stays behind.
func TestStageEngineShim_DanglingTargetFailsLoudAndLeavesNothing(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	shimDir, cleanup, err := stageEngineShim("run-dangling", "/nonexistent/iterion-gone")
	if err == nil {
		t.Fatalf("stageEngineShim succeeded with a dangling target %q, want a loud error", shimDir)
	}
	if cleanup != nil {
		cleanup()
	}
	left, _ := filepath.Glob(filepath.Join(filepath.Clean(os.TempDir()), "iterion-engine-shim-run-dangling-*"))
	if len(left) != 0 {
		t.Errorf("a dangling shim was left behind: %v", left)
	}
}

// TestEngineRun_HostDevbox_MissingDevboxEventStillReportsTheRunPath: a
// shim-only provisioning (devbox missing on the host, the engine shim
// still pushed) changes the run's PATH, and the sandbox_devbox_provisioned
// event is the operator's record of what commands will resolve against —
// it carries `path` even when no devbox bin dir made it.
//
// Mutation seen red: revert emitOutcome's `payload["path"]` to the
// len(binDirs) > 0 branch — the shim-only event loses `path`.
func TestEngineRun_HostDevbox_MissingDevboxEventStillReportsTheRunPath(t *testing.T) {
	_ = stubHostDevbox(t, errors.New(`exec: "devbox": executable file not found in $PATH`))
	_ = stubEngineBinary(t)
	workDir := writeDevboxConfig(t, t.TempDir(), "go@1.26")

	s := tmpStore(t)
	execRec := &envRecordingExecutor{stubExecutor: newStubExecutor()}
	eng := New(devboxTestWorkflow(), s, execRec,
		WithWorkDir(workDir),
		WithSandboxOverride("none"),
		WithLogger(iterlog.Nop()),
	)
	runID := "run-shim-only-event"
	if err := eng.Run(context.Background(), runID, nil); err != nil {
		t.Fatalf("engine.Run: %v", err)
	}
	data := devboxEvent(t, s, runID)
	if data == nil {
		t.Fatal("no sandbox_devbox_provisioned event emitted")
	}
	if got := stringsFromAny(data["errors"]); len(got) == 0 {
		t.Errorf("event carries no errors, want the missing-devbox failure recorded")
	}
	path, _ := data["path"].(string)
	if path == "" {
		t.Fatalf("shim-only event carries no path — the operator cannot see what the run's commands will resolve against (data: %v)", data)
	}
	_ = engineShimDirOf(t, "PATH="+path, runID)
}

// TestEngineRun_HostDevbox_UnlocatableEngineBinaryWarns: a run whose
// engine binary cannot be located loses the `iterion` shim silently on
// every host path — quiet only where no install is expected (a volatile
// `go run`/test binary). Anywhere else the run says why, once, at warn.
//
// Mutation seen red: drop the engineBin == "" warn block in
// provisionHostDevbox — the log stays empty.
func TestEngineRun_HostDevbox_UnlocatableEngineBinaryWarns(t *testing.T) {
	_ = stubHostDevbox(t, nil) // pins hostEngineBinPath to ""
	origVol := hostEngineVolatile
	hostEngineVolatile = func() bool { return false }
	t.Cleanup(func() { hostEngineVolatile = origVol })

	var buf bytes.Buffer
	s := tmpStore(t)
	execRec := &envRecordingExecutor{stubExecutor: newStubExecutor()}
	eng := New(devboxTestWorkflow(), s, execRec,
		WithWorkDir(t.TempDir()),
		WithSandboxOverride("none"),
		WithLogger(iterlog.New(iterlog.LevelWarn, &buf)),
	)
	if err := eng.Run(context.Background(), "run-no-engine-warn", nil); err != nil {
		t.Fatalf("engine.Run: %v", err)
	}
	if !strings.Contains(buf.String(), "no iterion binary could be located") {
		t.Fatalf("no warn about the unlocatable engine binary; log:\n%s", buf.String())
	}
}

// TestEngineRun_HostDevbox_DeclinedRepoSetterEventCarriesTheRunPath: the
// no-devbox short circuit's setter arm pushes the engine shim and emits
// the declined-repo event — that event is the operator's record of the
// run's PATH too, and carries `path` like every other provisioning event.
//
// Mutation seen red: drop the arm's `payload["path"] = path` — the event
// loses `path`.
func TestEngineRun_HostDevbox_DeclinedRepoSetterEventCarriesTheRunPath(t *testing.T) {
	_ = stubHostDevbox(t, nil)
	_ = stubEngineBinary(t)
	workDir := writeDevboxConfig(t, t.TempDir(), "go@1.26")

	s := tmpStore(t)
	execRec := &envRecordingExecutor{stubExecutor: newStubExecutor()}
	wf := devboxTestWorkflow()
	wf.RepoDevbox = "off"
	eng := New(wf, s, execRec,
		WithWorkDir(workDir),
		WithSandboxOverride("none"),
		WithLogger(iterlog.Nop()),
	)
	if err := eng.Run(context.Background(), "run-declined-repo-setter-path", nil); err != nil {
		t.Fatalf("engine.Run: %v", err)
	}
	data := devboxEvent(t, s, "run-declined-repo-setter-path")
	if data == nil {
		t.Fatal("no sandbox_devbox_provisioned event emitted")
	}
	if got := stringsFromAny(data["skipped_sources"]); len(got) != 1 || got[0] != "repo" {
		t.Fatalf("skipped_sources = %v, want [repo]", got)
	}
	path, _ := data["path"].(string)
	if path == "" {
		t.Fatalf("the declined-repo event carries no path although the shim PATH was pushed (data: %v)", data)
	}
	_ = engineShimDirOf(t, "PATH="+path, "run-declined-repo-setter-path")
}
