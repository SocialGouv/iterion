package bots

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// TestReviewPRStaleAnchorStillPublishes pins what a review owes the PR it is
// gating.
//
// `publish_review` is the ONE call that carries the merge-gate status, and
// `revi/review` is a required check. So a publish that does not happen is not
// a degraded review — it is a pull request nobody can merge, with no error on
// the run, the PR or the check. That has now happened twice: once because an
// unresolvable {{outputs.…}} ref made the stale-anchor guard fire on every run
// (the tree had not moved at all), and once by design, because the guard's
// intended path returned before the POST.
//
// Stale anchors therefore cost the inline comments and nothing else. The two
// halves are asserted separately: the guard must fire on two real, differing
// shas, and it must NOT fire on anything it cannot read as a sha.
func TestReviewPRStaleAnchorStillPublishes(t *testing.T) {
	for _, bin := range []string{"python3", "git"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not on PATH", bin)
		}
	}

	// A workspace whose HEAD is a known sha, plus its parent to play "the tree
	// moved on since the reviewers judged it".
	ws := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = ws
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "--quiet", "-b", "main")
	if err := os.WriteFile(ws+"/a.txt", []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "-A")
	git("commit", "-m", "one")
	parent := git("rev-parse", "HEAD")
	if err := os.WriteFile(ws+"/a.txt", []byte("two\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "-A")
	git("commit", "-m", "two")
	head := git("rev-parse", "HEAD")

	type published struct {
		Summary  string           `json:"summary"`
		Comments []map[string]any `json:"comments"`
		Gate     map[string]any   `json:"gate"`
	}

	run := func(t *testing.T, reviewedSHA string) (published, bool, map[string]any) {
		t.Helper()
		var got published
		var posted bool
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			posted = true
			raw, _ := io.ReadAll(r.Body)
			if err := json.Unmarshal(raw, &got); err != nil {
				t.Errorf("publish payload is not JSON: %v (%q)", err, raw)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"published": true, "review_url": "https://forge/review/1",
				"comments_posted": len(got.Comments), "suggestions_posted": 0,
				"gate_posted": got.Gate != nil, "gate_state": "success",
			})
		}))
		defer srv.Close()

		// One real finding, so "the comments were dropped" is distinguishable
		// from "there were none".
		findings := `[{"severity":"high","category":"correctness","title":"t","detail":"d","file":"a.txt","line":1}]`
		body := toolCommand(t, "review-pr/main.bot", "publish_review")
		for ref, val := range map[string]string{
			"{{vars.workspace_dir}}":       ws,
			"{{input.reviewed_sha}}":       reviewedSHA,
			"{{vars.forge_publish_url}}":   srv.URL + "/api/v1/forge/publish-review",
			"{{vars.forge_publish_token}}": "run-token",
			"{{vars.pr_review_mode}}":      "comment",
			"{{vars.review_mode}}":         "mono",
			"{{input.pr_url}}":             "https://github.com/acme/widgets/pull/7",
			"{{input.findings}}":           findings,
			"{{input.questions}}":          "",
			"{{input.claude_findings}}":    findings,
			"{{input.gpt_findings}}":       "[]",
			"{{vars.gate_enabled}}":        "true",
			"{{vars.gate_severity}}":       "high",
			"{{vars.gate_context}}":        "revi/review",
		} {
			if !strings.Contains(body, ref) {
				t.Fatalf("%s is no longer referenced by publish_review — the test wires nothing", ref)
			}
			// Shell-quote exactly as the engine does: a raw JSON value dropped
			// into a command line is split on spaces and brace-expanded.
			body = strings.ReplaceAll(body, ref, "'"+strings.ReplaceAll(val, "'", `'\''`)+"'")
		}
		out, err := exec.Command("sh", "-c", body).Output()
		if err != nil {
			t.Fatalf("publish_review failed: %v (out %q)", err, out)
		}
		var res map[string]any
		if err := json.Unmarshal(out, &res); err != nil {
			t.Fatalf("output is not publish_output JSON: %v (%q)", err, out)
		}
		return got, posted, res
	}

	t.Run("tree moved: drops the anchors, still publishes the gate", func(t *testing.T) {
		got, posted, res := run(t, parent)
		if !posted {
			t.Fatal("no publish at all — the merge gate never lands and the PR is stuck for good")
		}
		if got.Gate == nil {
			t.Error("published without a gate payload — the required status cannot be created")
		}
		if len(got.Comments) != 0 {
			t.Errorf("posted %d inline comment(s) anchored to a tree that moved", len(got.Comments))
		}
		if !strings.Contains(got.Summary, "moved after the reviewers judged it") {
			t.Errorf("the summary must say why the anchors are gone, got %q", got.Summary)
		}
		if res["published"] != true {
			t.Errorf("published = %v, want true", res["published"])
		}
	})

	// A mono review has one family, so "0 cross-confirmed" would describe a
	// comparison that never took place. Observed on
	// socialgouv/buildkit-operator#6, whose comment reported it under the
	// default topology.
	t.Run("mono topology: claims no cross-confirmation", func(t *testing.T) {
		got, _, _ := run(t, head)
		if strings.Contains(got.Summary, "cross-confirmed by both model families") {
			t.Errorf("a single-family review reports a cross-family comparison:\n%s", got.Summary)
		}
		if !strings.Contains(got.Summary, "single model family") {
			t.Errorf("the summary should name the topology it ran under:\n%s", got.Summary)
		}
	})

	t.Run("tree unchanged: keeps the anchors", func(t *testing.T) {
		got, posted, _ := run(t, head)
		if !posted || len(got.Comments) == 0 {
			t.Fatalf("a review of the current head must post its inline comments (posted=%v, comments=%d)", posted, len(got.Comments))
		}
	})

	// The value the guard reads is wiring, and wiring breaks. Anything it
	// cannot read as a sha means the guard cannot evaluate itself — and a
	// guard that cannot evaluate itself must not swallow the review.
	for _, tc := range []struct{ name, sha string }{
		{"unresolved template ref", "{{outputs.diff_precheck.reviewed_sha}}"},
		{"empty", ""},
		{"not hex", "not-a-sha-at-all"},
	} {
		t.Run("unreadable sha ("+tc.name+"): publishes normally", func(t *testing.T) {
			got, posted, _ := run(t, tc.sha)
			if !posted {
				t.Fatal("swallowed the review over a value the guard could not evaluate")
			}
			if len(got.Comments) == 0 {
				t.Error("dropped the inline comments over a value the guard could not evaluate")
			}
		})
	}
}
