package runtime

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestDevboxInstallSnippet_RepoLockBranchesExecuteUnderSh runs the generated
// prologue for real — `sh -c`, a fake `devbox` on PATH — through the four
// states the lock can be in around the install: rewritten (restored),
// created (removed), untouched (left alone), and the one the first cut got
// wrong: the pre-install copy to /tmp FAILS, which must leave the tracked
// lock in place (whatever devbox wrote), never remove it as "created".
func TestDevboxInstallSnippet_RepoLockBranchesExecuteUnderSh(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh on PATH")
	}
	realCp, err := exec.LookPath("cp")
	if err != nil {
		t.Skip("no cp on PATH")
	}
	const pinned = "{\"lockfile_version\":\"1\",\"plugin_version\":\"0.0.4\"}\n"
	const drifted = "{\"lockfile_version\":\"1\",\"plugin_version\":\"0.0.5\"}\n"

	cases := []struct {
		name       string
		lockBefore string // "" = no lock in the repo
		devbox     string // what the fake `devbox install` does to the lock
		cpFails    bool   // the copy to /tmp refuses
		wantLock   string // "" = must be absent
		wantStderr string
	}{
		{"rewritten is restored", pinned, "printf '%s' '" + strings.TrimSpace(drifted) + "' > \"$LOCK\"", false, pinned, "devbox rewrote"},
		{"created is removed", "", "printf '%s' '" + strings.TrimSpace(drifted) + "' > \"$LOCK\"", false, "", "devbox created"},
		{"untouched is left alone", pinned, "true", false, pinned, ""},
		{"a failed snapshot never removes the tracked lock", pinned, "printf '%s' '" + strings.TrimSpace(drifted) + "' > \"$LOCK\"", true, strings.TrimSpace(drifted), "could not copy"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := t.TempDir()
			lock := filepath.Join(repo, devboxLockName)
			if tc.lockBefore != "" {
				if err := os.WriteFile(lock, []byte(tc.lockBefore), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			bin := t.TempDir()
			// The fake devbox acts on the lock of the project it is told to install.
			fakeDevbox := "#!/bin/sh\nLOCK=\"$3/" + devboxLockName + "\"\n" + tc.devbox + "\nexit 0\n"
			if err := os.WriteFile(filepath.Join(bin, "devbox"), []byte(fakeDevbox), 0o755); err != nil {
				t.Fatal(err)
			}
			if tc.cpFails {
				// A cp that refuses the pre-install copy and forwards everything else.
				fakeCp := "#!/bin/sh\ncase \"$2\" in /tmp/iterion-devbox-lock-pre-*) exit 1;; esac\nexec " + realCp + " \"$@\"\n"
				if err := os.WriteFile(filepath.Join(bin, "cp"), []byte(fakeCp), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			snippet := devboxInstallSnippet([]devboxProject{{label: "repo", dir: repo}})
			cmd := exec.Command(sh, "-c", snippet)
			cmd.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			if err := cmd.Run(); err != nil {
				t.Fatalf("snippet exited non-zero: %v\n%s", err, stderr.String())
			}
			got, err := os.ReadFile(lock)
			switch {
			case tc.wantLock == "" && !os.IsNotExist(err):
				t.Fatalf("lock present after the run (%q, err=%v), want it removed", got, err)
			case tc.wantLock != "" && err != nil:
				t.Fatalf("lock missing after the run: %v — the tracked file was removed", err)
			case tc.wantLock != "" && strings.TrimSpace(string(got)) != strings.TrimSpace(tc.wantLock):
				t.Fatalf("lock after the run = %q, want %q", got, tc.wantLock)
			}
			if tc.wantStderr == "" && stderr.Len() != 0 {
				t.Fatalf("stderr = %q, want silence for an untouched lock", stderr.String())
			}
			if tc.wantStderr != "" && !strings.Contains(stderr.String(), tc.wantStderr) {
				t.Fatalf("stderr = %q, want it to say %q", stderr.String(), tc.wantStderr)
			}
			if strings.Contains(stderr.String(), "devbox created") && tc.lockBefore != "" {
				t.Fatalf("stderr claims devbox created a lock the repository tracked: %q", stderr.String())
			}
		})
	}
}
