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
	if calls() != 2 {
		t.Errorf("the drain attempted %d dirs, want both declared ones", calls())
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
		if !strings.Contains(err.Error(), "keep.md") {
			t.Errorf("error = %v, want the pre-drain entry the proof found named in the cause", err)
		}
		got, readErr := os.ReadFile(keep)
		if readErr != nil || string(got) != "the store held me before the drain" {
			t.Errorf("pre-drain keep.md = %q (%v), want it intact under the refused removal", got, readErr)
		}
		if calls() != 1 {
			t.Errorf("the drain archived the pod %d times, want the refusal to end it — no retry cures it", calls())
		}
	})
}

// The meter: the cap is ADR-143's named and metered bound. Oversized
// members are billed for what actually arrives and the drain fails rather
// than truncating silently.

func TestDrainFileMeterStopsOneBytePastTheBudget(t *testing.T) {
	hostDst := t.TempDir()
	target := filepath.Join(hostDst, "big.bin")
	r := drainRun(nil, "")
	hdr := &tar.Header{Name: "big.bin", Size: 10, Typeflag: tar.TypeReg, Mode: 0o600}
	src := bytes.NewReader(bytes.Repeat([]byte{7}, 10))

	n, err := r.writeDrainFile(target, 4, hdr, src)
	if err != nil {
		t.Fatalf("writeDrainFile = %v", err)
	}
	// budget+1: the copy carries the evidence of being over, which is what
	// the caller's cap check reads — a drain that stopped AT the cap would
	// look exactly like a complete file of cap size.
	if n != 5 {
		t.Errorf("metered %d bytes, want the budget plus its evidence byte (5)", n)
	}
	got, readErr := os.ReadFile(target)
	if readErr != nil || len(got) != 5 {
		t.Errorf("file holds %d bytes (%v), want the 5 the meter accepted", len(got), readErr)
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
	drained, _, err := r.extractDrain(pr, hostDst, nil)
	if err == nil {
		t.Fatal("extractDrain = nil, want the over-cap refusal")
	}
	if !strings.Contains(err.Error(), "cap") {
		t.Errorf("error = %v, want the cap named", err)
	}
	if drained <= sandbox.ReadBackCapBytes {
		t.Errorf("drained %d bytes, want the meter to have carried the evidence past the cap", drained)
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
