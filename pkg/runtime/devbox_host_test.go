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

// GetRunExtraEnvValue matches the ClawExecutor seam so devbox_host's
// PATH prepend can read an earlier writer's PATH override without
// clobbering it (M1 of #1384). The stub returns the value stored under
// runExtraEnv, empty when absent.
func (e *envRecordingExecutor) GetRunExtraEnvValue(key string) string {
	prefix := key + "="
	for _, entry := range e.runExtraEnv {
		if strings.HasPrefix(entry, prefix) {
			return entry[len(prefix):]
		}
	}
	return ""
}

// setEnvRecordingRunExtraEnv seeds runExtraEnv to the given entries so
// tests can simulate an earlier writer's SetRunExtraEnv call (e.g.
// runview.Service pushing projectenv). The devbox provisioning that
// follows must PRESERVE these values as the tail of PATH.
func (e *envRecordingExecutor) setEnvRecordingRunExtraEnv(entries []string) {
	e.runExtraEnv = entries
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

// TestEngineRun_HostDevbox_EngineBinDirPrependsPATH_Issue1384 pins the
// promise of #1384: a bot's shell tools (a tool node's command:, claw's
// diagnostic_shell, a claude_code Bash call) resolve `iterion` to THIS
// engine's binary, not to whatever `iterion` sits earlier on the
// operator's ambient PATH. The chokepoint is the run env the engine
// assembles from devbox_host.go's provisionHostDevbox — the same PATH
// entry every host-spawned command inherits (executor.runExtraEnv).
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
	if execRec.setCalls != 1 || len(execRec.runExtraEnv) != 1 {
		t.Fatalf("runExtraEnv = %v (setCalls=%d), want exactly one PATH entry",
			execRec.runExtraEnv, execRec.setCalls)
	}
	path := execRec.runExtraEnv[0]

	// The engine owner dir MUST NOT appear on PATH — a whole-dir
	// prepend would put node/git decoys next to iterion ahead of
	// the devbox toolchain (Rff1076).
	if strings.Contains(path, engineOwnerDir) {
		t.Fatalf("engine owner dir %q leaked into PATH — the whole-dir prepend regressed Rff1076\n  PATH: %s", engineOwnerDir, path)
	}

	// Instead, the FIRST entry is a per-run shim dir under
	// os.TempDir()/iterion-engine-shim/<runID>.
	shimDir := filepath.Join(os.TempDir(), "iterion-engine-shim", runID)
	wantPrefix := "PATH=" + shimDir + ":"
	if !strings.HasPrefix(path, wantPrefix) {
		t.Fatalf("PATH does not lead with the engine shim dir\n  got:  %s\n  want prefix: %s", path, wantPrefix)
	}
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

	if execRec.setCalls != 1 || len(execRec.runExtraEnv) != 1 {
		t.Fatalf("runExtraEnv = %v (setCalls=%d), want exactly one PATH entry — the fix must fire without devbox",
			execRec.runExtraEnv, execRec.setCalls)
	}
	shimDir := filepath.Join(os.TempDir(), "iterion-engine-shim", runID)
	if !strings.HasPrefix(execRec.runExtraEnv[0], "PATH="+shimDir+":") {
		t.Fatalf("PATH does not lead with the engine shim dir: %s (want prefix PATH=%s:)",
			execRec.runExtraEnv[0], shimDir)
	}
}

// TestEngineRun_HostDevbox_PreservesEarlierPATHOverride pins the M1
// property found by the #1384 adversarial round: an EARLIER writer's
// PATH override (e.g. projectenv.Snapshot pushed by
// pkg/runview/service_launch.go via `executor.SetRunExtraEnv(s.runEnv)`)
// MUST survive as the tail of the run's PATH after provisionHostDevbox
// prepends the engine binary dir + devbox bin dirs. `applyRunPath`
// reads the executor's current runExtraEnv via GetRunExtraEnvValue
// instead of `os.Getenv("PATH")`, so a `.env` PATH addition (or any
// other earlier SetRunExtraEnv PATH push) is not silently discarded.
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
	// Simulate the EARLIER SetRunExtraEnv call that
	// runview.Service.executor.SetRunExtraEnv(s.runEnv) makes with a
	// projectenv-derived PATH.
	execRec.setEnvRecordingRunExtraEnv([]string{"PATH=" + projectEnvPath})

	eng := New(devboxTestWorkflow(), s, execRec,
		WithWorkDir(t.TempDir()),
		WithSandboxOverride("none"),
		WithLogger(iterlog.Nop()),
	)

	runID := "run-devbox-preserve-projectenv"
	if err := eng.Run(context.Background(), runID, nil); err != nil {
		t.Fatalf("engine.Run: %v", err)
	}

	if len(execRec.runExtraEnv) != 1 {
		t.Fatalf("runExtraEnv = %v, want exactly one PATH entry after provisioning", execRec.runExtraEnv)
	}
	got := execRec.runExtraEnv[0]
	shimDir := filepath.Join(os.TempDir(), "iterion-engine-shim", runID)
	wantPrefix := "PATH=" + shimDir + ":"
	if !strings.HasPrefix(got, wantPrefix) {
		t.Fatalf("PATH does not lead with the engine shim dir\n  got:  %s\n  want prefix: %s", got, wantPrefix)
	}
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
