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
	drained, err := r.extractDrain(pr, hostDst)
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
