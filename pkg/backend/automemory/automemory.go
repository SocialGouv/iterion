// Package automemory is iterion's switch for the backends' native
// auto-memory: the MEMORY.md an agent maintains across runs to carry what it
// learned. Both wired backends have the mechanism natively — Claude Code reads
// and writes `~/.claude/projects/<cwd>/memory/`, claw renders an "# Auto
// memory" system section — but each points at its own store, in its own
// layout, keyed on the working directory.
//
// This package supplies the two pieces that turn that into one product
// behaviour: the precedence chain resolving the DSL `auto_memory:` field to a
// Mode, and the Mirror that materialises ONE iterion-owned memory space onto
// disk for the node to use and folds the agent's edits back into the store
// afterwards. Because the store is a knowledge.MemoryStore, the same code
// persists to the filesystem locally and to Mongo in cloud — where the runner
// pod's disk dies with the run.
//
// This is NOT the `memory:` DSL block (pkg/memory + the memory_read /
// memory_write / memory_list tools), which is a separate, richer, claw-only
// mechanism. The two share a store, not a surface.
package automemory

import (
	"fmt"
	"slices"
	"strings"

	clawrt "github.com/SocialGouv/claw-code-go/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/backend/backendcaps"
)

// ModeEnv sets the process-wide default for the backends' auto-memory when
// neither a node, the workflow, nor a run override sets one.
const ModeEnv = "ITERION_AUTO_MEMORY"

// SpaceName is the reserved knowledge-space name holding a bot's MEMORY.md
// tree. Reserved so an ordinary `memory:` block cannot collide with it.
const SpaceName = "auto-memory"

// StateDirName is the Task.StateDir component the mirror materialises under.
const StateDirName = "auto-memory"

// SupportsBackend reports whether the backend consumes the materialised
// MEMORY.md directory. The shared registry also records whether iterion must
// render its prompt section: Claude Code has a native mechanism, while claw
// and pi need the authored section. Unsupported backends still earn C132.
func SupportsBackend(name string) bool {
	return backendcaps.Offers(name, backendcaps.AutoMemory)
}

// NeedsPromptSection reports whether iterion must render the auto-memory
// section into this backend's system prompt, rather than pointing a native
// mechanism at the directory.
func NeedsPromptSection(name string) bool {
	return backendcaps.Offers(name, backendcaps.AutoMemoryPrompt)
}

// ClawToolGrant names the tools a claw node running auto-memory receives on
// top of a RESTRICTED `tools:` list — the three the instructions call for:
// read and write MEMORY.md, and list the directory it lives in. Copied, so a
// consumer cannot grow the grant for everyone else.
//
// The names are claw's OWN: the grant lands in the list the executor resolves
// against the tool registry, so a name claw does not have does not degrade —
// it fails the node before its first token (`glob` is how claw lists a
// directory; it read `list_files` until 2026-08, which turned
// `auto_memory: on` into a hard failure on every claw node that restricted
// its tools).
//
// It lives here, beside the switch, because the grant is the mechanism and
// two readers answer about it: the executor that APPENDS it
// (model.ClawExecutor.assembleEffectiveTools) and the lint that asks whether
// auto-memory gives a node a durable write path at all
// (bundlelint.autoMemoryGrantsAWriter, C274). Held apart, the second was
// crediting a write path the first never granted.
func ClawToolGrant() []string {
	return slices.Clone(clawToolGrant)
}

var clawToolGrant = []string{"read_file", "write_file", "glob"}

// Mode is the resolved auto-memory setting for a node or run.
type Mode int

const (
	// Off keeps MEMORY.md out of the node entirely — the default. A bot run
	// is hermetic unless it asks otherwise: it neither reads nor writes the
	// operator's memory.
	Off Mode = iota
	// On materialises the run's memory space and points the backend at it.
	On
)

func (m Mode) String() string {
	if m == On {
		return "on"
	}
	return "off"
}

// Enabled reports whether MEMORY.md reaches the node.
func (m Mode) Enabled() bool { return m == On }

// ParseMode maps a DSL/env/CLI string to a Mode (case-insensitive). The
// canonical values are on|off; true/1 are accepted for env/CLI ergonomics.
// Anything else — including the empty string — is Off, which is why the
// compiler rejects unknown values as C131 rather than letting a typo read as
// a silent opt-out.
func ParseMode(s string) Mode {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "on", "true", "1":
		return On
	}
	return Off
}

// ValidateMode rejects a value an operator typed by hand. ParseMode maps
// anything unrecognised to Off, which is the safe direction but a silent one:
// `--auto-memory noo` would read as "hermetic", and the operator who asked for
// memory would get none with nothing to tell them why. The DSL gets this from
// the compiler (C131); this is the same guarantee for the flag and the API.
//
// The env var is deliberately NOT validated: it is a machine default that must
// never abort a run it was merely present for.
func ValidateMode(s string) error {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "on", "off", "true", "false", "1", "0":
		return nil
	}
	return fmt.Errorf("invalid auto-memory value %q: expected on or off", s)
}

// Resolve picks the effective mode from iterion's precedence chain, highest
// priority first: run override (CLI --auto-memory / studio) > node DSL >
// workflow DSL > ITERION_AUTO_MEMORY env > Off. First non-empty level wins.
func Resolve(override, node, workflow, envDefault string) Mode {
	m, _ := ResolveSourced(override, node, workflow, envDefault)
	return m
}

// ResolveSourced is Resolve plus the winning precedence level
// ("run_override" | "node" | "workflow" | "env" | "default"), which the
// studio's settings-provenance caption reads so an operator can see WHY the
// knob is what it is.
func ResolveSourced(override, node, workflow, envDefault string) (Mode, string) {
	levels := []struct {
		value  string
		source string
	}{
		{override, "run_override"},
		{node, "node"},
		{workflow, "workflow"},
		{envDefault, "env"},
	}
	for _, l := range levels {
		if strings.TrimSpace(l.value) != "" {
			return ParseMode(l.value), l.source
		}
	}
	return Off, "default"
}

// PromptSection renders the system-prompt section that tells a model its
// memory directory exists and how to maintain it: the path, short maintenance
// instructions, and the current MEMORY.md content.
//
// It is claw's own renderer, reused rather than re-authored, so every backend
// iterion has to describe the mechanism to describes it identically — and a
// wording improvement upstream reaches all of them. The directory is passed
// explicitly because the workDir-derived default fingerprints the working
// directory, which for a `worktree: auto` run is a fresh path every time.
//
// Returns "" for an empty dir (auto-memory off).
func PromptSection(dir string) string {
	body := clawrt.BuildAutoMemorySection(dir)
	if body == "" {
		return ""
	}
	return "\n\n# Auto memory\n\n" + body
}
