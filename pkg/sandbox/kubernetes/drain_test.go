package kubernetes

import (
	"archive/tar"
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/sandbox"
)

// drainRun builds a Run whose pod-side state the kubectl shim it is handed
// answers for, carrying the given drain manifest. A run that only extracts
// host-side spawns nothing and is handed no kubectl.
func drainRun(dirs []sandbox.ReadBackDir, kubectl Kubectl) *Run {
	return &Run{
		driver:    &Driver{logger: iterlog.Nop(), kubectl: kubectl},
		podName:   "pod",
		namespace: "ns",
		prepared:  &Prepared{workspace: "/workspace"},
		info:      sandbox.RunInfo{ReadBackDirs: dirs},
	}
}

// The read-back drain (ADR-143 §3): a pod's run-files tree is driver-owned
// storage; the drain is what turns it into recoverable deliverables. Each
// test here reddens on the half of the drain it pins.

func TestExportRunFilesDrainsTheDeclaredTreeIntoTheRunFileStore(t *testing.T) {
	podTree := t.TempDir()
	if err := os.WriteFile(filepath.Join(podTree, "report.md"), []byte("# audit"), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(podTree, "out"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(podTree, "out", "deep.bin"), []byte{1, 2, 3}, 0o600); err != nil {
		t.Fatal(err)
	}
	hostDst := t.TempDir()
	kubectl, calls := exportShim(t, podTree, 0, "")
	r := drainRun([]sandbox.ReadBackDir{{ContainerPath: "/iterion/artifact-files", HostDir: hostDst}}, kubectl)

	if err := r.ExportRunFiles(context.Background()); err != nil {
		t.Fatalf("ExportRunFiles = %v, want the pod tree on the host", err)
	}
	if got, err := os.ReadFile(filepath.Join(hostDst, "report.md")); err != nil || string(got) != "# audit" {
		t.Errorf("drained report.md = %q (%v), want the pod's bytes on the host", got, err)
	}
	if got, err := os.ReadFile(filepath.Join(hostDst, "out", "deep.bin")); err != nil || len(got) != 3 {
		t.Errorf("drained out/deep.bin = %v (%v), want the nested file landed whole", got, err)
	}
	if calls() != 1 {
		t.Errorf("the drain archived the pod %d times, want once", calls())
	}
}

func TestExportRunFilesWithoutADeclarationTouchesNothing(t *testing.T) {
	kubectl, calls := exportShim(t, t.TempDir(), 0, "")
	r := drainRun(nil, kubectl)

	if err := r.ExportRunFiles(context.Background()); err != nil {
		t.Fatalf("ExportRunFiles = %v, want an honest no-op", err)
	}
	if calls() != 0 {
		t.Errorf("a run that declared no read-back path invoked the driver %d times, want zero", calls())
	}
}

func TestExportRunFilesJoinsEveryDeclaredDirFailure(t *testing.T) {
	first, second := t.TempDir(), t.TempDir()
	// One archive succeeds, the other's kubectl dies: a run declaring two
	// paths loses neither report when one tree fails.
	kubectl, calls := exportShimSeq(t, []string{t.TempDir(), t.TempDir()}, 1, "boom")
	r := drainRun([]sandbox.ReadBackDir{
		{ContainerPath: "/iterion/artifact-files", HostDir: first},
		{ContainerPath: "/iterion/artifact-files-2", HostDir: second},
	}, kubectl)

	err := r.ExportRunFiles(context.Background())
	if err == nil {
		t.Fatal("ExportRunFiles = nil, want the failed dir's cause")
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Errorf("error = %v, want the failing dir's own cause", err)
	}
	if got := sandbox.ReadBackLossReasonOf(err); got != sandbox.ReadBackArchiver {
		t.Errorf("reason = %q, want %q — kubectl's own failure is the archiver's, and the join of one lost dir reads as that dir's reason", got, sandbox.ReadBackArchiver)
	}
	if calls() != 2 {
		t.Errorf("the drain attempted %d dirs, want both declared ones", calls())
	}
}

// TestExportRunFilesReportsATreeThatNeverSettlesAsUnsettled: a tool still
// writing at teardown makes every attempt race; the drain gives up on the
// operator's bound and says so with its own reason — the outermost one of
// the chain, not the archiver's exit the last attempt wrapped.
func TestExportRunFilesReportsATreeThatNeverSettlesAsUnsettled(t *testing.T) {
	fastExportRetries(t)
	t.Setenv("ITERION_SANDBOX_EXPORT_ATTEMPTS", "3")
	hostDst := t.TempDir()
	kubectl, calls := exportShim(t, podTree(t, map[string]string{"report.md": "# still being written"}), 99, measuredRaceStderr)
	r := drainRun([]sandbox.ReadBackDir{{ContainerPath: "/iterion/artifact-files", HostDir: hostDst}}, kubectl)

	err := r.ExportRunFiles(context.Background())
	if err == nil || !strings.Contains(err.Error(), "kept changing") {
		t.Fatalf("ExportRunFiles = %v, want the tree that never settled refused", err)
	}
	if got := sandbox.ReadBackLossReasonOf(err); got != sandbox.ReadBackUnsettled {
		t.Errorf("reason = %q, want %q", got, sandbox.ReadBackUnsettled)
	}
	if n := calls(); n != 3 {
		t.Errorf("the drain archived the pod %d times, want exactly the 3 attempts the operator set", n)
	}
}

// The teardown race the export measured applies to the drain too: a tool
// appending to a deliverable while the pod's archiver reads makes tar warn
// and exit 1 with the race stderr and nothing else. The drain carries the
// export's discipline — that one failure is retried, the landed set is
// the archive that finally succeeded, and the attempt count tells it.

func TestExportRunFilesRetriesARacedArchive(t *testing.T) {
	raced, settled := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(raced, "report.md"), []byte("# partial"), 0o600); err != nil {
		t.Fatalf("write raced fixture: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(settled, "out"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(settled, "report.md"), []byte("# complete"), 0o600); err != nil {
		t.Fatalf("write settled fixture: %v", err)
	}
	if err := os.WriteFile(filepath.Join(settled, "out", "deep.bin"), []byte{9, 9}, 0o600); err != nil {
		t.Fatal(err)
	}
	hostDst := t.TempDir()
	// Attempt 1 archives the mid-write tree and exits 1 on the measured
	// race stderr; attempt 2 archives the settled tree.
	kubectl, calls := exportShimSeq(t, []string{raced, settled}, 1, measuredRaceStderr)
	r := drainRun([]sandbox.ReadBackDir{{ContainerPath: "/iterion/artifact-files", HostDir: hostDst}}, kubectl)

	if err := r.ExportRunFiles(context.Background()); err != nil {
		t.Fatalf("ExportRunFiles = %v, want the raced drain retried to success", err)
	}
	got, err := os.ReadFile(filepath.Join(hostDst, "report.md"))
	if err != nil || string(got) != "# complete" {
		t.Errorf("drained report.md = %q (%v), want attempt 2's settled archive as the landed set", got, err)
	}
	if got, err := os.ReadFile(filepath.Join(hostDst, "out", "deep.bin")); err != nil || len(got) != 2 {
		t.Errorf("drained out/deep.bin = %v (%v), want the settled tree's member landed", got, err)
	}
	if calls() != 2 {
		t.Errorf("the drain archived the pod %d times, want the raced attempt retried once", calls())
	}
}

// TestExportRunFilesDropsWhatOnlyTheRacedAttemptLanded: tar never deletes,
// so a retry that lands after a raced attempt keeps that attempt's writes —
// a file the pod dropped between the two attempts would survive in the
// run's collected set, and the landed set would be a union, not the archive
// that finally succeeded. The sweep takes only what neither the host dir
// held before the drain nor the successful archive brings.
func TestExportRunFilesDropsWhatOnlyTheRacedAttemptLanded(t *testing.T) {
	raced := podTree(t, map[string]string{
		"report.md": "# partial",
		"stale.txt": "the pod dropped me before the settled archive",
	})
	settled := podTree(t, map[string]string{"report.md": "# complete"})
	hostDst := t.TempDir()
	own := filepath.Join(hostDst, "host-own.txt")
	if err := os.WriteFile(own, []byte("the host already had me"), 0o600); err != nil {
		t.Fatalf("write the host's own file: %v", err)
	}
	// Attempt 1 archives the mid-write tree and exits 1 on the measured
	// race stderr; attempt 2 archives the tree the pod settled on, which
	// no longer carries stale.txt.
	kubectl, calls := exportShimSeq(t, []string{raced, settled}, 1, measuredRaceStderr)
	r := drainRun([]sandbox.ReadBackDir{{ContainerPath: "/iterion/artifact-files", HostDir: hostDst}}, kubectl)

	if err := r.ExportRunFiles(context.Background()); err != nil {
		t.Fatalf("ExportRunFiles = %v, want the raced drain retried to success", err)
	}
	if _, err := os.Stat(filepath.Join(hostDst, "stale.txt")); err == nil {
		t.Fatal("a file the pod dropped between attempts survived the retried drain — the landed set is not the archive that finally succeeded")
	}
	got, err := os.ReadFile(filepath.Join(hostDst, "report.md"))
	if err != nil || string(got) != "# complete" {
		t.Errorf("drained report.md = %q (%v), want attempt 2's settled archive as the landed set", got, err)
	}
	if _, err := os.Stat(own); err != nil {
		t.Fatalf("the sweep removed %s — a file the host already had is never touched: %v", own, err)
	}
	if calls() != 2 {
		t.Errorf("the drain archived the pod %d times, want the raced attempt retried once", calls())
	}
}

// TestExportRunFilesSweepSurvivesATrailingSeparatorHostDir: the sweep's
// containment judgment reads paths, not shapes — a host dir carrying a
// trailing separator names the same directory on the host, so the sweep
// must sweep it as it sweeps the cleaned form, not refuse every
// candidate as outside and fail a drain that landed.
func TestExportRunFilesSweepSurvivesATrailingSeparatorHostDir(t *testing.T) {
	raced := podTree(t, map[string]string{
		"report.md": "# partial",
		"stale.txt": "the pod dropped me before the settled archive",
	})
	settled := podTree(t, map[string]string{"report.md": "# complete"})
	hostDst := t.TempDir() + string(os.PathSeparator)
	own := filepath.Join(hostDst, "host-own.txt")
	if err := os.WriteFile(own, []byte("the host already had me"), 0o600); err != nil {
		t.Fatalf("write the host's own file: %v", err)
	}
	// Attempt 1 archives the mid-write tree and exits 1 on the measured
	// race stderr; attempt 2 archives the settled tree — the sweep then
	// judges stale.txt's containment against the separator-carrying dir.
	kubectl, calls := exportShimSeq(t, []string{raced, settled}, 1, measuredRaceStderr)
	r := drainRun([]sandbox.ReadBackDir{{ContainerPath: "/iterion/artifact-files", HostDir: hostDst}}, kubectl)

	if err := r.ExportRunFiles(context.Background()); err != nil {
		t.Fatalf("ExportRunFiles = %v, want the drain to land over a trailing-separator host dir", err)
	}
	if _, err := os.Stat(filepath.Join(hostDst, "stale.txt")); err == nil {
		t.Fatal("a file the pod dropped between attempts survived the retried drain — the sweep never swept the trailing-separator host dir")
	}
	got, err := os.ReadFile(filepath.Join(hostDst, "report.md"))
	if err != nil || string(got) != "# complete" {
		t.Errorf("drained report.md = %q (%v), want attempt 2's settled archive as the landed set", got, err)
	}
	if _, err := os.Stat(own); err != nil {
		t.Fatalf("the sweep removed %s — a file the host already had is never touched: %v", own, err)
	}
	if calls() != 2 {
		t.Errorf("the drain archived the pod %d times, want the raced attempt retried once", calls())
	}
}

// TestExportRunFilesSweepsTheRacedAttemptEmptyDirs: a raced attempt's
// extract builds directories as readily as it writes files — the archive's
// dir members and the parents its file writes need. The file sweep empties
// them; the dir sweep takes the emptied shells, while a dir the settled
// archive carries (even a genuinely empty one) and a dir the host already
// had stand.
func TestExportRunFilesSweepsTheRacedAttemptEmptyDirs(t *testing.T) {
	raced := podTree(t, map[string]string{
		"report.md":            "# partial",
		"stale/inner.md":       "raced into a dir the settled archive never carries",
		"nested/deep/last.txt": "a raced file two dirs deep",
	})
	settled := podTree(t, map[string]string{"report.md": "# complete", "out/deep.bin": "\x09\x09"})
	// The settled tree also carries a genuinely EMPTY dir member — content
	// the settled archive names, whatever its file count.
	if err := os.MkdirAll(filepath.Join(settled, "empty-member"), 0o755); err != nil {
		t.Fatal(err)
	}
	hostDst := t.TempDir()
	// A dir the host already had predates the drain — never a candidate.
	pre := filepath.Join(hostDst, "pre-drain")
	if err := os.MkdirAll(pre, 0o755); err != nil {
		t.Fatal(err)
	}
	// Attempt 1 archives the mid-write tree and exits 1 on the measured
	// race stderr; attempt 2 archives the tree the pod settled on.
	kubectl, calls := exportShimSeq(t, []string{raced, settled}, 1, measuredRaceStderr)
	r := drainRun([]sandbox.ReadBackDir{{ContainerPath: "/iterion/artifact-files", HostDir: hostDst}}, kubectl)

	if err := r.ExportRunFiles(context.Background()); err != nil {
		t.Fatalf("ExportRunFiles = %v, want the raced drain retried to success", err)
	}
	for _, gone := range []string{
		"stale", "stale/inner.md",
		"nested", "nested/deep", "nested/deep/last.txt",
	} {
		if _, err := os.Stat(filepath.Join(hostDst, filepath.FromSlash(gone))); err == nil {
			t.Errorf("%s survived the retried drain — what only the raced attempt built is not the settled archive's content", gone)
		}
	}
	for _, kept := range []string{
		"report.md", "out", "out/deep.bin",
		"empty-member", "pre-drain",
	} {
		if _, err := os.Stat(filepath.Join(hostDst, filepath.FromSlash(kept))); err != nil {
			t.Errorf("%s is missing (%v) — the sweep took settled or pre-drain state", kept, err)
		}
	}
	if calls() != 2 {
		t.Errorf("the drain archived the pod %d times, want the raced attempt retried once", calls())
	}
}

// TestExportRunFilesCuresATypeFlipBetweenAttempts: the same race the
// retry exists for includes a path changing KIND between attempts — a
// raced attempt's extract must not leave a member standing where the
// settled archive carries the other kind, and the retry must cure it
// rather than fail hard on MkdirAll or OpenFile.
func TestExportRunFilesCuresATypeFlipBetweenAttempts(t *testing.T) {
	t.Run("a file the settled archive carries as a dir", func(t *testing.T) {
		raced := podTree(t, map[string]string{"report.md": "# partial"})
		settled := podTree(t, map[string]string{"report.md/child.md": "# settled"})
		hostDst := t.TempDir()
		kubectl, calls := exportShimSeq(t, []string{raced, settled}, 1, measuredRaceStderr)
		r := drainRun([]sandbox.ReadBackDir{{ContainerPath: "/iterion/artifact-files", HostDir: hostDst}}, kubectl)

		if err := r.ExportRunFiles(context.Background()); err != nil {
			t.Fatalf("ExportRunFiles = %v, want the file-to-dir flip cured by the retry", err)
		}
		info, err := os.Stat(filepath.Join(hostDst, "report.md"))
		if err != nil || !info.IsDir() {
			t.Fatalf("drained report.md is not the settled archive's dir (%v)", err)
		}
		got, err := os.ReadFile(filepath.Join(hostDst, "report.md", "child.md"))
		if err != nil || string(got) != "# settled" {
			t.Errorf("drained child.md = %q (%v), want the settled member under the rebuilt dir", got, err)
		}
		if calls() != 2 {
			t.Errorf("the drain archived the pod %d times, want the raced attempt retried once", calls())
		}
	})
	t.Run("a dir the settled archive carries as a file", func(t *testing.T) {
		raced := podTree(t, map[string]string{"sub/inner.md": "# raced"})
		settled := podTree(t, map[string]string{"sub": "# settled as a file"})
		hostDst := t.TempDir()
		kubectl, calls := exportShimSeq(t, []string{raced, settled}, 1, measuredRaceStderr)
		r := drainRun([]sandbox.ReadBackDir{{ContainerPath: "/iterion/artifact-files", HostDir: hostDst}}, kubectl)

		if err := r.ExportRunFiles(context.Background()); err != nil {
			t.Fatalf("ExportRunFiles = %v, want the dir-to-file flip cured by the retry", err)
		}
		got, err := os.ReadFile(filepath.Join(hostDst, "sub"))
		if err != nil || string(got) != "# settled as a file" {
			t.Errorf("drained sub = %q (%v), want the settled archive's regular file", got, err)
		}
		if info, statErr := os.Lstat(filepath.Join(hostDst, "sub")); statErr != nil || !info.Mode().IsRegular() {
			t.Errorf("drained sub is not a regular file (%v) — the raced dir's tree survives where the settled archive carries a file", statErr)
		}
		if calls() != 2 {
			t.Errorf("the drain archived the pod %d times, want the raced attempt retried once", calls())
		}
	})
	t.Run("a pre-drain file under the path is not the drain's to take", func(t *testing.T) {
		hostDst := t.TempDir()
		keep := filepath.Join(hostDst, "sub", "keep.md")
		if err := os.MkdirAll(filepath.Dir(keep), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(keep, []byte("the store held me before the drain"), 0o644); err != nil {
			t.Fatal(err)
		}
		settled := podTree(t, map[string]string{"sub": "the pod settled on a file"})
		kubectl, calls := exportShim(t, settled, 0, "")
		r := drainRun([]sandbox.ReadBackDir{{ContainerPath: "/iterion/artifact-files", HostDir: hostDst}}, kubectl)

		err := r.ExportRunFiles(context.Background())
		if err == nil {
			t.Fatal("ExportRunFiles = nil, want the removal refused — a tree holding pre-drain state is not the drain's to take")
		}
		// The dir itself predates the drain, and the walk names it first —
		// the strongest witness, found before any entry under it.
		if !strings.Contains(err.Error(), "but sub predates the drain") {
			t.Errorf("error = %v, want the pre-drain entry the proof found named in the cause", err)
		}
		if got := sandbox.ReadBackLossReasonOf(err); got != sandbox.ReadBackRefused {
			t.Errorf("reason = %q, want %q — the host refused, the pod's archiver did nothing wrong", got, sandbox.ReadBackRefused)
		}
		got, readErr := os.ReadFile(keep)
		if readErr != nil || string(got) != "the store held me before the drain" {
			t.Errorf("pre-drain keep.md = %q (%v), want it intact under the refused removal", got, readErr)
		}
		if calls() != 1 {
			t.Errorf("the drain archived the pod %d times, want the refusal to end it — no retry cures it", calls())
		}
	})
	t.Run("a pre-drain dir alone under the path is not the drain's to take", func(t *testing.T) {
		// The flip's other blind spot: a dirs-only pre-drain tree holds no
		// file the proof could name — the removal must refuse on the dirs
		// half of the before snapshot all the same, or a dir the host
		// already had is taken wherever nothing under it matched.
		hostDst := t.TempDir()
		pre := filepath.Join(hostDst, "sub", "keepdir")
		if err := os.MkdirAll(pre, 0o755); err != nil {
			t.Fatal(err)
		}
		settled := podTree(t, map[string]string{"sub": "the pod settled on a file"})
		kubectl, calls := exportShim(t, settled, 0, "")
		r := drainRun([]sandbox.ReadBackDir{{ContainerPath: "/iterion/artifact-files", HostDir: hostDst}}, kubectl)

		err := r.ExportRunFiles(context.Background())
		if err == nil {
			t.Fatal("ExportRunFiles = nil, want the removal refused — a dirs-only tree the host already had is not the drain's to take")
		}
		if !strings.Contains(err.Error(), "but sub predates the drain") {
			t.Errorf("error = %v, want the pre-drain dir named in the cause", err)
		}
		if info, statErr := os.Stat(filepath.Join(hostDst, "sub")); statErr != nil || !info.IsDir() {
			t.Errorf("pre-drain sub = (%v, %v), want the dir the host already had intact", info, statErr)
		}
		if _, statErr := os.Stat(pre); statErr != nil {
			t.Errorf("pre-drain sub/keepdir = %v, want the whole pre-drain tree intact", statErr)
		}
		if calls() != 1 {
			t.Errorf("the drain archived the pod %d times, want the refusal to end it — no retry cures it", calls())
		}
	})
	// The link half of the same flip: a dir a raced attempt built where the
	// settled archive carries a symlink is the occupant os.Symlink cannot
	// replace (EEXIST) — cured on the same proof as the file half, refused
	// on the same pre-drain witness, and never a removal for a link the
	// drain refuses anyway.
	t.Run("a dir the settled archive carries as a link", func(t *testing.T) {
		raced := podTree(t, map[string]string{"sub/inner.md": "# raced"})
		settled := podTree(t, map[string]string{"target.md": "# the link's target"})
		if err := os.Symlink("target.md", filepath.Join(settled, "sub")); err != nil {
			t.Fatal(err)
		}
		hostDst := t.TempDir()
		kubectl, calls := exportShimSeq(t, []string{raced, settled}, 1, measuredRaceStderr)
		r := drainRun([]sandbox.ReadBackDir{{ContainerPath: "/iterion/artifact-files", HostDir: hostDst}}, kubectl)

		if err := r.ExportRunFiles(context.Background()); err != nil {
			t.Fatalf("ExportRunFiles = %v, want the dir-to-link flip cured by the retry", err)
		}
		info, statErr := os.Lstat(filepath.Join(hostDst, "sub"))
		if statErr != nil || info.Mode()&os.ModeSymlink == 0 {
			t.Fatalf("drained sub = %v (%v), want the settled archive's link — the raced dir's tree survives where the settled archive carries a link", info, statErr)
		}
		if target, readErr := os.Readlink(filepath.Join(hostDst, "sub")); readErr != nil || target != "target.md" {
			t.Errorf("drained sub -> %q (%v), want the settled link's target", target, readErr)
		}
		if _, statErr := os.Stat(filepath.Join(hostDst, "sub", "inner.md")); statErr == nil {
			t.Error("the raced dir's inner.md is still reachable under the settled link")
		}
		if calls() != 2 {
			t.Errorf("the drain archived the pod %d times, want the raced attempt retried once", calls())
		}
	})
	t.Run("a pre-drain dir where the archive carries a link is not the drain's to take", func(t *testing.T) {
		hostDst := t.TempDir()
		keep := filepath.Join(hostDst, "sub", "keep.md")
		if err := os.MkdirAll(filepath.Dir(keep), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(keep, []byte("the store held me before the drain"), 0o644); err != nil {
			t.Fatal(err)
		}
		settled := podTree(t, map[string]string{"target.md": "# the link's target"})
		if err := os.Symlink("target.md", filepath.Join(settled, "sub")); err != nil {
			t.Fatal(err)
		}
		kubectl, calls := exportShim(t, settled, 0, "")
		r := drainRun([]sandbox.ReadBackDir{{ContainerPath: "/iterion/artifact-files", HostDir: hostDst}}, kubectl)

		err := r.ExportRunFiles(context.Background())
		if err == nil {
			t.Fatal("ExportRunFiles = nil, want the removal refused — a tree holding pre-drain state is not the drain's to take for a link either")
		}
		if !strings.Contains(err.Error(), "but sub predates the drain") {
			t.Errorf("error = %v, want the pre-drain entry named in the cause", err)
		}
		if got := sandbox.ReadBackLossReasonOf(err); got != sandbox.ReadBackRefused {
			t.Errorf("reason = %q, want %q", got, sandbox.ReadBackRefused)
		}
		if got, readErr := os.ReadFile(keep); readErr != nil || string(got) != "the store held me before the drain" {
			t.Errorf("pre-drain keep.md = %q (%v), want it intact under the refused removal", got, readErr)
		}
		if calls() != 1 {
			t.Errorf("the drain archived the pod %d times, want the refusal to end it", calls())
		}
	})
	t.Run("a link that leaves the tree removes nothing, a pre-drain dir in its place included", func(t *testing.T) {
		hostDst := t.TempDir()
		keep := filepath.Join(hostDst, "sub", "keep.md")
		if err := os.MkdirAll(filepath.Dir(keep), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(keep, []byte("the store held me before the drain"), 0o644); err != nil {
			t.Fatal(err)
		}
		settled := podTree(t, map[string]string{"report.md": "# complete"})
		if err := os.Symlink("../outside.md", filepath.Join(settled, "sub")); err != nil {
			t.Fatal(err)
		}
		kubectl, calls := exportShim(t, settled, 0, "")
		r := drainRun([]sandbox.ReadBackDir{{ContainerPath: "/iterion/artifact-files", HostDir: hostDst}}, kubectl)

		if err := r.ExportRunFiles(context.Background()); err != nil {
			t.Fatalf("ExportRunFiles = %v, want the escaping link skipped and the rest landed", err)
		}
		if got, readErr := os.ReadFile(keep); readErr != nil || string(got) != "the store held me before the drain" {
			t.Errorf("pre-drain keep.md = %q (%v), want it intact — a member that lands nothing removes nothing", got, readErr)
		}
		if got, readErr := os.ReadFile(filepath.Join(hostDst, "report.md")); readErr != nil || string(got) != "# complete" {
			t.Errorf("drained report.md = %q (%v), want the rest of the set landed", got, readErr)
		}
		if calls() != 1 {
			t.Errorf("the drain archived the pod %d times, want once", calls())
		}
	})
}

// The meter: the cap is ADR-143's named and metered bound. Oversized
// members are billed for what actually arrives and the drain fails rather
// than truncating silently — and the member it stopped on never stands at
// its path, where the upload would carry a cut file as an entire one.

// hostDirEntries is what the host dir holds, by name: a staged member the
// drain forgot to drop is a file the upload would carry.
func hostDirEntries(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func TestDrainFileMeterStopsOneBytePastTheBudget(t *testing.T) {
	t.Run("a member past the budget is the failure, named, and lands nothing", func(t *testing.T) {
		hostDst := t.TempDir()
		target := filepath.Join(hostDst, "big.bin")
		r := drainRun(nil, "")
		hdr := &tar.Header{Name: "big.bin", Size: 10, Typeflag: tar.TypeReg, Mode: 0o600}
		src := bytes.NewReader(bytes.Repeat([]byte{7}, 10))

		n, err := r.writeDrainFile(target, "big.bin", 4, hdr, src)
		if err == nil {
			t.Fatal("writeDrainFile = nil, want the over-cap refusal")
		}
		if got := sandbox.ReadBackLossReasonOf(err); got != sandbox.ReadBackOverCap {
			t.Errorf("reason = %q, want %q", got, sandbox.ReadBackOverCap)
		}
		if !strings.Contains(err.Error(), "at big.bin") {
			t.Errorf("error = %v, want the member the meter stopped on named", err)
		}
		// budget+1: the copy carries the evidence of being over — a meter
		// that stopped AT the budget could not tell a complete member of
		// budget size from a cut one.
		if n != 5 {
			t.Errorf("metered %d bytes, want the budget plus its evidence byte (5)", n)
		}
		if _, statErr := os.Lstat(target); !os.IsNotExist(statErr) {
			t.Errorf("big.bin stands at its path (%v) — a member cut by the cap is a corrupt artifact the upload would serve as whole", statErr)
		}
		if names := hostDirEntries(t, hostDst); len(names) != 0 {
			t.Errorf("the host dir holds %v after the refusal, want nothing — a staged member left behind is uploaded like any other", names)
		}
	})
	t.Run("a member within the budget lands whole, with its mode, and leaves no stage behind", func(t *testing.T) {
		hostDst := t.TempDir()
		target := filepath.Join(hostDst, "small.bin")
		r := drainRun(nil, "")
		hdr := &tar.Header{Name: "small.bin", Size: 4, Typeflag: tar.TypeReg, Mode: 0o640}
		src := bytes.NewReader(bytes.Repeat([]byte{7}, 4))

		n, err := r.writeDrainFile(target, "small.bin", 4, hdr, src)
		if err != nil {
			t.Fatalf("writeDrainFile = %v, want a member of exactly the budget landed", err)
		}
		if n != 4 {
			t.Errorf("metered %d bytes, want the 4 the member carries", n)
		}
		got, readErr := os.ReadFile(target)
		if readErr != nil || len(got) != 4 {
			t.Errorf("small.bin holds %d bytes (%v), want the whole member", len(got), readErr)
		}
		if info, statErr := os.Stat(target); statErr != nil || info.Mode().Perm() != 0o640 {
			t.Errorf("small.bin mode = %v (%v), want the member's own 0640", info.Mode(), statErr)
		}
		if names := hostDirEntries(t, hostDst); len(names) != 1 || names[0] != "small.bin" {
			t.Errorf("the host dir holds %v, want the landed member alone — no stage survives a landing", names)
		}
	})
	t.Run("a member the path already holds is replaced, a read-only one included", func(t *testing.T) {
		hostDst := t.TempDir()
		target := filepath.Join(hostDst, "report.md")
		if err := os.WriteFile(target, []byte("the store's older copy"), 0o444); err != nil {
			t.Fatal(err)
		}
		r := drainRun(nil, "")
		hdr := &tar.Header{Name: "report.md", Size: 7, Typeflag: tar.TypeReg, Mode: 0o644}

		if _, err := r.writeDrainFile(target, "report.md", 100, hdr, strings.NewReader("# newer")); err != nil {
			t.Fatalf("writeDrainFile = %v, want the pod's member landed over the older copy", err)
		}
		if got, readErr := os.ReadFile(target); readErr != nil || string(got) != "# newer" {
			t.Errorf("report.md = %q (%v), want the pod's bytes", got, readErr)
		}
	})
}

// TestExtractDrainDropsAMemberThePodStreamCutShort: the archiver's stream
// ending inside a member — a pod killed mid-drain, a pipe cut — is the
// same class as the cap: a member that did not arrive whole must not stand
// at its path, and the failure names the archiver, not the host.
func TestExtractDrainDropsAMemberThePodStreamCutShort(t *testing.T) {
	hostDst := t.TempDir()
	var archive bytes.Buffer
	tw := tar.NewWriter(&archive)
	if err := tw.WriteHeader(&tar.Header{Name: "whole.txt", Size: 5, Typeflag: tar.TypeReg, Mode: 0o600}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte("whole")); err != nil {
		t.Fatal(err)
	}
	if err := tw.WriteHeader(&tar.Header{Name: "cut.bin", Size: 4096, Typeflag: tar.TypeReg, Mode: 0o600}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(bytes.Repeat([]byte{1}, 1024)); err != nil {
		t.Fatal(err)
	}
	// No tw.Close(), no padding: the stream ends 3072 bytes short of the
	// member it declared.
	r := drainRun(nil, "")
	drained, landed, err := r.extractDrain(bytes.NewReader(archive.Bytes()), hostDst, nil, nil)
	if err == nil {
		t.Fatal("extractDrain = nil, want the cut stream reported")
	}
	if got := sandbox.ReadBackLossReasonOf(err); got != sandbox.ReadBackArchiver {
		t.Errorf("reason = %q, want %q — the pod's stream ended, the host refused nothing", got, sandbox.ReadBackArchiver)
	}
	if !landed["whole.txt"] {
		t.Errorf("landed = %v, want the member that arrived whole kept — the panel reads partial, not empty", landed)
	}
	if drained != 5+1024 {
		t.Errorf("drained %d bytes, want the meter to bill what arrived (%d)", drained, 5+1024)
	}
	if _, statErr := os.Lstat(filepath.Join(hostDst, "cut.bin")); !os.IsNotExist(statErr) {
		t.Errorf("cut.bin stands at its path (%v) — a member the stream cut short would upload as whole", statErr)
	}
	if names := hostDirEntries(t, hostDst); len(names) != 1 || names[0] != "whole.txt" {
		t.Errorf("the host dir holds %v, want the whole member alone — no stage survives the failure", names)
	}
}

func TestExtractDrainRefusesASetOverTheCap(t *testing.T) {
	if testing.Short() {
		t.Skip("the cap is 256 MiB — the fixture streams it")
	}
	hostDst := t.TempDir()
	// One archive carrying one byte past the cap, streamed (never held in
	// memory whole): the drain must fail with its own cause, not land a
	// silently truncated set.
	over := sandbox.ReadBackCapBytes + 1
	pr, pw := io.Pipe()
	go func() {
		tw := tar.NewWriter(pw)
		if err := tw.WriteHeader(&tar.Header{Name: "big.bin", Size: over, Typeflag: tar.TypeReg, Mode: 0o600}); err != nil {
			pw.CloseWithError(err)
			return
		}
		if _, err := io.CopyN(tw, zeros{}, over); err != nil {
			pw.CloseWithError(err)
			return
		}
		pw.CloseWithError(tw.Close())
	}()
	r := drainRun(nil, "")
	drained, _, err := r.extractDrain(pr, hostDst, nil, nil)
	if err == nil {
		t.Fatal("extractDrain = nil, want the over-cap refusal")
	}
	if !strings.Contains(err.Error(), "cap") || !strings.Contains(err.Error(), "at big.bin") {
		t.Errorf("error = %v, want the cap and the member it stopped on named", err)
	}
	if got := sandbox.ReadBackLossReasonOf(err); got != sandbox.ReadBackOverCap {
		t.Errorf("reason = %q, want %q", got, sandbox.ReadBackOverCap)
	}
	if drained <= sandbox.ReadBackCapBytes {
		t.Errorf("drained %d bytes, want the meter to have carried the evidence past the cap", drained)
	}
	// The disk, not the verdict: the member the cap stopped is not left
	// standing cut at cap+1 bytes, where uploadRunFiles would carry it as
	// an artifact that looks entire.
	if _, statErr := os.Lstat(filepath.Join(hostDst, "big.bin")); !os.IsNotExist(statErr) {
		t.Errorf("big.bin stands at its path (%v) — a member cut by the cap is a corrupt artifact the panel would serve as whole", statErr)
	}
	if names := hostDirEntries(t, hostDst); len(names) != 0 {
		t.Errorf("the host dir holds %v after the refusal, want nothing", names)
	}
}

// TestExtractDrainAcceptsTheExactCapPlusATrailingZeroByteMember pins the
// cap's own boundary: a set totalling EXACTLY the cap leaves the meter at
// the bound, where equality is allowed — and a trailing zero-byte member
// bills nothing, because a copy cannot take a byte the member does not
// carry. The budget of 0 reads as "nothing more fits", not as "one byte
// more arrives": the whole dir must drain clean, every member landed.
func TestExtractDrainAcceptsTheExactCapPlusATrailingZeroByteMember(t *testing.T) {
	if testing.Short() {
		t.Skip("the cap is 256 MiB — the fixture streams it")
	}
	hostDst := t.TempDir()
	pr, pw := io.Pipe()
	go func() {
		tw := tar.NewWriter(pw)
		if err := tw.WriteHeader(&tar.Header{Name: "big.bin", Size: sandbox.ReadBackCapBytes, Typeflag: tar.TypeReg, Mode: 0o600}); err != nil {
			pw.CloseWithError(err)
			return
		}
		if _, err := io.CopyN(tw, zeros{}, sandbox.ReadBackCapBytes); err != nil {
			pw.CloseWithError(err)
			return
		}
		if err := tw.WriteHeader(&tar.Header{Name: "empty.txt", Size: 0, Typeflag: tar.TypeReg, Mode: 0o600}); err != nil {
			pw.CloseWithError(err)
			return
		}
		pw.CloseWithError(tw.Close())
	}()
	r := drainRun(nil, "")
	drained, landed, err := r.extractDrain(pr, hostDst, nil, nil)
	if err != nil {
		t.Fatalf("extractDrain = %v, want the exact-cap set to drain clean", err)
	}
	if drained != sandbox.ReadBackCapBytes {
		t.Errorf("drained %d bytes, want exactly the cap — the zero-byte member bills nothing", drained)
	}
	if !landed["empty.txt"] {
		t.Errorf("landed = %#v, want the trailing zero-byte member among the landed set", landed)
	}
	info, err := os.Stat(filepath.Join(hostDst, "empty.txt"))
	if err != nil || info.Size() != 0 {
		t.Errorf("empty.txt on the host = (%v, %v), want the zero-byte member landed", info, err)
	}
}

// zeros streams zero bytes without holding them.
type zeros struct{}

func (zeros) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 0
	}
	return len(p), nil
}
