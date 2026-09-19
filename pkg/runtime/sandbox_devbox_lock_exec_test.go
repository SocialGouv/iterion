package runtime

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/gittest"
)

// TestDevboxInstallSnippet_RepoLockBranchesExecuteUnderSh runs the generated
// prologue for real — `sh -c`, a fake `devbox` on PATH, `TMPDIR` pointed at a
// directory the test owns — through every state the lock can be in around
// the install: rewritten metadata (restored, the field first, last or only
// key of its entry), re-locked beyond metadata (kept), removed (restored),
// replaced by a directory (left alone, said), created where git would show
// it (removed), created where the repository ignores it, where no repository
// reads the worktree or where git has no work tree to answer for (kept),
// left empty or unterminated (restored, said unparseable — what still ends
// in a brace, a truncation stopping on an inner brace, the probe cannot
// see: kept as a re-lock, said), untouched (left alone, an unparseable pin
// included), and the ones earlier cuts got wrong: the
// pre-install copy FAILS, which must leave the tracked lock in place
// (whatever devbox wrote), never remove it as "created"; a comparison tool
// the image lacks — each of `tr`, `sed`, `cmp` in turn — which must leave
// the lock as found and say so, never read two empty outputs as "equal" and
// put the pin back over a genuine re-lock; and a restore or removal the
// filesystem refuses, which must be said as such, never announced as done.
func TestDevboxInstallSnippet_RepoLockBranchesExecuteUnderSh(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh on PATH")
	}
	realCp, err := exec.LookPath("cp")
	if err != nil {
		t.Skip("no cp on PATH")
	}
	realRm, err := exec.LookPath("rm")
	if err != nil {
		t.Skip("no rm on PATH")
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
	// A write cut short: no closing brace once blanks are removed — and the
	// one truncation the prologue's shape probe cannot see, ending exactly
	// on an inner brace (read as a re-lock, kept, said; the host parses and
	// restores).
	const truncated = "{\"lockfile_version\":\"1\",\"packages\":{\"go@1.26\":{\"plugin_vers\n"
	const truncatedOnBrace = "{\"lockfile_version\":\"1\",\"packages\":{\"go@1.26\":{}\n"
	// The realistic cut of a multi-package lock: an entry closed, the next
	// one open — a brace inside, none at the end.
	const truncatedAfterEntry = "{\"lockfile_version\":\"1\",\"packages\":{\"go@1.26\":{\"plugin_version\":\"0.0.4\"},\"jq@1.8\":{\"plugin_vers\n"
	// The shape devbox actually writes: indented, one field per line.
	const prettyPinned = "{\n  \"lockfile_version\": \"1\",\n  \"packages\": {\n    \"go@1.26\": {\n      \"plugin_version\": \"0.0.4\",\n      \"resolved\": \"github:NixOS/nixpkgs/abc#go\"\n    }\n  }\n}\n"
	const prettyDrifted = "{\n  \"lockfile_version\": \"1\",\n  \"packages\": {\n    \"go@1.26\": {\n      \"plugin_version\": \"0.0.5\",\n      \"resolved\": \"github:NixOS/nixpkgs/abc#go\"\n    }\n  }\n}\n"
	const prettyRelocked = "{\n  \"lockfile_version\": \"1\",\n  \"packages\": {\n    \"go@1.26\": {\n      \"plugin_version\": \"0.0.5\",\n      \"resolved\": \"github:NixOS/nixpkgs/def#go\"\n    }\n  }\n}\n"
	// devbox rewrites the whole file, trailing newline included.
	write := func(content string) string { return "printf '%s\\n' '" + strings.TrimSpace(content) + "' > \"$LOCK\"" }

	type tcase struct {
		name           string
		lockBefore     string // "" = no lock in the repo
		devbox         string // what the fake `devbox install` does to the lock
		gitInit        bool
		ignore         string // .gitignore content when gitInit
		cpFails        bool   // the copy aside refuses
		restoreCpFails bool   // the copy BACK into the worktree refuses (full layer, read-only mount)
		rmFails        bool   // the removal of a created lock refuses
		bare           bool   // the project dir is a bare repository: git answers, but has no work tree
		failTool       string // a comparison tool ("tr", "sed", "cmp") the image lacks: writes nothing, exits non-zero
		wantDir        bool   // the lock is a directory after the run, and nothing was written into it
		wantLock       string // "" = must be absent
		wantStderr     string
		wantStderrToo  string // a second phrase the notice must carry — the one that tells this site's notice from its twin's
	}
	cases := []tcase{
		{name: "rewritten metadata is restored", lockBefore: pinned, devbox: write(drifted), wantLock: pinned, wantStderr: "devbox rewrote"},
		{name: "re-locked beyond metadata is kept", lockBefore: pinned, devbox: write(relocked), wantLock: relocked, wantStderr: "beyond plugin metadata"},
		{name: "pretty-printed metadata drift is restored", lockBefore: prettyPinned, devbox: write(prettyDrifted), wantLock: prettyPinned, wantStderr: "devbox rewrote"},
		{name: "pretty-printed re-resolution is kept", lockBefore: prettyPinned, devbox: write(prettyRelocked), wantLock: prettyRelocked, wantStderr: "beyond plugin metadata"},
		{name: "plugin metadata added as the entry's first key is restored", lockBefore: unlocked, devbox: write(addedFirst), wantLock: unlocked, wantStderr: "devbox rewrote"},
		{name: "plugin metadata added as the entry's last key is restored", lockBefore: unlocked, devbox: write(addedLast), wantLock: unlocked, wantStderr: "devbox rewrote"},
		{name: "removed is restored", lockBefore: pinned, devbox: "rm -f \"$LOCK\"", wantLock: pinned, wantStderr: "devbox removed"},
		{name: "left unterminated is restored and said unparseable", lockBefore: pinned, devbox: write(truncated), wantLock: pinned, wantStderr: "unparseable"},
		{name: "left empty is restored and said unparseable", lockBefore: pinned, devbox: ": > \"$LOCK\"", wantLock: pinned, wantStderr: "unparseable"},
		{name: "left unterminated after a closed entry is restored and said unparseable", lockBefore: pinned, devbox: write(truncatedAfterEntry), wantLock: pinned, wantStderr: "unparseable"},
		{name: "left unterminated on an inner brace reads as a re-lock and is kept", lockBefore: pinned, devbox: write(truncatedOnBrace), wantLock: truncatedOnBrace, wantStderr: "beyond plugin metadata"},
		{name: "an untouched unparseable pin is left alone", lockBefore: truncated, devbox: "true", wantLock: truncated},
		{name: "created and left unterminated is removed even where the repository ignores it", devbox: write(truncated), gitInit: true, ignore: devboxLockName, wantStderr: "unparseable"},
		{name: "created and left unterminated with a broken tr is kept, not removed", devbox: write(truncated), gitInit: true, ignore: devboxLockName, failTool: "tr", wantLock: truncated, wantStderr: "kept"},
		{name: "replaced by a directory is left alone and said", lockBefore: pinned, devbox: "rm -f \"$LOCK\"; mkdir \"$LOCK\"", wantDir: true, wantStderr: "could not compare"},
		{name: "created where git would show it is removed", devbox: write(drifted), gitInit: true, wantStderr: "removed so the worktree stays clean"},
		{name: "created where the repository ignores it is kept", devbox: write(drifted), gitInit: true, ignore: devboxLockName, wantLock: drifted, wantStderr: "kept"},
		{name: "created outside any repository is kept", devbox: write(drifted), wantLock: drifted, wantStderr: "kept"},
		{name: "created inside a bare repository is kept", devbox: write(drifted), bare: true, wantLock: drifted, wantStderr: "kept"},
		{name: "untouched is left alone", lockBefore: pinned, devbox: "true", wantLock: pinned},
		{name: "a failed snapshot never removes the tracked lock", lockBefore: pinned, devbox: write(drifted), cpFails: true, wantLock: drifted, wantStderr: "could not copy"},
		{name: "a blank-stripping tool that fails keeps the re-lock and says so", lockBefore: pinned, devbox: write(relocked), failTool: "tr", wantLock: relocked, wantStderr: "could not compare"},
		{name: "a strip tool that fails keeps the re-lock and says so", lockBefore: pinned, devbox: write(relocked), failTool: "sed", wantLock: relocked, wantStderr: "could not compare"},
		{name: "a compare tool that fails keeps the lock and says so", lockBefore: pinned, devbox: write(drifted), failTool: "cmp", wantLock: drifted, wantStderr: "could not compare"},
		{name: "an untouched lock with a broken compare tool is left alone and said", lockBefore: pinned, devbox: "true", failTool: "cmp", wantLock: pinned, wantStderr: "could not compare"},
		{name: "a restore the filesystem refuses is said, not announced", lockBefore: pinned, devbox: write(drifted), restoreCpFails: true, wantLock: drifted, wantStderr: "could not restore"},
		{name: "a restore of a removed lock the filesystem refuses is said", lockBefore: pinned, devbox: "rm -f \"$LOCK\"", restoreCpFails: true, wantStderr: "could not restore", wantStderrToo: "after devbox removed it"},
		{name: "a removal the filesystem refuses is said, not announced", devbox: write(drifted), gitInit: true, rmFails: true, wantLock: drifted, wantStderr: "could not remove"},
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
			if tc.restoreCpFails {
				// A cp that refuses to write the lock back into the worktree
				// (a full writable layer, a read-only mount) and forwards
				// everything else — the copy aside included.
				fakeCp := "#!/bin/sh\ncase \"$2\" in */" + devboxLockName + ") exit 1;; esac\nexec " + realCp + " \"$@\"\n"
				if err := os.WriteFile(filepath.Join(bin, "cp"), []byte(fakeCp), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			if tc.rmFails {
				// An rm that refuses the lock and forwards everything else —
				// the scratch cleanup included.
				fakeRm := "#!/bin/sh\ncase \"$*\" in *" + devboxLockName + "*) exit 1;; esac\nexec " + realRm + " \"$@\"\n"
				if err := os.WriteFile(filepath.Join(bin, "rm"), []byte(fakeRm), 0o755); err != nil {
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
				t.Fatalf("lock present after the run (%q, err=%v), want it absent", got, err)
			case tc.wantLock != "" && err != nil:
				t.Fatalf("lock missing after the run: %v — the tracked file was removed", err)
			case tc.wantLock != "" && strings.TrimSpace(string(got)) != strings.TrimSpace(tc.wantLock):
				t.Fatalf("lock after the run = %q, want %q", got, tc.wantLock)
			}
			out := stderr.String()
			if tc.wantStderr == "" && out != "" {
				t.Fatalf("stderr = %q, want silence for an untouched lock", out)
			}
			if tc.wantStderr != "" && !strings.Contains(out, tc.wantStderr) {
				t.Fatalf("stderr = %q, want it to say %q", out, tc.wantStderr)
			}
			if tc.wantStderrToo != "" && !strings.Contains(out, tc.wantStderrToo) {
				t.Fatalf("stderr = %q, want it to say %q as well", out, tc.wantStderrToo)
			}
			if strings.Contains(out, "devbox created") && tc.lockBefore != "" {
				t.Fatalf("stderr claims devbox created a lock the repository tracked: %q", out)
			}
			if (tc.failTool != "" || tc.wantDir || tc.restoreCpFails) && strings.Contains(out, "restored") {
				t.Fatalf("stderr claims a restore that did not happen: %q", out)
			}
			if tc.rmFails && strings.Contains(out, "removed so the worktree stays clean") {
				t.Fatalf("stderr claims a removal that did not happen: %q", out)
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
// for — the exit git cannot answer with. Through gittest, like every git
// command a test runs, so auto-maintenance is refused.
func initBareGitRepo(t *testing.T, dir string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git on PATH")
	}
	gittest.Run(t, dir, "init", "-q", "--bare")
}
