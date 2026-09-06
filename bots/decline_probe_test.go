package bots

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// A fixer may decline its task — its mission can presume a defect that is not
// there, and pushing anyway is the worst available outcome (issue #706: an
// auto-heal ordered a force-push onto a pull request the bot itself had just
// recorded as having a queue build in flight). But a refusal that is merely
// ASSERTED is a way out of hard work, and a refusal from a pass that already
// committed something would strand that work behind a terminal failure.
//
// decline_probe is the deterministic oracle in between: it compares the
// repository to the state the run was handed — HEAD unmoved since
// workspace_probe, nothing staged or dirty — and only then is the decline
// honoured. This executes the REAL command body against each shape.
//
// Every case must answer on a ZERO exit status: the verdict is the node's
// output, routed by the graph; a non-zero exit would replace it with the
// engine's generic tool failure and the run would read as a crash.
func TestDeclineProbeHonoursOnlyAnUntouchedRepository(t *testing.T) {
	for _, bin := range []string{"python3", "git", "sh"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not on PATH", bin)
		}
	}
	command := toolCommand(t, "branch-improve-loop/main.bot", "decline_probe")
	env := hermeticGitEnv(t)

	type declineState struct {
		Honoured bool   `json:"honoured"`
		Reason   string `json:"reason"`
	}
	run := func(t *testing.T, ws, entryHead, reason string) declineState {
		t.Helper()
		rendered := strings.NewReplacer(
			"{{vars.workspace_dir}}", shellQuote(ws),
			"{{input.entry_head}}", shellQuote(entryHead),
			"{{input.decline_reason}}", shellQuote(reason),
		).Replace(command)
		cmd := exec.Command("sh", "-c", rendered)
		cmd.Env = env
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("decline_probe exited non-zero (%v): its verdict is the node OUTPUT, so a non-zero exit turns a refusal into a crash. out=%q", err, out)
		}
		var st declineState
		if uerr := json.Unmarshal(out, &st); uerr != nil {
			t.Fatalf("decline_probe output is not decline_state JSON: %v (out %q)", uerr, out)
		}
		return st
	}

	t.Run("untouched_repository_honours_the_decline", func(t *testing.T) {
		ws := initRepo(t, env)
		head := gitHermetic(t, env, ws, "rev-parse", "HEAD")
		st := run(t, ws, head, "the queue ejected this PR on an unrelated flaky test; the diff has no defect")
		if !st.Honoured {
			t.Fatalf("a run that changed nothing must be allowed to decline: %+v", st)
		}
		if !strings.Contains(st.Reason, "flaky test") {
			t.Errorf("the reason the author reads must survive into the run's error: %q", st.Reason)
		}
		if !strings.Contains(st.Reason, "HEAD unmoved") {
			t.Errorf("the verdict must carry its own evidence, not just the claim: %q", st.Reason)
		}
	})

	t.Run("a_pass_that_committed_cannot_decline", func(t *testing.T) {
		ws := initRepo(t, env)
		entry := gitHermetic(t, env, ws, "rev-parse", "HEAD")
		commitFile(t, env, ws, "fix.txt", "fix(scope): a real fix")
		st := run(t, ws, entry, "nothing to fix here")
		if st.Honoured {
			t.Fatalf("a pass that banked a commit did have something to fix — honouring the decline would strand that commit behind a terminal failure: %+v", st)
		}
		if !strings.Contains(st.Reason, "HEAD moved") {
			t.Errorf("the refusal must name what contradicted the claim: %q", st.Reason)
		}
	})

	t.Run("a_dirty_tree_cannot_decline", func(t *testing.T) {
		ws := initRepo(t, env)
		head := gitHermetic(t, env, ws, "rev-parse", "HEAD")
		if err := os.WriteFile(filepath.Join(ws, "scratch.txt"), []byte("half a fix\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		st := run(t, ws, head, "nothing to fix here")
		if st.Honoured {
			t.Fatalf("a pass that left edits in the tree did not leave the repository as it found it: %+v", st)
		}
		if !strings.Contains(st.Reason, "scratch.txt") {
			t.Errorf("the refusal must name the leftover paths: %q", st.Reason)
		}
	})

	t.Run("a_decline_with_no_reason_is_refused", func(t *testing.T) {
		ws := initRepo(t, env)
		head := gitHermetic(t, env, ws, "rev-parse", "HEAD")
		st := run(t, ws, head, "")
		if st.Honoured {
			t.Fatalf("a refusal the author cannot read is not one: %+v", st)
		}
	})

	t.Run("an_unverifiable_decline_ships_instead", func(t *testing.T) {
		// No entry head recorded (a resume onto a checkpoint that predates
		// the field, a probe that could not read HEAD): the claim cannot be
		// checked, so the run takes the ordinary tail rather than ending
		// terminal on an unproven refusal.
		ws := initRepo(t, env)
		st := run(t, ws, "", "nothing to fix here")
		if st.Honoured {
			t.Fatalf("an unverifiable decline must fail CLOSED to shipping, not to a terminal refusal: %+v", st)
		}
	})
}
