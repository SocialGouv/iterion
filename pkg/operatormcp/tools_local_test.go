package operatormcp

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/store"
)

const minimalBot = `tool probe:
  command: "echo hi"

workflow main:
  entry: probe
  probe -> done
`

// call invokes a tool and returns the text payload + isError flag.
func call(t *testing.T, s *Server, name, args string) (string, bool) {
	t.Helper()
	res, err := s.Call(context.Background(), name, json.RawMessage(args))
	if err != nil {
		t.Fatalf("Call(%s): %v", name, err)
	}
	if len(res.Content) != 1 {
		t.Fatalf("Call(%s): want one content block, got %+v", name, res)
	}
	return res.Content[0].Text, res.IsError
}

func TestLocalValidate(t *testing.T) {
	s := newTestServer(t)
	if err := os.WriteFile(filepath.Join(s.WorkDir, "ok.bot"), []byte(minimalBot), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.WorkDir, "broken.bot"), []byte("not a workflow at all"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Relative path exercises resolvePath against WorkDir.
	text, isErr := call(t, s, "local_validate", `{"file_path":"ok.bot"}`)
	if isErr {
		t.Fatalf("valid file flagged as tool error: %s", text)
	}
	var res struct {
		Valid        bool   `json:"valid"`
		WorkflowName string `json:"workflow_name"`
	}
	if err := json.Unmarshal([]byte(text), &res); err != nil {
		t.Fatalf("decode: %v\n%s", err, text)
	}
	if !res.Valid || res.WorkflowName != "main" {
		t.Fatalf("unexpected result: %+v", res)
	}

	// An INVALID workflow is a normal answer (valid:false + diagnostics),
	// not a tool error.
	text, isErr = call(t, s, "local_validate", `{"file_path":"broken.bot"}`)
	if isErr {
		t.Fatalf("invalid file should not be a tool error: %s", text)
	}
	if err := json.Unmarshal([]byte(text), &res); err != nil {
		t.Fatalf("decode: %v\n%s", err, text)
	}
	if res.Valid {
		t.Fatal("broken.bot reported valid")
	}

	// A missing file IS a tool error.
	if _, isErr := call(t, s, "local_validate", `{"file_path":"absent.bot"}`); !isErr {
		t.Fatal("missing file should be a tool error")
	}
}

func TestLocalRunsListAndGet(t *testing.T) {
	s := newTestServer(t)
	st, err := s.store()
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := st.CreateRun(ctx, "run-a", "wf-a", map[string]any{"k": "v"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateRun(ctx, "run-b", "wf-b", nil); err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateRunStatus(ctx, "run-a", store.RunStatusFinished, ""); err != nil {
		t.Fatal(err)
	}

	text, isErr := call(t, s, "local_runs_list", `{}`)
	if isErr {
		t.Fatalf("list errored: %s", text)
	}
	var list struct {
		Runs  []runSummary `json:"runs"`
		Total int          `json:"total"`
	}
	if err := json.Unmarshal([]byte(text), &list); err != nil {
		t.Fatalf("decode: %v\n%s", err, text)
	}
	if list.Total != 2 || len(list.Runs) != 2 {
		t.Fatalf("want 2 runs, got %+v", list)
	}

	text, _ = call(t, s, "local_runs_list", `{"status":"finished"}`)
	if err := json.Unmarshal([]byte(text), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Runs) != 1 || list.Runs[0].ID != "run-a" {
		t.Fatalf("status filter broken: %+v", list.Runs)
	}

	text, isErr = call(t, s, "local_run_get", `{"run_id":"run-a"}`)
	if isErr {
		t.Fatalf("get errored: %s", text)
	}
	var view map[string]any
	if err := json.Unmarshal([]byte(text), &view); err != nil {
		t.Fatal(err)
	}
	if view["status"] != "finished" || view["workflow_name"] != "wf-a" {
		t.Fatalf("unexpected view: %v", view)
	}
	if view["resumable"] != false {
		t.Fatalf("finished run reported resumable: %v", view["resumable"])
	}
}

func TestLocalRunEventsAndLog(t *testing.T) {
	s := newTestServer(t)
	st, err := s.store()
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := st.CreateRun(ctx, "run-ev", "wf", nil); err != nil {
		t.Fatal(err)
	}
	for _, typ := range []string{"run_started", "node_started", "node_finished"} {
		if _, err := st.AppendEvent(ctx, "run-ev", store.Event{Type: store.EventType(typ)}); err != nil {
			t.Fatal(err)
		}
	}

	text, isErr := call(t, s, "local_run_events", `{"run_id":"run-ev","since":1}`)
	if isErr {
		t.Fatalf("events errored: %s", text)
	}
	var ev struct {
		Events []store.Event `json:"events"`
		Count  int           `json:"count"`
	}
	if err := json.Unmarshal([]byte(text), &ev); err != nil {
		t.Fatalf("decode: %v\n%s", err, text)
	}
	if ev.Count != 2 || ev.Events[0].Seq != 1 {
		t.Fatalf("since filter broken: %+v", ev)
	}

	logs := store.AsRunLogStore(st)
	if logs == nil {
		t.Fatal("filesystem store should implement RunLogStore")
	}
	if err := logs.AppendRunLog(ctx, "run-ev", 0, []byte("line1\nline2\nline3\n")); err != nil {
		t.Fatal(err)
	}
	text, isErr = call(t, s, "local_run_log", `{"run_id":"run-ev","tail":2}`)
	if isErr {
		t.Fatalf("log errored: %s", text)
	}
	if strings.Contains(text, "line1") || !strings.Contains(text, "line3") {
		t.Fatalf("tail=2 should keep the last lines only:\n%s", text)
	}

	// The report tool returns the markdown body, not the "written to"
	// confirmation line.
	text, isErr = call(t, s, "local_run_report", `{"run_id":"run-ev"}`)
	if isErr {
		t.Fatalf("report errored: %s", text)
	}
	if strings.Contains(text, "Report written to") || !strings.Contains(text, "run-ev") {
		t.Fatalf("want the markdown report body:\n%s", text)
	}
}

func TestLocalRunPreflightRejectsInvalidWorkflow(t *testing.T) {
	s := newTestServer(t)
	if err := os.WriteFile(filepath.Join(s.WorkDir, "broken.bot"), []byte("nope"), 0o644); err != nil {
		t.Fatal(err)
	}
	text, isErr := call(t, s, "local_run", `{"file_path":"broken.bot"}`)
	if !isErr {
		t.Fatalf("invalid workflow should fail the launch: %s", text)
	}
	if !strings.Contains(text, "validation failed") {
		t.Fatalf("want validation diagnostics, got: %s", text)
	}
	// No phantom run doc may exist: validation runs BEFORE CreateRun.
	st, err := s.store()
	if err != nil {
		t.Fatal(err)
	}
	ids, err := st.ListRuns(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 0 {
		t.Fatalf("a failed pre-flight left run docs behind: %v", ids)
	}
}

func TestLocalResumeGuards(t *testing.T) {
	s := newTestServer(t)
	st, err := s.store()
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := st.CreateRun(ctx, "run-done", "wf", nil); err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateRunStatus(ctx, "run-done", store.RunStatusFinished, ""); err != nil {
		t.Fatal(err)
	}
	text, isErr := call(t, s, "local_resume", `{"run_id":"run-done"}`)
	if !isErr || !strings.Contains(text, "only paused, failed_resumable or cancelled") {
		t.Fatalf("finished run must not resume: isErr=%v text=%s", isErr, text)
	}

	// A resumable run with no recorded file path needs file_path.
	if _, err := st.CreateRun(ctx, "run-res", "wf", nil); err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateRunStatus(ctx, "run-res", store.RunStatusFailedResumable, "boom"); err != nil {
		t.Fatal(err)
	}
	text, isErr = call(t, s, "local_resume", `{"run_id":"run-res"}`)
	if !isErr || !strings.Contains(text, "pass file_path explicitly") {
		t.Fatalf("want explicit file_path error: isErr=%v text=%s", isErr, text)
	}
}

func TestLocalRunCancelGuards(t *testing.T) {
	s := newTestServer(t)
	st, err := s.store()
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := st.CreateRun(ctx, "run-x", "wf", nil); err != nil {
		t.Fatal(err)
	}

	// No .pid, no live lock holder → explicit refusal, no mutation.
	text, isErr := call(t, s, "local_run_cancel", `{"run_id":"run-x"}`)
	if !isErr || !strings.Contains(text, "no live process and no .pid") {
		t.Fatalf("cancel without .pid must error explicitly: isErr=%v text=%s", isErr, text)
	}
	if r, _ := st.LoadRun(ctx, "run-x"); r.Status != store.RunStatusRunning {
		t.Fatalf("refused cancel must not mutate the run (status=%s)", r.Status)
	}

	// Paused runs are answered/resumed, not cancelled.
	if err := st.UpdateRunStatus(ctx, "run-x", store.RunStatusPausedWaitingHuman, ""); err != nil {
		t.Fatal(err)
	}
	text, isErr = call(t, s, "local_run_cancel", `{"run_id":"run-x"}`)
	if !isErr || !strings.Contains(text, "paused") {
		t.Fatalf("paused cancel should point at resume/answer: %s", text)
	}

	// Terminal runs refuse the cancel outright.
	if err := st.UpdateRunStatus(ctx, "run-x", store.RunStatusFinished, ""); err != nil {
		t.Fatal(err)
	}
	text, isErr = call(t, s, "local_run_cancel", `{"run_id":"run-x"}`)
	if !isErr || !strings.Contains(text, "already finished") {
		t.Fatalf("terminal cancel should error: %s", text)
	}
}

// TestLocalRunCancelStalePid pins the PID-recycling guard: a dead
// recorded runner with a free run lock is an explicit repair
// (failed_resumable + .pid removed), NEVER a signal.
func TestLocalRunCancelStalePid(t *testing.T) {
	s := newTestServer(t)
	st, err := s.store()
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := st.CreateRun(ctx, "run-stale", "wf", nil); err != nil {
		t.Fatal(err)
	}

	// A child that already exited gives us a genuinely dead PID.
	dead := exec.Command("true")
	if err := dead.Start(); err != nil {
		t.Fatal(err)
	}
	_ = dead.Wait()
	pidS := store.AsPIDStore(st)
	if err := pidS.WritePIDFile("run-stale", dead.Process.Pid); err != nil {
		t.Fatal(err)
	}

	text, isErr := call(t, s, "local_run_cancel", `{"run_id":"run-stale"}`)
	if isErr {
		t.Fatalf("stale-pid cancel should repair, not error: %s", text)
	}
	if !strings.Contains(text, "failed_resumable") {
		t.Fatalf("repair should be reported: %s", text)
	}
	r, err := st.LoadRun(ctx, "run-stale")
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != store.RunStatusFailedResumable {
		t.Fatalf("status = %s, want failed_resumable", r.Status)
	}
	if pid, err := pidS.ReadPIDFile("run-stale"); err != nil || pid != 0 {
		t.Fatalf("stale .pid should be removed: pid=%d err=%v", pid, err)
	}
}

// TestLocalRunCancelRefusesWhenLockFreeButPidAlive pins the ambiguous
// window (runner booting, or recycled pid): never signal.
func TestLocalRunCancelRefusesWhenLockFreeButPidAlive(t *testing.T) {
	s := newTestServer(t)
	st, err := s.store()
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := st.CreateRun(ctx, "run-amb", "wf", nil); err != nil {
		t.Fatal(err)
	}
	// A live process (this test's own) that does NOT hold the run lock
	// models the recycled-pid / startup window.
	pidS := store.AsPIDStore(st)
	if err := pidS.WritePIDFile("run-amb", os.Getpid()); err != nil {
		t.Fatal(err)
	}
	text, isErr := call(t, s, "local_run_cancel", `{"run_id":"run-amb"}`)
	if !isErr || !strings.Contains(text, "refusing to signal") {
		t.Fatalf("ambiguous state must refuse to signal: isErr=%v text=%s", isErr, text)
	}
	if r, _ := st.LoadRun(ctx, "run-amb"); r.Status != store.RunStatusRunning {
		t.Fatalf("ambiguous cancel must not mutate the run (status=%s)", r.Status)
	}
}

func TestLocalAnswerAcceptsEmptyAnswer(t *testing.T) {
	s := newTestServer(t)
	st, err := s.store()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateRun(context.Background(), "run-q", "wf", nil); err != nil {
		t.Fatal(err)
	}
	// The empty answer must pass the argument gate; the resulting error
	// (if any) must come from the interaction layer, not the gate.
	text, _ := call(t, s, "local_answer", `{"run_id":"run-q","interaction_id":"i-1","answer":""}`)
	if strings.Contains(text, "are required") {
		t.Fatalf("empty answer must not be rejected by the argument gate: %s", text)
	}
}

func TestLocalBoardTools(t *testing.T) {
	s := newTestServer(t)
	text, isErr := call(t, s, "local_board_create_issue", `{"title":"From MCP"}`)
	if isErr {
		t.Fatalf("create_issue errored: %s", text)
	}
	var issue struct {
		ID    string `json:"id"`
		Title string `json:"title"`
	}
	if err := json.Unmarshal([]byte(text), &issue); err != nil {
		t.Fatalf("decode issue: %v\n%s", err, text)
	}
	if issue.Title != "From MCP" || issue.ID == "" {
		t.Fatalf("unexpected issue: %+v", issue)
	}

	text, isErr = call(t, s, "local_board_list_issues", `{}`)
	if isErr {
		t.Fatalf("list_issues errored: %s", text)
	}
	if !strings.Contains(text, "From MCP") {
		t.Fatalf("created issue missing from list: %s", text)
	}

	// The board lives at <store-dir>/dispatcher — same root as the CLI.
	if _, err := os.Stat(filepath.Join(s.StoreDir, "dispatcher", "board.json")); err != nil {
		t.Fatalf("board store not under <store-dir>/dispatcher: %v", err)
	}
}

func TestLocalBotsListSkipsMissingPaths(t *testing.T) {
	s := newTestServer(t)
	text, isErr := call(t, s, "local_bots_list", `{}`)
	if isErr {
		t.Fatalf("bots_list errored: %s", text)
	}
	if !strings.Contains(text, "no bot paths found") {
		t.Fatalf("empty workspace should say so explicitly: %s", text)
	}

	botDir := filepath.Join(s.WorkDir, "bots")
	if err := os.MkdirAll(botDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(botDir, "probe.bot"), []byte(minimalBot), 0o644); err != nil {
		t.Fatal(err)
	}
	text, isErr = call(t, s, "local_bots_list", `{}`)
	if isErr {
		t.Fatalf("bots_list errored: %s", text)
	}
	if !strings.Contains(text, "probe") {
		t.Fatalf("bot not discovered: %s", text)
	}
}
