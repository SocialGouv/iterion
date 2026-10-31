package tool

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeSkillFile plants <workspace>/.claude/skills/<name>/SKILL.md and
// returns the workspace.
func writeSkillFile(t *testing.T, workspace, name, body string) {
	t.Helper()
	dir := filepath.Join(workspace, ".claude", "skills", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(body), 0o644); err != nil {
		t.Fatalf("write skill: %v", err)
	}
}

func execTool(t *testing.T, reg *Registry, name string, input map[string]any) (string, error) {
	t.Helper()
	td, err := reg.Resolve(name)
	if err != nil {
		t.Fatalf("resolve %s: %v", name, err)
	}
	raw, err := json.Marshal(input)
	if err != nil {
		t.Fatalf("marshal input: %v", err)
	}
	return td.Execute(context.Background(), raw)
}

// The workspace-scoped lookup tools resolve `.claude/commands/` and
// `.claude/skills/` through a PROVIDER, consulted on every call: a host
// whose workspace moves after registration — the engine's SetWorkDir push
// for a `worktree: auto` run (#2870) — moves both lookups with one flip.
// The two surfaces take the SAME provider so they cannot drift apart again.
//
// Mutant: a registration capturing the provider's first answer → the
// second half of each subtest reddens.
func TestClawWorkspaceProvider_CommandAndSkillFollowTheProvider(t *testing.T) {
	dirA := writeCommandFile(t, "only-a", "Command body ALPHA-1.\n")
	dirB := writeCommandFile(t, "only-b", "Command body KUMQUAT-2870.\n")
	writeSkillFile(t, dirA, "skill-a", "---\nname: skill-a\ndescription: a\n---\nSkill body ALPHA-1.\n")
	writeSkillFile(t, dirB, "skill-b", "---\nname: skill-b\ndescription: b\n---\nSkill body KUMQUAT-2870.\n")

	cell := dirA
	provider := func() string { return cell }
	reg := NewRegistry()
	if err := RegisterClawCommandWithProvider(reg, provider, 0); err != nil {
		t.Fatalf("RegisterClawCommandWithProvider: %v", err)
	}
	if err := RegisterClawSkillWithProvider(reg, provider); err != nil {
		t.Fatalf("RegisterClawSkillWithProvider: %v", err)
	}

	out, err := execTool(t, reg, "command", map[string]any{"name": "only-a"})
	if err != nil || !strings.Contains(out, "ALPHA-1") {
		t.Fatalf("command in dirA: out=%q err=%v", out, err)
	}
	out, err = execTool(t, reg, "command", map[string]any{"name": "only-b"})
	if err != nil || !strings.HasPrefix(out, "Unknown command: /only-b") {
		t.Fatalf("only-b must be unknown while the provider answers dirA: out=%q err=%v", out, err)
	}
	out, err = execTool(t, reg, "skill", map[string]any{"skill": "skill-a"})
	if err != nil || !strings.Contains(out, "ALPHA-1") {
		t.Fatalf("skill in dirA: out=%q err=%v", out, err)
	}
	if _, err := execTool(t, reg, "skill", map[string]any{"skill": "skill-b"}); err == nil {
		t.Fatalf("skill-b must not resolve while the provider answers dirA")
	}

	// The flip the engine's SetWorkDir push stands in for.
	cell = dirB

	out, err = execTool(t, reg, "command", map[string]any{"name": "only-b"})
	if err != nil || !strings.Contains(out, "KUMQUAT-2870") {
		t.Fatalf("command must follow the provider to dirB: out=%q err=%v", out, err)
	}
	out, err = execTool(t, reg, "command", map[string]any{"name": "only-a"})
	if err != nil || !strings.HasPrefix(out, "Unknown command: /only-a") {
		t.Fatalf("only-a must be unknown once the provider answers dirB: out=%q err=%v", out, err)
	}
	out, err = execTool(t, reg, "skill", map[string]any{"skill": "skill-b"})
	if err != nil || !strings.Contains(out, "KUMQUAT-2870") {
		t.Fatalf("skill must follow the provider to dirB: out=%q err=%v", out, err)
	}
	if _, err := execTool(t, reg, "skill", map[string]any{"skill": "skill-a"}); err == nil {
		t.Fatalf("skill-a must not resolve once the provider answers dirB")
	}
}

// RegisterClawAll routes BOTH lookup tools through the one workspace
// provider, and a set provider supersedes the Workspace string entirely —
// the string is never a second, stale source of truth beside it.
func TestRegisterClawAll_WorkspaceProviderSupersedesWorkspaceString(t *testing.T) {
	staticDir := writeCommandFile(t, "static-cmd", "Command body STATIC.\n")
	liveDir := writeCommandFile(t, "live-cmd", "Command body KUMQUAT-2870 live.\n")
	writeSkillFile(t, liveDir, "live-skill", "---\nname: live-skill\ndescription: live\n---\nSkill body KUMQUAT-2870.\n")

	reg := NewRegistry()
	err := RegisterClawAll(reg, ClawDefaults{
		Workspace:         staticDir,
		WorkspaceProvider: func() string { return liveDir },
	})
	if err != nil {
		t.Fatalf("RegisterClawAll: %v", err)
	}

	out, err := execTool(t, reg, "command", map[string]any{"name": "live-cmd"})
	if err != nil || !strings.Contains(out, "KUMQUAT-2870") {
		t.Fatalf("command must resolve through the provider, not the string: out=%q err=%v", out, err)
	}
	out, err = execTool(t, reg, "command", map[string]any{"name": "static-cmd"})
	if err != nil || !strings.HasPrefix(out, "Unknown command: /static-cmd") {
		t.Fatalf("the Workspace string must not leak into the command lookup: out=%q err=%v", out, err)
	}
	out, err = execTool(t, reg, "skill", map[string]any{"skill": "live-skill"})
	if err != nil || !strings.Contains(out, "KUMQUAT-2870") {
		t.Fatalf("skill must resolve through the provider: out=%q err=%v", out, err)
	}
}

// Without a provider the Workspace string keeps its historical behaviour:
// wrapped once into the provider, so the tools see a static root.
func TestRegisterClawAll_WorkspaceStringIsTheStaticDefault(t *testing.T) {
	staticDir := writeCommandFile(t, "static-cmd", "Command body KUMQUAT-2870 static.\n")
	reg := NewRegistry()
	if err := RegisterClawAll(reg, ClawDefaults{Workspace: staticDir}); err != nil {
		t.Fatalf("RegisterClawAll: %v", err)
	}
	out, err := execTool(t, reg, "command", map[string]any{"name": "static-cmd"})
	if err != nil || !strings.Contains(out, "KUMQUAT-2870") {
		t.Fatalf("command must resolve against the Workspace string: out=%q err=%v", out, err)
	}
}
