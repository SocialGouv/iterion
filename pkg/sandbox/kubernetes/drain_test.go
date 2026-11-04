package kubernetes

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
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
	// No attempt settled, so no sweep ran: the host dir holds the union
	// of every attempt's members, and the panel will serve it. The error
	// is the one place a reader learns the set is a stale superset, not
	// the subset "partial" suggests.
	if !strings.Contains(err.Error(), "stale superset") {
		t.Errorf("error = %v, want the union of the attempts named as a stale superset of the pod's final tree", err)
	}
	if !sandbox.ReadBackStaleOf(err) {
		t.Errorf("ReadBackStaleOf = false on an unsettled drain, want the stale mark the event carries — the text alone is no field a reader filters on")
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
	t.Run("a pre-drain file under the path is not the drain's to take: the member is skipped, the rest lands", func(t *testing.T) {
		hostDst := t.TempDir()
		keep := filepath.Join(hostDst, "sub", "keep.md")
		if err := os.MkdirAll(filepath.Dir(keep), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(keep, []byte("the store held me before the drain"), 0o644); err != nil {
			t.Fatal(err)
		}
		settled := podTree(t, map[string]string{"sub": "the pod settled on a file", "report.md": "# complete"})
		kubectl, calls := exportShim(t, settled, 0, "")
		r := drainRun([]sandbox.ReadBackDir{{ContainerPath: "/iterion/artifact-files", HostDir: hostDst}}, kubectl)

		// A member the host declines is skipped like an escaping link or
		// a device: the collision is per member, and the rest of the set
		// is not its price — a resumed run whose previous attempt drained
		// `out/` and whose tool now writes `out` as a file keeps every
		// other deliverable. Skipped is not silent: the drain's verdict
		// names the member, so the run says what its panel is short of.
		wantDeclinedLoss(t, r.ExportRunFiles(context.Background()), sandbox.ReadBackDeclinedMember{Dir: "/iterion/artifact-files", Name: "sub", Why: sandbox.DeclinedPreDrainTree})
		got, readErr := os.ReadFile(keep)
		if readErr != nil || string(got) != "the store held me before the drain" {
			t.Errorf("pre-drain keep.md = %q (%v), want it intact under the declined removal", got, readErr)
		}
		if info, statErr := os.Lstat(filepath.Join(hostDst, "sub")); statErr != nil || !info.IsDir() {
			t.Errorf("sub = %v (%v), want the pre-drain tree still standing — the pod's file never replaces a tree the host already had", info, statErr)
		}
		if got, readErr := os.ReadFile(filepath.Join(hostDst, "report.md")); readErr != nil || string(got) != "# complete" {
			t.Errorf("drained report.md = %q (%v), want the rest of the set landed past the skipped member", got, readErr)
		}
		if calls() != 1 {
			t.Errorf("the drain archived the pod %d times, want once — a skip is no race", calls())
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
		settled := podTree(t, map[string]string{"sub": "the pod settled on a file", "report.md": "# complete"})
		kubectl, calls := exportShim(t, settled, 0, "")
		r := drainRun([]sandbox.ReadBackDir{{ContainerPath: "/iterion/artifact-files", HostDir: hostDst}}, kubectl)

		// A dirs-only tree the host already had is not the drain's to take,
		// and not the rest of the set's price — and the skip is named.
		wantDeclinedLoss(t, r.ExportRunFiles(context.Background()), sandbox.ReadBackDeclinedMember{Dir: "/iterion/artifact-files", Name: "sub", Why: sandbox.DeclinedPreDrainTree})
		if info, statErr := os.Stat(filepath.Join(hostDst, "sub")); statErr != nil || !info.IsDir() {
			t.Errorf("pre-drain sub = (%v, %v), want the dir the host already had intact", info, statErr)
		}
		if _, statErr := os.Stat(pre); statErr != nil {
			t.Errorf("pre-drain sub/keepdir = %v, want the whole pre-drain tree intact", statErr)
		}
		if got, readErr := os.ReadFile(filepath.Join(hostDst, "report.md")); readErr != nil || string(got) != "# complete" {
			t.Errorf("drained report.md = %q (%v), want the rest of the set landed past the skipped member", got, readErr)
		}
		if calls() != 1 {
			t.Errorf("the drain archived the pod %d times, want once — a skip is no race", calls())
		}
	})
	t.Run("a pre-drain file where the archive carries a dir is not the drain's to remove", func(t *testing.T) {
		// The mirror of the tree a file member never takes: the sweep's rule
		// — the drain removes only what it landed — holds for the dir member
		// too. The dir is declined, and so is every member under it, on the
		// same pre-drain witness: the file stands in their way.
		hostDst := t.TempDir()
		own := filepath.Join(hostDst, "out")
		if err := os.WriteFile(own, []byte("the store held me before the drain"), 0o644); err != nil {
			t.Fatal(err)
		}
		settled := podTree(t, map[string]string{"out/report.md": "# the pod's", "after.md": "# complete"})
		kubectl, calls := exportShim(t, settled, 0, "")
		r := drainRun([]sandbox.ReadBackDir{{ContainerPath: "/iterion/artifact-files", HostDir: hostDst}}, kubectl)

		wantDeclinedLoss(t, r.ExportRunFiles(context.Background()),
			sandbox.ReadBackDeclinedMember{Dir: "/iterion/artifact-files", Name: "out", Why: sandbox.DeclinedPreDrainFile},
			sandbox.ReadBackDeclinedMember{Dir: "/iterion/artifact-files", Name: "out/report.md", Why: sandbox.DeclinedPreDrainFile})
		if got, readErr := os.ReadFile(own); readErr != nil || string(got) != "the store held me before the drain" {
			t.Errorf("pre-drain out = %q (%v), want the file the host held intact — the drain removed what it never landed", got, readErr)
		}
		if got, readErr := os.ReadFile(filepath.Join(hostDst, "after.md")); readErr != nil || string(got) != "# complete" {
			t.Errorf("drained after.md = %q (%v), want the rest of the set landed past the declined members", got, readErr)
		}
		if calls() != 1 {
			t.Errorf("the drain archived the pod %d times, want once — a skip is no race", calls())
		}
	})
	t.Run("a pre-drain file where the archive carries a link is not the drain's to remove", func(t *testing.T) {
		// A link carries none of the set's bytes either: taking the host's
		// file for it trades content for a name, as a dir would.
		hostDst := t.TempDir()
		own := filepath.Join(hostDst, "report.md")
		if err := os.WriteFile(own, []byte("the store held me before the drain"), 0o644); err != nil {
			t.Fatal(err)
		}
		settled := podTree(t, map[string]string{"target.md": "# the link's target"})
		if err := os.Symlink("target.md", filepath.Join(settled, "report.md")); err != nil {
			t.Fatal(err)
		}
		kubectl, calls := exportShim(t, settled, 0, "")
		r := drainRun([]sandbox.ReadBackDir{{ContainerPath: "/iterion/artifact-files", HostDir: hostDst}}, kubectl)

		wantDeclinedLoss(t, r.ExportRunFiles(context.Background()),
			sandbox.ReadBackDeclinedMember{Dir: "/iterion/artifact-files", Name: "report.md", Why: sandbox.DeclinedPreDrainFile})
		if got, readErr := os.ReadFile(own); readErr != nil || string(got) != "the store held me before the drain" {
			t.Errorf("pre-drain report.md = %q (%v), want the file the host held intact — the drain removed what it never landed", got, readErr)
		}
		if got, readErr := os.ReadFile(filepath.Join(hostDst, "target.md")); readErr != nil || string(got) != "# the link's target" {
			t.Errorf("drained target.md = %q (%v), want the rest of the set landed past the declined link", got, readErr)
		}
		if calls() != 1 {
			t.Errorf("the drain archived the pod %d times, want once — a skip is no race", calls())
		}
	})
	// A dangling link is an entry like any other: the drain judges it where
	// the member lands — the host's, or the drain's own — and never refuses
	// a member's name for a link at its own path that no write follows.
	t.Run("a pre-drain dangling link where the archive carries a dir is the host's, not an unsafe name", func(t *testing.T) {
		hostDst := t.TempDir()
		if err := os.Symlink("missing", filepath.Join(hostDst, "l")); err != nil {
			t.Fatal(err)
		}
		kubectl, calls := exportShim(t, podTree(t, map[string]string{"l/f": "f", "after.md": "# complete"}), 0, "")
		r := drainRun([]sandbox.ReadBackDir{{ContainerPath: "/iterion/artifact-files", HostDir: hostDst}}, kubectl)

		wantDeclinedLoss(t, r.ExportRunFiles(context.Background()),
			sandbox.ReadBackDeclinedMember{Dir: "/iterion/artifact-files", Name: "l", Why: sandbox.DeclinedPreDrainFile},
			sandbox.ReadBackDeclinedMember{Dir: "/iterion/artifact-files", Name: "l/f", Why: sandbox.DeclinedPreDrainFile})
		if target, err := os.Readlink(filepath.Join(hostDst, "l")); err != nil || target != "missing" {
			t.Errorf("pre-drain l -> %q (%v), want the link the host held intact", target, err)
		}
		if got, readErr := os.ReadFile(filepath.Join(hostDst, "after.md")); readErr != nil || string(got) != "# complete" {
			t.Errorf("drained after.md = %q (%v), want the rest of the set landed past the declined members", got, readErr)
		}
		if calls() != 1 {
			t.Errorf("the drain archived the pod %d times, want once — a skip is no race", calls())
		}
	})
	t.Run("a pre-drain dangling link where the archive carries a file is overwritten, as any pre-drain file is", func(t *testing.T) {
		hostDst := t.TempDir()
		if err := os.Symlink("gone.md", filepath.Join(hostDst, "latest")); err != nil {
			t.Fatal(err)
		}
		kubectl, _ := exportShim(t, podTree(t, map[string]string{"latest": "# now a file"}), 0, "")
		r := drainRun([]sandbox.ReadBackDir{{ContainerPath: "/iterion/artifact-files", HostDir: hostDst}}, kubectl)

		if err := r.ExportRunFiles(context.Background()); err != nil {
			t.Fatalf("ExportRunFiles = %v, want the pod's file landed over the host's dangling link — the overlay a resumed drain lands", err)
		}
		if info, err := os.Lstat(filepath.Join(hostDst, "latest")); err != nil || !info.Mode().IsRegular() {
			t.Errorf("latest = %v (%v), want the pod's regular file", info, err)
		}
	})
	t.Run("a dangling link the settled archive carries as a file", func(t *testing.T) {
		raced := podTree(t, map[string]string{"scratch.txt": "x"})
		if err := os.Symlink("gone.md", filepath.Join(raced, "latest")); err != nil {
			t.Fatal(err)
		}
		settled := podTree(t, map[string]string{"latest": "# settled as a file"})
		hostDst := t.TempDir()
		kubectl, calls := exportShimSeq(t, []string{raced, settled}, 1, measuredRaceStderr)
		r := drainRun([]sandbox.ReadBackDir{{ContainerPath: "/iterion/artifact-files", HostDir: hostDst}}, kubectl)

		if err := r.ExportRunFiles(context.Background()); err != nil {
			t.Fatalf("ExportRunFiles = %v, want the link-to-file flip cured by the retry", err)
		}
		if got, err := os.ReadFile(filepath.Join(hostDst, "latest")); err != nil || string(got) != "# settled as a file" {
			t.Errorf("drained latest = %q (%v), want the settled archive's regular file", got, err)
		}
		if calls() != 2 {
			t.Errorf("the drain archived the pod %d times, want the raced attempt retried once", calls())
		}
	})
	t.Run("a dangling link both attempts carry lands", func(t *testing.T) {
		raced := podTree(t, map[string]string{"scratch.txt": "x"})
		settled := podTree(t, map[string]string{"report.md": "# complete"})
		for _, pod := range []string{raced, settled} {
			if err := os.Symlink("gone.md", filepath.Join(pod, "latest")); err != nil {
				t.Fatal(err)
			}
		}
		hostDst := t.TempDir()
		kubectl, calls := exportShimSeq(t, []string{raced, settled}, 1, measuredRaceStderr)
		r := drainRun([]sandbox.ReadBackDir{{ContainerPath: "/iterion/artifact-files", HostDir: hostDst}}, kubectl)

		if err := r.ExportRunFiles(context.Background()); err != nil {
			t.Fatalf("ExportRunFiles = %v, want the pod's dangling link landed again by the settled attempt", err)
		}
		if target, err := os.Readlink(filepath.Join(hostDst, "latest")); err != nil || target != "gone.md" {
			t.Errorf("drained latest -> %q (%v), want the settled archive's link", target, err)
		}
		if _, err := os.Lstat(filepath.Join(hostDst, "scratch.txt")); !os.IsNotExist(err) {
			t.Errorf("scratch.txt stands (%v), want it swept — only the raced attempt landed it", err)
		}
		if calls() != 2 {
			t.Errorf("the drain archived the pod %d times, want the raced attempt retried once", calls())
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

		// A tree holding pre-drain state is not the drain's to take for a
		// link either; the link is skipped, named, and the rest still lands.
		wantDeclinedLoss(t, r.ExportRunFiles(context.Background()), sandbox.ReadBackDeclinedMember{Dir: "/iterion/artifact-files", Name: "sub", Why: sandbox.DeclinedPreDrainTree})
		if got, readErr := os.ReadFile(keep); readErr != nil || string(got) != "the store held me before the drain" {
			t.Errorf("pre-drain keep.md = %q (%v), want it intact under the declined removal", got, readErr)
		}
		if info, statErr := os.Lstat(filepath.Join(hostDst, "sub")); statErr != nil || !info.IsDir() {
			t.Errorf("sub = %v (%v), want the pre-drain tree still standing, no link in its place", info, statErr)
		}
		if got, readErr := os.ReadFile(filepath.Join(hostDst, "target.md")); readErr != nil || string(got) != "# the link's target" {
			t.Errorf("drained target.md = %q (%v), want the rest of the set landed past the skipped link", got, readErr)
		}
		if calls() != 1 {
			t.Errorf("the drain archived the pod %d times, want once", calls())
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

		wantDeclinedLoss(t, r.ExportRunFiles(context.Background()), sandbox.ReadBackDeclinedMember{Dir: "/iterion/artifact-files", Name: "sub", Why: sandbox.DeclinedLinkLeavesTree})
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

		n, err := r.writeDrainFile(target, "big.bin", 4, 4, hdr.FileInfo().Mode(), src)
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

		n, err := r.writeDrainFile(target, "small.bin", 4, 4, hdr.FileInfo().Mode(), src)
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

		if _, err := r.writeDrainFile(target, "report.md", 100, 100, hdr.FileInfo().Mode(), strings.NewReader("# newer")); err != nil {
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
	hostDst := drainRootDir(t)
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
	x, err := r.extractDrain(bytes.NewReader(archive.Bytes()), hostDst, nil, nil, newDrainState(sandbox.ReadBackCapBytes))
	drained, landed := x.drained, x.landed
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
	hostDst := drainRootDir(t)
	// One archive carrying one byte past the cap, streamed (never held in
	// memory whole): the drain must fail with its own cause, not land a
	// silently truncated set. The cap is the dir's own figure, handed in
	// — a witness-sized one, far under the default, so a meter that read
	// the default instead of the figure it was given lands the member.
	const capBytes int64 = 4 << 10
	over := capBytes + 1
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
	x, err := r.extractDrain(pr, hostDst, nil, nil, newDrainState(capBytes))
	drained := x.drained
	if err == nil {
		t.Fatal("extractDrain = nil, want the over-cap refusal")
	}
	if !strings.Contains(err.Error(), "4096 bytes cap") || !strings.Contains(err.Error(), "at big.bin") {
		t.Errorf("error = %v, want the dir's own cap and the member it stopped on named", err)
	}
	if !strings.Contains(err.Error(), sandbox.ReadBackCapEnv) {
		t.Errorf("error = %v, want the escape hatch named — a bound the operator cannot find is a bound with no override", err)
	}
	if got := sandbox.ReadBackLossReasonOf(err); got != sandbox.ReadBackOverCap {
		t.Errorf("reason = %q, want %q", got, sandbox.ReadBackOverCap)
	}
	if drained <= capBytes {
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
	hostDst := drainRootDir(t)
	const capBytes int64 = 4 << 10
	pr, pw := io.Pipe()
	go func() {
		tw := tar.NewWriter(pw)
		if err := tw.WriteHeader(&tar.Header{Name: "big.bin", Size: capBytes, Typeflag: tar.TypeReg, Mode: 0o600}); err != nil {
			pw.CloseWithError(err)
			return
		}
		if _, err := io.CopyN(tw, zeros{}, capBytes); err != nil {
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
	x, err := r.extractDrain(pr, hostDst, nil, nil, newDrainState(capBytes))
	drained, landed := x.drained, x.landed
	if err != nil {
		t.Fatalf("extractDrain = %v, want the exact-cap set to drain clean", err)
	}
	if drained != capBytes {
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

// TestExtractDrainSkipsAPreDrainTreeAndLandsTheMembersAfterIt pins the
// order the shim cannot: the member the host declines comes FIRST in the
// stream, and the one after it must still land. With the refusal an error
// again, after.md is lost with the rest of the set — the defect of a
// resumed run whose previous attempt drained `out/` and whose tool now
// writes `out` as a file, paying every later deliverable for one name.
func TestExtractDrainSkipsAPreDrainTreeAndLandsTheMembersAfterIt(t *testing.T) {
	hostDst := drainRootDir(t)
	keep := filepath.Join(hostDst, "out", "keep.md")
	if err := os.MkdirAll(filepath.Dir(keep), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keep, []byte("the previous attempt drained me"), 0o644); err != nil {
		t.Fatal(err)
	}
	before, beforeDirs, err := readBackDirEntries(hostDst)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, m := range []struct{ name, body string }{{"out", "now a file"}, {"after.md", "# landed after the collision"}} {
		if err := tw.WriteHeader(&tar.Header{Name: m.name, Size: int64(len(m.body)), Typeflag: tar.TypeReg, Mode: 0o644}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(m.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	r := drainRun(nil, "")
	x, err := r.extractDrain(&buf, hostDst, before, beforeDirs, newDrainState(sandbox.ReadBackCapBytes))
	drained, landed := x.drained, x.landed
	if err != nil {
		t.Fatalf("extractDrain = %v, want the declined member skipped and the extract to go on", err)
	}
	// Skipped is not silent: the member is recorded with its reason, for
	// the run's event — the panel reads short of it, and the run says why.
	if want := []sandbox.ReadBackDeclinedMember{{Name: "out", Why: sandbox.DeclinedPreDrainTree}}; !reflect.DeepEqual(x.declined.List(), want) {
		t.Errorf("declined = %+v, want %+v", x.declined.List(), want)
	}
	if got, readErr := os.ReadFile(filepath.Join(hostDst, "after.md")); readErr != nil || string(got) != "# landed after the collision" {
		t.Errorf("after.md = %q (%v), want the member after the skipped one landed", got, readErr)
	}
	if !landed["after.md"] || landed["out"] {
		t.Errorf("landed = %#v, want after.md recorded and out not — a skipped member is nothing the sweep keeps", landed)
	}
	if drained != int64(len("# landed after the collision")) {
		t.Errorf("drained %d bytes, want the landed member's alone — a skipped member moves no meter", drained)
	}
	if got, readErr := os.ReadFile(keep); readErr != nil || string(got) != "the previous attempt drained me" {
		t.Errorf("pre-drain out/keep.md = %q (%v), want it intact", got, readErr)
	}
}

// TestExtractDrainJudgesPreDrainStateWhereTheMemberLands: the before
// snapshot holds each entry where it stands, so a member is judged against
// it where its write lands — never by the name that reached it through a
// link an earlier member laid. Read by name, a pre-drain entry behind an
// alias reads as the drain's own, and is removed. One scenario per kind of
// occupant.
func TestExtractDrainJudgesPreDrainStateWhereTheMemberLands(t *testing.T) {
	const preDrain = "the store held me before the drain"
	// hostWith is a host dir holding one pre-drain file at rel, and its
	// before snapshot.
	hostWith := func(t *testing.T, rel string) (hostDst, own string, before, beforeDirs map[string]bool) {
		t.Helper()
		hostDst = drainRootDir(t)
		own = filepath.Join(hostDst, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(own), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(own, []byte(preDrain), 0o644); err != nil {
			t.Fatal(err)
		}
		before, beforeDirs, err := readBackDirEntries(hostDst)
		if err != nil {
			t.Fatal(err)
		}
		return hostDst, own, before, beforeDirs
	}
	// A pre-drain tree behind a link, where the archive carries a member that
	// cannot land over a dir — one scenario per kind, each its own site.
	for _, tc := range []struct {
		kind   string
		member *tar.Header
	}{
		{"a file", &tar.Header{Name: "./l/sub", Typeflag: tar.TypeReg, Mode: 0o644}},
		{"a hardlink", &tar.Header{Name: "./l/sub", Typeflag: tar.TypeLink, Linkname: "./a.txt"}},
		{"a symlink", &tar.Header{Name: "./l/sub", Typeflag: tar.TypeSymlink, Linkname: "../after.md"}},
	} {
		t.Run("a pre-drain tree behind a link, where the archive carries "+tc.kind, func(t *testing.T) {
			hostDst, own, before, beforeDirs := hostWith(t, "d/sub/keep.md")
			archive := tarArchive(t, []*tar.Header{
				{Name: "./a.txt", Typeflag: tar.TypeReg, Mode: 0o644},
				{Name: "./l", Typeflag: tar.TypeSymlink, Linkname: "d"},
				tc.member,
				{Name: "./after.md", Typeflag: tar.TypeReg, Mode: 0o644},
			}, map[string]string{"./a.txt": "a", "./l/sub": "the pod settled on a file", "./after.md": "# complete"})
			x, err := drainRun(nil, "").extractDrain(archive, hostDst, before, beforeDirs, newDrainState(sandbox.ReadBackCapBytes))
			if err != nil {
				t.Fatalf("extractDrain = %v, want the member declined and the extract to go on", err)
			}
			if want := []sandbox.ReadBackDeclinedMember{{Name: "l/sub", Why: sandbox.DeclinedPreDrainTree}}; !reflect.DeepEqual(x.declined.List(), want) {
				t.Errorf("declined = %+v, want %+v", x.declined.List(), want)
			}
			if got, readErr := os.ReadFile(own); readErr != nil || string(got) != preDrain {
				t.Errorf("pre-drain d/sub/keep.md = %q (%v), want it intact — the tree was judged by the name l/sub and taken for the drain's", got, readErr)
			}
			if names := hostDirEntries(t, filepath.Join(hostDst, "d")); !reflect.DeepEqual(names, []string{"sub"}) {
				t.Errorf("d holds %v, want the pre-drain sub alone — a declined member leaves nothing beside it", names)
			}
			if got, readErr := os.ReadFile(filepath.Join(hostDst, "after.md")); readErr != nil || string(got) != "# complete" {
				t.Errorf("after.md = %q (%v), want the member after the declined one landed", got, readErr)
			}
		})
	}
	t.Run("a pre-drain file behind a link, where the archive carries a link", func(t *testing.T) {
		hostDst, own, before, beforeDirs := hostWith(t, "d/f")
		archive := tarArchive(t, []*tar.Header{
			{Name: "./l", Typeflag: tar.TypeSymlink, Linkname: "d"},
			{Name: "./l/f", Typeflag: tar.TypeSymlink, Linkname: "../after.md"},
			{Name: "./after.md", Typeflag: tar.TypeReg, Mode: 0o644},
		}, map[string]string{"./after.md": "# complete"})
		x, err := drainRun(nil, "").extractDrain(archive, hostDst, before, beforeDirs, newDrainState(sandbox.ReadBackCapBytes))
		if err != nil {
			t.Fatalf("extractDrain = %v, want the member declined and the extract to go on", err)
		}
		if want := []sandbox.ReadBackDeclinedMember{{Name: "l/f", Why: sandbox.DeclinedPreDrainFile}}; !reflect.DeepEqual(x.declined.List(), want) {
			t.Errorf("declined = %+v, want %+v", x.declined.List(), want)
		}
		if got, readErr := os.ReadFile(own); readErr != nil || string(got) != preDrain {
			t.Errorf("pre-drain d/f = %q (%v), want it intact — the file was judged by the name l/f and replaced by a link", got, readErr)
		}
	})
	t.Run("a pre-drain file above a member's path, behind a link", func(t *testing.T) {
		hostDst, own, before, beforeDirs := hostWith(t, "out")
		archive := tarArchive(t, []*tar.Header{
			{Name: "./l", Typeflag: tar.TypeSymlink, Linkname: "."},
			{Name: "./l/out/x", Typeflag: tar.TypeReg, Mode: 0o644},
			{Name: "./after.md", Typeflag: tar.TypeReg, Mode: 0o644},
		}, map[string]string{"./l/out/x": "x", "./after.md": "# complete"})
		x, err := drainRun(nil, "").extractDrain(archive, hostDst, before, beforeDirs, newDrainState(sandbox.ReadBackCapBytes))
		if err != nil {
			t.Fatalf("extractDrain = %v, want the member declined and the extract to go on", err)
		}
		// The reason is the host's file at out, reached through l: read by
		// the name l/out, it is no pre-drain entry, and the member reads as
		// an unsafe name.
		if want := []sandbox.ReadBackDeclinedMember{{Name: "l/out/x", Why: sandbox.DeclinedPreDrainFile}}; !reflect.DeepEqual(x.declined.List(), want) {
			t.Errorf("declined = %+v, want %+v", x.declined.List(), want)
		}
		if got, readErr := os.ReadFile(own); readErr != nil || string(got) != preDrain {
			t.Errorf("pre-drain out = %q (%v), want it intact", got, readErr)
		}
		if got, readErr := os.ReadFile(filepath.Join(hostDst, "after.md")); readErr != nil || string(got) != "# complete" {
			t.Errorf("after.md = %q (%v), want the member after the declined one landed", got, readErr)
		}
	})
	t.Run("a pre-drain file behind a link, where the archive carries a dir", func(t *testing.T) {
		hostDst, own, before, beforeDirs := hostWith(t, "d/f")
		archive := tarArchive(t, []*tar.Header{
			{Name: "./l", Typeflag: tar.TypeSymlink, Linkname: "d"},
			{Name: "./l/f/", Typeflag: tar.TypeDir, Mode: 0o755},
			{Name: "./after.md", Typeflag: tar.TypeReg, Mode: 0o644},
		}, map[string]string{"./after.md": "# complete"})
		x, err := drainRun(nil, "").extractDrain(archive, hostDst, before, beforeDirs, newDrainState(sandbox.ReadBackCapBytes))
		if err != nil {
			t.Fatalf("extractDrain = %v, want the member declined and the extract to go on", err)
		}
		if want := []sandbox.ReadBackDeclinedMember{{Name: "l/f", Why: sandbox.DeclinedPreDrainFile}}; !reflect.DeepEqual(x.declined.List(), want) {
			t.Errorf("declined = %+v, want %+v", x.declined.List(), want)
		}
		if got, readErr := os.ReadFile(own); readErr != nil || string(got) != preDrain {
			t.Errorf("pre-drain d/f = %q (%v), want it intact — the file was judged by the name l/f and removed for a dir", got, readErr)
		}
	})
}

// TestExportRunFilesMetersOnTheDirsOwnCap: the figure is the manifest
// entry's (ReadBackDir.Cap), carried from the mount that serves the path —
// a bot that sized its own emptyDir is metered on its figure, not cut down
// to the default. A drain that read the default instead of the entry's cap
// would land the 17-byte member under a 16-byte cap.
func TestExportRunFilesMetersOnTheDirsOwnCap(t *testing.T) {
	const body = "seventeen bytes!!"
	t.Run("one byte over the dir's cap is the failure", func(t *testing.T) {
		hostDst := t.TempDir()
		kubectl, calls := exportShim(t, podTree(t, map[string]string{"report.md": body}), 0, "")
		r := drainRun([]sandbox.ReadBackDir{{ContainerPath: "/iterion/artifact-files", HostDir: hostDst, CapBytes: int64(len(body)) - 1}}, kubectl)
		err := r.ExportRunFiles(context.Background())
		if got := sandbox.ReadBackLossReasonOf(err); got != sandbox.ReadBackOverCap {
			t.Fatalf("ExportRunFiles = %v (reason %q), want %q on the dir's own %d-byte cap", err, got, sandbox.ReadBackOverCap, len(body)-1)
		}
		if sandbox.ReadBackStaleOf(err) {
			t.Errorf("ReadBackStaleOf = true on a first-attempt failure, want false — no raced attempt left anything beside it")
		}
		if calls() != 1 {
			t.Errorf("the drain archived the pod %d times, want once — the cap is no race", calls())
		}
	})
	t.Run("exactly the dir's cap drains clean", func(t *testing.T) {
		hostDst := t.TempDir()
		kubectl, _ := exportShim(t, podTree(t, map[string]string{"report.md": body}), 0, "")
		r := drainRun([]sandbox.ReadBackDir{{ContainerPath: "/iterion/artifact-files", HostDir: hostDst, CapBytes: int64(len(body))}}, kubectl)
		if err := r.ExportRunFiles(context.Background()); err != nil {
			t.Fatalf("ExportRunFiles = %v, want a set of exactly the cap landed", err)
		}
		if got, readErr := os.ReadFile(filepath.Join(hostDst, "report.md")); readErr != nil || string(got) != body {
			t.Errorf("drained report.md = %q (%v), want the member whole", got, readErr)
		}
	})
	t.Run("an entry naming no cap meters on the default", func(t *testing.T) {
		if got := (sandbox.ReadBackDir{}).Cap(); got != sandbox.ReadBackCapBytes {
			t.Errorf("Cap() = %d on an entry naming none, want the %d default", got, sandbox.ReadBackCapBytes)
		}
	})
}

// wantDeclinedLoss asserts a drain whose set landed short only of the
// members the host declined: the declined reason, every member named with
// its reason and dir, no stale mark — a skip is no race.
func wantDeclinedLoss(t *testing.T, err error, want ...sandbox.ReadBackDeclinedMember) {
	t.Helper()
	if err == nil {
		t.Fatalf("ExportRunFiles = nil, want the declined members on the drain's verdict %+v — a skip that only reaches the runner's log leaves the panel partial in silence", want)
	}
	if got := sandbox.ReadBackLossReasonOf(err); got != sandbox.ReadBackDeclined {
		t.Errorf("reason = %q (%v), want %q — every other member landed", got, err, sandbox.ReadBackDeclined)
	}
	if got := sandbox.ReadBackDeclinedOf(err); !reflect.DeepEqual(got, want) {
		t.Errorf("declined = %+v, want %+v", got, want)
	}
	if sandbox.ReadBackStaleOf(err) {
		t.Errorf("ReadBackStaleOf = true, want false — one settled attempt landed the set")
	}
}

// tarArchive builds an archive in memory from headers, a regular member's
// body taken from bodies by name.
func tarArchive(t *testing.T, hdrs []*tar.Header, bodies map[string]string) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, h := range hdrs {
		if h.Typeflag == tar.TypeReg {
			h.Size = int64(len(bodies[h.Name]))
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatalf("write header %q: %v", h.Name, err)
		}
		if h.Typeflag == tar.TypeReg {
			if _, err := tw.Write([]byte(bodies[h.Name])); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return &buf
}

// archiveShimSeq plants a fake kubectl, returned by path, that streams archives[n-1] on
// call n (the last one from then on) — archives in a member order no real
// tar writes, which the pod's own archiver is free to — and, on the first
// failFor calls, writes stderr after the archive and exits 1.
func archiveShimSeq(t *testing.T, archives [][]byte, failFor int, stderr string) (kubectl Kubectl, calls func() int) {
	t.Helper()
	isolateExecEnvironment(t)
	shimDir := t.TempDir()
	counter := filepath.Join(shimDir, "calls")
	errFile := filepath.Join(shimDir, "stderr")
	if err := os.WriteFile(errFile, []byte(stderr+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	files := make([]string, len(archives))
	for i, archive := range archives {
		files[i] = filepath.Join(shimDir, fmt.Sprintf("archive-%d.tar", i+1))
		if err := os.WriteFile(files[i], archive, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	shim := "#!/bin/sh\n" +
		"n=$(cat '" + counter + "' 2>/dev/null || echo 0); n=$((n+1)); echo \"$n\" > '" + counter + "'\n" +
		"case \"$n\" in\n" + srcCases(files) + "esac\n" +
		"cat \"$src\" || exit 2\n" +
		"if [ \"$n\" -le " + strconv.Itoa(failFor) + " ]; then cat '" + errFile + "' >&2; exit 1; fi\n" +
		"exit 0\n"
	bin := filepath.Join(shimDir, kubeBinaryName)
	if err := os.WriteFile(bin, []byte(shim), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", shimDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return Kubectl(bin), func() int {
		raw, err := os.ReadFile(counter)
		if err != nil {
			return 0
		}
		n, _ := strconv.Atoi(strings.TrimSpace(string(raw)))
		return n
	}
}

// TestExportRunFilesKeepsWhatTheSettledArchiveLandedThroughALink: the record
// of what the settled attempt landed is the one the sweep after a raced
// attempt keeps against everything it walks — so it is keyed where each
// write landed, as the walk meets it, never by the name that reached it. An
// archive that lays l -> d and then sends l/x lands d/x; recorded as l/x,
// d/x reads as a raced leftover and the sweep removes the settled member.
// One scenario per kind of member the extract lands.
func TestExportRunFilesKeepsWhatTheSettledArchiveLandedThroughALink(t *testing.T) {
	for _, tc := range []struct {
		kind    string
		members []*tar.Header
		bodies  map[string]string
		// landed is the kind the settled member must still stand as at d/x.
		landed os.FileMode
	}{
		{"a file", []*tar.Header{
			{Name: "./l/x", Typeflag: tar.TypeReg, Mode: 0o644},
		}, map[string]string{"./l/x": "landed through the link"}, 0},
		{"a dir", []*tar.Header{
			{Name: "./l/x/", Typeflag: tar.TypeDir, Mode: 0o755},
		}, nil, os.ModeDir},
		{"a hardlink", []*tar.Header{
			{Name: "./l/x", Typeflag: tar.TypeLink, Linkname: "./a.txt"},
		}, nil, 0},
		{"a symlink", []*tar.Header{
			{Name: "./l/x", Typeflag: tar.TypeSymlink, Linkname: "../a.txt"},
		}, nil, os.ModeSymlink},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			fastExportRetries(t)
			hostDst := t.TempDir()
			raced := tarArchive(t, []*tar.Header{
				{Name: "./scratch.txt", Typeflag: tar.TypeReg, Mode: 0o644},
			}, map[string]string{"./scratch.txt": "the pod dropped me before the settled archive"})
			bodies := map[string]string{"./a.txt": "a"}
			for name, body := range tc.bodies {
				bodies[name] = body
			}
			settled := tarArchive(t, append([]*tar.Header{
				{Name: "./a.txt", Typeflag: tar.TypeReg, Mode: 0o644},
				{Name: "./d/", Typeflag: tar.TypeDir, Mode: 0o755},
				{Name: "./l", Typeflag: tar.TypeSymlink, Linkname: "d"},
			}, tc.members...), bodies)
			kubectl, calls := archiveShimSeq(t, [][]byte{raced.Bytes(), settled.Bytes()}, 1, measuredRaceStderr)
			r := drainRun([]sandbox.ReadBackDir{{ContainerPath: "/iterion/artifact-files", HostDir: hostDst}}, kubectl)

			if err := r.ExportRunFiles(context.Background()); err != nil {
				t.Fatalf("ExportRunFiles = %v, want the raced drain retried to success", err)
			}
			if n := calls(); n != 2 {
				t.Fatalf("the drain archived the pod %d times, want the raced attempt then the settled one — the sweep this pins never ran", n)
			}
			info, err := os.Lstat(filepath.Join(hostDst, "d", "x"))
			if err != nil || info.Mode().Type() != tc.landed {
				t.Errorf("d/x = %v (%v), want the settled member kept as type %v — the sweep took what the settled archive landed through l", info, err, tc.landed)
			}
			if _, err := os.Lstat(filepath.Join(hostDst, "scratch.txt")); !os.IsNotExist(err) {
				t.Errorf("scratch.txt stands (%v), want it swept — only the raced attempt landed it", err)
			}
		})
	}
}

// TestExportRunFilesLandsAHardlinkAsACopy: GNU tar archives every name of a
// file but the first as a hardlink to that first member — the realistic
// shape of a tool that publishes one report under two names. Both names
// reach the set, as files: the host lands the link as a copy of the member
// the same archive landed.
func TestExportRunFilesLandsAHardlinkAsACopy(t *testing.T) {
	pod := podTree(t, map[string]string{"report.html": "<h1>audit</h1>"})
	if err := os.Link(filepath.Join(pod, "report.html"), filepath.Join(pod, "latest.html")); err != nil {
		t.Skipf("no hardlink on this filesystem: %v", err)
	}
	hostDst := t.TempDir()
	kubectl, _ := exportShim(t, pod, 0, "")
	r := drainRun([]sandbox.ReadBackDir{{ContainerPath: "/iterion/artifact-files", HostDir: hostDst}}, kubectl)

	if err := r.ExportRunFiles(context.Background()); err != nil {
		t.Fatalf("ExportRunFiles = %v, want both names of the hardlinked file landed", err)
	}
	for _, name := range []string{"report.html", "latest.html"} {
		info, err := os.Lstat(filepath.Join(hostDst, name))
		if err != nil || !info.Mode().IsRegular() {
			t.Fatalf("%s on the host = (%v, %v), want a regular file — one name of a hardlinked file lost from the set", name, info, err)
		}
		if got, _ := os.ReadFile(filepath.Join(hostDst, name)); string(got) != "<h1>audit</h1>" {
			t.Errorf("%s = %q, want the file's bytes under every name", name, got)
		}
	}
}

// TestExtractDrainLandsAHardlinkAsAMeteredCopy pins the archive shape the
// shim leaves to the host's tar: a regular member, then a hardlink to it.
// The copy is metered like any file — the upload carries both names — so a
// cap that holds the first and not the second stops on the link.
func TestExtractDrainLandsAHardlinkAsAMeteredCopy(t *testing.T) {
	members := func() *bytes.Buffer {
		return tarArchive(t, []*tar.Header{
			{Name: "./report.html", Typeflag: tar.TypeReg, Mode: 0o640},
			{Name: "./latest.html", Typeflag: tar.TypeLink, Linkname: "./report.html", Mode: 0o640},
			{Name: "./after.txt", Typeflag: tar.TypeReg, Mode: 0o644},
		}, map[string]string{"./report.html": "0123456789", "./after.txt": "ok"})
	}
	t.Run("within the cap both names land, billed twice", func(t *testing.T) {
		hostDst := drainRootDir(t)
		x, err := drainRun(nil, "").extractDrain(members(), hostDst, nil, nil, newDrainState(22))
		if err != nil {
			t.Fatalf("extractDrain = %v, want the hardlink landed as a copy", err)
		}
		if got, readErr := os.ReadFile(filepath.Join(hostDst, "latest.html")); readErr != nil || string(got) != "0123456789" {
			t.Errorf("latest.html = %q (%v), want a copy of report.html's bytes", got, readErr)
		}
		if info, statErr := os.Stat(filepath.Join(hostDst, "latest.html")); statErr != nil || info.Mode().Perm() != 0o640 {
			t.Errorf("latest.html mode = %v (%v), want the file's own 0640", info, statErr)
		}
		if !x.landed["latest.html"] || x.declined.Count() != 0 {
			t.Errorf("landed = %v, declined = %+v — want the link among the landed members and nothing declined", x.landed, x.declined.List())
		}
		if x.drained != 22 {
			t.Errorf("drained %d bytes, want 22 — the copy is billed like the file it copies", x.drained)
		}
	})
	t.Run("the copy that takes the set past the cap is the failure", func(t *testing.T) {
		hostDst := drainRootDir(t)
		_, err := drainRun(nil, "").extractDrain(members(), hostDst, nil, nil, newDrainState(15))
		if got := sandbox.ReadBackLossReasonOf(err); got != sandbox.ReadBackOverCap {
			t.Fatalf("extractDrain = %v (reason %q), want %q at the link — a copy the meter never bills lets two names of one file double the set past its cap", err, got, sandbox.ReadBackOverCap)
		}
		if _, statErr := os.Lstat(filepath.Join(hostDst, "latest.html")); !os.IsNotExist(statErr) {
			t.Errorf("latest.html stands (%v), want the member the cap stopped on absent", statErr)
		}
	})
}

// TestExtractDrainLandsAHardlinkToAFileLandedThroughALink: the registry of
// files a hardlink may be landed from is keyed where each file landed, as a
// hardlink's target is read: a file that reached d/ through l -> d is the
// file at d/x, whatever name the hardlink gives it. Registered by name, it
// is no file this drain landed, and the hardlink is declined. One scenario
// per member that registers a file.
func TestExtractDrainLandsAHardlinkToAFileLandedThroughALink(t *testing.T) {
	for _, tc := range []struct {
		kind  string
		first *tar.Header
	}{
		{"a file", &tar.Header{Name: "./l/x", Typeflag: tar.TypeReg, Mode: 0o644}},
		{"a hardlink's copy", &tar.Header{Name: "./l/x", Typeflag: tar.TypeLink, Linkname: "./a.txt"}},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			hostDst := drainRootDir(t)
			archive := tarArchive(t, []*tar.Header{
				{Name: "./a.txt", Typeflag: tar.TypeReg, Mode: 0o644},
				{Name: "./d/", Typeflag: tar.TypeDir, Mode: 0o755},
				{Name: "./l", Typeflag: tar.TypeSymlink, Linkname: "d"},
				tc.first,
				{Name: "./h", Typeflag: tar.TypeLink, Linkname: "./d/x"},
			}, map[string]string{"./a.txt": "abc", "./l/x": "abc"})
			x, err := drainRun(nil, "").extractDrain(archive, hostDst, nil, nil, newDrainState(sandbox.ReadBackCapBytes))
			if err != nil {
				t.Fatalf("extractDrain = %v, want every member landed", err)
			}
			if x.declined.Count() != 0 {
				t.Errorf("declined = %+v, want nothing — d/x is a file this drain landed", x.declined.List())
			}
			if got, readErr := os.ReadFile(filepath.Join(hostDst, "h")); readErr != nil || string(got) != "abc" {
				t.Errorf("h = %q (%v), want a copy of d/x", got, readErr)
			}
		})
	}
}

// TestExtractDrainNamesEveryMemberItDeclines: each member the host declines
// on principle is skipped AND recorded with its reason — named in name
// order, whatever order the archive carried them in — and the members after
// it still land. A pax global header is archive metadata, never a member to
// decline. A link is judged where the kernel resolves it: from the
// directory it lands in, its target's ".." climbing from wherever a name
// before it leads.
func TestExtractDrainNamesEveryMemberItDeclines(t *testing.T) {
	hostDst := drainRootDir(t)
	archive := tarArchive(t, []*tar.Header{
		{Name: "pax_global_header", Typeflag: tar.TypeXGlobalHeader, PAXRecords: map[string]string{"comment": "archive metadata"}},
		{Name: "../escape.txt", Typeflag: tar.TypeReg, Mode: 0o644},
		{Name: "./pipe", Typeflag: tar.TypeFifo, Mode: 0o644},
		{Name: "./tty", Typeflag: tar.TypeChar, Mode: 0o644, Devmajor: 5, Devminor: 0},
		{Name: "./out-link", Typeflag: tar.TypeSymlink, Linkname: "../../etc/passwd"},
		// The self-link chain: l names the tree's own root, so l/l/l/x is
		// written at x, where the target's three ".." climb out of the tree
		// — though the name spells a link three levels down, from which the
		// same three land back inside.
		{Name: "./l", Typeflag: tar.TypeSymlink, Linkname: "."},
		{Name: "./l/l/l/x", Typeflag: tar.TypeSymlink, Linkname: "../../../outside"},
		// A ".." past a name climbs from wherever that name leads: sub/up
		// leads to the root, so up/../outside resolves above it, whatever
		// the text reads.
		{Name: "./sub/", Typeflag: tar.TypeDir, Mode: 0o755},
		{Name: "./sub/up", Typeflag: tar.TypeSymlink, Linkname: ".."},
		{Name: "./sub/y", Typeflag: tar.TypeSymlink, Linkname: "up/../outside"},
		{Name: "./orphan-link", Typeflag: tar.TypeLink, Linkname: "./never-landed.txt"},
		{Name: "./ok.txt", Typeflag: tar.TypeReg, Mode: 0o644},
	}, map[string]string{"../escape.txt": "out", "./ok.txt": "landed"})

	x, err := drainRun(nil, "").extractDrain(archive, hostDst, nil, nil, newDrainState(sandbox.ReadBackCapBytes))
	if err != nil {
		t.Fatalf("extractDrain = %v, want every declined member skipped and the extract to go on", err)
	}
	want := []sandbox.ReadBackDeclinedMember{
		{Name: "../escape.txt", Why: sandbox.DeclinedUnsafeName},
		{Name: "l/l/l/x", Why: sandbox.DeclinedLinkLeavesTree},
		{Name: "orphan-link", Why: sandbox.DeclinedHardlinkTarget},
		{Name: "out-link", Why: sandbox.DeclinedLinkLeavesTree},
		{Name: "pipe", Why: sandbox.DeclinedKind},
		{Name: "sub/y", Why: sandbox.DeclinedLinkLeavesTree},
		{Name: "tty", Why: sandbox.DeclinedKind},
	}
	if !reflect.DeepEqual(x.declined.List(), want) {
		t.Errorf("declined = %+v, want %+v", x.declined.List(), want)
	}
	if x.declined.Count() != len(want) {
		t.Errorf("declined count = %d, want %d", x.declined.Count(), len(want))
	}
	if got, readErr := os.ReadFile(filepath.Join(hostDst, "ok.txt")); readErr != nil || string(got) != "landed" {
		t.Errorf("ok.txt = %q (%v), want the member after the declined ones landed", got, readErr)
	}
	if names := hostDirEntries(t, hostDst); !reflect.DeepEqual(names, []string{"l", "ok.txt", "sub"}) {
		t.Errorf("the host dir holds %v, want the two links' homes and ok.txt alone — a declined member writes nothing", names)
	}
	if target, linkErr := os.Readlink(filepath.Join(hostDst, "x")); !os.IsNotExist(linkErr) {
		t.Errorf("x -> %q (%v) stands at the root, want nothing — the self-link chain planted a link out of the tree", target, linkErr)
	}
	if target, linkErr := os.Readlink(filepath.Join(hostDst, "sub", "y")); !os.IsNotExist(linkErr) {
		t.Errorf("sub/y -> %q (%v) stands, want nothing — its .. climbs from the root sub/up leads to, out of the tree", target, linkErr)
	}
}

// TestExtractDrainDeclinesALinkTheKernelRefuses: a link member whose own
// text the kernel refuses — an empty target, one longer than a path may be
// — is the member's fault, never the host's: it is declined under its own
// token and the members after it land, where a host refusal would end the
// extract and cost the rest of the set. And it is judged before anything
// is removed: what stood at its path still stands. One scenario per
// refusal.
func TestExtractDrainDeclinesALinkTheKernelRefuses(t *testing.T) {
	for _, tc := range []struct{ name, target string }{
		{"an empty target", ""},
		{"a target longer than a path", strings.Repeat("a", 4096)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hostDst := drainRootDir(t)
			archive := tarArchive(t, []*tar.Header{
				{Name: "./report.md", Typeflag: tar.TypeReg, Mode: 0o644},
				{Name: "./report.md", Typeflag: tar.TypeSymlink, Linkname: tc.target},
				{Name: "./after.md", Typeflag: tar.TypeReg, Mode: 0o644},
			}, map[string]string{"./report.md": "landed first", "./after.md": "# complete"})
			x, err := drainRun(nil, "").extractDrain(archive, hostDst, nil, nil, newDrainState(sandbox.ReadBackCapBytes))
			if err != nil {
				t.Fatalf("extractDrain = %v (reason %q), want the link declined and the extract to go on — the member's own text is no host refusal", err, sandbox.ReadBackLossReasonOf(err))
			}
			if want := []sandbox.ReadBackDeclinedMember{{Name: "report.md", Why: sandbox.DeclinedUnwritableLink}}; !reflect.DeepEqual(x.declined.List(), want) {
				t.Errorf("declined = %+v, want %+v", x.declined.List(), want)
			}
			if got, readErr := os.ReadFile(filepath.Join(hostDst, "report.md")); readErr != nil || string(got) != "landed first" {
				t.Errorf("report.md = %q (%v), want the file that stood there intact — a member that lands nothing removes nothing", got, readErr)
			}
			if names := hostDirEntries(t, hostDst); !reflect.DeepEqual(names, []string{"after.md", "report.md"}) {
				t.Errorf("the host dir holds %v, want after.md and report.md alone — a refused link leaves no staged entry", names)
			}
		})
	}
	t.Run("a raced dir at its path stands", func(t *testing.T) {
		hostDst := drainRootDir(t)
		s := newDrainState(sandbox.ReadBackCapBytes)
		if _, err := extractInto(t, hostDst, s, []*tar.Header{
			{Name: "./sub/inner.md", Typeflag: tar.TypeReg, Mode: 0o644},
		}, map[string]string{"./sub/inner.md": "# raced"}); err != nil {
			t.Fatalf("attempt 1: %v", err)
		}
		x, err := extractInto(t, hostDst, s, []*tar.Header{
			{Name: "./sub", Typeflag: tar.TypeSymlink, Linkname: ""},
			{Name: "./after.md", Typeflag: tar.TypeReg, Mode: 0o644},
		}, map[string]string{"./after.md": "# complete"})
		if err != nil {
			t.Fatalf("attempt 2 = %v, want the link declined and the extract to go on", err)
		}
		if want := []sandbox.ReadBackDeclinedMember{{Name: "sub", Why: sandbox.DeclinedUnwritableLink}}; !reflect.DeepEqual(x.declined.List(), want) {
			t.Errorf("declined = %+v, want %+v", x.declined.List(), want)
		}
		if got, readErr := os.ReadFile(filepath.Join(hostDst, "sub", "inner.md")); readErr != nil || string(got) != "# raced" {
			t.Errorf("sub/inner.md = %q (%v), want the raced dir standing — it was removed for a link that never landed", got, readErr)
		}
	})
}

// TestExtractDrainLandsANameAtTheHostsLimit: a member whose name the host's
// filesystem holds lands, however close to the limit — the drain stages
// each write beside its path, and a staged name built on the member's
// would outgrow the limit the member itself fits under, refusing the host
// for a name it can hold and ending the extract.
func TestExtractDrainLandsANameAtTheHostsLimit(t *testing.T) {
	hostDst := drainRootDir(t)
	name := strings.Repeat("n", 240)
	probe := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(probe, nil, 0o644); err != nil {
		t.Skipf("this filesystem holds no %d-byte name: %v", len(name), err)
	}
	archive := tarArchive(t, []*tar.Header{
		{Name: "./" + name, Typeflag: tar.TypeReg, Mode: 0o644},
		{Name: "./after.md", Typeflag: tar.TypeReg, Mode: 0o644},
	}, map[string]string{"./" + name: "long", "./after.md": "# complete"})
	x, err := drainRun(nil, "").extractDrain(archive, hostDst, nil, nil, newDrainState(sandbox.ReadBackCapBytes))
	if err != nil {
		t.Fatalf("extractDrain = %v, want the %d-byte name landed — the host holds it", err, len(name))
	}
	if got, readErr := os.ReadFile(filepath.Join(hostDst, name)); readErr != nil || string(got) != "long" {
		t.Errorf("the %d-byte member = %q (%v), want it landed whole", len(name), got, readErr)
	}
	if x.declined.Count() != 0 {
		t.Errorf("declined = %+v, want nothing", x.declined.List())
	}
}

// TestExtractDrainLandsAHardlinkOnlyFromAFileThisArchiveLanded: the two
// guards on a hardlink's target, one scenario each. A file the host dir
// holds that this archive never landed — the store's own, a raced
// attempt's — is no target: copying it would put bytes the pod never
// archived into the set under a new name. And a target this archive
// landed that no longer stands as a file — a later member took its parent
// — is declined like any other, never a failure that ends the extract.
func TestExtractDrainLandsAHardlinkOnlyFromAFileThisArchiveLanded(t *testing.T) {
	t.Run("a file the host held before the drain is no target", func(t *testing.T) {
		hostDst := drainRootDir(t)
		if err := os.WriteFile(filepath.Join(hostDst, "store-own.txt"), []byte("the store's, not the pod's"), 0o644); err != nil {
			t.Fatal(err)
		}
		before, beforeDirs, err := readBackDirEntries(hostDst)
		if err != nil {
			t.Fatal(err)
		}
		archive := tarArchive(t, []*tar.Header{
			{Name: "./borrowed.txt", Typeflag: tar.TypeLink, Linkname: "./store-own.txt"},
			{Name: "./after.txt", Typeflag: tar.TypeReg, Mode: 0o644},
		}, map[string]string{"./after.txt": "ok"})
		x, err := drainRun(nil, "").extractDrain(archive, hostDst, before, beforeDirs, newDrainState(sandbox.ReadBackCapBytes))
		if err != nil {
			t.Fatalf("extractDrain = %v, want the link declined and the extract to go on", err)
		}
		if want := []sandbox.ReadBackDeclinedMember{{Name: "borrowed.txt", Why: sandbox.DeclinedHardlinkTarget}}; !reflect.DeepEqual(x.declined.List(), want) {
			t.Errorf("declined = %+v, want %+v", x.declined.List(), want)
		}
		if _, statErr := os.Lstat(filepath.Join(hostDst, "borrowed.txt")); !os.IsNotExist(statErr) {
			t.Errorf("borrowed.txt stands (%v) — the store's own bytes landed in the run's set under a name the pod chose", statErr)
		}
	})
	t.Run("a landed target a later member took is declined, not fatal", func(t *testing.T) {
		hostDst := drainRootDir(t)
		// A raced archive: sub/a.txt lands, then sub arrives as a FILE —
		// the dir this extract built is replaced, a.txt with it — then a
		// hardlink names the a.txt that no longer stands.
		archive := tarArchive(t, []*tar.Header{
			{Name: "./sub/a.txt", Typeflag: tar.TypeReg, Mode: 0o644},
			{Name: "./sub", Typeflag: tar.TypeReg, Mode: 0o644},
			{Name: "./c.txt", Typeflag: tar.TypeLink, Linkname: "./sub/a.txt"},
			{Name: "./after.txt", Typeflag: tar.TypeReg, Mode: 0o644},
		}, map[string]string{"./sub/a.txt": "a", "./sub": "now a file", "./after.txt": "ok"})
		x, err := drainRun(nil, "").extractDrain(archive, hostDst, nil, nil, newDrainState(sandbox.ReadBackCapBytes))
		if err != nil {
			t.Fatalf("extractDrain = %v, want the dangling link declined and the extract to go on", err)
		}
		if want := []sandbox.ReadBackDeclinedMember{{Name: "c.txt", Why: sandbox.DeclinedHardlinkTarget}}; !reflect.DeepEqual(x.declined.List(), want) {
			t.Errorf("declined = %+v, want %+v", x.declined.List(), want)
		}
		if got, readErr := os.ReadFile(filepath.Join(hostDst, "after.txt")); readErr != nil || string(got) != "ok" {
			t.Errorf("after.txt = %q (%v), want the member after the declined link landed", got, readErr)
		}
	})
	t.Run("a landed name a later link redirected onto a pre-drain file is no target", func(t *testing.T) {
		hostDst := drainRootDir(t)
		own := filepath.Join(hostDst, "store", "own.txt")
		if err := os.MkdirAll(filepath.Dir(own), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(own, []byte("the store's, not the pod's"), 0o644); err != nil {
			t.Fatal(err)
		}
		before, beforeDirs, err := readBackDirEntries(hostDst)
		if err != nil {
			t.Fatal(err)
		}
		// p/own.txt lands, then p becomes a link to the store's dir: the
		// name the archive landed now reaches the store's own file. What
		// this archive landed is recorded where it landed, so the name is
		// judged where it leads today — never by the text it once landed
		// under.
		archive := tarArchive(t, []*tar.Header{
			{Name: "./p/own.txt", Typeflag: tar.TypeReg, Mode: 0o644},
			{Name: "./p", Typeflag: tar.TypeSymlink, Linkname: "store"},
			{Name: "./borrowed.txt", Typeflag: tar.TypeLink, Linkname: "./p/own.txt"},
			{Name: "./after.txt", Typeflag: tar.TypeReg, Mode: 0o644},
		}, map[string]string{"./p/own.txt": "the pod's", "./after.txt": "ok"})
		x, err := drainRun(nil, "").extractDrain(archive, hostDst, before, beforeDirs, newDrainState(sandbox.ReadBackCapBytes))
		if err != nil {
			t.Fatalf("extractDrain = %v, want the link declined and the extract to go on", err)
		}
		if want := []sandbox.ReadBackDeclinedMember{{Name: "borrowed.txt", Why: sandbox.DeclinedHardlinkTarget}}; !reflect.DeepEqual(x.declined.List(), want) {
			t.Errorf("declined = %+v, want %+v", x.declined.List(), want)
		}
		if got, readErr := os.ReadFile(filepath.Join(hostDst, "borrowed.txt")); !os.IsNotExist(readErr) {
			t.Errorf("borrowed.txt = %q (%v), want nothing — the store's own bytes landed in the run's set under a name the pod chose", got, readErr)
		}
	})
	t.Run("a landed target a later link redirected away is declined, not fatal", func(t *testing.T) {
		hostDst := drainRootDir(t)
		// p/a.txt lands, then p arrives as a link to the tree's own q/ —
		// the dir this extract built goes, a.txt with it, and the path now
		// resolves through the link to a file nobody landed: the name is
		// safe and stays inside the tree, the target is no file.
		archive := tarArchive(t, []*tar.Header{
			{Name: "./q/", Typeflag: tar.TypeDir, Mode: 0o755},
			{Name: "./p/a.txt", Typeflag: tar.TypeReg, Mode: 0o644},
			{Name: "./p", Typeflag: tar.TypeSymlink, Linkname: "q"},
			{Name: "./c.txt", Typeflag: tar.TypeLink, Linkname: "./p/a.txt"},
			{Name: "./after.txt", Typeflag: tar.TypeReg, Mode: 0o644},
		}, map[string]string{"./p/a.txt": "a", "./after.txt": "ok"})
		x, err := drainRun(nil, "").extractDrain(archive, hostDst, nil, nil, newDrainState(sandbox.ReadBackCapBytes))
		if err != nil {
			t.Fatalf("extractDrain = %v, want the dangling link declined and the extract to go on", err)
		}
		if want := []sandbox.ReadBackDeclinedMember{{Name: "c.txt", Why: sandbox.DeclinedHardlinkTarget}}; !reflect.DeepEqual(x.declined.List(), want) {
			t.Errorf("declined = %+v, want %+v", x.declined.List(), want)
		}
		if got, readErr := os.ReadFile(filepath.Join(hostDst, "after.txt")); readErr != nil || string(got) != "ok" {
			t.Errorf("after.txt = %q (%v), want the member after the declined link landed", got, readErr)
		}
	})
}

// TestExportRunFilesSaysAFailureAfterARacedAttemptLeavesAStaleSet: tar
// never deletes, and only a settled attempt sweeps. When a raced attempt
// landed members and the next attempt fails for a cause no retry cures,
// the drain ends before any sweep: the raced attempt's members stand beside
// what the failing one landed — a stale superset, as an unsettled drain's —
// and the verdict must say so, not read as the failure's partial set alone.
func TestExportRunFilesSaysAFailureAfterARacedAttemptLeavesAStaleSet(t *testing.T) {
	raced := podTree(t, map[string]string{"stale.txt": "the pod dropped me before the next attempt"})
	settled := podTree(t, map[string]string{"big.bin": strings.Repeat("x", 4096)})
	hostDst := t.TempDir()
	// Attempt 1 archives the mid-write tree and exits 1 on the measured
	// race stderr; attempt 2 meets a member past the dir's cap.
	kubectl, calls := exportShimSeq(t, []string{raced, settled}, 1, measuredRaceStderr)
	r := drainRun([]sandbox.ReadBackDir{{ContainerPath: "/iterion/artifact-files", HostDir: hostDst, CapBytes: 1024}}, kubectl)

	err := r.ExportRunFiles(context.Background())
	if got := sandbox.ReadBackLossReasonOf(err); got != sandbox.ReadBackOverCap {
		t.Fatalf("ExportRunFiles = %v (reason %q), want %q — the reason still says what ended the drain", err, got, sandbox.ReadBackOverCap)
	}
	if !sandbox.ReadBackStaleOf(err) {
		t.Errorf("ReadBackStaleOf = false, want the stale mark — stale.txt, which the pod no longer has, uploads with the set")
	}
	if !strings.Contains(err.Error(), "stale superset") {
		t.Errorf("error = %v, want the raced attempt's leftovers named a stale superset", err)
	}
	if _, statErr := os.Stat(filepath.Join(hostDst, "stale.txt")); statErr != nil {
		t.Fatalf("stale.txt is gone (%v) — this witness pins the reported state, and the state changed under it", statErr)
	}
	if calls() != 2 {
		t.Errorf("the drain archived the pod %d times, want the raced attempt then the failing one", calls())
	}
}

// hostDirBytes is what the regular files under dir hold in all — what the
// upload carries.
func hostDirBytes(t *testing.T, dir string) int64 {
	t.Helper()
	var total int64
	err := filepath.WalkDir(dir, func(_ string, d os.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		total += info.Size()
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", dir, err)
	}
	return total
}

// TestExportRunFilesMetersEveryAttemptAsOneSet: the cap bounds what the host
// holds — what the upload carries — never one attempt's archive alone. tar
// never deletes, and a drain no attempt settles keeps every attempt's
// files: metered from zero each time, a tool still writing at teardown
// under rotating names landed the cap once per attempt. Each guard of the
// shared meter has its own scenario: the union bounded, a settling archive
// never refused for a raced attempt's leftovers, and nothing swept that the
// cap did not need.
func TestExportRunFilesMetersEveryAttemptAsOneSet(t *testing.T) {
	const capBytes = 10
	t.Run("a tree that never settles holds no more than the cap", func(t *testing.T) {
		fastExportRetries(t)
		t.Setenv("ITERION_SANDBOX_EXPORT_ATTEMPTS", "3")
		hostDst := t.TempDir()
		// Each attempt archives a different log, each of the full cap: the
		// rotation of a tool still writing at teardown.
		kubectl, calls := exportShimSeq(t, []string{
			podTree(t, map[string]string{"log-1.txt": "0123456789"}),
			podTree(t, map[string]string{"log-2.txt": "0123456789"}),
			podTree(t, map[string]string{"log-3.txt": "0123456789"}),
		}, 99, measuredRaceStderr)
		r := drainRun([]sandbox.ReadBackDir{{ContainerPath: "/iterion/artifact-files", HostDir: hostDst, CapBytes: capBytes}}, kubectl)

		err := r.ExportRunFiles(context.Background())
		if got := sandbox.ReadBackLossReasonOf(err); got != sandbox.ReadBackUnsettled {
			t.Fatalf("ExportRunFiles = %v (reason %q), want %q — each attempt's log fits the cap once what only earlier attempts left gives its room back", err, got, sandbox.ReadBackUnsettled)
		}
		if held := hostDirBytes(t, hostDst); held > capBytes {
			t.Errorf("the host holds %d bytes of run files %v, want at most the %d-byte cap — the upload carries every one", held, hostDirEntries(t, hostDst), capBytes)
		}
		if got, readErr := os.ReadFile(filepath.Join(hostDst, "log-3.txt")); readErr != nil || string(got) != "0123456789" {
			t.Errorf("log-3.txt = %q (%v), want the last attempt's member whole — the newer archive keeps the room", got, readErr)
		}
		if !sandbox.ReadBackStaleOf(err) {
			t.Error("ReadBackStaleOf = false on an unsettled drain, want the stale mark")
		}
		if n := calls(); n != 3 {
			t.Errorf("the drain archived the pod %d times, want the 3 attempts", n)
		}
	})
	t.Run("a raced attempt's leftovers give way to the archive that settles", func(t *testing.T) {
		fastExportRetries(t)
		hostDst := t.TempDir()
		// The raced attempt lands a scratch file the tool removed before the
		// next archive; the settled report fits the cap alone, not beside it.
		kubectl, calls := exportShimSeq(t, []string{
			podTree(t, map[string]string{"scratch.bin": "01234567"}),
			podTree(t, map[string]string{"report.md": "abcdefgh"}),
		}, 1, measuredRaceStderr)
		r := drainRun([]sandbox.ReadBackDir{{ContainerPath: "/iterion/artifact-files", HostDir: hostDst, CapBytes: capBytes}}, kubectl)

		if err := r.ExportRunFiles(context.Background()); err != nil {
			t.Fatalf("ExportRunFiles = %v, want the settled archive landed — what a raced attempt left is a sweep's to take, never a reason to refuse the set that settled", err)
		}
		if got, readErr := os.ReadFile(filepath.Join(hostDst, "report.md")); readErr != nil || string(got) != "abcdefgh" {
			t.Errorf("report.md = %q (%v), want the settled member whole", got, readErr)
		}
		if _, statErr := os.Lstat(filepath.Join(hostDst, "scratch.bin")); !os.IsNotExist(statErr) {
			t.Errorf("scratch.bin stands (%v), want it swept — the pod dropped it before the archive that settled", statErr)
		}
		if n := calls(); n != 2 {
			t.Errorf("the drain archived the pod %d times, want the raced attempt then the settled one", n)
		}
	})
	t.Run("a union the cap holds stays whole", func(t *testing.T) {
		fastExportRetries(t)
		t.Setenv("ITERION_SANDBOX_EXPORT_ATTEMPTS", "3")
		hostDst := t.TempDir()
		kubectl, calls := exportShimSeq(t, []string{
			podTree(t, map[string]string{"a.txt": "aaa"}),
			podTree(t, map[string]string{"b.txt": "bbb"}),
			podTree(t, map[string]string{"c.txt": "ccc"}),
		}, 99, measuredRaceStderr)
		r := drainRun([]sandbox.ReadBackDir{{ContainerPath: "/iterion/artifact-files", HostDir: hostDst, CapBytes: capBytes}}, kubectl)

		err := r.ExportRunFiles(context.Background())
		if got := sandbox.ReadBackLossReasonOf(err); got != sandbox.ReadBackUnsettled {
			t.Fatalf("ExportRunFiles = %v (reason %q), want %q", err, got, sandbox.ReadBackUnsettled)
		}
		// Nothing needed the room: every attempt's member stays, the union
		// the stale mark reports — a sweep the cap did not ask for would
		// throw away what the drain recovered for nothing.
		for _, name := range []string{"a.txt", "b.txt", "c.txt"} {
			if _, statErr := os.Stat(filepath.Join(hostDst, name)); statErr != nil {
				t.Errorf("%s is gone (%v), want the union kept whole while the cap holds it", name, statErr)
			}
		}
		if n := calls(); n != 3 {
			t.Errorf("the drain archived the pod %d times, want the 3 attempts", n)
		}
	})
	t.Run("a pre-drain file a raced attempt overwrote still counts", func(t *testing.T) {
		fastExportRetries(t)
		t.Setenv("ITERION_SANDBOX_EXPORT_ATTEMPTS", "2")
		hostDst := t.TempDir()
		if err := os.WriteFile(filepath.Join(hostDst, "keep.txt"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		// The raced attempt overwrites the store's keep.txt with the pod's
		// 8 bytes; no sweep takes a name the host already had, so they
		// stand and upload whatever the next attempt carries.
		kubectl, _ := exportShimSeq(t, []string{
			podTree(t, map[string]string{"keep.txt": "01234567"}),
			podTree(t, map[string]string{"new.txt": "abcdefgh"}),
		}, 99, measuredRaceStderr)
		r := drainRun([]sandbox.ReadBackDir{{ContainerPath: "/iterion/artifact-files", HostDir: hostDst, CapBytes: capBytes}}, kubectl)

		err := r.ExportRunFiles(context.Background())
		if got := sandbox.ReadBackLossReasonOf(err); got != sandbox.ReadBackOverCap {
			t.Errorf("ExportRunFiles = %v (reason %q), want %q — keep.txt's 8 bytes leave no room for new.txt's, and no sweep may take them", err, got, sandbox.ReadBackOverCap)
		}
		if held := hostDirBytes(t, hostDst); held > capBytes {
			t.Errorf("the host holds %d bytes %v, want at most the %d-byte cap", held, hostDirEntries(t, hostDst), capBytes)
		}
	})
	t.Run("a raced file replaced by a dir gives its room back", func(t *testing.T) {
		fastExportRetries(t)
		hostDst := t.TempDir()
		kubectl, _ := exportShimSeq(t, []string{
			podTree(t, map[string]string{"out": "01234567"}),
			podTree(t, map[string]string{"out/report.md": "abcdefgh"}),
		}, 1, measuredRaceStderr)
		r := drainRun([]sandbox.ReadBackDir{{ContainerPath: "/iterion/artifact-files", HostDir: hostDst, CapBytes: capBytes}}, kubectl)

		if err := r.ExportRunFiles(context.Background()); err != nil {
			t.Fatalf("ExportRunFiles = %v, want the settled tree landed — the file the dir replaced holds nothing any more", err)
		}
		if got, readErr := os.ReadFile(filepath.Join(hostDst, "out", "report.md")); readErr != nil || string(got) != "abcdefgh" {
			t.Errorf("out/report.md = %q (%v), want the settled member", got, readErr)
		}
	})
	t.Run("a raced dir replaced by a file gives its room back, and the union keeps the rest", func(t *testing.T) {
		fastExportRetries(t)
		t.Setenv("ITERION_SANDBOX_EXPORT_ATTEMPTS", "2")
		hostDst := t.TempDir()
		const cap20 = 20
		// The raced dir sub/ goes when the next attempt carries sub as a
		// file: its 8 bytes leave the host, and the meter must say so —
		// counted still, they would make the file's 10 bytes look like
		// they need keep.txt's room, and the union would lose a member the
		// cap never asked for.
		kubectl, _ := exportShimSeq(t, []string{
			podTree(t, map[string]string{"sub/inner.md": "01234567", "keep.txt": "abcd"}),
			podTree(t, map[string]string{"sub": "0123456789"}),
		}, 99, measuredRaceStderr)
		r := drainRun([]sandbox.ReadBackDir{{ContainerPath: "/iterion/artifact-files", HostDir: hostDst, CapBytes: cap20}}, kubectl)

		err := r.ExportRunFiles(context.Background())
		if got := sandbox.ReadBackLossReasonOf(err); got != sandbox.ReadBackUnsettled {
			t.Fatalf("ExportRunFiles = %v (reason %q), want %q", err, got, sandbox.ReadBackUnsettled)
		}
		if got, readErr := os.ReadFile(filepath.Join(hostDst, "keep.txt")); readErr != nil || string(got) != "abcd" {
			t.Errorf("keep.txt = %q (%v), want it kept — 14 bytes of the 20-byte cap held the union whole", got, readErr)
		}
		if got, readErr := os.ReadFile(filepath.Join(hostDst, "sub")); readErr != nil || string(got) != "0123456789" {
			t.Errorf("sub = %q (%v), want the last attempt's file", got, readErr)
		}
	})
}

// extractInto feeds one archive to extractDrain against a drain state the
// caller shares across calls — each call one attempt of the same dir's
// drain, in the order the archive lists its members.
func extractInto(t *testing.T, hostDst string, s *drainState, hdrs []*tar.Header, bodies map[string]string) (drainExtract, error) {
	t.Helper()
	return drainRun(nil, "").extractDrain(tarArchive(t, hdrs, bodies), hostDst, nil, nil, s)
}

// drainRootDir is a fresh host dir as extractDrain receives it: resolved by
// drainRoot, the way drainRunFilesDir resolves every declared dir before
// its first attempt — a temp dir reached through a link of the platform's
// own is otherwise a root every landing reads as outside.
func drainRootDir(t *testing.T) string {
	t.Helper()
	root, err := drainRoot(t.TempDir())
	if err != nil {
		t.Fatalf("resolve the test's host dir: %v", err)
	}
	return root
}

// TestTheDrainMeterFollowsTheBytes pins, in the archive orders the shim
// cannot choose, the two guards that keep the meter on the bytes the host
// holds: a link that replaces a file takes that file's bytes off the
// meter, and a file is counted where the write landed, not under the name
// that reached it through a link.
func TestTheDrainMeterFollowsTheBytes(t *testing.T) {
	const capBytes = 10
	t.Run("a raced file replaced by a link gives its room back", func(t *testing.T) {
		hostDst := drainRootDir(t)
		s := newDrainState(capBytes)
		if _, err := extractInto(t, hostDst, s, []*tar.Header{
			{Name: "./link", Typeflag: tar.TypeReg, Mode: 0o644},
		}, map[string]string{"./link": "01234567"}); err != nil {
			t.Fatalf("attempt 1: %v", err)
		}
		// The link lands first, over the file: the file's 8 bytes leave the
		// host, and its target needs them.
		if _, err := extractInto(t, hostDst, s, []*tar.Header{
			{Name: "./link", Typeflag: tar.TypeSymlink, Linkname: "target.md"},
			{Name: "./target.md", Typeflag: tar.TypeReg, Mode: 0o644},
		}, map[string]string{"./target.md": "abcdefgh"}); err != nil {
			t.Fatalf("attempt 2 = %v, want target.md landed — the file the link replaced holds nothing any more", err)
		}
	})
	// A raced file replaced through a link: the replacement releases the
	// bytes where they are, never under the name that reached them — read by
	// name, the meter keeps the 8 bytes the host no longer holds, and the
	// file after it is refused for room the host has. One scenario per kind
	// that replaces a file.
	for _, tc := range []struct {
		kind        string
		replacement *tar.Header
	}{
		{"a dir", &tar.Header{Name: "./l/a/", Typeflag: tar.TypeDir, Mode: 0o755}},
		{"a link", &tar.Header{Name: "./l/a", Typeflag: tar.TypeSymlink, Linkname: "../b"}},
	} {
		t.Run("a raced file "+tc.kind+" replaces through a link gives its room back", func(t *testing.T) {
			hostDst := drainRootDir(t)
			s := newDrainState(capBytes)
			if _, err := extractInto(t, hostDst, s, []*tar.Header{
				{Name: "./d/", Typeflag: tar.TypeDir, Mode: 0o755},
				{Name: "./l", Typeflag: tar.TypeSymlink, Linkname: "d"},
				{Name: "./l/a", Typeflag: tar.TypeReg, Mode: 0o644},
			}, map[string]string{"./l/a": "01234567"}); err != nil {
				t.Fatalf("attempt 1: %v", err)
			}
			if _, err := extractInto(t, hostDst, s, []*tar.Header{
				tc.replacement,
				{Name: "./b", Typeflag: tar.TypeReg, Mode: 0o644},
			}, map[string]string{"./b": "abcdefgh"}); err != nil {
				t.Fatalf("attempt 2 = %v, want b landed — the file at d/a the replacement took holds nothing any more", err)
			}
			if held := hostDirBytes(t, hostDst); held > capBytes {
				t.Errorf("the host holds %d bytes %v, want at most the %d-byte cap", held, hostDirEntries(t, hostDst), capBytes)
			}
		})
	}
	t.Run("a file counts where it landed, not under the name that reached it", func(t *testing.T) {
		hostDst := drainRootDir(t)
		s := newDrainState(capBytes)
		// A crafted archive: p links to q, p/a.txt lands through it at
		// q/a.txt, then p becomes a real dir and p/a.txt a link — a removal
		// at the NAME p/a.txt that removes no bytes. Keyed by name, the
		// meter released 8 bytes q/a.txt still holds.
		if _, err := extractInto(t, hostDst, s, []*tar.Header{
			{Name: "./q/", Typeflag: tar.TypeDir, Mode: 0o755},
			{Name: "./p", Typeflag: tar.TypeSymlink, Linkname: "q"},
			{Name: "./p/a.txt", Typeflag: tar.TypeReg, Mode: 0o644},
			{Name: "./p/", Typeflag: tar.TypeDir, Mode: 0o755},
			{Name: "./p/a.txt", Typeflag: tar.TypeSymlink, Linkname: "../q/a.txt"},
		}, map[string]string{"./p/a.txt": "01234567"}); err != nil {
			t.Fatalf("attempt 1: %v", err)
		}
		if got, readErr := os.ReadFile(filepath.Join(hostDst, "q", "a.txt")); readErr != nil || string(got) != "01234567" {
			t.Fatalf("q/a.txt = %q (%v) — the fixture did not land the bytes through the link, so this proves nothing", got, readErr)
		}
		if _, err := extractInto(t, hostDst, s, []*tar.Header{
			{Name: "./big.bin", Typeflag: tar.TypeReg, Mode: 0o644},
		}, map[string]string{"./big.bin": "0123456789"}); err != nil {
			t.Fatalf("attempt 2 = %v, want big.bin landed — q/a.txt is an earlier attempt's leftover, swept for its room", err)
		}
		if held := hostDirBytes(t, hostDst); held > capBytes {
			t.Errorf("the host holds %d bytes %v, want at most the %d-byte cap — the meter lost the bytes a link had moved", held, hostDirEntries(t, hostDst), capBytes)
		}
	})
	t.Run("a hardlink's copy counts where it landed, not under the name that reached it", func(t *testing.T) {
		hostDst := drainRootDir(t)
		s := newDrainState(capBytes)
		// p/h lands through p -> q as a copy at q/h, then p becomes a real
		// dir and p/h a link: a removal at the NAME p/h that removes no
		// bytes. Keyed by name, the meter released the 4 bytes q/h holds,
		// and the next file fits without the sweep that frees them.
		if _, err := extractInto(t, hostDst, s, []*tar.Header{
			{Name: "./q/", Typeflag: tar.TypeDir, Mode: 0o755},
			{Name: "./p", Typeflag: tar.TypeSymlink, Linkname: "q"},
			{Name: "./s", Typeflag: tar.TypeReg, Mode: 0o644},
			{Name: "./p/h", Typeflag: tar.TypeLink, Linkname: "./s"},
			{Name: "./p/", Typeflag: tar.TypeDir, Mode: 0o755},
			{Name: "./p/h", Typeflag: tar.TypeSymlink, Linkname: "../q/h"},
		}, map[string]string{"./s": "0123"}); err != nil {
			t.Fatalf("attempt 1: %v", err)
		}
		if got, readErr := os.ReadFile(filepath.Join(hostDst, "q", "h")); readErr != nil || string(got) != "0123" {
			t.Fatalf("q/h = %q (%v) — the fixture did not land the copy through the link, so this proves nothing", got, readErr)
		}
		if _, err := extractInto(t, hostDst, s, []*tar.Header{
			{Name: "./big.bin", Typeflag: tar.TypeReg, Mode: 0o644},
		}, map[string]string{"./big.bin": "012345"}); err != nil {
			t.Fatalf("attempt 2 = %v, want big.bin landed — the earlier attempt's files are swept for its room", err)
		}
		if held := hostDirBytes(t, hostDst); held > capBytes {
			t.Errorf("the host holds %d bytes %v, want at most the %d-byte cap — the meter lost the bytes a link had moved", held, hostDirEntries(t, hostDst), capBytes)
		}
	})
	t.Run("a sweep that takes a link in a member's path re-reads where it lands", func(t *testing.T) {
		hostDst := drainRootDir(t)
		s := newDrainState(capBytes)
		if _, err := extractInto(t, hostDst, s, []*tar.Header{
			{Name: "./q/", Typeflag: tar.TypeDir, Mode: 0o755},
			{Name: "./p", Typeflag: tar.TypeSymlink, Linkname: "q"},
			{Name: "./q/old.bin", Typeflag: tar.TypeReg, Mode: 0o644},
		}, map[string]string{"./q/old.bin": "01234567"}); err != nil {
			t.Fatalf("attempt 1: %v", err)
		}
		// p/x would land at q/x through the earlier attempt's link — but the
		// room it needs is that attempt's, and the sweep that frees it
		// takes the link too: the write lands at p/x, and the meter must
		// hold it there, where a later removal will look for it.
		if _, err := extractInto(t, hostDst, s, []*tar.Header{
			{Name: "./p/x", Typeflag: tar.TypeReg, Mode: 0o644},
		}, map[string]string{"./p/x": "abcdefgh"}); err != nil {
			t.Fatalf("attempt 2: %v", err)
		}
		if got, readErr := os.ReadFile(filepath.Join(hostDst, "p", "x")); readErr != nil || string(got) != "abcdefgh" {
			t.Fatalf("p/x = %q (%v) — the fixture did not land the member at its own path, so this proves nothing", got, readErr)
		}
		if want := map[string]int64{"p/x": 8}; !reflect.DeepEqual(s.held, want) || s.total != 8 {
			t.Errorf("the meter holds %v (total %d), want %v — counted where the link USED to send the write, its bytes would never leave the meter with the file", s.held, s.total, want)
		}
	})
}

// TestExportRunFilesDrainsIntoAHostDirReachedThroughALink: where a write
// lands is read through every link of its chain, the host dir's own
// ancestors included — a store dir reached through a link must still hold
// every landing inside it, or every member would be refused as outside.
func TestExportRunFilesDrainsIntoAHostDirReachedThroughALink(t *testing.T) {
	realDir := t.TempDir()
	via := filepath.Join(t.TempDir(), "store")
	if err := os.Symlink(realDir, via); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	kubectl, _ := exportShim(t, podTree(t, map[string]string{"report.md": "# audit", "out/deep.bin": "\x01\x02"}), 0, "")
	r := drainRun([]sandbox.ReadBackDir{{ContainerPath: "/iterion/artifact-files", HostDir: via}}, kubectl)

	if err := r.ExportRunFiles(context.Background()); err != nil {
		t.Fatalf("ExportRunFiles = %v, want the tree drained through the host dir's link", err)
	}
	if got, err := os.ReadFile(filepath.Join(realDir, "report.md")); err != nil || string(got) != "# audit" {
		t.Errorf("report.md = %q (%v), want it landed in the dir the link names", got, err)
	}
	if got, err := os.ReadFile(filepath.Join(realDir, "out", "deep.bin")); err != nil || len(got) != 2 {
		t.Errorf("out/deep.bin = %v (%v), want the nested member landed", got, err)
	}
}

// TestExportRunFilesJudgesAHostDirReachedThroughALinkWhereItStands: every
// judgment the drain makes of what the host held before it — the proof a
// raced dir's removal stands on, the sweep a settled attempt runs, and the
// one a member that needs the room runs — reads the directory the writes
// land in. A snapshot taken through a host dir that is itself a link sees
// the link alone: a pre-drain tree then reads as a raced one and is
// removed, a pre-drain file as a raced leftover, and a sweep handed the
// link walks nothing. One scenario per judgment.
func TestExportRunFilesJudgesAHostDirReachedThroughALinkWhereItStands(t *testing.T) {
	const preDrain = "the store held me before the drain"
	// linkedHostDir is a host dir reached through a link of its own, the
	// directory the link names, and a pre-drain file written at rel there.
	linkedHostDir := func(t *testing.T, rel string) (via, realDir, own string) {
		t.Helper()
		realDir = t.TempDir()
		via = filepath.Join(t.TempDir(), "store")
		if err := os.Symlink(realDir, via); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		own = filepath.Join(realDir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(own), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(own, []byte(preDrain), 0o644); err != nil {
			t.Fatal(err)
		}
		return via, realDir, own
	}
	wantIntact := func(t *testing.T, own string) {
		t.Helper()
		if got, err := os.ReadFile(own); err != nil || string(got) != preDrain {
			t.Errorf("pre-drain %s = %q (%v), want it intact — the drain took what the host already had", own, got, err)
		}
	}
	t.Run("a pre-drain tree where the archive carries a file is declined, never removed", func(t *testing.T) {
		via, realDir, own := linkedHostDir(t, "keep/old.txt")
		kubectl, calls := exportShim(t, podTree(t, map[string]string{"keep": "the pod settled on a file", "report.md": "# complete"}), 0, "")
		r := drainRun([]sandbox.ReadBackDir{{ContainerPath: "/iterion/artifact-files", HostDir: via}}, kubectl)

		wantDeclinedLoss(t, r.ExportRunFiles(context.Background()), sandbox.ReadBackDeclinedMember{Dir: "/iterion/artifact-files", Name: "keep", Why: sandbox.DeclinedPreDrainTree})
		wantIntact(t, own)
		if got, err := os.ReadFile(filepath.Join(realDir, "report.md")); err != nil || string(got) != "# complete" {
			t.Errorf("report.md = %q (%v), want the rest of the set landed past the declined member", got, err)
		}
		if calls() != 1 {
			t.Errorf("the drain archived the pod %d times, want once", calls())
		}
	})
	t.Run("a retried drain sweeps what only the raced attempt landed", func(t *testing.T) {
		fastExportRetries(t)
		via, realDir, own := linkedHostDir(t, "host-own.txt")
		kubectl, calls := exportShimSeq(t, []string{
			podTree(t, map[string]string{"report.md": "# partial", "stale.txt": "the pod dropped me before the settled archive"}),
			podTree(t, map[string]string{"report.md": "# complete"}),
		}, 1, measuredRaceStderr)
		r := drainRun([]sandbox.ReadBackDir{{ContainerPath: "/iterion/artifact-files", HostDir: via}}, kubectl)

		if err := r.ExportRunFiles(context.Background()); err != nil {
			t.Fatalf("ExportRunFiles = %v, want the raced drain retried to success", err)
		}
		if _, err := os.Lstat(filepath.Join(realDir, "stale.txt")); !os.IsNotExist(err) {
			t.Errorf("stale.txt stands (%v) — the sweep walked the link instead of the dir it names, and the clean verdict covers a file the pod no longer has", err)
		}
		wantIntact(t, own)
		if got, err := os.ReadFile(filepath.Join(realDir, "report.md")); err != nil || string(got) != "# complete" {
			t.Errorf("report.md = %q (%v), want the settled archive's member", got, err)
		}
		if calls() != 2 {
			t.Errorf("the drain archived the pod %d times, want the raced attempt retried once", calls())
		}
	})
	t.Run("the sweep a member that needs the room runs never takes a pre-drain file", func(t *testing.T) {
		fastExportRetries(t)
		via, realDir, own := linkedHostDir(t, "host-own.txt")
		// The raced attempt lands 8 bytes the settled archive no longer
		// carries; the settled member's 8 need their room under a 10-byte
		// cap, and the sweep that frees it runs mid-attempt.
		kubectl, calls := exportShimSeq(t, []string{
			podTree(t, map[string]string{"scratch.bin": "01234567"}),
			podTree(t, map[string]string{"report.md": "abcdefgh"}),
		}, 1, measuredRaceStderr)
		r := drainRun([]sandbox.ReadBackDir{{ContainerPath: "/iterion/artifact-files", HostDir: via, CapBytes: 10}}, kubectl)

		if err := r.ExportRunFiles(context.Background()); err != nil {
			t.Fatalf("ExportRunFiles = %v, want the settled member landed in the room the raced leftover held", err)
		}
		wantIntact(t, own)
		if _, err := os.Lstat(filepath.Join(realDir, "scratch.bin")); !os.IsNotExist(err) {
			t.Errorf("scratch.bin stands (%v), want it swept for the settled member's room", err)
		}
		if got, err := os.ReadFile(filepath.Join(realDir, "report.md")); err != nil || string(got) != "abcdefgh" {
			t.Errorf("report.md = %q (%v), want the settled member whole", got, err)
		}
		if calls() != 2 {
			t.Errorf("the drain archived the pod %d times, want the raced attempt then the settled one", calls())
		}
	})
}

// TestExportRunFilesStopsThePodArchiverOnceTheExtractFails: an extract the
// cap ended lands nothing more of the archive, so the drain stops the pod's
// archiver instead of reading the rest of its stream only to throw it away —
// a tool that wrote far past the cap would otherwise stream every byte of it
// through the teardown until the drain's deadline killed kubectl.
func TestExportRunFilesStopsThePodArchiverOnceTheExtractFails(t *testing.T) {
	const body = 32 << 20
	// The archive's head: one member declaring far more than the cap lets
	// land; its body follows from the shim, streamed.
	var head bytes.Buffer
	if err := tar.NewWriter(&head).WriteHeader(&tar.Header{Name: "big.bin", Size: body, Typeflag: tar.TypeReg, Mode: 0o644}); err != nil {
		t.Fatal(err)
	}
	kubectl, streamedToTheEnd := streamingArchiverShim(t, head.Bytes(), body)
	r := drainRun([]sandbox.ReadBackDir{{ContainerPath: "/iterion/artifact-files", HostDir: t.TempDir(), CapBytes: 16}}, kubectl)

	err := r.ExportRunFiles(context.Background())
	if got := sandbox.ReadBackLossReasonOf(err); got != sandbox.ReadBackOverCap {
		t.Fatalf("ExportRunFiles = %v (reason %q), want %q", err, got, sandbox.ReadBackOverCap)
	}
	if _, statErr := os.Stat(streamedToTheEnd); statErr == nil {
		t.Errorf("the pod's archiver streamed its whole archive after the cap ended the extract — the drain read %d bytes it would never land", body)
	}
}

// archiveWithTail is an archive of one member followed by a megabyte of
// what tar pads its last record with — more than any pipe's default buffer
// holds, so an archiver whose tail nobody reads blocks on it and never
// exits.
func archiveWithTail(t *testing.T, name, body string) []byte {
	t.Helper()
	archive := tarArchive(t, []*tar.Header{{Name: name, Typeflag: tar.TypeReg, Mode: 0o644}}, map[string]string{name: body}).Bytes()
	return append(archive, make([]byte, 1<<20)...)
}

// TestExportRunFilesReadsTheArchiverToItsEnd: the extract stops at the
// end-of-archive marker, and what tar sends past it is padding — but an
// archiver whose output nobody reads blocks on it and never exits: the
// drain would wait on it until its deadline, and never read the exit that
// says whether the archive raced a writer. Here the first attempt's exit
// says it raced; only an exit read past the tail retries it.
func TestExportRunFilesReadsTheArchiverToItsEnd(t *testing.T) {
	fastExportRetries(t)
	hostDst := t.TempDir()
	kubectl, calls := archiveShimSeq(t, [][]byte{archiveWithTail(t, "./report.md", "# audit")}, 1, measuredRaceStderr)
	r := drainRun([]sandbox.ReadBackDir{{ContainerPath: "/iterion/artifact-files", HostDir: hostDst}}, kubectl)

	if err := r.ExportRunFiles(context.Background()); err != nil {
		t.Fatalf("ExportRunFiles = %v, want the raced archive retried to success", err)
	}
	if n := calls(); n != 2 {
		t.Errorf("the drain archived the pod %d times, want the raced attempt then the settled one — the race warning in the first exit went unread", n)
	}
	if got, err := os.ReadFile(filepath.Join(hostDst, "report.md")); err != nil || string(got) != "# audit" {
		t.Errorf("report.md = %q (%v), want the member landed", got, err)
	}
}

// TestExportRunFilesSaysOneRacedAttemptLeftNoUnion: the stale mark says a
// raced attempt's members may stand beside those of the attempt that ended
// the drain — it needs an earlier attempt. One raced archive alone, whether
// the retry budget allowed no other (ITERION_SANDBOX_EXPORT_ATTEMPTS=1, an
// accepted override) or the drain's context ended the wait for the next, is
// unsettled — the tree changed under it — and no union: neither the event's
// mark nor the error may call it a stale superset. Abandoned after a second
// attempt, the union is there, and so is the mark.
func TestExportRunFilesSaysOneRacedAttemptLeftNoUnion(t *testing.T) {
	for _, tc := range []struct {
		name     string
		attempts string
		// pause is the first pause between attempts; abandonAfter the retry
		// line the drain's context is cancelled on (0: never). An abandoned
		// wait is pinned by a pause no test outlasts, or by a second pause
		// twice the first, the context already done before it starts.
		pause        string
		abandonAfter int
		wantCalls    int
		wantStale    bool
	}{
		{name: "a retry budget of one", attempts: "1", pause: "5ms", wantCalls: 1},
		{name: "a retry abandoned after the first attempt", attempts: "3", pause: "1h", abandonAfter: 1, wantCalls: 1},
		{name: "a retry abandoned after the second attempt", attempts: "3", pause: "50ms", abandonAfter: 2, wantCalls: 2, wantStale: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("ITERION_SANDBOX_EXPORT_ATTEMPTS", tc.attempts)
			t.Setenv("ITERION_SANDBOX_EXPORT_RETRY_PAUSE", tc.pause)
			hostDst := t.TempDir()
			kubectl, calls := exportShimSeq(t, []string{
				podTree(t, map[string]string{"report.md": "# first", "first.txt": "only the first archive carries me"}),
				podTree(t, map[string]string{"report.md": "# second"}),
			}, 99, measuredRaceStderr)
			r := drainRun([]sandbox.ReadBackDir{{ContainerPath: "/iterion/artifact-files", HostDir: hostDst}}, kubectl)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			retries := 0
			r.driver.logger = iterlog.New(iterlog.LevelWarn, io.Discard)
			r.driver.logger.SetHook(func(_ iterlog.Level, msg string, _ map[string]any) {
				if strings.Contains(msg, "retrying") {
					if retries++; retries == tc.abandonAfter {
						cancel()
					}
				}
			})

			err := r.ExportRunFiles(ctx)
			if got := sandbox.ReadBackLossReasonOf(err); got != sandbox.ReadBackUnsettled {
				t.Fatalf("ExportRunFiles = %v (reason %q), want %q — every attempt that ran raced a writer", err, got, sandbox.ReadBackUnsettled)
			}
			if n := calls(); n != tc.wantCalls {
				t.Fatalf("the drain archived the pod %d times, want %d — the scenario never reached the verdict it pins", n, tc.wantCalls)
			}
			if got := sandbox.ReadBackStaleOf(err); got != tc.wantStale {
				t.Errorf("ReadBackStaleOf = %t after %d attempt(s), want %t — the mark says an earlier attempt's members may stand beside the last one's", got, tc.wantCalls, tc.wantStale)
			}
			if says := strings.Contains(err.Error(), "stale superset"); says != tc.wantStale {
				t.Errorf("error = %v\nnames a stale superset: %t, want %t", err, says, tc.wantStale)
			}
			// The disk the verdict describes: the first archive's own member
			// stands either way — alone with its archive after one attempt,
			// beside a second archive that lacks it after two.
			if _, statErr := os.Stat(filepath.Join(hostDst, "first.txt")); statErr != nil {
				t.Errorf("first.txt = %v, want the first archive's member standing", statErr)
			}
		})
	}
}
