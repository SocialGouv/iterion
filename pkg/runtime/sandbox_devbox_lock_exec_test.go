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
// prologue for real — `sh -c`, a fake `devbox` on PATH, `TMPDIR` pointed at a
// directory the test owns — through every state the lock can be in around
// the install: rewritten metadata (restored, the field first, last or only
// key of its entry), re-locked beyond metadata (kept), removed (restored),
// replaced by a directory (left alone, said), created where git would show
// it (removed), created where the repository ignores it, where no repository
// reads the worktree or where git has no work tree to answer for (kept),
// untouched (left alone), and the ones earlier cuts got wrong: the
// pre-install copy FAILS, which must leave the tracked lock in place
// (whatever devbox wrote), never remove it as "created"; and a comparison
// tool the image lacks — each of `tr`, `sed`, `cmp` in turn — which must
// leave the lock as found and say so, never read two empty outputs as
// "equal" and put the pin back over a genuine re-lock.
func TestDevboxInstallSnippet_RepoLockBranchesExecuteUnderSh(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh on PATH")
	}
	realCp, err := exec.LookPath("cp")
	if err != nil {
		t.Skip("no cp on PATH")
	}
	const pinned = "{\"lockfile_version\":\"1\",\"packages\":{\"go@1.26\":{\"plugin_version\":\"0.0.4\"}}}\n"
	const drifted = "{\"lockfile_version\":\"1\",\"packages\":{\"go@1.26\":{\"plugin_version\":\"0.0.5\"}}}\n"
	const relocked = "{\"lockfile_version\":\"1\",\"packages\":{\"go@1.26\":{\"plugin_version\":\"0.0.4\"},\"jq@1.8\":{\"plugin_version\":\"0.0.4\"}}}\n"
	// A lock without plugin metadata, and the same lock once devbox added
	// its `plugin_version` as the entry's FIRST or LAST key: metadata only,
	// the resolved store path is untouched.
	const unlocked = "{\"lockfile_version\":\"1\",\"packages\":{\"go@1.26\":{\"resolved\":\"github:NixOS/nixpkgs/abc#go\"}}}\n"
	const addedFirst = "{\"lockfile_version\":\"1\",\"packages\":{\"go@1.26\":{\"plugin_version\":\"0.0.5\",\"resolved\":\"github:NixOS/nixpkgs/abc#go\"}}}\n"
	const addedLast = "{\"lockfile_version\":\"1\",\"packages\":{\"go@1.26\":{\"resolved\":\"github:NixOS/nixpkgs/abc#go\",\"plugin_version\":\"0.0.5\"}}}\n"
	// The shape devbox actually writes: indented, one field per line.
	const prettyPinned = "{\n  \"lockfile_version\": \"1\",\n  \"packages\": {\n    \"go@1.26\": {\n      \"plugin_version\": \"0.0.4\",\n      \"resolved\": \"github:NixOS/nixpkgs/abc#go\"\n    }\n  }\n}\n"
	const prettyDrifted = "{\n  \"lockfile_version\": \"1\",\n  \"packages\": {\n    \"go@1.26\": {\n      \"plugin_version\": \"0.0.5\",\n      \"resolved\": \"github:NixOS/nixpkgs/abc#go\"\n    }\n  }\n}\n"
	const prettyRelocked = "{\n  \"lockfile_version\": \"1\",\n  \"packages\": {\n    \"go@1.26\": {\n      \"plugin_version\": \"0.0.5\",\n      \"resolved\": \"github:NixOS/nixpkgs/def#go\"\n    }\n  }\n}\n"
	// devbox rewrites the whole file, trailing newline included.
	write := func(content string) string { return "printf '%s\\n' '" + strings.TrimSpace(content) + "' > \"$LOCK\"" }

	cases := []struct {
		name       string
		lockBefore string // "" = no lock in the repo
		devbox     string // what the fake `devbox install` does to the lock
		gitInit    bool
		ignore     string // .gitignore content when gitInit
		cpFails    bool   // the copy aside refuses
		bare       bool   // the project dir is a bare repository: git answers, but has no work tree
		failTool   string // a comparison tool ("tr", "sed", "cmp") the image lacks: writes nothing, exits non-zero
		wantDir    bool   // the lock is a directory after the run, and nothing was written into it
		wantLock   string // "" = must be absent
		wantStderr string
	}{
		{"rewritten metadata is restored", pinned, write(drifted), false, "", false, false, "", false, pinned, "devbox rewrote"},
		{"re-locked beyond metadata is kept", pinned, write(relocked), false, "", false, false, "", false, relocked, "beyond plugin metadata"},
		{"pretty-printed metadata drift is restored", prettyPinned, write(prettyDrifted), false, "", false, false, "", false, prettyPinned, "devbox rewrote"},
		{"pretty-printed re-resolution is kept", prettyPinned, write(prettyRelocked), false, "", false, false, "", false, prettyRelocked, "beyond plugin metadata"},
		{"plugin metadata added as the entry's first key is restored", unlocked, write(addedFirst), false, "", false, false, "", false, unlocked, "devbox rewrote"},
		{"plugin metadata added as the entry's last key is restored", unlocked, write(addedLast), false, "", false, false, "", false, unlocked, "devbox rewrote"},
		{"removed is restored", pinned, "rm -f \"$LOCK\"", false, "", false, false, "", false, pinned, "devbox removed"},
		{"replaced by a directory is left alone and said", pinned, "rm -f \"$LOCK\"; mkdir \"$LOCK\"", false, "", false, false, "", true, "", "could not compare"},
		{"created where git would show it is removed", "", write(drifted), true, "", false, false, "", false, "", "removed so the worktree stays clean"},
		{"created where the repository ignores it is kept", "", write(drifted), true, devboxLockName, false, false, "", false, drifted, "kept"},
		{"created outside any repository is kept", "", write(drifted), false, "", false, false, "", false, drifted, "kept"},
		{"created inside a bare repository is kept", "", write(drifted), false, "", false, true, "", false, drifted, "kept"},
		{"untouched is left alone", pinned, "true", false, "", false, false, "", false, pinned, ""},
		{"a failed snapshot never removes the tracked lock", pinned, write(drifted), false, "", true, false, "", false, drifted, "could not copy"},
		{"a blank-stripping tool that fails keeps the re-lock and says so", pinned, write(relocked), false, "", false, false, "tr", false, relocked, "could not compare"},
		{"a strip tool that fails keeps the re-lock and says so", pinned, write(relocked), false, "", false, false, "sed", false, relocked, "could not compare"},
		{"a compare tool that fails keeps the lock and says so", pinned, write(drifted), false, "", false, false, "cmp", false, drifted, "could not compare"},
		{"an untouched lock with a broken compare tool is left alone and said", pinned, "true", false, "", false, false, "cmp", false, pinned, "could not compare"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := t.TempDir()
			if tc.gitInit {
				initGitRepo(t, repo, tc.ignore)
			}
			if tc.bare {
				initBareGitRepo(t, repo)
			}
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
				// A cp that refuses the copy aside and forwards everything else.
				fakeCp := "#!/bin/sh\ncase \"$2\" in */iterion-devbox-lock-pre-*) exit 1;; esac\nexec " + realCp + " \"$@\"\n"
				if err := os.WriteFile(filepath.Join(bin, "cp"), []byte(fakeCp), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			if tc.failTool != "" {
				// A tool the image lacks, or one that dies mid-write: nothing
				// written, a non-zero exit — the same as `command not found`
				// to the prologue's exit-status reads.
				fakeTool := "#!/bin/sh\necho '" + tc.failTool + ": simulated failure' >&2\nexit 2\n"
				if err := os.WriteFile(filepath.Join(bin, tc.failTool), []byte(fakeTool), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			// The prologue's scratch lives under $TMPDIR: a directory this test
			// owns, so the leftovers check below reads its own files only.
			tmp := t.TempDir()
			snippet := devboxInstallSnippet([]devboxProject{{label: "repo", dir: repo}})
			cmd := exec.Command(sh, "-c", snippet)
			cmd.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"), "TMPDIR="+tmp)
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			if err := cmd.Run(); err != nil {
				t.Fatalf("snippet exited non-zero: %v\n%s", err, stderr.String())
			}
			got, err := os.ReadFile(lock)
			switch {
			case tc.wantDir:
				st, serr := os.Stat(lock)
				if serr != nil || !st.IsDir() {
					t.Fatalf("lock after the run: %v (err %v), want the directory devbox left in its place", st, serr)
				}
				if entries, _ := os.ReadDir(lock); len(entries) != 0 {
					t.Fatalf("the prologue wrote into the directory that replaced the lock: %v", entries)
				}
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
			if (tc.failTool != "" || tc.wantDir) && strings.Contains(stderr.String(), "restored") {
				t.Fatalf("stderr claims a restore it could not have decided: %q", stderr.String())
			}
			leftovers, _ := filepath.Glob(filepath.Join(tmp, "iterion-devbox-lock-pre-*"))
			if len(leftovers) != 0 {
				t.Fatalf("scratch files left under $TMPDIR: %v", leftovers)
			}
		})
	}
}

// initBareGitRepo makes dir a bare repository: `git rev-parse` answers there
// (exit 0, "false"), but there is no work tree for `check-ignore` to answer
// for — the exit git cannot answer with.
func initBareGitRepo(t *testing.T, dir string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git on PATH")
	}
	out, err := exec.Command("git", "-C", dir, "init", "-q", "--bare").CombinedOutput()
	if err != nil {
		t.Fatalf("git init --bare: %v\n%s", err, out)
	}
}
