package tool

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeCommandFile plants <workspace>/.claude/commands/<name>.md — the file
// a plugin's `contributes: commands` mirrors there — and returns the
// workspace.
func writeCommandFile(t *testing.T, name, body string) string {
	t.Helper()
	ws := t.TempDir()
	rel := filepath.Join(strings.Split(name, ":")...) + ".md"
	path := filepath.Join(ws, ".claude", "commands", rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write command: %v", err)
	}
	return ws
}

func mustCommandTool(t *testing.T, reg *Registry) *ToolDef {
	t.Helper()
	td, err := reg.Resolve("command")
	if err != nil {
		t.Fatalf("resolve command: %v", err)
	}
	return td
}

func execCommand(t *testing.T, td *ToolDef, input map[string]any) (string, error) {
	t.Helper()
	raw, err := json.Marshal(input)
	if err != nil {
		t.Fatalf("marshal input: %v", err)
	}
	return td.Execute(context.Background(), raw)
}

// The `command` tool is the model-invoked half of the workspace
// slash-command capability (#1716): the model reaches a command the
// operator did not put at the head of the prompt, through the same
// contained lookup the prompt path uses. Mutant: the registration dropped
// from RegisterClawAll → TestRegisterClawAll_RegistersFullSet and the
// kill-switch test redden; the exec's lookup swapped for a no-op → every
// assertion here reddens.
func TestClawCommand_ModelInvokesAWorkspaceCommand(t *testing.T) {
	ws := writeCommandFile(t, "probe-secret", "---\ndescription: probe\nallowed-tools: [bash]\n---\nReport the KUMQUAT-7731 token for $ARGUMENTS.\n")
	reg := NewRegistry()
	if err := RegisterClawCommand(reg, ws, 0); err != nil {
		t.Fatalf("RegisterClawCommand: %v", err)
	}
	td := mustCommandTool(t, reg)
	out, err := execCommand(t, td, map[string]any{"name": "probe-secret", "args": "now"})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	// The body, frontmatter STRIPPED, the arguments substituted.
	if !strings.Contains(out, "Report the KUMQUAT-7731 token for now.") {
		t.Errorf("output = %q, want the expanded body", out)
	}
	for _, unwanted := range []string{"description:", "allowed-tools:", "$ARGUMENTS"} {
		if strings.Contains(out, unwanted) {
			t.Errorf("output = %q carries %q — the frontmatter and the placeholders stay in the file", out, unwanted)
		}
	}

	// The model thinks in invocations: a leading slash is accepted.
	if _, err := execCommand(t, td, map[string]any{"name": "/probe-secret"}); err != nil {
		t.Fatalf("execute with a leading slash: %v", err)
	}

	// A namespaced command reads its directory.
	ws2 := writeCommandFile(t, "db:migrate", "Run the migration.\n")
	reg2 := NewRegistry()
	if err := RegisterClawCommand(reg2, ws2, 0); err != nil {
		t.Fatalf("RegisterClawCommand: %v", err)
	}
	out, err = execCommand(t, mustCommandTool(t, reg2), map[string]any{"name": "db:migrate"})
	if err != nil || !strings.Contains(out, "Run the migration.") {
		t.Fatalf("namespaced command: out=%q err=%v", out, err)
	}
}

// claude_code answers an unknown command LOCALLY (is_error: false), so the
// tool's domain outcomes are its text, never a node-failing error.
func TestClawCommand_DegradedNeverSilent(t *testing.T) {
	ws := writeCommandFile(t, "present", "Do the thing.\n")
	reg := NewRegistry()
	if err := RegisterClawCommand(reg, ws, 0); err != nil {
		t.Fatalf("RegisterClawCommand: %v", err)
	}
	td := mustCommandTool(t, reg)

	out, err := execCommand(t, td, map[string]any{"name": "nope"})
	if err != nil || !strings.HasPrefix(out, "Unknown command: /nope") {
		t.Errorf("unknown command: out=%q err=%v, want a local 'Unknown command' answer", out, err)
	}
	// A body that consumes no arguments keeps them, appended — dropping
	// them would delete the operator's message.
	out, err = execCommand(t, td, map[string]any{"name": "present", "args": "carefully please"})
	if err != nil || !strings.Contains(out, "Do the thing.\n\nARGUMENTS: carefully please") {
		t.Errorf("unconsumed args: out=%q err=%v, want them appended", out, err)
	}
	// A name is required — the one input error, so the model fixes its call.
	if _, err = execCommand(t, td, map[string]any{}); err == nil {
		t.Error("a missing name must error — the model rewrites its arguments")
	}
	// The ceiling refuses rather than materialises: an oversized body, and
	// an expansion that crosses the bound.
	big := strings.Repeat("x", 64) + " $ARGUMENTS"
	wsBig := writeCommandFile(t, "big", big)
	regBig := NewRegistry()
	if err := RegisterClawCommand(regBig, wsBig, 32); err != nil {
		t.Fatalf("RegisterClawCommand: %v", err)
	}
	out, err = execCommand(t, mustCommandTool(t, regBig), map[string]any{"name": "big", "args": "y"})
	if err != nil || !strings.Contains(out, "ceiling") {
		t.Errorf("oversized body: out=%q err=%v, want the ceiling refusal as text", out, err)
	}
	// No workspace at all: every call answers unknown, nothing panics.
	regEmpty := NewRegistry()
	if err := RegisterClawCommand(regEmpty, "", 0); err != nil {
		t.Fatalf("RegisterClawCommand: %v", err)
	}
	out, err = execCommand(t, mustCommandTool(t, regEmpty), map[string]any{"name": "x"})
	if err != nil || !strings.HasPrefix(out, "Unknown command: /x") {
		t.Errorf("no workspace: out=%q err=%v", out, err)
	}
}

// The capability's kill switch takes BOTH surfaces: the host that answers
// ITERION_CLAW_SLASH_COMMANDS=off (model.SlashCommandsDisabled) drops the
// tool from the set RegisterClawAll builds — a node declaring it fails
// loudly at dispatch rather than the capability half-living.
func TestRegisterClawAll_WorkspaceCommandsKillSwitchDropsTheTool(t *testing.T) {
	reg := NewRegistry()
	if err := RegisterClawAll(reg, ClawDefaults{Workspace: t.TempDir()}); err != nil {
		t.Fatalf("RegisterClawAll: %v", err)
	}
	if _, err := reg.Resolve("command"); err != nil {
		t.Fatal("the command tool must register by default — the model-invoked surface of the workspace-commands capability")
	}
	regOff := NewRegistry()
	if err := RegisterClawAll(regOff, ClawDefaults{Workspace: t.TempDir(), DisableWorkspaceCommands: true}); err != nil {
		t.Fatalf("RegisterClawAll (disabled): %v", err)
	}
	if _, err := regOff.Resolve("command"); err == nil {
		t.Error("DisableWorkspaceCommands must drop the command tool from the set")
	}
}

// The two divergences the lookup reports travel with the body, because this
// surface's only channel is its own answer (#1716): the frontmatter it
// discarded — the one with a security consequence, since nothing here
// honours `allowed-tools:` while #1717's enforcement is deferred — and the
// body forms claw expands differently. Mutant: drop the commandNotices call
// from executeWorkspaceCommand → every assertion below reddens, and the
// model invokes a command whose own header claims a narrowing that is not
// there.
func TestClawCommand_ReportsTheDiscardedFrontmatterAndTheDivergentForms(t *testing.T) {
	ws := writeCommandFile(t, "narrow",
		"---\nallowed-tools: Read\nmodel: haiku\ndisable-model-invocation: true\ndescription: narrow\n---\nRead only. $1 is the target.\n")
	reg := NewRegistry()
	if err := RegisterClawCommand(reg, ws, 0); err != nil {
		t.Fatalf("RegisterClawCommand: %v", err)
	}
	out, err := execCommand(t, mustCommandTool(t, reg), map[string]any{"name": "narrow", "args": "pkg/auth"})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	for _, want := range []string{
		"declares allowed-tools, disable-model-invocation, model in its frontmatter",
		"does not narrow the tools you hold (#1717)",
		"uses $N",
		"docs/backends.md#workspace-slash-commands",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output = %q, want it to carry %q", out, want)
		}
	}
	// The notices lead, so the host persists them even when the answer is
	// large enough to be stored as a preview of its first bytes
	// (model.persistToolPayload), and the body still arrives expanded.
	if !strings.HasPrefix(out, "NOTE (iterion): /narrow") {
		t.Errorf("output = %q, want the notices at the head", out)
	}
	if !strings.Contains(out, "Read only. pkg/auth is the target.") {
		t.Errorf("output = %q, want the expanded body after the notices", out)
	}

	// A command with nothing to report answers with the body alone: a
	// notice on every invocation would cost the real ones their audience.
	wsPlain := writeCommandFile(t, "plain", "---\ndescription: plain\n---\nDo the thing.\n")
	regPlain := NewRegistry()
	if err := RegisterClawCommand(regPlain, wsPlain, 0); err != nil {
		t.Fatalf("RegisterClawCommand (plain): %v", err)
	}
	out, err = execCommand(t, mustCommandTool(t, regPlain), map[string]any{"name": "plain"})
	if err != nil || out != "Do the thing." {
		t.Errorf("plain command: out=%q err=%v, want exactly the body", out, err)
	}

	// The notice is added AFTER the ceiling check, so what it interpolates
	// is bounded itself: a frontmatter key of 4 KiB cannot carry the
	// untrusted file past the bound that exists to hold it.
	huge := strings.Repeat("k", 4096)
	wsHuge := writeCommandFile(t, "huge", "---\n"+huge+": x\ndescription: huge\n---\nBody.\n")
	regHuge := NewRegistry()
	if err := RegisterClawCommand(regHuge, wsHuge, 0); err != nil {
		t.Fatalf("RegisterClawCommand (huge): %v", err)
	}
	out, err = execCommand(t, mustCommandTool(t, regHuge), map[string]any{"name": "huge"})
	if err != nil {
		t.Fatalf("execute (huge): %v", err)
	}
	if !strings.Contains(out, "[truncated]") || len(out) > 1024 {
		t.Errorf("oversized frontmatter key: out is %d bytes (%q), want it truncated", len(out), out)
	}
}
