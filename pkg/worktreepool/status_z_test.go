package worktreepool

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/SocialGouv/iterion/internal/gittest"
)

// #1577: the line-oriented worktreeStatus cut a rename at the first " -> ",
// so a source literally named `x -> y.md` — printed UNQUOTED by git, the
// name holding no byte it escapes — was judged instead of the destination.
// The destination decides whether the entry is the mirror's own: a staged
// rename INTO the managed bookkeeping is scaffold, a rename to an ordinary
// path is someone's work. The probe now reads the `-z` porcelain, where the
// two are separate fields; these run end to end against real git.
func TestWorktreeStatusARenameSourceHoldingAnArrow(t *testing.T) {
	setup := func(t *testing.T) string {
		t.Helper()
		dir := t.TempDir()
		gittest.InitRepo(t, dir)
		if err := os.WriteFile(filepath.Join(dir, "x -> y.md"), []byte("content\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		gittest.Run(t, dir, "add", "-A")
		gittest.Run(t, dir, "commit", "-qm", "track the arrowed name")
		return dir
	}

	t.Run("renamed into the mirror's bookkeeping: not dirty", func(t *testing.T) {
		dir := setup(t)
		if err := os.MkdirAll(filepath.Join(dir, ".claude", "skills", ".iterion-managed"), 0o755); err != nil {
			t.Fatal(err)
		}
		gittest.Run(t, dir, "mv", "x -> y.md", filepath.Join(".claude", "skills", ".iterion-managed", "z.md"))
		dirty, _, err := worktreeStatus(context.Background(), dir, false, false)
		if err != nil {
			t.Fatal(err)
		}
		if dirty {
			t.Fatalf("a rename into the managed bookkeeping read as someone's work — the source was cut at its own arrow")
		}
	})

	t.Run("renamed to an ordinary path: dirty", func(t *testing.T) {
		dir := setup(t)
		gittest.Run(t, dir, "mv", "x -> y.md", "z.md")
		dirty, _, err := worktreeStatus(context.Background(), dir, false, false)
		if err != nil {
			t.Fatal(err)
		}
		if !dirty {
			t.Fatalf("a rename to an ordinary path read as clean — the destination was never seen")
		}
	})
}
