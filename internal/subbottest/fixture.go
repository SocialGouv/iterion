// Package subbottest shares source-level bundle fixtures across the four
// launch surfaces. Assertions run real shell tools, not a simulated mirror.
package subbottest

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/SocialGouv/iterion/pkg/store"
)

type Fixture struct {
	Parent, Child, Workspace, Store string
	Bare                            bool
}

func New(t *testing.T, kind string) Fixture {
	t.Helper()
	root := t.TempDir()
	f := Fixture{Parent: filepath.Join(root, "parent", "main.bot"), Child: filepath.Join(root, "child", "main.bot"), Workspace: filepath.Join(root, "workspace"), Store: filepath.Join(root, "store"), Bare: kind == "bare"}
	write := func(path, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"parent", "child"} {
		dir := filepath.Join(root, name)
		write(filepath.Join(dir, "manifest.yaml"), fmt.Sprintf("name: %s\nversion: 1.0.0\n", name))
		write(filepath.Join(dir, "skills", "shared.md"), name)
		write(filepath.Join(dir, "skills", "directory", "SKILL.md"), name)
	}
	check := func(want string) string {
		return fmt.Sprintf(`test "$(cat .claude/skills/shared.md)" = %s && test "$(cat .claude/skills/directory/SKILL.md)" = %s && printf '{"ok":true}'`, want, want)
	}
	child := func(want string) string {
		return fmt.Sprintf("schema result:\n  ok: bool\ntool check:\n  command: `%s`\n  output: result\nworkflow child:\n  worktree: none\n  sandbox: none\n  entry: check\n  check -> done\n", check(want))
	}
	write(f.Child, child("child"))
	source := "../child/main.bot"
	switch kind {
	case "member":
		f.Child = filepath.Join(root, "child", "step.bot")
		write(f.Child, child("child"))
		source = "../child/step.bot"
	case "bare":
		f.Child = filepath.Join(root, "loose", "step.bot")
		write(f.Child, child("parent"))
		// Resource-looking siblings alone must not invent a bundle.
		write(filepath.Join(root, "loose", "skills", "shared.md"), "decoy")
		source = "../loose/step.bot"
	}
	write(f.Parent, fmt.Sprintf("schema result:\n  ok: bool\ntool before:\n  command: `%s`\n  output: result\nsubbot child:\n  source: %q\n  output: result\ntool after:\n  command: `%s`\n  output: result\nworkflow parent:\n  worktree: none\n  sandbox: none\n  entry: before\n  before -> child\n  child -> after\n  after -> done\n", check("parent"), source, check("parent")))
	if err := os.MkdirAll(f.Workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f Fixture) Assert(t *testing.T, ctx context.Context, s store.RunStore, parentID string) {
	t.Helper()
	r, err := s.LoadRun(ctx, parentID)
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != store.RunStatusFinished {
		t.Fatalf("parent status=%s error=%s", r.Status, r.Error)
	}
	ids, err := s.ListChildRuns(ctx, parentID)
	if err != nil || len(ids) != 1 {
		t.Fatalf("children=%v err=%v", ids, err)
	}
	child, err := s.LoadRun(ctx, ids[0])
	if err != nil {
		t.Fatal(err)
	}
	wantBundle := filepath.Dir(f.Child)
	if f.Bare {
		wantBundle = ""
	}
	if child.Status != store.RunStatusFinished || child.BundlePath != wantBundle || child.FilePath != f.Child {
		t.Fatalf("child status=%s error=%s bundle=%q file=%q", child.Status, child.Error, child.BundlePath, child.FilePath)
	}
	for _, rel := range []string{"shared.md", "shared/SKILL.md", "directory/SKILL.md"} {
		raw, err := os.ReadFile(filepath.Join(f.Workspace, ".claude", "skills", rel))
		if err != nil || string(raw) != "parent" {
			t.Fatalf("parent resource %s=%q: %v", rel, raw, err)
		}
	}
}
