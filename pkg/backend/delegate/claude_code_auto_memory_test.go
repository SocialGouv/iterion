package delegate

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
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
		t.Errorf("no settings should be emitted when memory is off: %s", settings)
	}
	// And the option list actually carries it — the mapping above is only
	// worth testing if it reaches the spawn.
	if got := len(autoMemoryOpts(Task{})); got != 1 {
		t.Errorf("off must emit exactly the env option, got %d options", got)
	}
}

func TestAutoMemorySpawn_OnPinsDirectory(t *testing.T) {
	const dir = "/srv/state/auto-memory/space"
	disable, settings := autoMemorySpawn(Task{AutoMemoryDir: dir})

	// "0" is not a no-op: it force-ENABLES over an operator settings.json
	// that turned auto-memory off, so the node's declared behaviour holds.
	if disable != "0" {
		t.Errorf("an on node must force auto-memory on, got %q", disable)
	}
	var parsed struct {
		Enabled bool   `json:"autoMemoryEnabled"`
		Dir     string `json:"autoMemoryDirectory"`
	}
	if err := json.Unmarshal(settings, &parsed); err != nil {
		t.Fatalf("settings is not valid JSON (%s): %v", settings, err)
	}
	if !parsed.Enabled {
		t.Error("autoMemoryEnabled must be true")
	}
	if parsed.Dir != dir {
		t.Errorf("autoMemoryDirectory = %q, want %q", parsed.Dir, dir)
	}
	if got := len(autoMemoryOpts(Task{AutoMemoryDir: dir})); got != 2 {
		t.Errorf("on must emit the env option AND the settings option, got %d", got)
	}
}

// The settings blob merges over the operator's own configuration, so it must
// carry nothing beyond the two memory keys.
func TestAutoMemorySpawn_SettingsCarryOnlyMemoryKeys(t *testing.T) {
	_, settings := autoMemorySpawn(Task{AutoMemoryDir: "/tmp/mem"})
	var parsed map[string]any
	if err := json.Unmarshal(settings, &parsed); err != nil {
		t.Fatal(err)
	}
	if len(parsed) == 0 {
		t.Fatal("settings must not be empty when memory is on")
	}
	for k := range parsed {
		if !strings.HasPrefix(k, "autoMemory") {
			t.Errorf("settings carries an unrelated key %q — it merges over the "+
				"operator's own settings and must touch nothing else", k)
		}
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
	if indexOf(offArgs, "--settings") >= 0 {
		t.Errorf("off spawn must not emit --settings: %v", offArgs)
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
	if indexOf(offArgs, "--settings") >= 0 {
		t.Errorf("off must not emit --settings: %v", offArgs)
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
