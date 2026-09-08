package server

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	"github.com/SocialGouv/iterion/pkg/store"
)

// resetTaskWith POSTs the reset action with an explicit body, so a test can
// exercise the `fresh` opt-in and the historical (pointer-keeping) shape
// through the same endpoint.
func resetTaskWith(t *testing.T, env *pipelineBoardTestEnv, id, body string) *http.Response {
	t.Helper()
	resp, err := http.Post(
		env.http.URL+"/api/v1/pipeline-board/tasks/"+id+"/reset",
		"application/json", bytes.NewBufferString(body))
	if err != nil {
		t.Fatalf("POST reset: %v", err)
	}
	return resp
}

func respBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(raw)
}

// lastRunAsTheDispatcherSeesIt reads the pointer through the SAME interface
// the dispatcher's resolveRunID type-asserts for (native.Adapter's
// LastRunForIssue) — not through the raw issue field. That is the first link
// of the real resolution chain: whatever this returns is what
// resumableRunID is handed.
func lastRunAsTheDispatcherSeesIt(t *testing.T, env *pipelineBoardTestEnv, issueID string) string {
	t.Helper()
	type lastRunLookup interface {
		LastRunForIssue(id string) (string, error)
	}
	var look lastRunLookup = native.NewAdapter(env.board)
	prev, err := look.LastRunForIssue(issueID)
	if err != nil {
		t.Fatalf("LastRunForIssue(%s): %v", issueID, err)
	}
	return prev
}

// Reset with `fresh` is the board-side answer to "start over, discard that
// run": it drops the last-run pointer the dispatcher resumes from, so a run
// that died in a way resuming cannot fix stops being resumed for ever.
//
// The pointer is read back through the dispatcher's OWN lookup, and the
// negative half (a plain reset) pins today's behaviour so `fresh` cannot
// quietly become the default.
func TestPipelineBoardResetFreshDropsTheResumePointer(t *testing.T) {
	seed := func(t *testing.T, env *pipelineBoardTestEnv, runID string) *native.Issue {
		t.Helper()
		issue, err := env.board.Create(native.Issue{
			Title: "Deterministic failure", State: native.StateInProgress, Bot: "review",
		})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		env.seedRun(t, runID, "review", store.RunStatusFailedResumable, func(run *store.Run) {
			run.FilePath = env.botPath
			run.Checkpoint = &store.Checkpoint{NodeID: "approval"}
		})
		if err := env.board.SetLastRun(issue.ID, runID, ""); err != nil {
			t.Fatalf("SetLastRun: %v", err)
		}
		if got := lastRunAsTheDispatcherSeesIt(t, env, issue.ID); got != runID {
			t.Fatalf("seed: LastRunForIssue = %q, want %q", got, runID)
		}
		return issue
	}

	t.Run("fresh clears it", func(t *testing.T) {
		env := newPipelineBoardTestEnv(t)
		issue := seed(t, env, "fresh-clear")

		resp := resetTaskWith(t, env, issue.ID, `{"fresh":true}`)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("reset status = %d, want 200 (body: %s)", resp.StatusCode, respBody(t, resp))
		}
		var updated native.Issue
		decodeJSONResp(t, resp, &updated)
		if updated.State != native.StateReady {
			t.Errorf("state = %q, want %q", updated.State, native.StateReady)
		}
		if updated.LastRunID != "" {
			t.Errorf("response still carries last_run_id %q — the body contradicts its own write", updated.LastRunID)
		}
		if got := lastRunAsTheDispatcherSeesIt(t, env, issue.ID); got != "" {
			t.Errorf("LastRunForIssue = %q, want \"\" — the dispatcher would still resume the dead run", got)
		}
		// History is kept: the run happened, and its record is the only
		// place the failure is written down.
		after, err := env.board.Get(issue.ID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		found := false
		for _, ref := range after.Runs {
			if ref.RunID == "fresh-clear" {
				found = true
			}
			if ref.RunID == "" {
				t.Errorf("a blank run ref was appended by the clear: %+v", after.Runs)
			}
		}
		if !found {
			t.Errorf("run history lost the attempt: %+v", after.Runs)
		}
		if _, err := env.runStore(t).LoadRun(context.Background(), "fresh-clear"); err != nil {
			t.Errorf("the run record was deleted: %v", err)
		}
	})

	t.Run("without fresh the pointer is kept", func(t *testing.T) {
		env := newPipelineBoardTestEnv(t)
		issue := seed(t, env, "keep-pointer")

		resp := resetTaskWith(t, env, issue.ID, `{}`)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("reset status = %d, want 200 (body: %s)", resp.StatusCode, respBody(t, resp))
		}
		resp.Body.Close()
		if got := lastRunAsTheDispatcherSeesIt(t, env, issue.ID); got != "keep-pointer" {
			t.Errorf("LastRunForIssue = %q, want %q — a plain reset must not change what Retry means", got, "keep-pointer")
		}
	})

	t.Run("an absent body is still a plain reset", func(t *testing.T) {
		env := newPipelineBoardTestEnv(t)
		issue := seed(t, env, "no-body")

		req, err := http.NewRequest(http.MethodPost, env.http.URL+"/api/v1/pipeline-board/tasks/"+issue.ID+"/reset", nil)
		if err != nil {
			t.Fatalf("build POST: %v", err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("POST reset: %v", err)
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("reset status = %d, want 200 (body: %s)", resp.StatusCode, respBody(t, resp))
		}
		resp.Body.Close()
		if got := lastRunAsTheDispatcherSeesIt(t, env, issue.ID); got != "no-body" {
			t.Errorf("LastRunForIssue = %q, want %q", got, "no-body")
		}
	})

	t.Run("a malformed body is refused, not silently read as not-fresh", func(t *testing.T) {
		env := newPipelineBoardTestEnv(t)
		issue := seed(t, env, "bad-body")

		resp := resetTaskWith(t, env, issue.ID, `{"fresh":`)
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("reset status = %d, want 400 (body: %s)", resp.StatusCode, respBody(t, resp))
		}
		resp.Body.Close()
		after, err := env.board.Get(issue.ID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if after.State != native.StateInProgress {
			t.Errorf("state = %q, want untouched %q", after.State, native.StateInProgress)
		}
	})
}

// The clear must land BEFORE the restage. Both orders end in the same
// persisted state, so only the ORDER of the board's own events tells them
// apart — and the order is what matters: restaging is what makes the ticket
// eligible again, so a Ready ticket that still names a resumable run is a
// window in which a live dispatcher resumes the run this call was told to
// discard.
func TestPipelineBoardResetFreshClearsThePointerBeforeRestaging(t *testing.T) {
	env := newPipelineBoardTestEnv(t)
	issue, err := env.board.Create(native.Issue{Title: "Order matters", State: native.StateInProgress, Bot: "review"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	env.seedRun(t, "order-run", "review", store.RunStatusFailedResumable, func(run *store.Run) {
		run.FilePath = env.botPath
	})
	if err := env.board.SetLastRun(issue.ID, "order-run", ""); err != nil {
		t.Fatalf("SetLastRun: %v", err)
	}

	resp := resetTaskWith(t, env, issue.ID, `{"fresh":true}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("reset status = %d, want 200 (body: %s)", resp.StatusCode, respBody(t, resp))
	}
	resp.Body.Close()

	// events.jsonl is append-only, so its order IS the write order: the
	// clear (issue_last_run_updated carrying an empty run_id) must precede
	// the restage to Ready.
	clearedAt, restagedAt, seq := -1, -1, 0
	if err := env.board.ScanEvents(func(ev *native.Event) bool {
		if ev.IssueID != issue.ID {
			return true
		}
		seq++
		switch ev.Type {
		case native.EvtIssueLastRun:
			if id, _ := ev.Payload["run_id"].(string); id == "" {
				clearedAt = seq
			}
		case native.EvtIssueState:
			if to, _ := ev.Payload["to"].(string); to == native.StateReady {
				restagedAt = seq
			}
		}
		return true
	}); err != nil {
		t.Fatalf("ScanEvents: %v", err)
	}
	if clearedAt < 0 {
		t.Fatal("no last-run clear event on the ticket — the pointer was never dropped")
	}
	if restagedAt < 0 {
		t.Fatalf("no restage-to-%s event on the ticket", native.StateReady)
	}
	if clearedAt > restagedAt {
		t.Errorf("the pointer was cleared AFTER the restage (clear at #%d, restage at #%d) — between them the ticket is Ready and still names a resumable run",
			clearedAt, restagedAt)
	}
}

// THE PIN CANARY. Reset deliberately PINS a still-dying run as the ticket's
// current attempt so the relaunch waits for it to stop. Clearing the pointer
// in that window reopens exactly what the pin closes, so `fresh` refuses
// while anything in the ticket's tree is non-terminal — and refuses BEFORE
// the cancel sweep, so nothing at all is touched.
//
// Falsification: delete the guard and this test goes red four ways — the run
// is cancelled, the pointer is dropped, the ticket is restaged, and the
// status is 200.
func TestPipelineBoardResetFreshRefusedWhileARunIsNonTerminal(t *testing.T) {
	t.Run("the pointer run itself", func(t *testing.T) {
		env := newPipelineBoardTestEnv(t)
		issue, err := env.board.Create(native.Issue{Title: "Still parked", State: native.StateInProgress, Bot: "review"})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		env.seedRun(t, "fresh-parked", "review", store.RunStatusPausedWaitingHuman, func(run *store.Run) {
			run.FilePath = env.botPath
			run.Checkpoint = &store.Checkpoint{NodeID: "approval", InteractionID: "int-1"}
		})
		if err := env.board.SetLastRun(issue.ID, "fresh-parked", ""); err != nil {
			t.Fatalf("SetLastRun: %v", err)
		}

		resp := resetTaskWith(t, env, issue.ID, `{"fresh":true}`)
		if resp.StatusCode != http.StatusConflict {
			t.Fatalf("reset status = %d, want 409 (body: %s)", resp.StatusCode, respBody(t, resp))
		}
		body := respBody(t, resp)
		if !strings.Contains(body, "fresh-parked") {
			t.Errorf("refusal does not name the run holding it back: %s", body)
		}

		// Nothing was touched: the refusal is side-effect free.
		run, err := env.runStore(t).LoadRun(context.Background(), "fresh-parked")
		if err != nil {
			t.Fatalf("LoadRun: %v", err)
		}
		if run.Status != store.RunStatusPausedWaitingHuman {
			t.Errorf("run status = %q, want untouched %q — the refusal cancelled it anyway", run.Status, store.RunStatusPausedWaitingHuman)
		}
		if got := lastRunAsTheDispatcherSeesIt(t, env, issue.ID); got != "fresh-parked" {
			t.Errorf("LastRunForIssue = %q, want %q — the pointer was dropped on a LIVE run", got, "fresh-parked")
		}
		after, err := env.board.Get(issue.ID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if after.State != native.StateInProgress {
			t.Errorf("state = %q, want untouched %q", after.State, native.StateInProgress)
		}
	})

	// The guard reads the whole ticket TREE, not just the pointer: a
	// descendant still parked under a terminal root is exactly the run the
	// pin exists for (it never stamped LastRunID).
	t.Run("a non-terminal descendant of a terminal root", func(t *testing.T) {
		env := newPipelineBoardTestEnv(t)
		issue, err := env.board.Create(native.Issue{Title: "Live child", State: native.StateInProgress, Bot: "review"})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		env.seedRun(t, "fresh-dead-root", "review", store.RunStatusFailedResumable, func(run *store.Run) {
			run.FilePath = env.botPath
		})
		env.seedRun(t, "fresh-live-child", "review", store.RunStatusPausedWaitingHuman, func(run *store.Run) {
			run.FilePath = env.botPath
			run.ParentRunID = "fresh-dead-root"
			run.Checkpoint = &store.Checkpoint{NodeID: "approval", InteractionID: "int-2"}
		})
		if err := env.board.SetLastRun(issue.ID, "fresh-dead-root", ""); err != nil {
			t.Fatalf("SetLastRun: %v", err)
		}

		resp := resetTaskWith(t, env, issue.ID, `{"fresh":true}`)
		if resp.StatusCode != http.StatusConflict {
			t.Fatalf("reset status = %d, want 409 (body: %s)", resp.StatusCode, respBody(t, resp))
		}
		if body := respBody(t, resp); !strings.Contains(body, "fresh-live-child") {
			t.Errorf("refusal does not name the live descendant: %s", body)
		}
		if got := lastRunAsTheDispatcherSeesIt(t, env, issue.ID); got != "fresh-dead-root" {
			t.Errorf("LastRunForIssue = %q, want %q", got, "fresh-dead-root")
		}
	})

	// The same card, reset WITHOUT fresh, keeps today's behaviour: the
	// parked run is cancelled and the ticket restaged. The guard is scoped
	// to the clear, never to reset itself.
	t.Run("a plain reset on the same card still cancels and restages", func(t *testing.T) {
		env := newPipelineBoardTestEnv(t)
		issue, err := env.board.Create(native.Issue{Title: "Still parked", State: native.StateInProgress, Bot: "review"})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		env.seedRun(t, "plain-parked", "review", store.RunStatusPausedWaitingHuman, func(run *store.Run) {
			run.FilePath = env.botPath
			run.Checkpoint = &store.Checkpoint{NodeID: "approval", InteractionID: "int-3"}
		})
		if err := env.board.SetLastRun(issue.ID, "plain-parked", ""); err != nil {
			t.Fatalf("SetLastRun: %v", err)
		}

		resp := resetTaskWith(t, env, issue.ID, `{"fresh":false}`)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("reset status = %d, want 200 (body: %s)", resp.StatusCode, respBody(t, resp))
		}
		resp.Body.Close()
		run, err := env.runStore(t).LoadRun(context.Background(), "plain-parked")
		if err != nil {
			t.Fatalf("LoadRun: %v", err)
		}
		if run.Status != store.RunStatusCancelled {
			t.Errorf("run status = %q, want cancelled", run.Status)
		}
	})
}
