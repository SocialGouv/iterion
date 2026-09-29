package kubernetes

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/sandbox"
)

// tarRacedWriter is the stderr GNU tar leaves when a file changed while it was
// archived: the archive is complete, the exit status is 1.
const tarRacedWriter = "tar: ./.git: file changed as we read it"

// exportShim puts a fake kubectl on PATH for ExportWorkspace. Each call archives
// src the way the in-pod tar would and counts itself; the first failFor calls
// then write stderr and exit 1 — a racing writer's shape, or kubectl's own
// failure, depending on the text.
func exportShim(t *testing.T, src string, failFor int, stderr string) (calls func() int) {
	t.Helper()
	return exportShimSeq(t, []string{src}, failFor, stderr)
}

// exportShimSeq archives srcs[n-1] on call n (the last one from then on), so a
// test can change what the pod holds between two attempts.
func exportShimSeq(t *testing.T, srcs []string, failFor int, stderr string) (calls func() int) {
	t.Helper()
	if _, err := exec.LookPath("tar"); err != nil {
		t.Skip("tar not on PATH")
	}
	shimDir := t.TempDir()
	counter := filepath.Join(shimDir, "calls")
	errFile := filepath.Join(shimDir, "stderr")
	if err := os.WriteFile(errFile, []byte(stderr+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	shim := "#!/bin/sh\n" +
		"n=$(cat '" + counter + "' 2>/dev/null || echo 0); n=$((n+1)); echo \"$n\" > '" + counter + "'\n" +
		"case \"$n\" in\n" + srcCases(srcs) + "esac\n" +
		"tar -C \"$src\" -cf - .\n" +
		"if [ \"$n\" -le " + strconv.Itoa(failFor) + " ]; then cat '" + errFile + "' >&2; exit 1; fi\n" +
		"exit 0\n"
	if err := os.WriteFile(filepath.Join(shimDir, kubeBinaryName), []byte(shim), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", shimDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return func() int {
		raw, err := os.ReadFile(counter)
		if err != nil {
			return 0
		}
		n, _ := strconv.Atoi(strings.TrimSpace(string(raw)))
		return n
	}
}

// srcCases renders the shell case arms that pick the archived tree per call.
func srcCases(srcs []string) string {
	var b strings.Builder
	for i, src := range srcs[:len(srcs)-1] {
		b.WriteString("  " + strconv.Itoa(i+1) + ") src='" + src + "';;\n")
	}
	b.WriteString("  *) src='" + srcs[len(srcs)-1] + "';;\n")
	return b.String()
}

// exportTarget is a fresh clone under the test's temp dir. The export clears
// loose refs under the RESOLVED clone root, so the resolution is asserted
// before anything runs: a root this test does not own stops the test.
func exportTarget(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	ws := filepath.Join(t.TempDir(), "clone")
	for _, args := range [][]string{{"init", "-q", ws}, {"-C", ws, "commit", "-q", "--allow-empty", "-m", "init"}} {
		cmd := exec.Command("git", args...)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t", "GIT_CONFIG_GLOBAL=/dev/null")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	want, err := filepath.EvalSymlinks(ws)
	if err != nil {
		t.Fatal(err)
	}
	got, err := filepath.EvalSymlinks(resolveCloneRoot(context.Background(), ws))
	if err != nil || got != want {
		t.Fatalf("resolveCloneRoot(%q) = %q (%v) — refusing to export over a tree this test does not own", ws, got, err)
	}
	return ws
}

// podWork is the pod workspace the shim archives: one file the host must end
// up with once the export succeeds.
func podWork(t *testing.T) string {
	t.Helper()
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "note.txt"), []byte("pod work"), 0o644); err != nil {
		t.Fatal(err)
	}
	return src
}

func exportRun(ws string) *Run {
	return &Run{
		driver:    &Driver{logger: iterlog.Nop()},
		podName:   "pod",
		namespace: "ns",
		prepared:  &Prepared{workspace: "/workspace"},
		info:      sandbox.RunInfo{WorkspacePath: ws},
	}
}

func fastExportRetries(t *testing.T) {
	t.Helper()
	prev := exportRetryPause
	exportRetryPause = 10 * time.Millisecond
	t.Cleanup(func() { exportRetryPause = prev })
}

// TestExportWorkspace_RetriesATarThatRacedAWriter: a git process still
// finishing in the pod makes tar warn and exit 1 over a complete archive.
// Dropping that export threw the run's in-pod work away; one more archive
// brings it home.
func TestExportWorkspace_RetriesATarThatRacedAWriter(t *testing.T) {
	fastExportRetries(t)
	ws := exportTarget(t)
	calls := exportShim(t, podWork(t), 1, tarRacedWriter)
	if err := exportRun(ws).ExportWorkspace(context.Background()); err != nil {
		t.Fatalf("an export whose only failure was a racing writer was dropped: %v", err)
	}
	if n := calls(); n != 2 {
		t.Fatalf("pod tar ran %d time(s), want 2 (the raced archive, then a clean one)", n)
	}
	if got, err := os.ReadFile(filepath.Join(ws, "note.txt")); err != nil || string(got) != "pod work" {
		t.Fatalf("the pod's work did not land on the host: %q, %v", got, err)
	}
}

// TestExportWorkspace_RefusesATreeThatKeepsChanging: the retry is bounded — a
// workspace that never settles is still an export failure, never an archive
// accepted half-written.
func TestExportWorkspace_RefusesATreeThatKeepsChanging(t *testing.T) {
	fastExportRetries(t)
	ws := exportTarget(t)
	calls := exportShim(t, podWork(t), 99, tarRacedWriter)
	err := exportRun(ws).ExportWorkspace(context.Background())
	if err == nil || !strings.Contains(err.Error(), "kept changing") {
		t.Fatalf("a workspace that never settled was accepted: %v", err)
	}
	if n := calls(); n != exportAttempts {
		t.Fatalf("pod tar ran %d time(s), want exactly %d", n, exportAttempts)
	}
}

// TestExportWorkspace_NeverRetriesAKubectlFailure: kubectl fails with exit 1
// too; its own error is not a racing writer and is reported at once.
func TestExportWorkspace_NeverRetriesAKubectlFailure(t *testing.T) {
	fastExportRetries(t)
	ws := exportTarget(t)
	calls := exportShim(t, podWork(t), 99, "error: unable to upgrade connection: container not found (\"workload\")")
	err := exportRun(ws).ExportWorkspace(context.Background())
	if err == nil || !strings.Contains(err.Error(), "unable to upgrade connection") {
		t.Fatalf("a kubectl failure was not reported as such: %v", err)
	}
	if n := calls(); n != 1 {
		t.Fatalf("pod tar ran %d time(s), want 1 — only a racing writer is worth a retry", n)
	}
}

// TestExportWorkspace_ClearsLooseRefsBeforeEveryAttempt: between two attempts
// a pod-side gc may pack a ref the first extract wrote LOOSE. Git reads loose
// before packed, so a loose file left over from the raced archive would shadow
// the value the successful one brings — the run's work unreachable by ref.
func TestExportWorkspace_ClearsLooseRefsBeforeEveryAttempt(t *testing.T) {
	fastExportRetries(t)
	ws := exportTarget(t)
	const stale, packed = "1111111111111111111111111111111111111111", "2222222222222222222222222222222222222222"
	raced := t.TempDir()
	if err := os.MkdirAll(filepath.Join(raced, ".git/refs/heads"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(raced, ".git/refs/heads/feature"), []byte(stale+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	settled := t.TempDir()
	if err := os.MkdirAll(filepath.Join(settled, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(settled, ".git/packed-refs"), []byte(packed+" refs/heads/feature\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	exportShimSeq(t, []string{raced, settled}, 1, tarRacedWriter)
	if err := exportRun(ws).ExportWorkspace(context.Background()); err != nil {
		t.Fatalf("export failed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(ws, ".git/refs/heads/feature")); err == nil {
		t.Fatalf("the raced attempt's LOOSE ref survived the retry — it shadows the packed value the successful archive brought")
	}
	raw, err := os.ReadFile(filepath.Join(ws, ".git/packed-refs"))
	if err != nil || !strings.Contains(string(raw), packed) {
		t.Fatalf("the successful archive's packed ref did not land: %q, %v", raw, err)
	}
}

// TestOnlyFileChangedWarnings pins the one failure the export retries.
func TestOnlyFileChangedWarnings(t *testing.T) {
	exitWith := func(code int) error {
		err := exec.Command("sh", "-c", "exit "+strconv.Itoa(code)).Run()
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			t.Fatalf("sh exit %d gave %v", code, err)
		}
		return err
	}
	two := tarRacedWriter + "\n" + "tar: ./docs/page.md: file changed as we read it"
	for _, tc := range []struct {
		name   string
		err    error
		stderr string
		want   bool
	}{
		{"one racing writer", exitWith(1), tarRacedWriter, true},
		{"several racing writers", exitWith(1), two, true},
		{"a racing writer and another tar error", exitWith(1), tarRacedWriter + "\ntar: ./x: Cannot open: Permission denied", false},
		{"kubectl's own failure", exitWith(1), "error: unable to upgrade connection", false},
		{"no stderr at all", exitWith(1), "", false},
		{"a fatal tar exit", exitWith(2), tarRacedWriter, false},
		{"not an exit status", errors.New("pipe broke"), tarRacedWriter, false},
		{"no error", nil, tarRacedWriter, false},
	} {
		if got := onlyFileChangedWarnings(tc.err, tc.stderr); got != tc.want {
			t.Errorf("%s: onlyFileChangedWarnings = %v, want %v", tc.name, got, tc.want)
		}
	}
}
