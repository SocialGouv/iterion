package runtime

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/SocialGouv/iterion/pkg/store"
)

// A handle that stands for a MISSING one grants nothing: the zero value of
// the guarantee object is the refusal. Every operation fails typed, so a
// caller that forgot to carry the pass cannot become a passthrough.
func TestWorkspaceRoot_ARefusingHandleFailsClosed(t *testing.T) {
	wr := refusingWorkspaceRoot()
	target := filepath.Join(t.TempDir(), "anything")
	checks := []struct {
		name string
		op   func() error
	}{
		{"MkdirAll", func() error { return wr.MkdirAll(target, 0o755) }},
		{"RemoveAll", func() error { return wr.RemoveAll(target) }},
		{"Remove", func() error { return wr.Remove(target) }},
		{"WriteFile", func() error { return wr.WriteFile(target, []byte("x"), 0o644) }},
		{"WriteFileAtomic", func() error { return wr.WriteFileAtomic(target, []byte("x"), 0o644) }},
		{"Chmod", func() error { return wr.Chmod(target, 0o644) }},
		{"Symlink", func() error { return wr.Symlink("x", target) }},
		{"CopyFrom", func() error { return wr.CopyFrom("x", target) }},
		{"CopyWithin", func() error { return wr.CopyWithin("x", target) }},
		{"CopyTreeFrom", func() error { return wr.CopyTreeFrom("x", target) }},
		{"CopyTreeWithin", func() error { return wr.CopyTreeWithin("x", target) }},
		{"RefuseEscapingLink", func() error { return wr.RefuseEscapingLink(target) }},
	}
	for _, c := range checks {
		if err := c.op(); !errors.Is(err, ErrSkillsDestEscapesWorkspace) {
			t.Errorf("%s on a refusing handle returned %v, want %v", c.name, err, ErrSkillsDestEscapesWorkspace)
		}
	}
	if _, err := wr.Stat(target); !errors.Is(err, ErrSkillsDestEscapesWorkspace) {
		t.Errorf("Stat on a refusing handle returned %v", err)
	}
	if _, err := wr.ReadDir(target); !errors.Is(err, ErrSkillsDestEscapesWorkspace) {
		t.Errorf("ReadDir on a refusing handle returned %v", err)
	}
	if _, err := wr.ReadFile(target); !errors.Is(err, ErrSkillsDestEscapesWorkspace) {
		t.Errorf("ReadFile on a refusing handle returned %v", err)
	}
	if wr.Exists(target) || wr.IsRealDir(target) {
		t.Error("a refusing handle answered a probe positively")
	}
}

// The guarantee is the object, not a verdict: a path any component of which
// leaves the workspace refuses, at every operation — write, remove, read,
// stat, list — and nothing outside moves.
func TestWorkspaceRoot_AnEscapingComponentIsRefusedAtEveryOperation(t *testing.T) {
	outside := t.TempDir()
	victim := filepath.Join(outside, "operator-notes.md")
	if err := os.WriteFile(victim, []byte("# the operator's own notes\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	workDir := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(workDir, "linked")); err != nil {
		t.Fatal(err)
	}
	wr, err := openWorkspaceRoot(workDir, store.RunTrustFork)
	if err != nil {
		t.Fatal(err)
	}
	defer wr.Close()

	through := filepath.Join(workDir, "linked", "planted.md")
	checks := []struct {
		name string
		op   func() error
	}{
		{"WriteFile", func() error { return wr.WriteFile(through, []byte("x"), 0o644) }},
		{"WriteFileAtomic", func() error { return wr.WriteFileAtomic(through, []byte("x"), 0o644) }},
		{"MkdirAll", func() error { return wr.MkdirAll(filepath.Join(workDir, "linked", "d"), 0o755) }},
		{"RemoveAll", func() error { return wr.RemoveAll(outside) }},
		{"Remove", func() error { return wr.Remove(victim) }},
		{"Chmod", func() error { return wr.Chmod(victim, 0o600) }},
		{"CopyFrom", func() error {
			src := filepath.Join(t.TempDir(), "src.md")
			if err := os.WriteFile(src, []byte("x"), 0o644); err != nil {
				t.Fatal(err)
			}
			return wr.CopyFrom(src, through)
		}},
		{"CopyTreeFrom", func() error { return wr.CopyTreeFrom(t.TempDir(), filepath.Join(workDir, "linked", "tree")) }},
		{"CopyWithin", func() error { return wr.CopyWithin(victim, filepath.Join(workDir, "in.md")) }},
		{"CopyTreeWithin", func() error {
			return wr.CopyTreeWithin(filepath.Join(workDir, "linked"), filepath.Join(workDir, "tree"))
		}},
		{"Symlink", func() error { return wr.Symlink("target", through) }},
	}
	for _, c := range checks {
		if err := c.op(); !errors.Is(err, ErrSkillsDestEscapesWorkspace) {
			t.Errorf("%s through an escaping component returned %v, want %v", c.name, err, ErrSkillsDestEscapesWorkspace)
		}
	}
	if _, err := wr.ReadFile(victim); !errors.Is(err, ErrSkillsDestEscapesWorkspace) {
		t.Errorf("ReadFile of a path outside returned %v", err)
	}
	if _, err := wr.Stat(through); !errors.Is(err, ErrSkillsDestEscapesWorkspace) {
		t.Errorf("Stat through an escaping component returned %v", err)
	}
	if _, err := wr.ReadDir(filepath.Join(workDir, "linked")); !errors.Is(err, ErrSkillsDestEscapesWorkspace) {
		t.Errorf("ReadDir through an escaping component returned %v", err)
	}
	if wr.Exists(victim) {
		t.Error("Exists answered true for a path outside the workspace")
	}

	if got, rerr := os.ReadFile(victim); rerr != nil || string(got) != "# the operator's own notes\n" {
		t.Errorf("the operator's file moved: err=%v body=%q", rerr, got)
	}
	entries, _ := os.ReadDir(outside)
	if len(entries) != 1 {
		t.Errorf("something outside the workspace was created: %d entries", len(entries))
	}
}

// The check is about provenance, not symlinks: a TRUSTED workspace keeps the
// operator's own arrangement — a linked directory is followed, written
// through, listed, stat'ed, the way the os package does.
func TestWorkspaceRoot_ATrustedHandleKeepsTheOperatorsArrangement(t *testing.T) {
	outside := t.TempDir()
	workDir := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(workDir, "linked")); err != nil {
		t.Fatal(err)
	}
	wr, err := openWorkspaceRoot(workDir, store.RunTrustDefault)
	if err != nil {
		t.Fatal(err)
	}
	defer wr.Close()

	target := filepath.Join(workDir, "linked", "mirrored.md")
	if err := wr.WriteFile(target, []byte("body\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if got, _ := os.ReadFile(filepath.Join(outside, "mirrored.md")); string(got) != "body\n" {
		t.Errorf("the write did not follow the operator's link: %q", got)
	}
	if got, err := wr.ReadFile(target); err != nil || string(got) != "body\n" {
		t.Errorf("ReadFile = %q, %v", got, err)
	}
	if info, err := wr.Stat(target); err != nil || info.IsDir() {
		t.Errorf("Stat = %v", err)
	}
	if !wr.Exists(target) {
		t.Error("Exists = false for the file the handle just wrote")
	}
	if entries, err := wr.ReadDir(filepath.Join(workDir, "linked")); err != nil || len(entries) != 1 {
		t.Errorf("ReadDir = %d entries, %v", len(entries), err)
	}
	if !wr.IsRealDir(outside) {
		t.Error("IsRealDir = false for a real directory")
	}
	if wr.IsRealDir(filepath.Join(workDir, "linked")) {
		t.Error("IsRealDir = true for a symlink to a directory — Lstat is the point")
	}
	if err := wr.RefuseEscapingLink(filepath.Join(workDir, "linked")); err != nil {
		t.Errorf("a trusted handle refused the operator's own link: %v", err)
	}
	if err := wr.Chmod(target, 0o600); err != nil {
		t.Errorf("Chmod: %v", err)
	}
	if err := wr.Symlink("mirrored.md", filepath.Join(workDir, "linked", "alias.md")); err != nil {
		t.Errorf("Symlink: %v", err)
	}
	src := filepath.Join(t.TempDir(), "src.md")
	if err := os.WriteFile(src, []byte("copied\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := wr.CopyFrom(src, filepath.Join(workDir, "linked", "copied.md")); err != nil {
		t.Errorf("CopyFrom: %v", err)
	}
	if err := wr.CopyWithin(filepath.Join(workDir, "linked", "copied.md"), filepath.Join(workDir, "re-carried.md")); err != nil {
		t.Errorf("CopyWithin: %v", err)
	}
	if got, _ := os.ReadFile(filepath.Join(workDir, "re-carried.md")); string(got) != "copied\n" {
		t.Errorf("CopyWithin delivered %q", got)
	}
	if err := os.MkdirAll(filepath.Join(src+"-tree", "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src+"-tree", "sub", "f.md"), []byte("tree\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := wr.CopyTreeFrom(src+"-tree", filepath.Join(workDir, "tree")); err != nil {
		t.Errorf("CopyTreeFrom: %v", err)
	}
	if err := wr.CopyTreeWithin(filepath.Join(workDir, "tree"), filepath.Join(workDir, "tree2")); err != nil {
		t.Errorf("CopyTreeWithin: %v", err)
	}
	if got, _ := os.ReadFile(filepath.Join(workDir, "tree2", "sub", "f.md")); string(got) != "tree\n" {
		t.Errorf("CopyTreeWithin delivered %q", got)
	}
	if err := wr.WriteFileAtomic(target, []byte("atomic\n"), 0o644); err != nil {
		t.Errorf("WriteFileAtomic: %v", err)
	}
	if err := wr.Remove(filepath.Join(workDir, "re-carried.md")); err != nil {
		t.Errorf("Remove: %v", err)
	}
	if err := wr.RemoveAll(filepath.Join(workDir, "tree2")); err != nil {
		t.Errorf("RemoveAll: %v", err)
	}
}

// RefuseEscapingLink judges "does it leave the workspace", not "does it
// resolve": a dangling link whose target is INSIDE is not an escape, and a
// plain file is not a link at all. Judging resolvability instead is what
// parked a trusted run over a broken link of the operator's own.
func TestWorkspaceRoot_RefuseEscapingLinkJudgesEscapeNotResolution(t *testing.T) {
	workDir := t.TempDir()
	wr, err := openWorkspaceRoot(workDir, store.RunTrustFork)
	if err != nil {
		t.Fatal(err)
	}
	defer wr.Close()

	outside := t.TempDir()
	escaping := filepath.Join(workDir, "escaping")
	if err := os.Symlink(outside, escaping); err != nil {
		t.Fatal(err)
	}
	if err := wr.RefuseEscapingLink(escaping); !errors.Is(err, ErrSkillsDestEscapesWorkspace) {
		t.Errorf("an escaping link returned %v, want %v", err, ErrSkillsDestEscapesWorkspace)
	}

	dangling := filepath.Join(workDir, "dangling")
	if err := os.Symlink(filepath.Join(workDir, "not-there-yet"), dangling); err != nil {
		t.Fatal(err)
	}
	if err := wr.RefuseEscapingLink(dangling); err != nil {
		t.Errorf("a dangling link staying inside was refused: %v", err)
	}

	plain := filepath.Join(workDir, "plain.md")
	if err := os.WriteFile(plain, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := wr.RefuseEscapingLink(plain); err != nil {
		t.Errorf("a plain file was refused: %v", err)
	}
	if err := wr.RefuseEscapingLink(filepath.Join(workDir, "absent")); err != nil {
		t.Errorf("an absent path was refused: %v", err)
	}
}

// Symlink checks its TARGET: os.Root does not ("Symlink does not validate
// oldname"), and a re-created link into the operator's tree would still be one
// even though the next write THROUGH it would be refused.
func TestWorkspaceRoot_SymlinkRefusesAnEscapingTarget(t *testing.T) {
	workDir := t.TempDir()
	wr, err := openWorkspaceRoot(workDir, store.RunTrustFork)
	if err != nil {
		t.Fatal(err)
	}
	defer wr.Close()

	if err := wr.Symlink(t.TempDir(), filepath.Join(workDir, "link")); !errors.Is(err, ErrSkillsDestEscapesWorkspace) {
		t.Errorf("a symlink with an escaping target returned %v", err)
	}
	if err := wr.Symlink("inside.md", filepath.Join(workDir, "ok.md")); err != nil {
		t.Errorf("a symlink staying inside was refused: %v", err)
	}
}

// CopyFrom replaces by rename, on both branches of the handle: an interrupted
// copy never leaves a truncated destination under a marker claiming iterion
// wrote it. The observable shape of "rename, not truncate-in-place": a
// destination that shares its inode with a sibling (a hardlink) is REPLACED,
// and the sibling keeps its bytes — a truncate-in-place copy would have
// written through the link.
func TestCopyFromReplacesRatherThanWritesThrough(t *testing.T) {
	for _, trusted := range []bool{true, false} {
		name := "untrusted handle (os.Root)"
		trust := store.RunTrustFork
		if trusted {
			name = "trusted passthrough"
			trust = store.RunTrustDefault
		}
		t.Run(name, func(t *testing.T) {
			workDir := t.TempDir()
			src := filepath.Join(t.TempDir(), "src.md")
			if err := os.WriteFile(src, []byte("new body\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			sibling := filepath.Join(workDir, "sibling.md")
			if err := os.WriteFile(sibling, []byte("old body\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			dst := filepath.Join(workDir, "dst.md")
			if err := os.Link(sibling, dst); err != nil {
				t.Skipf("hardlinks unavailable: %v", err)
			}

			wr, err := openWorkspaceRoot(workDir, trust)
			if err != nil {
				t.Fatal(err)
			}
			defer wr.Close()
			if err := wr.CopyFrom(src, dst); err != nil {
				t.Fatalf("CopyFrom: %v", err)
			}

			if got, _ := os.ReadFile(dst); string(got) != "new body\n" {
				t.Errorf("dst = %q, want the source's bytes", got)
			}
			if got, _ := os.ReadFile(sibling); string(got) != "old body\n" {
				t.Errorf("the sibling changed (%q) — the copy wrote through the hardlink instead of replacing by rename", got)
			}
			// The source's mode is carried: a 0600 helper is not widened to
			// 0644.
			info, err := os.Stat(dst)
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode().Perm() != 0o600 {
				t.Errorf("dst mode = %o, want 0600, the source's own", info.Mode().Perm())
			}
			// No staging residue.
			if entries, _ := filepath.Glob(filepath.Join(workDir, "*atomic*")); len(entries) != 0 {
				t.Errorf("staging files left behind: %v", entries)
			}
		})
	}
}

// Two mirrors of one destination run concurrently across branches of a
// fan-out: a FIXED staging name let one rename the other's half-written temp
// from under it ("renameat: no such file"). The run stayed fail-closed — the
// copy errors, nothing corrupt is served — but the window existed by
// construction. The temp name is now random per call.
func TestCopyFromConcurrentMirrorsShareNoTempName(t *testing.T) {
	workDir := t.TempDir()
	src := filepath.Join(t.TempDir(), "src.md")
	if err := os.WriteFile(src, []byte("body\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(workDir, "dst.md")
	wr, err := openWorkspaceRoot(workDir, store.RunTrustFork)
	if err != nil {
		t.Fatal(err)
	}
	defer wr.Close()

	var wg sync.WaitGroup
	errs := make(chan error, 120)
	for g := 0; g < 2; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 60; i++ {
				errs <- wr.CopyFrom(src, dst)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("a concurrent copy failed: %v", err)
		}
	}
	if got, _ := os.ReadFile(dst); string(got) != "body\n" {
		t.Errorf("dst = %q, want the source's bytes", got)
	}
}

// A rooted CopyTreeFrom keeps every source file's own mode, per file.
func TestCopyTreeFrom_CarriesEachSourcesOwnMode(t *testing.T) {
	srcDir := t.TempDir()
	plain := filepath.Join(srcDir, "plain.md")
	secret := filepath.Join(srcDir, "secret.md")
	if err := os.WriteFile(plain, []byte("p\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(secret, []byte("s\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	workDir := t.TempDir()
	wr, err := openWorkspaceRoot(workDir, store.RunTrustFork)
	if err != nil {
		t.Fatal(err)
	}
	defer wr.Close()
	dst := filepath.Join(workDir, "tree")
	if err := wr.CopyTreeFrom(srcDir, dst); err != nil {
		t.Fatalf("CopyTreeFrom: %v", err)
	}
	for name, want := range map[string]os.FileMode{"plain.md": 0o644, "secret.md": 0o600} {
		info, err := os.Stat(filepath.Join(dst, name))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != want {
			t.Errorf("%s mode = %o, want %o", name, info.Mode().Perm(), want)
		}
	}
}

// The rooted CopyTreeWithin skips a planted non-regular entry rather than
// following it: the mirror writes plain files, so anything else is not the
// mirror's.
func TestCopyTreeWithin_SkipsAPlantedLink(t *testing.T) {
	workDir := t.TempDir()
	src := filepath.Join(workDir, "src")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "real.md"), []byte("real\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "operator.md"), []byte("o\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "operator.md"), filepath.Join(src, "planted.md")); err != nil {
		t.Fatal(err)
	}

	wr, err := openWorkspaceRoot(workDir, store.RunTrustFork)
	if err != nil {
		t.Fatal(err)
	}
	defer wr.Close()
	dst := filepath.Join(workDir, "dst")
	if err := wr.CopyTreeWithin(src, dst); err != nil {
		t.Fatalf("CopyTreeWithin: %v", err)
	}
	if got, _ := os.ReadFile(filepath.Join(dst, "real.md")); string(got) != "real\n" {
		t.Errorf("the real file did not cross: %q", got)
	}
	if _, err := os.Lstat(filepath.Join(dst, "planted.md")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the planted link was carried (err=%v)", err)
	}
}
