package bots

import (
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/internal/gittest"
)

// TestReviewPRResolvesTheBaseItActuallyReviews drives the real `diff_precheck`
// command — extracted from the .bot, never retyped — over a workspace shaped
// like the one that produced a wrong review in production.
//
// The scope of a review is `git diff <merge-base(base_ref, HEAD)>` taken in the
// workspace. A workspace reused across runs carries whatever `base_ref` meant
// when it was made, and a merge-base against a stale base lands BELOW the
// branch point: every file merged into the base since falls into the diff, and
// the review reports on code the branch never touched as if it had. Measured
// 2026-09-15 on PR #1224 — a review scoped itself onto five files of a pull
// request merged three days earlier and emitted a blocking finding with no line
// in the real diff to anchor to.
//
// The bug class is invisible without this test: a wrong base produces a
// plausible review of the wrong files, not a failure.
func TestReviewPRResolvesTheBaseItActuallyReviews(t *testing.T) {
	for _, bin := range []string{"python3", "git"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not on PATH", bin)
		}
	}

	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Skipf("git not on PATH: %v", err)
	}

	root := t.TempDir()
	upstream, ws := root+"/upstream", root+"/ws"

	up := func(args ...string) string { t.Helper(); return gittest.Run(t, upstream, args...) }
	inWS := func(args ...string) string { t.Helper(); return gittest.Run(t, ws, args...) }
	write := func(dir, name, body string) {
		t.Helper()
		if err := os.WriteFile(dir+"/"+name, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	if err := os.MkdirAll(upstream, 0o755); err != nil {
		t.Fatal(err)
	}
	up("init", "--quiet", "-b", "main")
	write(upstream, "old.txt", "a\n")
	up("add", "-A")
	up("commit", "-m", "A")

	// The workspace is cloned HERE: its local `main` is A, and nothing in the
	// run ever moves it again.
	gittest.Run(t, root, "clone", "--quiet", upstream, ws)

	// `main` advances upstream — this is the pull request that merged while the
	// workspace sat in the pool.
	write(upstream, "merged_since.txt", "b\n")
	up("add", "-A")
	up("commit", "-m", "B: merged after the workspace was cloned")

	// The branch under review is created from the NEW main and adds one file.
	up("checkout", "--quiet", "-b", "feat")
	write(upstream, "mine.txt", "c\n")
	up("add", "-A")
	up("commit", "-m", "the only change this branch introduces")

	inWS("fetch", "--quiet", "origin", "feat")
	inWS("checkout", "--quiet", "-b", "feat", "FETCH_HEAD")
	head := inWS("rev-parse", "HEAD")
	if stale := inWS("rev-parse", "main"); stale == head {
		t.Fatal("fixture is wrong: the workspace's main must be stale for this test to mean anything")
	}

	type precheck struct {
		IsEmpty       bool   `json:"is_empty"`
		ChangedFiles  int    `json:"changed_files"`
		ReviewedSHA   string `json:"reviewed_sha"`
		BaseSHA       string `json:"base_sha"`
		BaseIsCurrent bool   `json:"base_is_current"`
	}

	runWithPath := func(t *testing.T, workspace, base, pathPrefix string) precheck {
		t.Helper()
		body := toolCommand(t, "review-pr/main.bot", "diff_precheck")
		for ref, val := range map[string]string{
			"{{vars.workspace_dir}}": workspace,
			"{{vars.base_ref}}":      base,
		} {
			if !strings.Contains(body, ref) {
				t.Fatalf("%s is no longer referenced by diff_precheck — the test wires nothing", ref)
			}
			body = strings.ReplaceAll(body, ref, "'"+strings.ReplaceAll(val, "'", `'\''`)+"'")
		}
		cmd := exec.Command("sh", "-c", body)
		if pathPrefix != "" {
			cmd.Env = append(os.Environ(), "PATH="+pathPrefix+":"+os.Getenv("PATH"))
		}
		done := make(chan struct{})
		var out []byte
		var err error
		go func() {
			out, err = cmd.Output()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(3 * time.Minute):
			t.Fatal("diff_precheck never returned — it is the workflow's ENTRY node, so a hang here parks the run until max_duration and leaves the required check pending for good")
		}
		if err != nil {
			t.Fatalf("diff_precheck failed: %v (out %q)", err, out)
		}
		var got precheck
		if e := json.Unmarshal(out, &got); e != nil {
			t.Fatalf("output is not diff_precheck_output JSON: %v (%q)", e, out)
		}
		return got
	}
	run := func(t *testing.T, workspace, base string) precheck {
		t.Helper()
		return runWithPath(t, workspace, base, "")
	}

	t.Run("a stale local base does not widen the scope", func(t *testing.T) {
		got := run(t, ws, "main")
		if !got.BaseIsCurrent {
			t.Fatal("the base was not refreshed from the remote — without that the merge-base is taken against whatever the checkout last knew")
		}
		if got.ChangedFiles != 1 {
			t.Errorf("changed_files = %d, want 1 — the scope holds a file merged before this branch existed, and the review will report on code the branch never touched",
				got.ChangedFiles)
		}
		if got.BaseSHA == got.ReviewedSHA {
			t.Error("base_sha == reviewed_sha: the diff against it is empty and the review sees nothing")
		}
	})

	t.Run("an unreachable remote degrades and SAYS so", func(t *testing.T) {
		offline := root + "/offline"
		gittest.Run(t, root, "clone", "--quiet", ws, offline)
		gittest.Run(t, offline, "remote", "set-url", "origin", root+"/does-not-exist.git")

		got := run(t, offline, "main")
		if got.BaseIsCurrent {
			t.Error("base_is_current is true with no reachable remote — the review would be read as authoritative about what the branch introduced")
		}
	})

	// The regression that the first version of this fix introduced, and which is
	// strictly worse than the bug it fixed: publishing HEAD as the base when the
	// merge-base does not resolve. The reviewers diff against base_sha, and a
	// diff against HEAD is EMPTY on a clean checkout — so they find no changed
	// files, report nothing, and the gate posts success on a pull request nobody
	// reviewed. A broken base has to break loudly.
	t.Run("an unresolvable base never collapses to HEAD", func(t *testing.T) {
		got := run(t, ws, "no-such-ref-anywhere")
		if got.BaseSHA == got.ReviewedSHA {
			t.Fatal("base_sha collapsed to HEAD on an unresolvable base — the reviewers would see an empty diff and the gate would go green on an unreviewed pull request")
		}
		if got.BaseIsCurrent {
			t.Error("base_is_current must be false when the base did not resolve, or the prompt's unresolved-scope rule never fires")
		}
		if got.IsEmpty {
			t.Error("is_empty must stay false on an unresolved base — it is what routes the run to the reviewers instead of skipping the review")
		}
	})

	// The same shape one level down, and the one the bounded-timeout wrapper
	// created: a git call that fails or times out returns EMPTY stdout, so a
	// diff parsed without checking its return code is indistinguishable from a
	// branch that changed nothing — which short-circuits past the reviewers to
	// `done`. "No files" has two readings a whole review apart, and only one of
	// them may skip the review.
	t.Run("a diff that fails is not a branch that changed nothing", func(t *testing.T) {
		shim := t.TempDir()
		// Forwards everything to the real git except `diff`, which fails the way
		// a timeout does: nonzero, no stdout.
		script := "#!/bin/sh\nif [ \"$1\" = diff ]; then exit 1; fi\nexec " + realGit + " \"$@\"\n"
		if err := os.WriteFile(shim+"/git", []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}

		got := runWithPath(t, ws, "main", shim)
		if got.IsEmpty {
			t.Error("is_empty is true because the diff FAILED — the run skips both reviewers and the gate goes green on a pull request nobody read")
		}
		if got.ChangedFiles != -1 {
			t.Errorf("changed_files = %d, want the -1 unresolved sentinel", got.ChangedFiles)
		}
		if got.BaseIsCurrent {
			t.Error("base_is_current must be false when the scope could not be computed")
		}
	})

	t.Run("base_ref HEAD still means HEAD versus the working tree", func(t *testing.T) {
		got := run(t, ws, "HEAD")
		if got.BaseSHA != got.ReviewedSHA {
			t.Errorf("base_sha = %s, want HEAD %s — the review-uncommitted-only mode must be untouched", got.BaseSHA, got.ReviewedSHA)
		}
		if !got.IsEmpty {
			t.Errorf("changed_files = %d on a clean tree, want 0", got.ChangedFiles)
		}
	})
}
