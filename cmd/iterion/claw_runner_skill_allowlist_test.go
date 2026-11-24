package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/backend/tool"
	"github.com/SocialGouv/iterion/pkg/store"
)

// The runner half of the pinned skill mode (iterion#1719): the allowlist
// crosses the wire workspace-RELATIVE and the in-container registration
// joins it against the container's own workspace — the same name the
// checkout planted under .claude/skills is never read.
func TestRegisterSandboxLocalTools_PinnedSkillMode(t *testing.T) {
	workspace := t.TempDir()
	// The engine-owned channel content, where the launcher pinned it.
	pinnedRel := ".claude/iterion-run-skills/.claude/skills/review/SKILL.md"
	if err := os.MkdirAll(filepath.Join(workspace, filepath.Dir(pinnedRel)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, pinnedRel), []byte("owned body\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// The planted twin the walk would serve.
	planted := filepath.Join(workspace, ".claude", "skills", "review", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(planted), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(planted, []byte("planted\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	reg := tool.NewRegistry()
	if err := registerSandboxLocalTools(reg, workspace, sandboxScratchScope{},
		model.NewSubagentRunner(model.NewRegistry(), tool.NewRegistry(), model.EventHooks{}, nil, ""), 0,
		map[string]string{"review": pinnedRel}, true, store.RunTrustDefault); err != nil {
		t.Fatalf("registerSandboxLocalTools: %v", err)
	}
	td, err := reg.Resolve("skill")
	if err != nil {
		t.Fatalf("Resolve(skill): %v", err)
	}
	in, _ := json.Marshal(map[string]any{"skill": "review"})
	out, err := td.Execute(context.Background(), in)
	if err != nil {
		t.Fatalf("the pinned skill = %v, want served", err)
	}
	if !strings.Contains(out, "owned body") || strings.Contains(out, "planted") {
		t.Errorf("armed, the runner served %q — want the joined channel file, never the planted twin", out)
	}

	// Armed with nothing, every name refuses — no silent fall back to the walk.
	reg2 := tool.NewRegistry()
	if err := registerSandboxLocalTools(reg2, workspace, sandboxScratchScope{}, nil, 0, nil, true, store.RunTrustDefault); err != nil {
		t.Fatalf("registerSandboxLocalTools (empty pin): %v", err)
	}
	td2, _ := reg2.Resolve("skill")
	if _, err := td2.Execute(context.Background(), in); err == nil ||
		!strings.Contains(err.Error(), "not in this run's allowed skills") {
		t.Errorf("armed-empty = %v, want the pinned-mode refusal", err)
	}
}
