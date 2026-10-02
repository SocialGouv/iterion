package delegate

import (
	"context"
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/delegate/claudesdk"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// Auto-memory OFF is not "leave it alone": the CLI's own default is ON, so a
// run that did not ask for memory would otherwise read and write the
// operator's personal ~/.claude/projects/<cwd>/memory/.
func TestAutoMemorySpawn_OffDisablesExplicitly(t *testing.T) {
	disable, settings := autoMemorySpawn(Task{})
	if disable != "1" {
		t.Errorf("an off node must actively disable auto-memory, got %q", disable)
	}
	if settings != nil {
		t.Errorf("no memory keys should be pinned when memory is off: %v", settings)
	}
	// And the pins actually carry it — the mapping above is only worth
	// testing if it reaches the spawn, in both layers.
	env, raw := claudeSpawnPins(Task{})
	if env[autoMemoryDisableEnv] != "1" {
		t.Errorf("off must pin %s=1 in the process environment, got %q", autoMemoryDisableEnv, env[autoMemoryDisableEnv])
	}
	if got := flagSettingsEnv(t, raw)[autoMemoryDisableEnv]; got != "1" {
		t.Errorf("off must pin %s=1 in the flag settings layer, got %q", autoMemoryDisableEnv, got)
	}
}

// flagSettingsEnv parses the `env` block of a --settings object.
func flagSettingsEnv(t *testing.T, raw []byte) map[string]string {
	t.Helper()
	var parsed struct {
		Env map[string]string `json:"env"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("the settings object is not valid JSON (%s): %v", raw, err)
	}
	return parsed.Env
}

func TestAutoMemorySpawn_OnPinsDirectory(t *testing.T) {
	const dir = "/srv/state/auto-memory/space"
	disable, settings := autoMemorySpawn(Task{AutoMemoryDir: dir})

	// "0" is not a no-op: it force-ENABLES over an operator settings.json
	// that turned auto-memory off, so the node's declared behaviour holds.
	if disable != "0" {
		t.Errorf("an on node must force auto-memory on, got %q", disable)
	}
	if settings["autoMemoryEnabled"] != true {
		t.Error("autoMemoryEnabled must be true")
	}
	if settings["autoMemoryDirectory"] != dir {
		t.Errorf("autoMemoryDirectory = %v, want %q", settings["autoMemoryDirectory"], dir)
	}
	env, raw := claudeSpawnPins(Task{AutoMemoryDir: dir})
	if env[autoMemoryDisableEnv] != "0" {
		t.Errorf("on must pin %s=0 in the process environment, got %q", autoMemoryDisableEnv, env[autoMemoryDisableEnv])
	}
	if got := flagSettingsEnv(t, raw)[autoMemoryDisableEnv]; got != "0" {
		t.Errorf("on must pin %s=0 in the flag settings layer, got %q", autoMemoryDisableEnv, got)
	}
}

// The settings object merges over the operator's own configuration, so it
// must carry nothing beyond the two memory keys, the ambient-context
// exclusions and the pinned environment — and the environment nothing beyond
// the four pinned variables. claudeMdExcludes is safe there: the CLI
// concatenates it across layers, so a repository's or an operator's own
// exclusions survive it (measured on CLI 2.1.282, ADR-119).
func TestFlagSettingsCarryOnlyMemoryKeysAndThePinnedEnvironment(t *testing.T) {
	t.Setenv("ITERION_CLAUDE_CODE_BACKGROUND_TASKS", "")
	t.Setenv("ITERION_CLAUDE_CODE_BACKGROUND_LIFECYCLE", "")
	_, memory := autoMemorySpawn(Task{AutoMemoryDir: "/tmp/mem"})
	if len(memory) == 0 {
		t.Fatal("memory keys must not be empty when memory is on")
	}
	for k := range memory {
		if !strings.HasPrefix(k, "autoMemory") {
			t.Errorf("the memory half carries an unrelated key %q", k)
		}
	}
	_, raw := claudeSpawnPins(Task{AutoMemoryDir: "/tmp/mem"})
	var parsed map[string]json.RawMessage
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("the settings object is not valid JSON (%s): %v", raw, err)
	}
	for k := range parsed {
		if k != "env" && k != "claudeMdExcludes" && !strings.HasPrefix(k, "autoMemory") {
			t.Errorf("settings carries an unrelated key %q — it merges over the "+
				"operator's own settings and must touch nothing else", k)
		}
	}
	env := flagSettingsEnv(t, raw)
	// The four switches, the CLI's wind-down ceiling, and the background
	// lifecycle's signals (the lifecycle is on by default).
	want := []string{"BASH_DEFAULT_TIMEOUT_MS", "BASH_MAX_TIMEOUT_MS", "CLAUDE_CODE_BG_TASKS_REPORT_RUNNING", "CLAUDE_CODE_DISABLE_AUTO_MEMORY",
		"CLAUDE_CODE_DISABLE_BACKGROUND_TASKS", "CLAUDE_CODE_EMIT_SESSION_STATE_EVENTS", "CLAUDE_CODE_EXIT_AFTER_STOP_DELAY",
		"CLAUDE_CODE_PRINT_BG_WAIT_CEILING_MS"}
	if got := slices.Sorted(maps.Keys(env)); !slices.Equal(got, want) {
		t.Errorf("the env block carries %v, want exactly %v", got, want)
	}
}

// The mapping above is worthless if nothing calls it. This asserts the
// COMPOSITION: the options actually reach the assembled spawn. Both claude
// spawns — the main pass and the structured-output formatting pass that
// resumes the same session — go through perTaskSpawnOpts, so covering
// buildTransportOptions covers both by construction.
func TestBuildTransportOptions_CarriesAutoMemory(t *testing.T) {
	b := &ClaudeCodeBackend{Logger: iterlog.Nop()}
	dir := t.TempDir()

	opts, cleanup := b.buildTransportOptions(Task{WorkDir: t.TempDir(), AutoMemoryDir: dir})
	if cleanup != nil {
		defer cleanup()
	}
	env, args := claudesdk.ResolveSpawn(opts...)
	if env[autoMemoryDisableEnv] != "0" {
		t.Errorf("main spawn env %s = %q, want 0", autoMemoryDisableEnv, env[autoMemoryDisableEnv])
	}
	if idx := indexOf(args, "--settings"); idx < 0 || !strings.Contains(args[idx+1], dir) {
		t.Errorf("main spawn must pin the memory directory via --settings: %v", args)
	}

	// And the off case must actively disable, not merely omit.
	offOpts, offCleanup := b.buildTransportOptions(Task{WorkDir: t.TempDir()})
	if offCleanup != nil {
		defer offCleanup()
	}
	offEnv, offArgs := claudesdk.ResolveSpawn(offOpts...)
	if offEnv[autoMemoryDisableEnv] != "1" {
		t.Errorf("off spawn env %s = %q, want 1", autoMemoryDisableEnv, offEnv[autoMemoryDisableEnv])
	}
	if idx := indexOf(offArgs, "--settings"); idx >= 0 && strings.Contains(offArgs[idx+1], "autoMemory") {
		t.Errorf("off spawn must pin no memory key: %v", offArgs)
	}
}

func indexOf(hay []string, needle string) int {
	for i, v := range hay {
		if v == needle {
			return i
		}
	}
	return -1
}

// perTaskSpawnOpts is what makes the two spawns agree. If it ever stops
// carrying auto-memory, the formatting pass silently reverts to the CLI's
// own default halfway through a node.
func TestPerTaskSpawnOpts_CarriesAutoMemory(t *testing.T) {
	dir := t.TempDir()
	env, args := claudesdk.ResolveSpawn(perTaskSpawnOpts(Task{AutoMemoryDir: dir})...)
	if env[autoMemoryDisableEnv] != "0" {
		t.Errorf("env %s = %q, want 0", autoMemoryDisableEnv, env[autoMemoryDisableEnv])
	}
	if idx := indexOf(args, "--settings"); idx < 0 || !strings.Contains(args[idx+1], dir) {
		t.Errorf("the memory directory must be pinned: %v", args)
	}

	offEnv, offArgs := claudesdk.ResolveSpawn(perTaskSpawnOpts(Task{})...)
	if offEnv[autoMemoryDisableEnv] != "1" {
		t.Errorf("off env %s = %q, want 1", autoMemoryDisableEnv, offEnv[autoMemoryDisableEnv])
	}
	if idx := indexOf(offArgs, "--settings"); idx >= 0 && strings.Contains(offArgs[idx+1], "autoMemory") {
		t.Errorf("off must pin no memory key: %v", offArgs)
	}
}

// pi has no auto-memory concept, so the rendered section IS the mechanism —
// and pi only ever sees a system prompt through a FILE. Asserting that
// BuildSystemPrompt contains the section proves nothing about what pi reads;
// this follows the section all the way to the bytes on disk that
// `--append-system-prompt` points at.
func TestWriteSystemPromptFile_CarriesTheAutoMemorySection(t *testing.T) {
	const section = "\n\n# Auto memory\n\nYour persistent memory directory is: /srv/mem\n"
	task := Task{
		NodeID:           "n",
		StoreDir:         t.TempDir(),
		SystemPrompt:     "do the thing",
		AutoMemoryPrompt: section,
	}

	composed := task.BuildSystemPrompt()
	path, cleanup, err := writeSystemPromptFile(context.Background(), task, BackendPi, composed)
	if err != nil {
		t.Fatalf("writeSystemPromptFile: %v", err)
	}
	if cleanup != nil {
		defer cleanup()
	}
	body, err := os.ReadFile(path) // #nosec G304 — path produced by the function under test.
	if err != nil {
		t.Fatalf("read prompt file: %v", err)
	}
	if !strings.Contains(string(body), "# Auto memory") {
		t.Errorf("the prompt file pi reads does not carry the memory section:\n%s", body)
	}
	if !strings.Contains(string(body), "/srv/mem") {
		t.Errorf("the prompt file does not name the memory directory:\n%s", body)
	}
	if !strings.Contains(string(body), "do the thing") {
		t.Errorf("the node's own system prompt was lost:\n%s", body)
	}
}

// The three state-root guards share one contract, so all three must name the
// caller. refuseSymlinkedPath kept pi's wording after the extraction, so an
// auto-memory run refused by a repo-planted symlink told the operator to go
// look at "the pi credential" — and the memoised guard repeated it on every
// node of the run.
func TestPrepareStateRoot_ErrorsNameTheirCaller(t *testing.T) {
	work := t.TempDir()
	// A symlinked ANCESTOR: the component walk, not the leaf check.
	if err := os.MkdirAll(filepath.Join(work, "real"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(work, "real"), filepath.Join(work, ".iterion")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	root := filepath.Join(work, ".iterion", "auto-memory")

	err := PrepareStateRoot(Task{WorkDir: work}, root, StateInCheckout,
		"auto-memory", "the agent's MEMORY.md notes", nil)
	if err == nil {
		t.Fatal("a symlinked ancestor inside the checkout must be refused")
	}
	if strings.Contains(err.Error(), "pi") {
		t.Errorf("an auto-memory refusal points the operator at pi: %v", err)
	}
	if !strings.Contains(err.Error(), "MEMORY.md") {
		t.Errorf("the refusal does not say what would have been written: %v", err)
	}
}
