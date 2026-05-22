package runtime

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseConflictHunks_TwoWay(t *testing.T) {
	content := `alpha
<<<<<<< HEAD
ours-1
ours-2
=======
theirs-1
>>>>>>> feature
zeta
`
	hunks := parseConflictHunks(content)
	if len(hunks) != 1 {
		t.Fatalf("hunks=%d, want 1", len(hunks))
	}
	h := hunks[0]
	if h.StartLine != 2 || h.EndLine != 7 {
		t.Errorf("StartLine=%d EndLine=%d, want 2/7", h.StartLine, h.EndLine)
	}
	if h.OursLabel != "HEAD" {
		t.Errorf("OursLabel=%q, want HEAD", h.OursLabel)
	}
	if h.TheirsLabel != "feature" {
		t.Errorf("TheirsLabel=%q, want feature", h.TheirsLabel)
	}
	if strings.Join(h.OursLines, "/") != "ours-1/ours-2" {
		t.Errorf("OursLines=%v", h.OursLines)
	}
	if strings.Join(h.TheirsLines, "/") != "theirs-1" {
		t.Errorf("TheirsLines=%v", h.TheirsLines)
	}
	if len(h.BaseLines) != 0 {
		t.Errorf("BaseLines should be empty for 2-way merge, got %v", h.BaseLines)
	}
}

func TestParseConflictHunks_Diff3(t *testing.T) {
	content := `<<<<<<< HEAD
ours
||||||| base
base-line
=======
theirs
>>>>>>> feature
`
	hunks := parseConflictHunks(content)
	if len(hunks) != 1 {
		t.Fatalf("hunks=%d, want 1", len(hunks))
	}
	if strings.Join(hunks[0].BaseLines, "/") != "base-line" {
		t.Errorf("BaseLines=%v, want [base-line]", hunks[0].BaseLines)
	}
	if strings.Join(hunks[0].OursLines, "/") != "ours" {
		t.Errorf("OursLines=%v", hunks[0].OursLines)
	}
	if strings.Join(hunks[0].TheirsLines, "/") != "theirs" {
		t.Errorf("TheirsLines=%v", hunks[0].TheirsLines)
	}
}

func TestParseConflictHunks_MultipleHunks(t *testing.T) {
	content := `line-a
<<<<<<< HEAD
ours-a
=======
theirs-a
>>>>>>> br
middle
<<<<<<< HEAD
ours-b
=======
theirs-b
>>>>>>> br
end
`
	hunks := parseConflictHunks(content)
	if len(hunks) != 2 {
		t.Fatalf("hunks=%d, want 2", len(hunks))
	}
	if hunks[0].OursLines[0] != "ours-a" || hunks[1].OursLines[0] != "ours-b" {
		t.Errorf("hunk content mismatch: %v / %v", hunks[0], hunks[1])
	}
}

func TestParseConflictHunks_StrayMarker(t *testing.T) {
	// Lone <<<<<<< without closer should not panic and should yield
	// zero complete hunks.
	content := "alpha\n<<<<<<< HEAD\nours\nbeta\n"
	hunks := parseConflictHunks(content)
	if len(hunks) != 0 {
		t.Errorf("hunks=%d, want 0 for incomplete marker", len(hunks))
	}
}

func TestParseConflictHunks_Context(t *testing.T) {
	content := `a
b
c
<<<<<<< HEAD
ours
=======
theirs
>>>>>>> br
x
y
z
`
	hunks := parseConflictHunks(content)
	if len(hunks) != 1 {
		t.Fatalf("hunks=%d, want 1", len(hunks))
	}
	if strings.Join(hunks[0].ContextBefore, "/") != "a/b/c" {
		t.Errorf("ContextBefore=%v", hunks[0].ContextBefore)
	}
	if strings.Join(hunks[0].ContextAfter, "/") != "x/y/z" {
		t.Errorf("ContextAfter=%v", hunks[0].ContextAfter)
	}
}

// TestParseConflicts_EndToEnd drives the parser against a real git
// repo with a real `git merge --squash` conflict to make sure the
// `git ls-files -u` path picks up the expected files.
func TestParseConflicts_EndToEnd(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t.t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t.t",
			"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
			"LC_ALL=C",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\noutput: %s", args, err, string(out))
		}
	}
	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	run("init", "-q", "-b", "main")
	run("config", "user.email", "t@t.t")
	run("config", "user.name", "t")
	run("config", "commit.gpgsign", "false")
	write("file.txt", "alpha\nbravo\ncharlie\n")
	run("add", "file.txt")
	run("commit", "-qm", "base")

	run("checkout", "-qb", "feature")
	write("file.txt", "alpha\nBRAVO-FEATURE\ncharlie\n")
	run("commit", "-qam", "feat")

	run("checkout", "-q", "main")
	write("file.txt", "alpha\nbravo-main\ncharlie\n")
	run("commit", "-qam", "main-change")

	// Squash into main; conflict expected.
	cmd := exec.Command("git", "merge", "--squash", "feature")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	_, _ = cmd.CombinedOutput() // intentionally ignore the non-zero exit

	det, err := ParseConflicts(dir)
	if err != nil {
		t.Fatalf("ParseConflicts: %v", err)
	}
	if len(det.Files) != 1 {
		t.Fatalf("Files=%d, want 1", len(det.Files))
	}
	cf := det.Files[0]
	if cf.Path != "file.txt" {
		t.Errorf("Path=%q, want file.txt", cf.Path)
	}
	if len(cf.Hunks) != 1 {
		t.Errorf("Hunks=%d, want 1", len(cf.Hunks))
	}
	if !strings.Contains(cf.Content, "<<<<<<<") || !strings.Contains(cf.Content, ">>>>>>>") {
		t.Errorf("Content missing markers: %q", cf.Content)
	}

	// Resolve via StageResolvedFile and finalize.
	resolved := "alpha\nresolved\ncharlie\n"
	if err := StageResolvedFile(dir, "file.txt", resolved); err != nil {
		t.Fatalf("StageResolvedFile: %v", err)
	}
	det2, err := ParseConflicts(dir)
	if err != nil {
		t.Fatalf("ParseConflicts post-stage: %v", err)
	}
	if len(det2.Files) != 0 {
		t.Errorf("after stage Files=%d, want 0", len(det2.Files))
	}
	sha, err := FinalizeConflictMerge(dir, "resolved squash")
	if err != nil {
		t.Fatalf("FinalizeConflictMerge: %v", err)
	}
	if sha == "" {
		t.Error("expected non-empty SHA after finalize")
	}
}

func TestStageResolvedFile_RejectsTraversal(t *testing.T) {
	if err := StageResolvedFile(t.TempDir(), "../escape", "x"); err == nil {
		t.Error("expected error for path traversal")
	}
}
