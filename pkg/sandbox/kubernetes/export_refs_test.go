package kubernetes

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The export is a tar overlay, and tar cannot delete: when the pod ran
// `git gc`/`pack-refs --all --prune`, its refs live ONLY in packed-refs
// while the host still holds the pre-run loose files — and git resolves
// loose before packed, so the exported clone would read a pre-run HEAD
// with every object present but unreachable by ref. clearHostLooseRefs
// makes the pod's ref state authoritative. Falsified both ways: the
// control run WITHOUT clearing reads the stale baseline (the exact
// defect), the run WITH clearing reads the pod's HEAD and the objects
// resolve.

func trun(t *testing.T, dir string, name string, args ...string) string {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v: %v\n%s", name, args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// overlayExport mimics ExportWorkspace's tar pipe byte-for-byte:
// pod-side archive of "." with the export excludes, extracted over host.
func overlayExport(t *testing.T, pod, host string) {
	t.Helper()
	archive := filepath.Join(t.TempDir(), "export.tar")
	trun(t, pod, "tar", "--exclude=./.git/config", "--exclude=./.git/iterion-credentials", "-cf", archive, ".")
	trun(t, host, "tar", "-xf", archive)
}

// gcShadowFixture builds: a host clone at BASE with a loose ref, and a
// pod copy that committed work then packed its refs (loose deleted).
// Returns (host, pod, base, podHead).
func gcShadowFixture(t *testing.T) (string, string, string, string) {
	t.Helper()
	tmp := t.TempDir()
	origin := filepath.Join(tmp, "origin")
	trun(t, tmp, "git", "init", "-q", origin)
	trun(t, origin, "git", "config", "user.email", "t@test.invalid")
	trun(t, origin, "git", "config", "user.name", "t")
	trun(t, origin, "git", "commit", "-q", "--allow-empty", "-m", "base")

	host := filepath.Join(tmp, "host")
	trun(t, tmp, "git", "clone", "-q", origin, host)
	base := trun(t, host, "git", "rev-parse", "HEAD")
	// git clone may deliver packed refs; the runner's checkout -B (and any
	// branch update) writes a LOOSE ref — pin the loose state explicitly.
	trun(t, host, "git", "update-ref", "refs/heads/"+trun(t, host, "git", "rev-parse", "--abbrev-ref", "HEAD"), base)

	pod := filepath.Join(tmp, "pod")
	trun(t, tmp, "cp", "-r", host, pod)
	trun(t, pod, "git", "config", "user.email", "t@test.invalid")
	trun(t, pod, "git", "config", "user.name", "t")
	trun(t, pod, "git", "commit", "-q", "--allow-empty", "-m", "work")
	podHead := trun(t, pod, "git", "rev-parse", "HEAD")
	trun(t, pod, "git", "pack-refs", "--all", "--prune")
	return host, pod, base, podHead
}

func TestClearHostLooseRefsMakesExportRefsAuthoritative(t *testing.T) {
	t.Run("control: WITHOUT clearing, the stale loose ref shadows the pod's packed ref", func(t *testing.T) {
		host, pod, base, podHead := gcShadowFixture(t)
		overlayExport(t, pod, host)
		if got := trun(t, host, "git", "rev-parse", "HEAD"); got != base {
			t.Fatalf("control invalidated: host reads %s, expected the stale baseline %s — the defect this fix targets no longer reproduces", got, base)
		}
		// The objects DID arrive — that is what makes the stale read a lie.
		trun(t, host, "git", "cat-file", "-e", podHead+"^{commit}")
	})
	t.Run("with clearing, the pod's ref state lands authoritative", func(t *testing.T) {
		host, pod, _, podHead := gcShadowFixture(t)
		if err := clearHostLooseRefs(filepath.Join(host, ".git")); err != nil {
			t.Fatalf("clearHostLooseRefs: %v", err)
		}
		overlayExport(t, pod, host)
		if got := trun(t, host, "git", "rev-parse", "HEAD"); got != podHead {
			t.Fatalf("host reads %s, want the pod's HEAD %s", got, podHead)
		}
	})
	t.Run("nominal loose-ref export is untouched by clearing", func(t *testing.T) {
		host, pod, _, _ := gcShadowFixture(t)
		// Pod makes a FURTHER commit after the gc: the branch ref is loose
		// again — the common shape. Clearing must not disturb it.
		trun(t, pod, "git", "commit", "-q", "--allow-empty", "-m", "post-gc work")
		finalHead := trun(t, pod, "git", "rev-parse", "HEAD")
		if err := clearHostLooseRefs(filepath.Join(host, ".git")); err != nil {
			t.Fatalf("clearHostLooseRefs: %v", err)
		}
		overlayExport(t, pod, host)
		if got := trun(t, host, "git", "rev-parse", "HEAD"); got != finalHead {
			t.Fatalf("host reads %s, want %s", got, finalHead)
		}
	})
	t.Run("missing refs dir is a no-op", func(t *testing.T) {
		if err := clearHostLooseRefs(filepath.Join(t.TempDir(), ".git")); err != nil {
			t.Fatalf("expected no-op on a missing refs dir, got %v", err)
		}
	})
}
