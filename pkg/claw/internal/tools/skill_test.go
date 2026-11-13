package tools

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseSkill_Frontmatter(t *testing.T) {
	content := []byte(`---
name: my-skill
description: A test skill
argument-hint: "start|stop"
disable-model-invocation: true
allowed-tools: [Read, Bash]
version: 1.0.0
license: MIT
---

# My Skill

Body content here.`)

	front, body, ok := parseSkill(content)
	if !ok {
		t.Fatal("expected frontmatter to be detected")
	}
	if front.Name != "my-skill" {
		t.Errorf("Name: got %q, want %q", front.Name, "my-skill")
	}
	if front.Description != "A test skill" {
		t.Errorf("Description: got %q", front.Description)
	}
	if front.ArgumentHint != "start|stop" {
		t.Errorf("ArgumentHint: got %q", front.ArgumentHint)
	}
	if !front.DisableModelInvocation {
		t.Errorf("DisableModelInvocation: expected true")
	}
	if len(front.AllowedTools) != 2 || front.AllowedTools[0] != "Read" {
		t.Errorf("AllowedTools: got %v", front.AllowedTools)
	}
	if front.Version != "1.0.0" {
		t.Errorf("Version: got %q", front.Version)
	}
	if front.License != "MIT" {
		t.Errorf("License: got %q", front.License)
	}
	if !strings.HasPrefix(body, "# My Skill") {
		t.Errorf("body should start with '# My Skill', got %q", body[:min(20, len(body))])
	}
}

func TestParseSkill_Legacy(t *testing.T) {
	content := []byte("# Legacy Skill\n\nNo frontmatter here.")
	front, body, ok := parseSkill(content)
	if ok {
		t.Fatal("expected no frontmatter")
	}
	if front.Name != "" {
		t.Errorf("expected zero frontmatter, got Name=%q", front.Name)
	}
	if body != string(content) {
		t.Errorf("body should equal full content")
	}
}

func TestParseSkill_MalformedYAML(t *testing.T) {
	content := []byte("---\nname: foo\n  bad: indent: nope\n---\nbody")
	_, body, ok := parseSkill(content)
	if ok {
		t.Fatal("malformed YAML should fall through to legacy")
	}
	if body != string(content) {
		t.Errorf("malformed YAML should return original content as body")
	}
}

func TestParseSkill_DoesNotMatchMalformedClosing(t *testing.T) {
	// "---foo" inside the YAML block must NOT be treated as a closing marker.
	content := []byte("---\nname: foo\nargument-hint: \"a----b\"\n---\nbody after.")
	front, body, ok := parseSkill(content)
	if !ok {
		t.Fatal("expected frontmatter to parse")
	}
	if front.Name != "foo" {
		t.Errorf("Name: got %q", front.Name)
	}
	if front.ArgumentHint != "a----b" {
		t.Errorf("ArgumentHint should preserve dashes, got %q", front.ArgumentHint)
	}
	if !strings.HasPrefix(body, "body after.") {
		t.Errorf("body should start with 'body after.', got %q", body)
	}
}

func TestParseSkill_NoClosingDelimiter(t *testing.T) {
	content := []byte("---\nname: foo\nbody without closing")
	_, body, ok := parseSkill(content)
	if ok {
		t.Fatal("missing closing delimiter should fall back to legacy")
	}
	if body != string(content) {
		t.Errorf("body should equal full content on fallback")
	}
}

func TestExecuteSkill_DisableModelInvocation(t *testing.T) {
	tmp := t.TempDir()
	skillDir := filepath.Join(tmp, ".claude", "skills", "guarded", "skills", "guarded")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	content := []byte(`---
name: guarded
description: user-only
disable-model-invocation: true
---
Body.`)
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), content, 0o644); err != nil {
		t.Fatal(err)
	}

	input := map[string]any{"skill": "guarded:guarded"}

	if _, err := ExecuteSkill(input, tmp); err == nil {
		t.Fatal("model-invoked call to disable-model-invocation skill should error")
	}
	out, err := ExecuteSkillUserInvoked(input, tmp)
	if err != nil {
		t.Fatalf("user-invoked call should succeed, got %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatalf("payload is not JSON: %v", err)
	}
	if payload["description"] != "user-only" {
		t.Errorf("description: got %v", payload["description"])
	}
	if payload["prompt"] != "Body." {
		t.Errorf("prompt should be body only, got %q", payload["prompt"])
	}
}

func TestResolveSkill_Namespaced(t *testing.T) {
	tmp := t.TempDir()
	skillDir := filepath.Join(tmp, ".claude", "skills", "myplugin", "skills", "myskill")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("body"), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := ResolveSkill("myplugin:myskill", tmp)
	if err != nil {
		t.Fatal(err)
	}
	if res.CanonicalName != "myplugin:myskill" {
		t.Errorf("canonical: got %q", res.CanonicalName)
	}
	if res.Source != "filesystem" {
		t.Errorf("source: got %q", res.Source)
	}
}

func TestResolveSkill_BareUnique(t *testing.T) {
	tmp := t.TempDir()
	skillDir := filepath.Join(tmp, ".claude", "skills", "myplugin", "skills", "uniqueskill")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("body"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Ensure no bundled match conflicts: use a clearly invented name.
	res, err := ResolveSkill("uniqueskill", tmp)
	if err != nil {
		t.Fatal(err)
	}
	if res.CanonicalName != "myplugin:uniqueskill" {
		t.Errorf("canonical: got %q", res.CanonicalName)
	}
}

func TestResolveSkill_BareAmbiguous(t *testing.T) {
	tmp := t.TempDir()
	for _, plug := range []string{"alpha", "beta"} {
		dir := filepath.Join(tmp, ".claude", "skills", plug, "skills", "common")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("body"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	_, err := ResolveSkill("common", tmp)
	if err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("expected ambiguity error, got %v", err)
	}
	if !strings.Contains(err.Error(), "alpha:common") || !strings.Contains(err.Error(), "beta:common") {
		t.Errorf("error should list candidates, got %q", err.Error())
	}
}

func TestResolveSkill_FilesystemOverridesBundled(t *testing.T) {
	// skill-creator is in the bundle. Override locally.
	tmp := t.TempDir()
	skillDir := filepath.Join(tmp, ".claude", "skills", "skill-creator", "skills", "skill-creator")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("local override"), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := ResolveSkill("skill-creator:skill-creator", tmp)
	if err != nil {
		t.Fatal(err)
	}
	if res.Source != "filesystem" {
		t.Errorf("expected filesystem source to win, got %q", res.Source)
	}
}

func TestResolveSkill_BundledFallback(t *testing.T) {
	tmp := t.TempDir()
	// Avoid touching the user's home by hiding it: point HOME to tmp so
	// skillLookupRoots doesn't find anything filesystem-wise.
	t.Setenv("HOME", tmp)
	t.Setenv("CLAW_CONFIG_HOME", "")
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("CODEX_HOME", "")

	res, err := ResolveSkill("skill-creator:skill-creator", tmp)
	if err != nil {
		t.Fatal(err)
	}
	if res.Source != "bundled" {
		t.Errorf("expected bundled fallback, got %q", res.Source)
	}
	if res.CanonicalName != "skill-creator:skill-creator" {
		t.Errorf("canonical: got %q", res.CanonicalName)
	}
}

func TestListBundledSkills(t *testing.T) {
	got := ListBundledSkills()
	want := []string{
		"claude-code-setup:claude-automation-recommender",
		"claude-md-management:claude-md-improver",
		"frontend-design:frontend-design",
		"hookify:writing-rules",
		"math-olympiad:math-olympiad",
		"mcp-server-dev:build-mcp-app",
		"mcp-server-dev:build-mcp-server",
		"mcp-server-dev:build-mcpb",
		"playground:playground",
		"plugin-dev:agent-development",
		"plugin-dev:command-development",
		"plugin-dev:hook-development",
		"plugin-dev:mcp-integration",
		"plugin-dev:plugin-settings",
		"plugin-dev:plugin-structure",
		"plugin-dev:skill-development",
		"session-report:session-report",
		"skill-creator:skill-creator",
	}
	if len(got) != len(want) {
		t.Fatalf("count: got %d, want %d (%v)", len(got), len(want), got)
	}
	set := map[string]struct{}{}
	for _, s := range got {
		set[s] = struct{}{}
	}
	for _, w := range want {
		if _, ok := set[w]; !ok {
			t.Errorf("missing expected bundled skill: %s", w)
		}
	}
}

func TestSplitSkillName(t *testing.T) {
	cases := []struct {
		in         string
		plugin     string
		skill      string
		namespaced bool
	}{
		{"foo", "", "foo", false},
		{"foo:bar", "foo", "bar", true},
		{"foo/bar", "foo", "bar", true},
		{"foo.md", "", "foo.md", false},
		{":bar", "", ":bar", false}, // empty plugin, not namespaced
	}
	for _, c := range cases {
		p, s, n := splitSkillName(c.in)
		if p != c.plugin || s != c.skill || n != c.namespaced {
			t.Errorf("splitSkillName(%q) = (%q, %q, %v); want (%q, %q, %v)",
				c.in, p, s, n, c.plugin, c.skill, c.namespaced)
		}
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
