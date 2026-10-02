package kubernetes

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/sandbox"
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
	overlayExportArchived(t, pod, host, tarExcludeArgs())
}

// overlayExportArchived streams pod into host the way exportOnce does: the
// archiver is given podArgs, and the host side is the production extract.
func overlayExportArchived(t *testing.T, pod, host string, podArgs []string) {
	t.Helper()
	archive := filepath.Join(t.TempDir(), "export.tar")
	trun(t, pod, "tar", append(append([]string{}, podArgs...), "-cf", archive, ".")...)
	f, err := os.Open(archive)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	run := &Run{driver: &Driver{logger: iterlog.Nop()}, info: sandbox.RunInfo{WorkspacePath: host}}
	if _, err := run.extractExport(f, host); err != nil {
		t.Fatalf("host extract: %v", err)
	}
}

// muteGitBackground disables git's detached background maintenance in a
// repo: auto-gc forked by `git commit` can still be rewriting
// .git/objects when the test's tar starts, and GNU tar then exits 1
// ("file changed as we read it") — a fixture race, not the behavior
// under test.
func muteGitBackground(t *testing.T, repo string) {
	t.Helper()
	trun(t, repo, "git", "config", "gc.auto", "0")
	trun(t, repo, "git", "config", "gc.autoDetach", "false")
	trun(t, repo, "git", "config", "maintenance.auto", "false")
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
	muteGitBackground(t, origin)
	trun(t, origin, "git", "commit", "-q", "--allow-empty", "-m", "base")

	host := filepath.Join(tmp, "host")
	trun(t, tmp, "git", "clone", "-q", origin, host)
	muteGitBackground(t, host)
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

// A hook the pod wrote stays in the pod: the export never carries .git/hooks
// back, so the host's next commit or push runs none of the sandboxed run's
// code.
func TestExportNeverCarriesThePodsHooksToTheHost(t *testing.T) {
	host, pod, _, _ := gcShadowFixture(t)
	marker := filepath.Join(t.TempDir(), "pod-hook-ran")
	hook := filepath.Join(pod, ".git", "hooks", "pre-commit")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\ntouch "+marker+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	overlayExport(t, pod, host)
	if _, err := os.Stat(filepath.Join(host, ".git", "hooks", "pre-commit")); err == nil {
		t.Fatal("the pod's hook reached the host clone")
	}
	trun(t, host, "git", "config", "user.email", "t@test.invalid")
	trun(t, host, "git", "config", "user.name", "t")
	trun(t, host, "git", "commit", "-q", "--allow-empty", "-m", "host commit")
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the host's commit ran a hook the pod wrote")
	}
}

// The exclusions are the HOST's: the archiver runs in the sandbox, on an
// image the workflow chooses, so what the host must not receive it drops
// itself — whatever the archive carries.
func TestTheHostExtractDropsTheExcludedMembersWhateverTheArchiveCarries(t *testing.T) {
	host, pod, _, _ := gcShadowFixture(t)
	hostConfig := filepath.Join(host, ".git", "config")
	before, err := os.ReadFile(hostConfig)
	if err != nil {
		t.Fatal(err)
	}
	creds := filepath.Join(host, ".git", "iterion-credentials")
	if err := os.WriteFile(creds, []byte("host-live-credential\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for rel, body := range map[string]string{
		".git/config":              "[core]\n\tpod = wrote-this\n",
		".git/iterion-credentials": "pod-stale-credential\n",
		".git/hooks/pre-commit":    "#!/bin/sh\nexit 0\n",
	} {
		if err := os.WriteFile(filepath.Join(pod, filepath.FromSlash(rel)), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// An archiver that honours none of the --exclude flags it was given.
	overlayExportArchived(t, pod, host, nil)

	if got, err := os.ReadFile(hostConfig); err != nil || string(got) != string(before) {
		t.Errorf("the host clone's .git/config was overwritten by the archive: %q (%v)", got, err)
	}
	if got, err := os.ReadFile(creds); err != nil || string(got) != "host-live-credential\n" {
		t.Errorf("the host's live credential was overwritten by the archive: %q (%v)", got, err)
	}
	if _, err := os.Stat(filepath.Join(host, ".git", "hooks", "pre-commit")); err == nil {
		t.Error("a hook the archive carried reached the host clone")
	}
	// The run's work still arrives: the exclusions are not a blanket refusal.
	if _, err := os.Stat(filepath.Join(host, ".git", "packed-refs")); err != nil {
		t.Errorf("the export carried nothing: %v", err)
	}
}

// exportExcluded answers for a member and for everything under it, with or
// without the archive's "./" prefix.
func TestExportExcludedCoversTheTreeUnderEachMember(t *testing.T) {
	for _, rel := range []string{".git/hooks", "./.git/hooks", ".git/hooks/pre-commit", "./.git/hooks/sub/dir/x", ".git/config", ".git/iterion-credentials"} {
		if !exportExcluded(rel) {
			t.Errorf("exportExcluded(%q) = false", rel)
		}
	}
	for _, rel := range []string{".git/packed-refs", ".git/config.worktree", ".git/hooks-extra", "./src/.git/hooks", ".gitignore"} {
		if exportExcluded(rel) {
			t.Errorf("exportExcluded(%q) = true", rel)
		}
	}
}
