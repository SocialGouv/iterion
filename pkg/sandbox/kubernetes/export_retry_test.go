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

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/sandbox"
)

// measuredRaceStderr is the stderr a real export returned when a git process
// was still writing in the pod: tar's warning, then the trailer `kubectl exec`
// adds for a remote exit 1. The fake kubectl reproduces it verbatim — a shim
// that drops the trailer tests a predicate production never meets.
const measuredRaceStderr = "tar: ./.git: file changed as we read it\n" + kubectlRemoteExit1

// exportShim puts a fake kubectl on PATH for ExportWorkspace. Each call archives
// src the way the in-pod tar would and counts itself; the first failFor calls
// then write stderr and exit 1.
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
// loose refs and removes raced leftovers under the RESOLVED clone root, so the
// resolution is asserted before anything runs: a root this test does not own
// stops the test.
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

// podTree writes files (path -> content) into a fresh dir: what the pod holds.
func podTree(t *testing.T, files map[string]string) string {
	t.Helper()
	src := t.TempDir()
	for rel, content := range files {
		path := filepath.Join(src, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return src
}

func podWork(t *testing.T) string {
	t.Helper()
	return podTree(t, map[string]string{"note.txt": "pod work"})
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

// fastExportRetries shrinks the pause through the operator's own escape hatch.
func fastExportRetries(t *testing.T) {
	t.Helper()
	t.Setenv("ITERION_SANDBOX_EXPORT_RETRY_PAUSE", "5ms")
}

// TestExportWorkspace_RetriesATarThatRacedAWriter: a git process still
// finishing in the pod makes tar warn and exit 1 over a complete archive, and
// kubectl adds its own trailer. Dropping that export threw the run's in-pod
// work away; one more archive brings it home.
func TestExportWorkspace_RetriesATarThatRacedAWriter(t *testing.T) {
	fastExportRetries(t)
	ws := exportTarget(t)
	calls := exportShim(t, podWork(t), 1, measuredRaceStderr)
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
// accepted half-written. The bound is the operator's to set.
func TestExportWorkspace_RefusesATreeThatKeepsChanging(t *testing.T) {
	fastExportRetries(t)
	t.Setenv("ITERION_SANDBOX_EXPORT_ATTEMPTS", "3")
	ws := exportTarget(t)
	calls := exportShim(t, podWork(t), 99, measuredRaceStderr)
	err := exportRun(ws).ExportWorkspace(context.Background())
	if err == nil || !strings.Contains(err.Error(), "kept changing") {
		t.Fatalf("a workspace that never settled was accepted: %v", err)
	}
	if n := calls(); n != 3 {
		t.Fatalf("pod tar ran %d time(s), want exactly the 3 attempts the operator set", n)
	}
}

// TestExportWorkspace_KeepsTheDefaultBoundOverAnUnusableKnob: a knob that is
// not an integer is reported and the default kept — the export is not the
// place to fail, nor to run unbounded.
func TestExportWorkspace_KeepsTheDefaultBoundOverAnUnusableKnob(t *testing.T) {
	fastExportRetries(t)
	t.Setenv("ITERION_SANDBOX_EXPORT_ATTEMPTS", "many")
	ws := exportTarget(t)
	calls := exportShim(t, podWork(t), 99, measuredRaceStderr)
	if err := exportRun(ws).ExportWorkspace(context.Background()); err == nil {
		t.Fatal("a workspace that never settled was accepted")
	}
	if n := calls(); n != defaultExportAttempts {
		t.Fatalf("pod tar ran %d time(s), want the default %d", n, defaultExportAttempts)
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
// the value the successful one brings — the run's work unreachable by ref. The
// ref also exists loose on the host before the export: the leftover cleanup
// never touches a file the host already had, so only the per-attempt clearing
// can remove it.
func TestExportWorkspace_ClearsLooseRefsBeforeEveryAttempt(t *testing.T) {
	fastExportRetries(t)
	ws := exportTarget(t)
	branch := exec.Command("git", "-C", ws, "branch", "feature")
	branch.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null")
	if out, err := branch.CombinedOutput(); err != nil {
		t.Fatalf("git branch: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(ws, ".git/refs/heads/feature")); err != nil {
		t.Fatalf("the host clone does not carry the loose ref the scenario needs: %v", err)
	}
	const stale, packed = "1111111111111111111111111111111111111111", "2222222222222222222222222222222222222222"
	raced := podTree(t, map[string]string{".git/refs/heads/feature": stale + "\n"})
	settled := podTree(t, map[string]string{".git/packed-refs": packed + " refs/heads/feature\n"})
	exportShimSeq(t, []string{raced, settled}, 1, measuredRaceStderr)
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

// TestExportWorkspace_DropsWhatOnlyTheRacedArchiveWrote: a raced archive
// catches a git operation mid-flight — its lock, its MERGE_HEAD. tar never
// deletes, so without cleanup the host would read an operation still in
// progress ("Unable to create '.git/index.lock': File exists"). The files the
// host already had are never touched, even those the landing archive lacks.
func TestExportWorkspace_DropsWhatOnlyTheRacedArchiveWrote(t *testing.T) {
	fastExportRetries(t)
	ws := exportTarget(t)
	raced := podTree(t, map[string]string{
		".git/index.lock": "lock",
		".git/MERGE_HEAD": "3333333333333333333333333333333333333333\n",
		"note.txt":        "pod work",
	})
	settled := podTree(t, map[string]string{"note.txt": "pod work"})
	exportShimSeq(t, []string{raced, settled}, 1, measuredRaceStderr)
	if err := exportRun(ws).ExportWorkspace(context.Background()); err != nil {
		t.Fatalf("export failed: %v", err)
	}
	for _, leftover := range []string{".git/index.lock", ".git/MERGE_HEAD"} {
		if _, err := os.Stat(filepath.Join(ws, leftover)); err == nil {
			t.Fatalf("%s from the raced archive survived the retry — host-side git reads an operation in progress", leftover)
		}
	}
	if _, err := os.Stat(filepath.Join(ws, ".git/HEAD")); err != nil {
		t.Fatalf("the host's own .git/HEAD was removed — the cleanup touched a file the export did not write: %v", err)
	}
}

// TestOnlyTarRaceWarnings pins the one failure the export retries.
func TestOnlyTarRaceWarnings(t *testing.T) {
	exitWith := func(code int) error {
		err := exec.Command("sh", "-c", "exit "+strconv.Itoa(code)).Run()
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			t.Fatalf("sh exit %d gave %v", code, err)
		}
		return err
	}
	for _, tc := range []struct {
		name   string
		err    error
		stderr string
		want   bool
	}{
		{"the measured stderr, kubectl trailer included", exitWith(1), measuredRaceStderr, true},
		{"a file listed then removed", exitWith(1), "tar: ./.git/index.lock: File removed before we read it\n" + kubectlRemoteExit1, true},
		{"several racing writers", exitWith(1), "tar: ./.git: file changed as we read it\ntar: ./docs/page.md: file changed as we read it", true},
		{"a racing writer and another tar error", exitWith(1), "tar: ./.git: file changed as we read it\ntar: ./x: Cannot open: Permission denied\n" + kubectlRemoteExit1, false},
		{"the kubectl trailer alone", exitWith(1), kubectlRemoteExit1, false},
		{"kubectl's own failure", exitWith(1), "error: unable to upgrade connection", false},
		{"no stderr at all", exitWith(1), "", false},
		{"a fatal tar exit", exitWith(2), measuredRaceStderr, false},
		{"not an exit status", errors.New("pipe broke"), measuredRaceStderr, false},
		{"no error", nil, measuredRaceStderr, false},
	} {
		if got := onlyTarRaceWarnings(tc.err, tc.stderr); got != tc.want {
			t.Errorf("%s: onlyTarRaceWarnings = %v, want %v", tc.name, got, tc.want)
		}
	}
}
