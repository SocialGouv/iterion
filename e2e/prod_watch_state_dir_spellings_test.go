package e2e

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/gittest"
)

// TestProdWatch_AStateDirSpelledOddStillCommits: with state_commit=true, a
// state dir spelled `./x`, holding a non-ASCII byte, or given absolute never
// committed (#2838): the preflight compared git's quoted, repo-normalized
// paths with the dir as typed, so every tick died on "refusing to commit
// path(s) outside the state dir" and the state never reached git. The
// preflight now compares in git's own frame — relpath against the toplevel,
// NUL-separated unquoted `git diff -z` — and each spelling commits and
// pushes to a local bare remote.
func TestProdWatch_AStateDirSpelledOddStillCommits(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	for name, sdir := range map[string]string{
		"dot_slash":     "./.prod-watch",
		"non_ascii":     "données",
		"quoted_char":   `x"y`,
		"backslash":     `a\b`,
		"absolute_path": "", // filled per run: the workspace's own .prod-watch, absolute
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			wf := compileFixture(t, "prod-watch/main.bot")
			h := newPWHarness(t)
			git := func(args ...string) string { return gittest.Run(t, h.ws, args...) }
			bare := filepath.Join(t.TempDir(), "remote.git")
			gittest.Run(t, filepath.Dir(bare), "init", "--bare", "-q", bare)
			git("init", "-q", "-b", "main")
			_ = os.WriteFile(filepath.Join(h.ws, "README.md"), []byte("ops\n"), 0o644)
			git("add", "README.md")
			git("commit", "-q", "-m", "init")
			git("remote", "add", "origin", bare)
			git("push", "-q", "-u", "origin", "main")
			pushed := strings.TrimSpace(strings.SplitN(git("ls-remote", "origin", "refs/heads/main"), "\t", 2)[0])

			// The spelled state dir, its files already on disk (as a tick
			// would have staged them) — `.prod-watch` is also the dir behind
			// the `./` and the absolute spellings.
			if sdir == "" {
				sdir = filepath.Join(h.ws, ".prod-watch")
			}
			sd := filepath.Join(h.ws, sdir)
			if err := os.MkdirAll(sd, 0o755); err != nil {
				t.Fatal(err)
			}
			_ = os.WriteFile(filepath.Join(sd, "state.json"), []byte(`{"version":1,"generation":1,"cursors":{"loki":{}},"incidents":{},"health":{}}`), 0o644)
			_ = os.WriteFile(filepath.Join(sd, "alertlog.jsonl"), []byte(""), 0o644)
			_ = os.WriteFile(filepath.Join(sd, "ticks.jsonl"), []byte(""), 0o644)
			st := filepath.Join(h.scratch, "state_next.json")
			_ = os.WriteFile(st, []byte(`{"version":1,"generation":2,"cursors":{"loki":{}},"incidents":{},"health":{}}`), 0o644)
			al := filepath.Join(h.scratch, "alertlog_delta.jsonl")
			_ = os.WriteFile(al, []byte(""), 0o644)
			tk := filepath.Join(h.scratch, "tick.json")
			_ = os.WriteFile(tk, []byte(`{"at":"x","generation":2}`), 0o644)
			out, stderr, err := runPyEnv(t, h.ws, pwSub(t, pwTool(t, wf, "commit_state").Script, map[string]any{
				"state_next_file": st, "alertlog_file": al, "tick_file": tk, "session_next_file": "", "generation": 1,
				"state_commit": true, "workspace": h.ws, "state_dir": sdir, "ledger": pwLedgerOff()}, nil, nil), gittest.Env())
			if err != nil || out["committed"] != true {
				t.Fatalf("the state dir %s must commit and push: %v %s %v", sdir, out, stderr, err)
			}
			nowPushed := strings.TrimSpace(strings.SplitN(git("ls-remote", "origin", "refs/heads/main"), "\t", 2)[0])
			if nowPushed == pushed || nowPushed == "" {
				t.Fatalf("the state commit of %s never reached the bare remote: %s", sdir, nowPushed)
			}
			// The commit's TREE carries the files at git's own spelling of
			// the dir — normalized in the repo frame, never the dir as
			// typed, and never a quoted/escaped name. `ls-tree -z` prints
			// the raw names: the tree is the effect, not `show`'s display.
			inRepo := sdir
			if filepath.IsAbs(inRepo) {
				if rel, err := filepath.Rel(h.ws, inRepo); err == nil {
					inRepo = rel
				}
			}
			inRepo = strings.TrimPrefix(filepath.ToSlash(inRepo), "./")
			tree := git("ls-tree", "-r", "--name-only", "-z", "HEAD")
			found := false
			for _, e := range strings.Split(tree, "\x00") {
				// `-z` prints raw names: a C-quoted one arrives wrapped in a
				// quote pair ("donn\303\251es/…") — and a dir genuinely named
				// x"y keeps its own quote, INSIDE the raw name, never at both
				// ends.
				if strings.HasPrefix(e, `"`) && strings.HasSuffix(e, `"`) {
					t.Fatalf("the commit carries a quoted/escaped path (git output leaked into the tree): %q", e)
				}
				if e == inRepo+"/state.json" || e == inRepo+"/ticks.jsonl" {
					found = true
				}
			}
			if !found {
				t.Fatalf("the commit of %s lacks the state files at %s/{state,ticks}.json in its tree", sdir, inRepo)
			}
			// A later tick must not refuse at the preflight either: the state
			// dir is judged by the same frame the index is written in.
			_ = os.WriteFile(st, []byte(`{"version":1,"generation":3,"cursors":{"loki":{}},"incidents":{},"health":{}}`), 0o644)
			out, stderr, err = runPyEnv(t, h.ws, pwSub(t, pwTool(t, wf, "commit_state").Script, map[string]any{
				"state_next_file": st, "alertlog_file": al, "tick_file": tk, "session_next_file": "", "generation": 2,
				"state_commit": true, "workspace": h.ws, "state_dir": sdir, "ledger": pwLedgerOff()}, nil, nil), gittest.Env())
			if err != nil || out["committed"] != true {
				t.Fatalf("the second tick of the state dir %s must commit, not refuse at the preflight: %v %s %v", sdir, out, stderr, err)
			}
		})
	}
}

// TestProdWatch_ATrackedGoneRotationLeavesTheIndexEvenQuoted: the
// tracked-but-gone rotation re-stager — a rotation pruned while
// state_commit was false stays in the index, and the next committing tick
// hands its deletion to git — reads the index with `git ls-files -z`
// (#2838's third reader of git's output). Under a dir git C-quotes
// (données), the quoted form's basename ends in `"` and never matches the
// rotation names: only the unquoted, NUL-separated read stages the
// deletion. The witness holds the reader, not the claim.
func TestProdWatch_ATrackedGoneRotationLeavesTheIndexEvenQuoted(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	git := func(args ...string) string { return gittest.Run(t, h.ws, args...) }
	bare := filepath.Join(t.TempDir(), "remote.git")
	gittest.Run(t, filepath.Dir(bare), "init", "--bare", "-q", bare)
	git("init", "-q", "-b", "main")
	_ = os.WriteFile(filepath.Join(h.ws, "README.md"), []byte("ops\n"), 0o644)
	git("add", "README.md")
	git("commit", "-q", "-m", "init")
	git("remote", "add", "origin", bare)
	git("push", "-q", "-u", "origin", "main")
	pushed := strings.TrimSpace(strings.SplitN(git("ls-remote", "origin", "refs/heads/main"), "\t", 2)[0])

	// The rotation the state repo still tracks, gone from the tree: it was
	// committed, then pruned off the disk during a state_commit=false era.
	sdir := "données"
	sd := filepath.Join(h.ws, sdir)
	if err := os.MkdirAll(sd, 0o755); err != nil {
		t.Fatal(err)
	}
	rot := filepath.Join(sd, "ticks-2.jsonl")
	_ = os.WriteFile(rot, []byte("old ticks\n"), 0o644)
	git("add", sdir+"/ticks-2.jsonl")
	git("commit", "-q", "-m", "rotation")
	if err := os.Remove(rot); err != nil {
		t.Fatal(err)
	}
	if _, err := gittest.Try(h.ws, "ls-files", "--error-unmatch", sdir+"/ticks-2.jsonl"); err != nil {
		t.Fatalf("the fixture must seed a TRACKED rotation: %v", err)
	}

	st := filepath.Join(h.scratch, "state_next.json")
	_ = os.WriteFile(st, []byte(`{"version":1,"generation":2,"cursors":{"loki":{}},"incidents":{},"health":{}}`), 0o644)
	al := filepath.Join(h.scratch, "alertlog_delta.jsonl")
	_ = os.WriteFile(al, []byte(""), 0o644)
	tk := filepath.Join(h.scratch, "tick.json")
	_ = os.WriteFile(tk, []byte(`{"at":"x","generation":2}`), 0o644)
	out, stderr, err := runPyEnv(t, h.ws, pwSub(t, pwTool(t, wf, "commit_state").Script, map[string]any{
		"state_next_file": st, "alertlog_file": al, "tick_file": tk, "session_next_file": "", "generation": 1,
		"state_commit": true, "workspace": h.ws, "state_dir": sdir, "ledger": pwLedgerOff()}, nil, nil), gittest.Env())
	if err != nil || out["committed"] != true {
		t.Fatalf("the tick must commit and push: %v %s %v", out, stderr, err)
	}
	nowPushed := strings.TrimSpace(strings.SplitN(git("ls-remote", "origin", "refs/heads/main"), "\t", 2)[0])
	if nowPushed == pushed || nowPushed == "" {
		t.Fatalf("the state commit never reached the bare remote: %s", nowPushed)
	}
	// The deletion is STAGED — out of the index (git reads it, the disk
	// never had it back) and out of the commit's tree.
	if _, err := gittest.Try(h.ws, "ls-files", "--error-unmatch", sdir+"/ticks-2.jsonl"); err == nil {
		t.Fatalf("the gone rotation is still tracked: the index reader never staged its deletion (quoted read?)")
	}
	tree := git("ls-tree", "-r", "--name-only", "HEAD", sdir)
	if strings.Contains(tree, "ticks-2.jsonl") {
		t.Fatalf("the commit still carries the gone rotation: %s", tree)
	}
	// The tick's own duty stands: the state files reached the same commit.
	if !strings.Contains(tree, "state.json") || !strings.Contains(tree, "ticks.jsonl") {
		t.Fatalf("the commit lacks the state files: %s", tree)
	}
}
