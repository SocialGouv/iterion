package lane

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func initBaseTestRepo(t *testing.T) string {
	t.Helper()
	root := filepath.Join(os.TempDir(), "stale-base-test-"+time.Now().Format("20060102150405.000000000"))
	os.MkdirAll(root, 0o755)
	t.Cleanup(func() { os.RemoveAll(root) })

	baseGitRun(t, root, "init", "--quiet", "-b", "main")
	baseGitRun(t, root, "config", "user.email", "tests@example.com")
	baseGitRun(t, root, "config", "user.name", "Stale Base Tests")
	os.WriteFile(filepath.Join(root, "init.txt"), []byte("initial\n"), 0o644)
	baseGitRun(t, root, "add", ".")
	baseGitRun(t, root, "commit", "-m", "initial commit", "--quiet")
	return root
}

func baseGitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s failed: %v\n%s", strings.Join(args, " "), err, out)
	}
}

func baseCommitFile(t *testing.T, dir, name, msg string) {
	t.Helper()
	os.WriteFile(filepath.Join(dir, name), []byte(msg+"\n"), 0o644)
	baseGitRun(t, dir, "add", name)
	baseGitRun(t, dir, "commit", "-m", msg, "--quiet")
}

func headSHA(t *testing.T, dir string) string {
	t.Helper()
	cmd := exec.Command("git", "rev-parse", "HEAD")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(out))
}

func TestMatchesWhenHeadEqualsExpectedBase(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	root := initBaseTestRepo(t)
	sha := headSHA(t, root)
	source := &BaseCommitSource{Kind: BaseSourceFlag, Value: sha}

	state := CheckBaseCommit(root, source)

	if state.Kind != BaseMatches {
		t.Errorf("expected Matches, got %s", state.Kind)
	}
}

func TestDivergedWhenHeadMovedPastExpected(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	root := initBaseTestRepo(t)
	oldSHA := headSHA(t, root)
	baseCommitFile(t, root, "extra.txt", "move head forward")
	newSHA := headSHA(t, root)
	source := &BaseCommitSource{Kind: BaseSourceFlag, Value: oldSHA}

	state := CheckBaseCommit(root, source)

	if state.Kind != BaseDiverged {
		t.Fatalf("expected Diverged, got %s", state.Kind)
	}
	if state.Expected != oldSHA || state.Actual != newSHA {
		t.Errorf("expected=%s actual=%s", state.Expected, state.Actual)
	}
}

func TestNoExpectedBaseWhenNil(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	root := initBaseTestRepo(t)

	state := CheckBaseCommit(root, nil)

	if state.Kind != BaseNoExpected {
		t.Errorf("expected NoExpectedBase, got %s", state.Kind)
	}
}

func TestNotAGitRepoWhenOutsideRepo(t *testing.T) {
	root := filepath.Join(os.TempDir(), "not-a-repo-"+time.Now().Format("20060102150405.000000000"))
	os.MkdirAll(root, 0o755)
	t.Cleanup(func() { os.RemoveAll(root) })

	source := &BaseCommitSource{Kind: BaseSourceFlag, Value: "abc1234"}

	state := CheckBaseCommit(root, source)

	if state.Kind != BaseNotAGitRepo {
		t.Errorf("expected NotAGitRepo, got %s", state.Kind)
	}
}

func TestReadClawBaseFile(t *testing.T) {
	root := filepath.Join(os.TempDir(), "claw-base-test-"+time.Now().Format("20060102150405.000000000"))
	os.MkdirAll(root, 0o755)
	t.Cleanup(func() { os.RemoveAll(root) })

	os.WriteFile(filepath.Join(root, ".claw-base"), []byte("abc1234def5678\n"), 0o644)

	value, ok := ReadClawBaseFile(root)
	if !ok || value != "abc1234def5678" {
		t.Errorf("got (%q, %v)", value, ok)
	}
}

func TestReadClawBaseFileMissing(t *testing.T) {
	root := filepath.Join(os.TempDir(), "claw-base-missing-"+time.Now().Format("20060102150405.000000000"))
	os.MkdirAll(root, 0o755)
	t.Cleanup(func() { os.RemoveAll(root) })

	_, ok := ReadClawBaseFile(root)
	if ok {
		t.Error("expected false for missing file")
	}
}

func TestReadClawBaseFileEmpty(t *testing.T) {
	root := filepath.Join(os.TempDir(), "claw-base-empty-"+time.Now().Format("20060102150405.000000000"))
	os.MkdirAll(root, 0o755)
	t.Cleanup(func() { os.RemoveAll(root) })

	os.WriteFile(filepath.Join(root, ".claw-base"), []byte("  \n"), 0o644)

	_, ok := ReadClawBaseFile(root)
	if ok {
		t.Error("expected false for empty file")
	}
}

func TestResolveExpectedBasePrefersFlag(t *testing.T) {
	root := filepath.Join(os.TempDir(), "resolve-base-flag-"+time.Now().Format("20060102150405.000000000"))
	os.MkdirAll(root, 0o755)
	t.Cleanup(func() { os.RemoveAll(root) })

	os.WriteFile(filepath.Join(root, ".claw-base"), []byte("from_file\n"), 0o644)
	flag := "from_flag"

	source := ResolveExpectedBase(&flag, root)

	if source == nil || source.Kind != BaseSourceFlag || source.Value != "from_flag" {
		t.Errorf("source = %+v", source)
	}
}

func TestResolveExpectedBaseFallsBackToFile(t *testing.T) {
	root := filepath.Join(os.TempDir(), "resolve-base-file-"+time.Now().Format("20060102150405.000000000"))
	os.MkdirAll(root, 0o755)
	t.Cleanup(func() { os.RemoveAll(root) })

	os.WriteFile(filepath.Join(root, ".claw-base"), []byte("from_file\n"), 0o644)

	source := ResolveExpectedBase(nil, root)

	if source == nil || source.Kind != BaseSourceFile || source.Value != "from_file" {
		t.Errorf("source = %+v", source)
	}
}

func TestResolveExpectedBaseReturnsNilWhenNothingAvailable(t *testing.T) {
	root := filepath.Join(os.TempDir(), "resolve-base-nil-"+time.Now().Format("20060102150405.000000000"))
	os.MkdirAll(root, 0o755)
	t.Cleanup(func() { os.RemoveAll(root) })

	source := ResolveExpectedBase(nil, root)

	if source != nil {
		t.Errorf("expected nil, got %+v", source)
	}
}

func TestFormatWarningDiverged(t *testing.T) {
	state := &BaseCommitState{
		Kind:     BaseDiverged,
		Expected: "abc1234",
		Actual:   "def5678",
	}

	msg, ok := FormatStaleBaseWarning(state)
	if !ok {
		t.Fatal("expected warning")
	}
	if !strings.Contains(msg, "abc1234") || !strings.Contains(msg, "def5678") {
		t.Errorf("msg = %s", msg)
	}
	if !strings.Contains(msg, "stale codebase") {
		t.Errorf("msg missing 'stale codebase': %s", msg)
	}
}

func TestFormatWarningReturnsNoneForMatches(t *testing.T) {
	state := &BaseCommitState{Kind: BaseMatches}
	_, ok := FormatStaleBaseWarning(state)
	if ok {
		t.Error("expected no warning for Matches")
	}
}

func TestFormatWarningReturnsNoneForNoExpected(t *testing.T) {
	state := &BaseCommitState{Kind: BaseNoExpected}
	_, ok := FormatStaleBaseWarning(state)
	if ok {
		t.Error("expected no warning for NoExpectedBase")
	}
}

func TestMatchesWithClawBaseFileInRealRepo(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	root := initBaseTestRepo(t)
	sha := headSHA(t, root)
	os.WriteFile(filepath.Join(root, ".claw-base"), []byte(sha+"\n"), 0o644)
	source := ResolveExpectedBase(nil, root)

	state := CheckBaseCommit(root, source)

	if state.Kind != BaseMatches {
		t.Errorf("expected Matches, got %s", state.Kind)
	}
}

func TestDivergedWithClawBaseFileAfterNewCommit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	root := initBaseTestRepo(t)
	oldSHA := headSHA(t, root)
	os.WriteFile(filepath.Join(root, ".claw-base"), []byte(oldSHA+"\n"), 0o644)
	baseCommitFile(t, root, "new.txt", "advance head")
	newSHA := headSHA(t, root)
	source := ResolveExpectedBase(nil, root)

	state := CheckBaseCommit(root, source)

	if state.Kind != BaseDiverged {
		t.Fatalf("expected Diverged, got %s", state.Kind)
	}
	if state.Expected != oldSHA || state.Actual != newSHA {
		t.Errorf("expected=%s actual=%s", state.Expected, state.Actual)
	}
}
