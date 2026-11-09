package kubernetes

import (
	"archive/tar"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/sandbox"
)

// WITNESSES (ADR-162 U5): the `.git` floor on the kubernetes driver — the
// only driver whose SupportsNetworkPolicy admits an untrusted run. The
// docker lane floors the tree by NOT binding it (addWorktreeGitMount);
// here the workspace is a COPY, so the floor is the copy itself: the
// populate tar carries the working tree only (populateWorkspace), the
// git plumbing is never wired in the pod (fixupWorkspaceGit skipped), a
// `.git` planted in the pod never lands on the host clone (exportTarget),
// and the host's own loose refs are not cleared for a run whose pod
// carries none to replace them (ExportWorkspace). Every row drives the
// REAL path — Driver.Start end to end against the kubectl shim, the
// production extractExport, the production ExportWorkspace — never a
// reimplementation.
//
// Mutants: drop the `.git` skip in populateWorkspace → the first row;
// drop the skip in fixupWorkspaceGit → the second; drop the
// untrustedExportExcluded term in exportTarget → the third; drop the
// `!untrusted` guard around clearHostLooseRefs in ExportWorkspace → the
// fourth.

// untrustedCloneFixture builds a real clone shaped like the runner's: a
// committed working tree plus the live credential store the runner writes
// at .git/iterion-credentials (installGitCredentialStore).
func untrustedCloneFixture(t *testing.T) string {
	t.Helper()
	isolateExecEnvironment(t)
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	tmp := t.TempDir()
	clone := filepath.Join(tmp, "clone")
	trun(t, tmp, "git", "init", "-q", clone)
	trun(t, clone, "git", "config", "user.email", "t@test.invalid")
	trun(t, clone, "git", "config", "user.name", "t")
	muteGitBackground(t, clone)
	if err := os.WriteFile(filepath.Join(clone, "README.md"), []byte("work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	trun(t, clone, "git", "add", "README.md")
	trun(t, clone, "git", "commit", "-q", "-m", "base")
	if err := os.WriteFile(filepath.Join(clone, ".git", "iterion-credentials"),
		[]byte("https://oauth2:host-live-token@example.com\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return clone
}

// startCapturingThePodCopy runs the REAL Driver.Start on a clone fixture
// and returns the members of the populate tar it streamed through
// `kubectl exec -i … tar -xf -` plus the argv log of every kubectl call.
func startCapturingThePodCopy(t *testing.T, untrusted bool) (members []string, argv string) {
	t.Helper()
	clone := untrustedCloneFixture(t)
	argvLog, stdinLog := kubectlStdinShim(t)
	sched, _ := schedulingFromEnv(envOf(nil))
	d := &Driver{namespace: "ns", kubectl: "kubectl", logger: iterlog.New(iterlog.LevelInfo, io.Discard), sched: sched}
	prepared := &Prepared{spec: sandbox.Spec{Image: "img", User: "1000:1000"}, workspace: "/workspace"}
	if _, err := d.Start(context.Background(), prepared, sandbox.RunInfo{
		RunID:         "01a0-run",
		WorkspacePath: clone,
		UntrustedRun:  untrusted,
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	raw, err := os.ReadFile(stdinLog)
	if err != nil {
		t.Fatalf("no tar was streamed into the pod: %v", err)
	}
	argvRaw, err := os.ReadFile(argvLog)
	if err != nil {
		t.Fatalf("no kubectl argv was recorded: %v", err)
	}
	return tarMembers(t, raw), string(argvRaw)
}

// isGitMember reports whether one populate-tar member names the git-private
// tree (directory entries carry a trailing slash; ".github" is working
// tree, not git state).
func isGitMember(name string) bool {
	name = strings.TrimSuffix(name, "/")
	name = strings.TrimPrefix(name, "./")
	return name == ".git" || strings.HasPrefix(name, ".git/")
}

// TestStartUntrustedCopyCrossesNoGitTree: the populate tar an UNTRUSTED
// run streams into its pod carries the working tree only — never the
// clone's `.git`, the rotated forge token above all — while the trusted
// control still carries the whole clone (the copy stays the trusted
// lane's).
func TestStartUntrustedCopyCrossesNoGitTree(t *testing.T) {
	t.Run("untrusted: no git-private member crosses", func(t *testing.T) {
		members, _ := startCapturingThePodCopy(t, true)
		for _, m := range members {
			if isGitMember(m) {
				t.Fatalf("the populate tar carried git-private state: %s (members: %v)", m, members)
			}
		}
		if !slices.ContainsFunc(members, func(m string) bool {
			return strings.TrimPrefix(strings.TrimSuffix(m, "/"), "./") == "README.md"
		}) {
			t.Fatalf("the working tree did not cross — the floor is not a blanket refusal: %v", members)
		}
	})
	t.Run("trusted control: the whole clone crosses", func(t *testing.T) {
		members, _ := startCapturingThePodCopy(t, false)
		if !slices.ContainsFunc(members, func(m string) bool {
			return strings.TrimPrefix(m, "./") == ".git/iterion-credentials"
		}) {
			t.Fatalf("the trusted copy lost its git tree — the floor is the fork lane's, not the trusted lane's: %v", members)
		}
	})
}

// TestStartUntrustedRunGetsNoGitFixup: the post-copy fixup re-points the
// copied clone's credential.helper at the in-pod credential file — wiring
// the very channel the floor withholds. An UNTRUSTED run must never issue
// it; the trusted control still does.
func TestStartUntrustedRunGetsNoGitFixup(t *testing.T) {
	t.Run("untrusted: the fixup exec is never issued", func(t *testing.T) {
		_, argv := startCapturingThePodCopy(t, true)
		for _, line := range strings.Split(argv, "\n") {
			if strings.Contains(line, ".git/worktrees") { // the script's own bytes
				t.Fatalf("the git fixup ran for an untrusted run: %s", line)
			}
		}
	})
	t.Run("trusted control: the fixup still wires the copy", func(t *testing.T) {
		_, argv := startCapturingThePodCopy(t, false)
		if !strings.Contains(argv, ".git/worktrees") {
			t.Fatal("the trusted control no longer runs the fixup — this row proves the skip is the untrusted gate, not a lost phase")
		}
	})
}

// plantedGitArchive builds the archive an adversarial pod would send back:
// the run's work beside a planted `.git` — config, a credential store, a
// hook, a ref — with the "./" prefixes the in-pod tar emits.
func plantedGitArchive(t *testing.T) *os.File {
	t.Helper()
	f, err := os.Create(filepath.Join(t.TempDir(), "export.tar"))
	if err != nil {
		t.Fatal(err)
	}
	tw := tar.NewWriter(f)
	write := func(name, body string) {
		t.Helper()
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	write("./work.txt", "the run's work\n")
	write("./.git/HEAD", "ref: refs/heads/pwned\n")
	write("./.git/config", "[core]\n\tfsmonitor = /pod/bin/pwn\n")
	write("./.git/iterion-credentials", "https://oauth2:pod-planted@example.com\n")
	write("./.git/hooks/pre-commit", "#!/bin/sh\n/pod/bin/pwn\n")
	write("./.git/refs/heads/pwned", "0000000000000000000000000000000000000001\n")
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := os.Open(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	return reopened
}

// TestUntrustedExportDropsTheWholeGitTree: whatever the pod's archiver
// carries, an UNTRUSTED run's export extract lands no member under `.git`
// on the host clone — decided on the landing path, host-side, the same
// place the classic excludes are. The trusted control still receives the
// pod's git state minus those classic excludes.
func TestUntrustedExportDropsTheWholeGitTree(t *testing.T) {
	exportFixture := func(t *testing.T) string {
		host := t.TempDir()
		for rel, body := range map[string]string{
			".git/HEAD":                "host-head\n",
			".git/refs/heads/main":     "host-loose-ref\n",
			".git/iterion-credentials": "host-live-credential\n",
		} {
			if err := os.MkdirAll(filepath.Dir(filepath.Join(host, filepath.FromSlash(rel))), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(host, filepath.FromSlash(rel)), []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		return host
	}

	t.Run("untrusted: the work lands, no .git member does", func(t *testing.T) {
		host := exportFixture(t)
		archive := plantedGitArchive(t)
		defer func() { _ = archive.Close() }()
		run := &Run{driver: &Driver{logger: iterlog.Nop()}, info: sandbox.RunInfo{WorkspacePath: host, UntrustedRun: true}}
		if _, err := run.extractExport(archive, host); err != nil {
			t.Fatalf("extractExport: %v", err)
		}
		if got, err := os.ReadFile(filepath.Join(host, "work.txt")); err != nil || string(got) != "the run's work\n" {
			t.Errorf("the run's work did not land: %q (%v) — the floor is not a blanket refusal", got, err)
		}
		for rel, want := range map[string]string{
			".git/HEAD":                "host-head\n",
			".git/config":              "", // must not even exist: the host fixture wrote none
			".git/refs/heads/pwned":    "",
			".git/refs/heads/main":     "host-loose-ref\n",
			".git/iterion-credentials": "host-live-credential\n",
			".git/hooks/pre-commit":    "",
		} {
			got, err := os.ReadFile(filepath.Join(host, filepath.FromSlash(rel)))
			if want == "" {
				if err == nil {
					t.Errorf("%s landed from the pod's archive: %q", rel, got)
				}
				continue
			}
			if err != nil || string(got) != want {
				t.Errorf("%s = %q (%v), want the host's own %q", rel, got, err, want)
			}
		}
	})

	t.Run("trusted control: the pod's git state lands, minus the classic excludes", func(t *testing.T) {
		host := exportFixture(t)
		archive := plantedGitArchive(t)
		defer func() { _ = archive.Close() }()
		run := &Run{driver: &Driver{logger: iterlog.Nop()}, info: sandbox.RunInfo{WorkspacePath: host}}
		if _, err := run.extractExport(archive, host); err != nil {
			t.Fatalf("extractExport: %v", err)
		}
		if got, err := os.ReadFile(filepath.Join(host, ".git", "HEAD")); err != nil || string(got) != "ref: refs/heads/pwned\n" {
			t.Errorf("trusted control: .git/HEAD = %q (%v), want the pod's — the export keeps carrying git state for a trusted run", got, err)
		}
		// config and hooks are the CLASSIC excludes and the host fixture
		// carries neither: they must not appear. The credential store the
		// host DOES carry (its live copy) must keep the host's bytes — the
		// pod's planted one never overwrites it.
		for _, rel := range []string{".git/config", ".git/hooks/pre-commit"} {
			if _, err := os.Stat(filepath.Join(host, filepath.FromSlash(rel))); err == nil {
				t.Errorf("trusted control: %s landed although it is a classic exclude", rel)
			}
		}
		if got, err := os.ReadFile(filepath.Join(host, ".git", "iterion-credentials")); err != nil || string(got) != "host-live-credential\n" {
			t.Errorf("trusted control: the host's live credential was overwritten by the pod's: %q (%v)", got, err)
		}
		if got, err := os.ReadFile(filepath.Join(host, ".git", "refs", "heads", "pwned")); err != nil || string(got) != "0000000000000000000000000000000000000001\n" {
			t.Errorf("trusted control: the pod's loose ref did not land: %q (%v)", got, err)
		}
	})
}

// TestUntrustedExportClearsNoHostLooseRefs: the pre-extract
// clearHostLooseRefs exists so the pod's packed refs are not shadowed by
// the host's stale loose ones — a premise that holds only when git state
// crosses BACK. For an UNTRUSTED run nothing under `.git` can arrive, so
// clearing would delete the host's refs with nothing to replace them: the
// export of the REAL ExportWorkspace path must leave them alone. The
// trusted control still clears (that is the gc-shadow fix's own contract).
func TestUntrustedExportClearsNoHostLooseRefs(t *testing.T) {
	kubectlStdinShim(t) // exec replies an empty archive: nothing lands
	host := t.TempDir()
	loose := filepath.Join(host, ".git", "refs", "heads", "main")
	if err := os.MkdirAll(filepath.Dir(loose), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(loose, []byte("host-loose-ref\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	d := &Driver{namespace: "ns", kubectl: "kubectl", logger: iterlog.Nop()}
	run := &Run{driver: d, podName: "p", namespace: "ns",
		prepared: &Prepared{workspace: "/workspace"},
		info:     sandbox.RunInfo{WorkspacePath: host, UntrustedRun: true}}
	if err := run.ExportWorkspace(context.Background()); err != nil {
		t.Fatalf("ExportWorkspace: %v", err)
	}
	if got, err := os.ReadFile(loose); err != nil || string(got) != "host-loose-ref\n" {
		t.Fatalf("the host's loose ref was cleared for a run whose pod carries no git state to replace it: %q (%v)", got, err)
	}

	// Trusted control: the same export clears the loose ref — the gc-shadow
	// fix's contract, proving the skip is the untrusted gate and not a lost
	// phase.
	ctlHost := t.TempDir()
	ctlLoose := filepath.Join(ctlHost, ".git", "refs", "heads", "main")
	if err := os.MkdirAll(filepath.Dir(ctlLoose), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ctlLoose, []byte("host-loose-ref\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctl := &Run{driver: d, podName: "p", namespace: "ns",
		prepared: &Prepared{workspace: "/workspace"},
		info:     sandbox.RunInfo{WorkspacePath: ctlHost}}
	if err := ctl.ExportWorkspace(context.Background()); err != nil {
		t.Fatalf("trusted control ExportWorkspace: %v", err)
	}
	if _, err := os.Stat(ctlLoose); !os.IsNotExist(err) {
		t.Fatalf("trusted control: the loose ref survived (%v) — the clearing phase was lost, not gated", err)
	}
}
