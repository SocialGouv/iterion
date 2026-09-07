package bots

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// push_back_tool is the ONE node that pushes a fixer's commits onto a pull
// request's branch, so it is where two production defects meet.
//
//   - #863: a fixer kept working half an hour past the squash-merge of its
//     pull request and pushed six commits onto a branch nobody merges any
//     more. git alone cannot see this — a squash leaves the source branch
//     present and its head no ancestor of the base — so the tool has to ask.
//   - #773: two runs banked $16.81 and $23.70 of reviewed, test-backed work
//     and neither verdict named the branch it was on. "No commits pushed"
//     with no pointer is work nobody can reach.
//
// Both answers are the same shape: when the fixes cannot land on the pull
// request's own branch, push them somewhere NAMED and say where.
type pushState struct {
	Pushed       bool   `json:"pushed"`
	Superseded   bool   `json:"superseded"`
	Reason       string `json:"reason"`
	BankedBranch string `json:"banked_branch"`
	HowToTake    string `json:"how_to_take"`
}

// prStateServer stands in for the iterion server's read half of the publish
// grant (GET /api/v1/forge/pull-request).
func prStateServer(t *testing.T, state string, status int) (*httptest.Server, *int) {
	t.Helper()
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if status != http.StatusOK {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"error":"boom"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"open": state == "" || state == "open", "state": state, "head_sha": "cafe",
		})
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

func TestPushBackToolBanksAndNamesTheBranchItCannotLandOn(t *testing.T) {
	for _, bin := range []string{"python3", "git", "sh"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not on PATH", bin)
		}
	}
	command := toolCommand(t, "branch-improve-loop/main.bot", "push_back_tool")
	env := hermeticGitEnv(t)

	// upstream = the forge; ws = the run's worktree, cloned from it, with the
	// fixer's commits on top of the PR branch.
	world := func(t *testing.T) (upstream, ws string) {
		t.Helper()
		upstream = initRepo(t, env)
		gitHermetic(t, env, upstream, "checkout", "-q", "-b", "feature/x")
		commitFile(t, env, upstream, "base.txt", "feat: the PR's own work")
		gitHermetic(t, env, upstream, "checkout", "-q", "main")
		ws = t.TempDir()
		gitHermetic(t, env, ".", "clone", "-q", "--no-tags", upstream, ws)
		gitHermetic(t, env, ws, "fetch", "-q", "origin", "feature/x")
		gitHermetic(t, env, ws, "checkout", "-q", "-B", "feature/x", "FETCH_HEAD")
		commitFile(t, env, ws, "fix1.txt", "fix: the first defect")
		commitFile(t, env, ws, "fix2.txt", "fix: the second defect")
		return upstream, ws
	}

	run := func(t *testing.T, ws, branch, prURL, stateURL, token string) pushState {
		t.Helper()
		rendered := strings.NewReplacer(
			"{{vars.push_branch}}", shellQuote(branch),
			"{{vars.pr_url}}", shellQuote(prURL),
			"{{vars.forge_pr_state_url}}", shellQuote(stateURL),
			"{{vars.forge_publish_token}}", shellQuote(token),
			"{{vars.base_ref}}", shellQuote("main"),
		).Replace(command)
		cmd := exec.Command("sh", "-c", rendered)
		cmd.Dir = ws
		cmd.Env = append(env, "GH_TOKEN=t0k", "HOME="+t.TempDir())
		var stderr strings.Builder
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("push_back_tool exited non-zero (%v): its result is the node OUTPUT, so a non-zero exit turns a delivery report into a crash.\nstdout=%q\nstderr=%s", err, out, stderr.String())
		}
		var st pushState
		if uerr := json.Unmarshal(out, &st); uerr != nil {
			t.Fatalf("push_back_tool output is not push_state JSON: %v (out %q)", uerr, out)
		}
		return st
	}

	t.Run("a merged pull request is never pushed onto", func(t *testing.T) {
		upstream, ws := world(t)
		srv, hits := prStateServer(t, "merged", http.StatusOK)
		before := gitHermetic(t, env, upstream, "rev-parse", "feature/x")

		st := run(t, ws, "feature/x", "https://github.com/o/r/pull/7", srv.URL, "tok")
		if *hits == 0 {
			t.Fatal("the tool never asked whether the pull request was still open")
		}
		if st.Pushed {
			t.Fatalf("pushed onto a merged pull request's branch: %+v", st)
		}
		if after := gitHermetic(t, env, upstream, "rev-parse", "feature/x"); after != before {
			t.Fatalf("the merged branch moved (%s -> %s) — that is exactly the production incident", before, after)
		}
		if !st.Superseded {
			t.Fatalf("the tail has to be able to route on this: %+v", st)
		}
		if !strings.Contains(st.Reason, "merged") {
			t.Fatalf("the reason must name the state: %q", st.Reason)
		}
		if st.BankedBranch == "" {
			t.Fatalf("the fixes must be banked somewhere named, not dropped: %+v", st)
		}
		// The banked branch is REAL: the commits are on the forge under it.
		got := gitHermetic(t, env, upstream, "rev-parse", st.BankedBranch)
		want := gitHermetic(t, env, ws, "rev-parse", "HEAD")
		if got != want {
			t.Fatalf("banked branch %s is at %s, want the run's HEAD %s", st.BankedBranch, got, want)
		}
		if !strings.Contains(st.HowToTake, st.BankedBranch) || !strings.Contains(st.HowToTake, "cherry-pick") {
			t.Fatalf("a reader needs the command that takes the work, got %q", st.HowToTake)
		}
	})

	t.Run("an open pull request still gets its push", func(t *testing.T) {
		upstream, ws := world(t)
		srv, _ := prStateServer(t, "open", http.StatusOK)
		st := run(t, ws, "feature/x", "https://github.com/o/r/pull/7", srv.URL, "tok")
		if !st.Pushed || st.Superseded {
			t.Fatalf("an open pull request must still be pushed onto: %+v", st)
		}
		got := gitHermetic(t, env, upstream, "rev-parse", "feature/x")
		want := gitHermetic(t, env, ws, "rev-parse", "HEAD")
		if got != want {
			t.Fatalf("the PR branch is at %s, want the run's HEAD %s", got, want)
		}
	})

	t.Run("no grant means no state to read, and the push proceeds", func(t *testing.T) {
		upstream, ws := world(t)
		// A local run holds no publish grant. The tool must not invent a
		// closure from the absence of an endpoint.
		st := run(t, ws, "feature/x", "https://github.com/o/r/pull/7", "", "")
		if !st.Pushed {
			t.Fatalf("a run with no publish grant must still deliver: %+v", st)
		}
		if got, want := gitHermetic(t, env, upstream, "rev-parse", "feature/x"), gitHermetic(t, env, ws, "rev-parse", "HEAD"); got != want {
			t.Fatalf("branch %s != HEAD %s", got, want)
		}
	})

	t.Run("an unreachable endpoint says so and does not decide for itself", func(t *testing.T) {
		_, ws := world(t)
		srv, _ := prStateServer(t, "", http.StatusBadGateway)
		st := run(t, ws, "feature/x", "https://github.com/o/r/pull/7", srv.URL, "tok")
		if st.Superseded {
			t.Fatalf("an unreadable state is not a closure: %+v", st)
		}
		if !strings.Contains(st.Reason, "state") && !strings.Contains(st.Reason, "unreadable") {
			t.Fatalf("the run report must carry that the check could not be made: %q", st.Reason)
		}
	})

	t.Run("a push the forge refuses banks the work and names it", func(t *testing.T) {
		upstream, ws := world(t)
		// The remote branch advances with a CONFLICTING change, so the
		// auto-rebase fails — the shape of run 01a0716a in #773.
		gitHermetic(t, env, upstream, "checkout", "-q", "feature/x")
		writeConflicting(t, env, upstream)
		gitHermetic(t, env, upstream, "checkout", "-q", "main")
		writeConflicting(t, env, ws)

		srv, _ := prStateServer(t, "open", http.StatusOK)
		st := run(t, ws, "feature/x", "https://github.com/o/r/pull/7", srv.URL, "tok")
		if st.Pushed {
			t.Fatalf("a conflicting advance cannot be pushed: %+v", st)
		}
		if st.BankedBranch == "" || st.HowToTake == "" {
			t.Fatalf("#773: work that could not land must still be reachable — %+v", st)
		}
		if got := gitHermetic(t, env, upstream, "rev-parse", st.BankedBranch); got == "" {
			t.Fatalf("banked branch %s is not on the forge", st.BankedBranch)
		}
	})

	t.Run("nothing new to push names what it compared", func(t *testing.T) {
		upstream, _ := world(t)
		// A fresh clone with no commits of its own: HEAD is origin/feature/x.
		ws2 := t.TempDir()
		gitHermetic(t, env, ".", "clone", "-q", "--no-tags", upstream, ws2)
		gitHermetic(t, env, ws2, "fetch", "-q", "origin", "feature/x")
		gitHermetic(t, env, ws2, "checkout", "-q", "-B", "feature/x", "FETCH_HEAD")

		srv, _ := prStateServer(t, "open", http.StatusOK)
		st := run(t, ws2, "feature/x", "https://github.com/o/r/pull/7", srv.URL, "tok")
		if st.Pushed || st.BankedBranch != "" {
			t.Fatalf("a tree with nothing new must bank nothing: %+v", st)
		}
		head := gitHermetic(t, env, ws2, "rev-parse", "HEAD")
		if !strings.Contains(st.Reason, head[:12]) {
			t.Fatalf("#773: %q asserts a git fact with no evidence — it must name the two revisions it compared (HEAD %s)", st.Reason, head[:12])
		}
	})
}

func writeConflicting(t *testing.T, env []string, dir string) {
	t.Helper()
	const name = "conflict.txt"
	if err := os.WriteFile(filepath.Join(dir, name), []byte(dir+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitHermetic(t, env, dir, "add", "--", name)
	gitHermetic(t, env, dir, "commit", "-q", "-m", "chore: touch the same line")
}
