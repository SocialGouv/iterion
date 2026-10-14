package bots

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"
)

// TestBranchImproveGateNeverGreenOnAnUnresolvedFinding is the guard that keeps
// the fixer from grading its own homework.
//
// The fixer now fills the same shared merge-gate context a reviewer does. That
// is only legitimate while the verdict stays a COUNT: if a `refused` entry
// could green the check, a fixer would clear any review by contesting every
// finding — the exact self-certification the deterministic-gate doctrine
// exists to prevent. A refusal is an argument for a human to arbitrate; it
// never unblocks a merge.
//
// The bot's real python body is executed, so the assertion is on shipped
// behaviour rather than on a description of it.
func TestBranchImproveGateNeverGreenOnAnUnresolvedFinding(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not on PATH")
	}

	type published struct {
		Summary string         `json:"summary"`
		Gate    map[string]any `json:"gate"`
	}
	// The run page the engine resolves into the comment's footer. A subtest
	// blanks it to exercise a deployment with no public address.
	runPage := "https://iterion.example/studio/runs/01a0bil"
	runID := "01a0bil"
	runBanked := func(t *testing.T, ledger, clean, pushed, verifyOK, verifySkipped, verifyPassed, banked, take string) (published, bool) {
		t.Helper()
		var got published
		var posted bool
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			posted = true
			raw, _ := io.ReadAll(r.Body)
			if err := json.Unmarshal(raw, &got); err != nil {
				t.Errorf("payload is not JSON: %v (%q)", err, raw)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"published": true, "gate_posted": got.Gate != nil})
		}))
		defer srv.Close()

		body := toolCommand(t, "branch-improve-loop/main.bot", "publish_verdict")
		for ref, val := range map[string]string{
			"{{vars.forge_publish_url}}":   srv.URL + "/api/v1/forge/publish-review",
			"{{vars.forge_publish_token}}": "run-token",
			"{{input.pr_url}}":             "https://github.com/acme/widgets/pull/7",
			"{{input.finding_ledger}}":     ledger,
			"{{input.review_summary}}":     "did the work",
			"{{input.branch_clean}}":       clean,
			"{{input.commits_pushed}}":     pushed,
			"{{input.push_reason}}":        "pushed 2 commits",
			"{{input.banked_branch}}":      banked,
			"{{input.how_to_take}}":        take,
			"{{input.superseded}}":         "false",
			"{{input.verify_ok}}":          verifyOK,
			"{{input.verify_skipped}}":     verifySkipped,
			"{{input.verify_passed}}":      verifyPassed,
			"{{vars.gate_enabled}}":        "true",
			"{{vars.gate_context}}":        "iterion/review",
			"{{run.url}}":                  runPage,
			"{{run.id}}":                   runID,
		} {
			if !strings.Contains(body, ref) {
				t.Fatalf("%s is no longer referenced by publish_verdict — the test wires nothing", ref)
			}
			body = strings.ReplaceAll(body, ref, "'"+strings.ReplaceAll(val, "'", `'\''`)+"'")
		}
		out, err := exec.Command("sh", "-c", body).Output()
		if err != nil {
			t.Fatalf("publish_verdict failed: %v (out %q)", err, out)
		}
		var res map[string]any
		if err := json.Unmarshal(out, &res); err != nil {
			t.Fatalf("output is not post_feedback_output JSON: %v (%q)", err, out)
		}
		return got, posted
	}

	// Everywhere but the split case below, the build verdict tracks convergence.
	runWith := func(t *testing.T, ledger, clean, pushed, verifyOK, verifySkipped string) (published, bool) {
		t.Helper()
		return runBanked(t, ledger, clean, pushed, verifyOK, verifySkipped, verifyOK, "", "")
	}
	// The one case where they must differ: the build passed and the review did not.
	runSplit := func(t *testing.T, ledger, clean, pushed, verifyOK, verifySkipped, verifyPassed string) (published, bool) {
		t.Helper()
		return runBanked(t, ledger, clean, pushed, verifyOK, verifySkipped, verifyPassed, "", "")
	}
	run := func(t *testing.T, ledger, clean, pushed, verifyOK string) (published, bool) {
		return runWith(t, ledger, clean, pushed, verifyOK, "false")
	}

	blocking := func(t *testing.T, g map[string]any) float64 {
		t.Helper()
		if g == nil {
			t.Fatal("no gate payload — the required check never lands and the PR is stuck")
		}
		n, ok := g["blocking_count"].(float64)
		if !ok {
			t.Fatalf("gate carries no blocking_count: %v", g)
		}
		return n
	}

	allFixed := `[{"id":"R1111","status":"fixed","commit":"abc1234567890"}]`

	// Every comment a bot posts names the run that wrote it: the reader's way
	// to the steps, the models and the cost behind the verdict, and the way
	// into the platform for someone meeting it on their own forge. With no
	// public address the footer carries the id alone — a root-relative link
	// posted on a forge resolves against the FORGE.
	t.Run("the comment links the run that wrote it", func(t *testing.T) {
		got, _ := run(t, allFixed, "true", "true", "true")
		if want := "run [`" + runID + "`](" + runPage + ")"; !strings.Contains(got.Summary, want) {
			t.Errorf("the comment does not link its run (%s):\n%s", want, got.Summary)
		}

		restore := runPage
		runPage = ""
		defer func() { runPage = restore }()
		got, _ = run(t, allFixed, "true", "true", "true")
		// The product reference stays (it is a constant, not an address of
		// this deployment); only the RUN link must be absent.
		if strings.Contains(got.Summary, "run [") {
			t.Errorf("with no public address the footer must not link a run:\n%s", got.Summary)
		}
		if !strings.Contains(got.Summary, "run `"+runID+"`") {
			t.Errorf("with no public address the footer must still name the run id:\n%s", got.Summary)
		}
	})

	// A run id is CALLER-CHOSEN at launch and screened only for path traversal
	// (store.SanitizePathComponent accepts `]`, `(`, `)` and a backtick), so an
	// id can hold markdown. Pasted raw into the footer's label it closes the
	// link early and publishes an attacker's host under this bot's signature,
	// on someone else's repository — WHATWG resolves a slash-free
	// `https:evil.example`, so no path separator is needed.
	t.Run("a run id holding markdown never becomes a link", func(t *testing.T) {
		restore := runID
		runID = "x`](https:evil.example)`"
		defer func() { runID = restore }()
		got, _ := run(t, allFixed, "true", "true", "true")
		if strings.Contains(got.Summary, "evil.example)") || strings.Contains(got.Summary, "](https:evil") {
			t.Errorf("the id was pasted into the footer and published a foreign link:\n%s", got.Summary)
		}
		if !strings.Contains(got.Summary, "run [this run]("+runPage+")") {
			t.Errorf("the run link must survive with a fixed label when the id is unusable:\n%s", got.Summary)
		}
	})

	// The two degradations COMPOSED: no public address and an id no label can
	// carry. Each alone is covered above; together they used to leave the
	// comment with no identifier at all, which is the assertion this footer
	// exists to avoid.
	t.Run("no address and an unusable id still name the run", func(t *testing.T) {
		restoreP, restoreI := runPage, runID
		runPage, runID = "", "x`](https:evil.example)`"
		defer func() { runPage, runID = restoreP, restoreI }()
		got, _ := run(t, allFixed, "true", "true", "true")
		// No link at all (no address to link to), and the id named inside a
		// code span — where its brackets and parentheses are inert text, which
		// is why naming it there is safe when labelling it is not.
		if strings.Contains(got.Summary, "run [") {
			t.Errorf("a link was published with no address:\n%s", got.Summary)
		}
		if !strings.Contains(got.Summary, "run `x](https:evil.example)`") {
			t.Errorf("the run must still be named, inside a code span:\n%s", got.Summary)
		}
	})

	// Same for the engine that predates the member: C153 is a warning, so the
	// bundle admits and the reference arrives unsubstituted. The footer must
	// degrade to the id, never publish the delimiter inside a link.
	t.Run("an older engine degrades to the id, never a template delimiter", func(t *testing.T) {
		restore := runPage
		runPage = "{" + "{run.url}}"
		defer func() { runPage = restore }()
		got, _ := run(t, allFixed, "true", "true", "true")
		if strings.Contains(got.Summary, "run.url") || strings.Contains(got.Summary, "run [") {
			t.Errorf("the unresolved reference was published as a link:\n%s", got.Summary)
		}
		if !strings.Contains(got.Summary, "run `"+runID+"`") {
			t.Errorf("the footer must still name the run id:\n%s", got.Summary)
		}
	})

	t.Run("a refused finding keeps the gate red", func(t *testing.T) {
		got, _ := run(t, `[{"id":"R1111","status":"fixed"},{"id":"R2222","status":"refused","note":"not reachable from any caller"}]`,
			"true", "true", "true")
		if n := blocking(t, got.Gate); n < 1 {
			t.Errorf("blocking_count = %v with a refused finding — a fixer could clear any review by contesting it", n)
		}
		if !strings.Contains(got.Summary, "keeps the merge gate red") {
			t.Errorf("the PR must say a contested finding still blocks:\n%s", got.Summary)
		}
		// The argument itself must reach the human who arbitrates it.
		if !strings.Contains(got.Summary, "not reachable from any caller") {
			t.Errorf("the refusal's argument is missing from the table:\n%s", got.Summary)
		}
	})

	t.Run("a deferred finding keeps the gate red", func(t *testing.T) {
		got, _ := run(t, `[{"id":"R3333","status":"deferred","note":"needs a migration"}]`, "true", "true", "true")
		if n := blocking(t, got.Gate); n < 1 {
			t.Errorf("blocking_count = %v with a deferred finding", n)
		}
	})

	t.Run("a red build keeps the gate red even with every finding fixed", func(t *testing.T) {
		got, _ := run(t, allFixed, "true", "true", "false")
		if n := blocking(t, got.Gate); n < 1 {
			t.Errorf("blocking_count = %v on a tree that does not build", n)
		}
	})

	t.Run("remaining issues in the diff keep the gate red", func(t *testing.T) {
		got, _ := run(t, allFixed, "false", "true", "true")
		if n := blocking(t, got.Gate); n < 1 {
			t.Errorf("blocking_count = %v while the fixer still reports issues in the diff", n)
		}
	})

	// Pushing code that nothing has reviewed must not be reported as vetted:
	// with no ledger there is no review of this revision to speak for.
	t.Run("code pushed with no review of it is not green", func(t *testing.T) {
		got, _ := run(t, `[]`, "true", "true", "true")
		if n := blocking(t, got.Gate); n < 1 {
			t.Errorf("blocking_count = %v on a head nothing reviewed", n)
		}
	})

	// Found by an adversarial pass. `pushed=false` is routine — a protected
	// branch, a non-fast-forward, a rebase conflict, a dead token — and the head
	// then still carries every finding the ledger calls fixed.
	t.Run("a fix that never reached the head is not a fix", func(t *testing.T) {
		got, _ := run(t, allFixed, "true", "false", "true")
		if n := blocking(t, got.Gate); n < 1 {
			t.Errorf("blocking_count = %v while the claimed fixes never landed on this head — the PR would merge unfixed", n)
		}
	})

	// Same pass: filtering entries on a non-empty id ran BEFORE the unresolved
	// count, so a malformed refusal vanished from the count and the table both.
	// Bad agent output must fail closed.
	t.Run("a refused entry with no id still blocks", func(t *testing.T) {
		got, _ := run(t, `[{"id":"R1111","status":"fixed"},{"status":"refused","note":"not reachable"}]`,
			"true", "true", "true")
		if n := blocking(t, got.Gate); n < 1 {
			t.Errorf("blocking_count = %v — an id-less refusal cleared the gate", n)
		}
	})

	// A skipped build arrives as verify_ok=false too: verify_ok is
	// gate.converged, which reads verify_run.passed, and a missing verify.sh is
	// a refusal (#1598). "true/true" is therefore not a state this node can be
	// handed any more — the realistic pair is false/true.
	t.Run("a skipped build is not a green build", func(t *testing.T) {
		got, _ := runWith(t, allFixed, "true", "true", "false", "true")
		if n := blocking(t, got.Gate); n < 1 {
			t.Errorf("blocking_count = %v with a build that never ran", n)
		}
	})

	// The required check is read by a human deciding what to do next, so the
	// note must not send them after a build failure that never happened. The
	// skip is the MORE SPECIFIC case and has to be tested first: verify_ok is
	// false on a skip as well, so an `if not verify_ok` written first makes the
	// skip branch unreachable and the status says "red" about a build that
	// never ran.
	t.Run("a skipped build is diagnosed as never run, not as red", func(t *testing.T) {
		got, _ := runWith(t, allFixed, "true", "true", "false", "true")
		note, _ := got.Gate["note"].(string)
		if !strings.Contains(note, "never ran") {
			t.Errorf("note = %q, want it to say the build never ran", note)
		}
		if strings.Contains(note, "build/tests red") {
			t.Errorf("note = %q — it accuses a build failure that never happened", note)
		}
	})

	t.Run("a genuinely red build is still diagnosed as red", func(t *testing.T) {
		got, _ := runWith(t, allFixed, "true", "true", "false", "false")
		note, _ := got.Gate["note"].(string)
		if !strings.Contains(note, "build/tests red") {
			t.Errorf("note = %q, want it to name the red build", note)
		}
		if strings.Contains(note, "never ran") {
			t.Errorf("note = %q — a red build did run", note)
		}
	})

	// `verify_ok` is gate.converged — build AND clean tree AND clean review — so it
	// is false whenever ANY of the three is, including on a green build whose review
	// found issues. That is the dominant blocking state for a review-and-improve
	// bot, and deriving the note from it accuses a build failure that did not
	// happen. The build verdict travels on its own input for exactly this case.
	t.Run("a green build with an unclean review is not accused of being red", func(t *testing.T) {
		got, _ := runSplit(t, allFixed, "false", "true", "false", "false", "true")
		note, _ := got.Gate["note"].(string)
		if strings.Contains(note, "build/tests red") {
			t.Errorf("note = %q — the build passed; only the review was unclean", note)
		}
		if !strings.Contains(note, "issues remain in the diff") {
			t.Errorf("note = %q, want it to name the unclean review", note)
		}
	})

	t.Run("everything fixed, clean and green passes", func(t *testing.T) {
		got, _ := run(t, allFixed, "true", "true", "true")
		if n := blocking(t, got.Gate); n != 0 {
			t.Errorf("blocking_count = %v when every finding is fixed, the re-review is clean and the build is green", n)
		}
		// It must not read as an independent review — the fixer wrote the code.
		note, _ := got.Gate["note"].(string)
		if !strings.Contains(note, "by the fixer") {
			t.Errorf("a green gate must say whose re-review it rests on, got %q", note)
		}
	})

	// Speaking for a revision this run did not change would overwrite whatever
	// verdict already sits on it — including a reviewer's.
	t.Run("nothing pushed and nothing answered posts nothing at all", func(t *testing.T) {
		_, posted := run(t, `[]`, "true", "false", "true")
		if posted {
			t.Error("posted a verdict for a head this run neither changed nor reviewed")
		}
	})

	// The one exception, and the reason it is one: a BANKED branch is not a
	// verdict about the head, it is a fact about work that exists. Silence
	// there strands it — iterion#773 measured two runs, $16.81 and $23.70 of
	// reviewed, test-backed commits, whose verdicts named no branch and whose
	// readers had no way to reach them.
	t.Run("a banked branch is always named, even with nothing pushed", func(t *testing.T) {
		const branch = "iterion/banked/feature-x-1234567890ab"
		got, posted := runBanked(t, `[]`, "true", "false", "true", "false", "true",
			branch, "git fetch origin "+branch+" && git cherry-pick abcdef123456..FETCH_HEAD")
		if !posted {
			t.Fatal("work banked on a branch nobody is told about is work nobody has")
		}
		if !strings.Contains(got.Summary, branch) {
			t.Errorf("the comment must name the branch:\n%s", got.Summary)
		}
		if !strings.Contains(got.Summary, "cherry-pick") {
			t.Errorf("the comment must carry the command that takes the work:\n%s", got.Summary)
		}
	})
}
