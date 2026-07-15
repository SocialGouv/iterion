package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

const pipelineBoardTestBot = `schema approval_result:
  approved: bool
  feedback: string

human approval:
  output: approval_result

workflow review:
  entry: approval
  approval -> done
`

type pipelineBoardTestEnv struct {
	server   *Server
	http     *httptest.Server
	board    *native.Store
	workDir  string
	storeDir string
	botPath  string
}

func newPipelineBoardTestEnv(t *testing.T) *pipelineBoardTestEnv {
	t.Helper()
	workDir := t.TempDir()
	storeDir := filepath.Join(workDir, ".iterion")
	botDir := filepath.Join(workDir, "bots")
	if err := os.MkdirAll(botDir, 0o755); err != nil {
		t.Fatal(err)
	}
	botPath := filepath.Join(botDir, "review.bot")
	if err := os.WriteFile(botPath, []byte(pipelineBoardTestBot), 0o644); err != nil {
		t.Fatal(err)
	}
	board, err := native.NewStore(filepath.Join(storeDir, "dispatcher"))
	if err != nil {
		t.Fatalf("native.NewStore: %v", err)
	}
	server := New(Config{
		WorkDir:                 workDir,
		StoreDir:                storeDir,
		NativeTrackerStore:      board,
		DisableAuth:             true,
		SkipProjectRegistration: true,
	}, iterlog.Nop())
	httpServer := httptest.NewServer(server.mux)
	t.Cleanup(func() {
		httpServer.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = server.Shutdown(ctx)
		_ = board.Close()
	})
	return &pipelineBoardTestEnv{
		server:   server,
		http:     httpServer,
		board:    board,
		workDir:  workDir,
		storeDir: storeDir,
		botPath:  botPath,
	}
}

func (e *pipelineBoardTestEnv) runStore(t *testing.T) *store.FilesystemRunStore {
	t.Helper()
	rs, err := store.New(e.storeDir)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	return rs
}

func (e *pipelineBoardTestEnv) seedRun(t *testing.T, id, workflow string, status store.RunStatus, mutate func(*store.Run)) *store.Run {
	t.Helper()
	rs := e.runStore(t)
	if _, err := rs.CreateRun(context.Background(), id, workflow, nil); err != nil {
		t.Fatalf("CreateRun(%s): %v", id, err)
	}
	run, err := rs.LoadRun(context.Background(), id)
	if err != nil {
		t.Fatalf("LoadRun(%s): %v", id, err)
	}
	run.Status = status
	if mutate != nil {
		mutate(run)
	}
	if err := rs.SaveRun(context.Background(), run); err != nil {
		t.Fatalf("SaveRun(%s): %v", id, err)
	}
	return run
}

func (e *pipelineBoardTestEnv) seedArtifact(t *testing.T, runID, nodeID string, data map[string]any) {
	t.Helper()
	rs := e.runStore(t)
	if err := rs.WriteArtifact(context.Background(), &store.Artifact{
		RunID:     runID,
		NodeID:    nodeID,
		Version:   1,
		Data:      data,
		WrittenAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("WriteArtifact(%s/%s): %v", runID, nodeID, err)
	}
}

func (e *pipelineBoardTestEnv) seedNodeStarted(t *testing.T, runID string, nodeIDs ...string) {
	t.Helper()
	rs := e.runStore(t)
	for _, nodeID := range nodeIDs {
		if _, err := rs.AppendEvent(context.Background(), runID, store.Event{
			Type:      store.EventNodeStarted,
			RunID:     runID,
			NodeID:    nodeID,
			Timestamp: time.Now().UTC(),
		}); err != nil {
			t.Fatalf("AppendEvent(%s/%s): %v", runID, nodeID, err)
		}
	}
}

func (e *pipelineBoardTestEnv) projection(t *testing.T) PipelineBoardResponse {
	t.Helper()
	resp, err := http.Get(e.http.URL + "/api/v1/pipeline-board")
	if err != nil {
		t.Fatalf("GET projection: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		var body any
		_ = json.NewDecoder(resp.Body).Decode(&body)
		t.Fatalf("GET projection: status = %d, body=%v", resp.StatusCode, body)
	}
	var projection PipelineBoardResponse
	decodeJSONResp(t, resp, &projection)
	return projection
}

// The five fixed lanes, in order.
func TestPipelineBoardHasFourFixedColumns(t *testing.T) {
	env := newPipelineBoardTestEnv(t)
	projection := env.projection(t)
	want := []string{pipelineColumnBacklog, pipelineColumnTodo, pipelineColumnInProgress, pipelineColumnDone, pipelineColumnFailed}
	if len(projection.Columns) != len(want) {
		t.Fatalf("columns = %+v, want %v", projection.Columns, want)
	}
	for i, id := range want {
		if projection.Columns[i].ID != id {
			t.Errorf("column[%d] = %q, want %q", i, projection.Columns[i].ID, id)
		}
	}
}

// A root's whole descendant tree is folded into ONE card: the child does
// not get its own card; its pending review surfaces in the root's
// PendingReviews. Global — roots of every bot appear on the one board.
func TestPipelineBoardFoldsDescendantsAndCollectsReviews(t *testing.T) {
	env := newPipelineBoardTestEnv(t)
	issue, err := env.board.Create(native.Issue{
		Title:    "Review the release",
		Body:     "Check the candidate before shipping.",
		State:    native.StateInProgress,
		Bot:      "review",
		Labels:   []string{"release"},
		Priority: 2,
	})
	if err != nil {
		t.Fatalf("Create issue: %v", err)
	}

	env.seedRun(t, "run-root", "review", store.RunStatusRunning, func(run *store.Run) {
		run.FilePath = env.botPath
		run.Name = "Release review"
	})
	env.seedRun(t, "run-child", "child_workflow", store.RunStatusPausedWaitingHuman, func(run *store.Run) {
		run.ParentRunID = "run-root"
		run.Name = "Child approval"
		run.Checkpoint = &store.Checkpoint{
			NodeID:               "child_approval",
			InteractionID:        "int-child",
			InteractionQuestions: map[string]any{"approved": "Ship it?"},
		}
	})
	if err := env.board.SetLastRun(issue.ID, "run-root", ""); err != nil {
		t.Fatalf("SetLastRun: %v", err)
	}

	// A standalone root of a DIFFERENT bot must also appear (global board).
	env.seedRun(t, "run-other", "other_workflow", store.RunStatusFinished, func(run *store.Run) {
		run.BotID = "other"
	})

	projection := env.projection(t)

	if projection.TopologyError != "" {
		t.Fatalf("topology error = %q", projection.TopologyError)
	}
	if hasPipelineCard(projection.Cards, "run:run-child") {
		t.Fatalf("child run must be folded into its root, not a separate card: %+v", projection.Cards)
	}
	root := findPipelineCard(t, projection.Cards, "run:run-root")
	if root.ColumnID != pipelineColumnInProgress {
		t.Errorf("root column = %q, want in_progress", root.ColumnID)
	}
	if root.IssueID != issue.ID || root.Title != issue.Title {
		t.Errorf("root issue association = %+v", root)
	}
	if root.DescendantCount != 1 {
		t.Errorf("descendant_count = %d, want 1", root.DescendantCount)
	}
	if len(root.PendingReviews) != 1 {
		t.Fatalf("pending_reviews = %+v, want the child's gate", root.PendingReviews)
	}
	pr := root.PendingReviews[0]
	if pr.RunID != "run-child" || pr.NodeID != "child_approval" || pr.Depth != 1 {
		t.Errorf("pending review = %+v", pr)
	}
	if pr.InteractionID != "int-child" || pr.Questions["approved"] != "Ship it?" {
		t.Errorf("pending review interaction = %+v", pr)
	}
	// The whole tree's run ids surface so the studio can aggregate a sub-bot's
	// produced elements onto the root card (root first, then descendants).
	if len(root.TreeRunIDs) != 2 || root.TreeRunIDs[0] != "run-root" || root.TreeRunIDs[1] != "run-child" {
		t.Errorf("tree_run_ids = %v, want [run-root run-child]", root.TreeRunIDs)
	}
	if !hasPipelineCard(projection.Cards, "run:run-other") {
		t.Errorf("standalone root of another bot missing (board is global): %+v", projection.Cards)
	}
}

// Root status maps to the five lanes; queued is TODO (waiting for a slot).
func TestPipelineBoardColumnBucketing(t *testing.T) {
	env := newPipelineBoardTestEnv(t)
	cases := []struct {
		id     string
		status store.RunStatus
		column string
	}{
		{"r-running", store.RunStatusRunning, pipelineColumnInProgress},
		{"r-paused", store.RunStatusPausedWaitingHuman, pipelineColumnInProgress},
		{"r-finished", store.RunStatusFinished, pipelineColumnDone},
		{"r-failed", store.RunStatusFailed, pipelineColumnFailed},
		{"r-resumable", store.RunStatusFailedResumable, pipelineColumnFailed},
		{"r-cancelled", store.RunStatusCancelled, pipelineColumnFailed},
		// An operator soft-pause is resumable mid-flight state, NOT a
		// failure — it must never offer Retry-from-zero (L1, PR review).
		{"r-operator", store.RunStatusPausedOperator, pipelineColumnInProgress},
	}
	for _, c := range cases {
		env.seedRun(t, c.id, "review", c.status, func(run *store.Run) {
			run.FilePath = env.botPath
			// r-paused carries a checkpoint so it is a genuine human gate,
			// not routed to attention.
			if c.status == store.RunStatusPausedWaitingHuman {
				run.Checkpoint = &store.Checkpoint{NodeID: "approval", InteractionID: "int-x"}
			}
		})
	}
	// A queued root waiting for a concurrency slot lands in TODO with a position.
	env.seedRun(t, "r-queued", "review", store.RunStatusQueued, func(run *store.Run) {
		run.FilePath = env.botPath
		now := time.Now().UTC()
		run.QueuedAt = &now
	})

	projection := env.projection(t)
	for _, c := range cases {
		card := findPipelineCard(t, projection.Cards, "run:"+c.id)
		if card.ColumnID != c.column {
			t.Errorf("%s column = %q, want %q", c.id, card.ColumnID, c.column)
		}
		// Failed runs land in the FAILED lane and must carry the Failed flag
		// so the UI offers Retry and shows the error as the reason.
		if c.column == pipelineColumnFailed && !card.Failed {
			t.Errorf("%s in Failed must have Failed=true", c.id)
		}
	}
	queued := findPipelineCard(t, projection.Cards, "run:r-queued")
	if queued.ColumnID != pipelineColumnTodo {
		t.Errorf("queued column = %q, want todo", queued.ColumnID)
	}
	if queued.QueuePosition != 1 {
		t.Errorf("queued position = %d, want 1", queued.QueuePosition)
	}
}

// Progress = distinct node_started / total nodes; finished clamps to 100%;
// DONE surfaces the final_answer artifact.
func TestPipelineBoardProgressAndOutput(t *testing.T) {
	env := newPipelineBoardTestEnv(t)

	// A running root that has entered 1 of the workflow's 3 nodes.
	env.seedRun(t, "run-progress", "review", store.RunStatusRunning, func(run *store.Run) {
		run.FilePath = env.botPath
	})
	env.seedNodeStarted(t, "run-progress", "approval")

	// A finished root with a final_answer artifact.
	env.seedRun(t, "run-output", "review", store.RunStatusFinished, func(run *store.Run) {
		run.FilePath = env.botPath
		run.ArtifactIndex = map[string]int{"summary": 1}
	})
	env.seedArtifact(t, "run-output", "summary", map[string]any{"final_answer": "Shipped v2 cleanly."})

	projection := env.projection(t)

	prog := findPipelineCard(t, projection.Cards, "run:run-progress")
	if prog.TotalNodes != 3 {
		t.Errorf("progress total_nodes = %d, want 3", prog.TotalNodes)
	}
	if prog.ExecutedNodes != 1 {
		t.Errorf("progress executed_nodes = %d, want 1", prog.ExecutedNodes)
	}
	if prog.TreeTotalNodes != 3 || prog.TreeExecutedNodes != 1 {
		t.Errorf("progress tree = %d/%d, want 1/3", prog.TreeExecutedNodes, prog.TreeTotalNodes)
	}

	out := findPipelineCard(t, projection.Cards, "run:run-output")
	if out.ColumnID != pipelineColumnDone {
		t.Errorf("finished column = %q, want done", out.ColumnID)
	}
	if out.ExecutedNodes != out.TotalNodes || out.TotalNodes != 3 {
		t.Errorf("finished progress = %d/%d, want 3/3 (100%% clamp)", out.ExecutedNodes, out.TotalNodes)
	}
	if out.Output != "Shipped v2 cleanly." {
		t.Errorf("output = %q, want the final_answer", out.Output)
	}
}

func TestPipelineBoardTaskCreatePinsBotFromBody(t *testing.T) {
	env := newPipelineBoardTestEnv(t)
	post := func(body string) *http.Response {
		t.Helper()
		resp, err := http.Post(env.http.URL+"/api/v1/pipeline-board/tasks", "application/json", bytes.NewBufferString(body))
		if err != nil {
			t.Fatalf("POST task: %v", err)
		}
		return resp
	}

	resp := post(`{"bot":"review","title":"Todo task","labels":["demo"],"bot_args":{"scope":"api"}}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("todo status = %d", resp.StatusCode)
	}
	var todo native.Issue
	decodeJSONResp(t, resp, &todo)
	if todo.Bot != "review" || todo.State != native.StateInbox || todo.BotArgs["scope"] != "api" {
		t.Errorf("todo = %+v", todo)
	}

	resp = post(`{"bot":"review","title":"Start task","start":true}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("start status = %d", resp.StatusCode)
	}
	var started native.Issue
	decodeJSONResp(t, resp, &started)
	if started.Bot != "review" || started.State != native.StateReady {
		t.Errorf("started = %+v, want bot=review state=ready", started)
	}

	if resp := post(`{"bot":"review","title":""}`); resp.StatusCode != http.StatusBadRequest {
		resp.Body.Close()
		t.Errorf("empty title status = %d, want 400", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
	if resp := post(`{"title":"No bot"}`); resp.StatusCode != http.StatusBadRequest {
		resp.Body.Close()
		t.Errorf("missing bot status = %d, want 400", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
	if resp := post(`{"bot":"ghost","title":"Unknown bot"}`); resp.StatusCode != http.StatusNotFound {
		resp.Body.Close()
		t.Errorf("unknown bot status = %d, want 404", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
}

func TestPipelineBoardTaskCreateEnsuresUniqueTitle(t *testing.T) {
	env := newPipelineBoardTestEnv(t)
	create := func(title string) string {
		t.Helper()
		resp, err := http.Post(env.http.URL+"/api/v1/pipeline-board/tasks", "application/json",
			bytes.NewBufferString(`{"bot":"review","title":"`+title+`"}`))
		if err != nil {
			t.Fatalf("POST: %v", err)
		}
		if resp.StatusCode != http.StatusCreated {
			resp.Body.Close()
			t.Fatalf("status = %d", resp.StatusCode)
		}
		var out native.Issue
		decodeJSONResp(t, resp, &out)
		return out.Title
	}
	if got := create("Episode"); got != "Episode" {
		t.Errorf("first = %q, want %q", got, "Episode")
	}
	if got := create("Episode"); got != "#2 - Episode" {
		t.Errorf("second = %q, want %q", got, "#2 - Episode")
	}
	if got := create("Episode"); got != "#3 - Episode" {
		t.Errorf("third = %q, want %q", got, "#3 - Episode")
	}
	// A distinct title is untouched.
	if got := create("Other"); got != "Other" {
		t.Errorf("distinct = %q, want %q", got, "Other")
	}
}

func TestPipelineBoardTaskCreateRejectsCrossOrigin(t *testing.T) {
	env := newPipelineBoardTestEnv(t)
	req, err := http.NewRequest(
		http.MethodPost,
		env.http.URL+"/api/v1/pipeline-board/tasks",
		bytes.NewBufferString(`{"bot":"review","title":"Must not be created"}`),
	)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "https://evil.example.com")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST task: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-origin POST status = %d, want 403", resp.StatusCode)
	}
	issues, err := env.board.List(native.ListFilter{})
	if err != nil {
		t.Fatalf("List issues: %v", err)
	}
	if len(issues) != 0 {
		t.Fatalf("rejected cross-origin POST created issues: %+v", issues)
	}
}

func TestPipelineBoardUsesLastRunAsCurrentAttemptWithoutDuplicatingHistory(t *testing.T) {
	env := newPipelineBoardTestEnv(t)
	issue, err := env.board.Create(native.Issue{
		Title: "Retry review",
		State: native.StateInProgress,
		Bot:   "review",
	})
	if err != nil {
		t.Fatalf("Create issue: %v", err)
	}

	base := time.Date(2026, time.July, 14, 10, 0, 0, 0, time.UTC)
	env.seedRun(t, "run-old", "review", store.RunStatusFinished, func(run *store.Run) {
		run.FilePath = env.botPath
		run.CreatedAt = base.Add(time.Hour)
		run.UpdatedAt = run.CreatedAt
	})
	env.seedRun(t, "run-current", "review", store.RunStatusRunning, func(run *store.Run) {
		run.FilePath = env.botPath
		run.CreatedAt = base
		run.UpdatedAt = run.CreatedAt
	})
	if err := env.board.SetLastRun(issue.ID, "run-old", ""); err != nil {
		t.Fatalf("SetLastRun(old): %v", err)
	}
	if err := env.board.SetLastRun(issue.ID, "run-current", ""); err != nil {
		t.Fatalf("SetLastRun(current): %v", err)
	}

	projection := env.projection(t)
	current := findPipelineCard(t, projection.Cards, "run:run-current")
	if current.IssueID != issue.ID {
		t.Errorf("current issue = %q, want %q", current.IssueID, issue.ID)
	}
	if len(current.Attempts) != 2 || current.Attempts[0].RunID != "run-old" || current.Attempts[1].RunID != "run-current" {
		t.Errorf("attempts = %+v, want old then current", current.Attempts)
	}
	if hasPipelineCard(projection.Cards, "run:run-old") {
		t.Error("old issue-owned attempt must not also be projected as a standalone card")
	}
}

func TestPipelineBoardAssociatesInflightSourceAndStandaloneRun(t *testing.T) {
	env := newPipelineBoardTestEnv(t)
	issue, err := env.board.Create(native.Issue{Title: "In flight", Bot: "review"})
	if err != nil {
		t.Fatal(err)
	}
	env.seedRun(t, "run-inflight", "review", store.RunStatusRunning, func(run *store.Run) {
		run.FilePath = env.botPath
		run.Source = &store.RunSource{Kind: store.RunSourceKindDispatcher, IssueID: issue.ID}
	})
	env.seedRun(t, "run-manual", "review", store.RunStatusFinished, func(run *store.Run) {
		run.FilePath = env.botPath
	})

	projection := env.projection(t)
	inflight := findPipelineCard(t, projection.Cards, "run:run-inflight")
	if inflight.IssueID != issue.ID || inflight.Title != issue.Title {
		t.Errorf("inflight source association = %+v", inflight)
	}
	if findPipelineCard(t, projection.Cards, "run:run-manual").ColumnID != pipelineColumnDone {
		t.Error("manual finished run should be projected in Done")
	}
	for _, card := range projection.Cards {
		if card.Kind == "task" {
			t.Errorf("in-flight issue was duplicated as a task: %+v", card)
		}
	}
}

// A child whose parent belongs to another bot folds into that parent; it is
// never promoted to its own root card.
func TestPipelineBoardFoldsChildOfAnotherBot(t *testing.T) {
	env := newPipelineBoardTestEnv(t)
	env.seedRun(t, "run-other-parent", "other_workflow", store.RunStatusRunning, func(run *store.Run) {
		run.BotID = "other"
	})
	env.seedRun(t, "run-review-child", "review", store.RunStatusRunning, func(run *store.Run) {
		run.BotID = "review"
		run.ParentRunID = "run-other-parent"
	})

	projection := env.projection(t)
	if hasPipelineCard(projection.Cards, "run:run-review-child") {
		t.Fatalf("child must fold into its parent, not become a root: %+v", projection.Cards)
	}
	parent := findPipelineCard(t, projection.Cards, "run:run-other-parent")
	if parent.DescendantCount != 1 {
		t.Errorf("parent descendant_count = %d, want 1", parent.DescendantCount)
	}
}

// A not-yet-ready native task (non-eligible state) is a Draft card; an
// eligible (ready) one is a Todo card the launch loop will start.
func TestPipelineBoardProjectsTaskDraftVsTodo(t *testing.T) {
	env := newPipelineBoardTestEnv(t)
	draft, err := env.board.Create(native.Issue{
		Title:   "Being prepared",
		State:   native.StateInbox, // non-eligible → Draft
		Bot:     "review",
		BotArgs: map[string]string{"scope": "api"},
	})
	if err != nil {
		t.Fatalf("Create draft issue: %v", err)
	}
	ready, err := env.board.Create(native.Issue{
		Title: "Ready to run",
		State: native.StateReady, // eligible → Todo
		Bot:   "review",
	})
	if err != nil {
		t.Fatalf("Create ready issue: %v", err)
	}

	projection := env.projection(t)
	d := findPipelineCard(t, projection.Cards, "task:"+draft.ID)
	if d.Kind != "task" || d.ColumnID != pipelineColumnBacklog || d.Ready {
		t.Errorf("draft card = %+v, want kind=task column=draft ready=false", d)
	}
	if d.EntryInput["scope"] != "api" {
		t.Errorf("draft entry_input = %+v, want bot_args", d.EntryInput)
	}
	r := findPipelineCard(t, projection.Cards, "task:"+ready.ID)
	if r.ColumnID != pipelineColumnTodo || !r.Ready {
		t.Errorf("ready card = %+v, want column=todo ready=true", r)
	}
}

func TestPipelineBoardTaskReadyTogglesState(t *testing.T) {
	env := newPipelineBoardTestEnv(t)
	issue, err := env.board.Create(native.Issue{Title: "Prep me", State: native.StateInbox, Bot: "review"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	post := func(ready bool) native.Issue {
		t.Helper()
		body := `{"ready":false}`
		if ready {
			body = `{"ready":true}`
		}
		resp, err := http.Post(env.http.URL+"/api/v1/pipeline-board/tasks/"+issue.ID+"/ready", "application/json", bytes.NewBufferString(body))
		if err != nil {
			t.Fatalf("POST ready: %v", err)
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			t.Fatalf("ready status = %d", resp.StatusCode)
		}
		var out native.Issue
		decodeJSONResp(t, resp, &out)
		return out
	}
	if got := post(true); got.State != native.StateReady {
		t.Errorf("ready=true state = %q, want %q", got.State, native.StateReady)
	}
	// On the board the readied ticket is now in Todo.
	if card := findPipelineCard(t, env.projection(t).Cards, "task:"+issue.ID); card.ColumnID != pipelineColumnTodo || !card.Ready {
		t.Errorf("after ready: card = %+v, want column=todo ready=true", card)
	}
	if got := post(false); got.State != native.StateInbox {
		t.Errorf("ready=false state = %q, want %q", got.State, native.StateInbox)
	}
	if card := findPipelineCard(t, env.projection(t).Cards, "task:"+issue.ID); card.ColumnID != pipelineColumnBacklog {
		t.Errorf("after unready: card column = %q, want draft", card.ColumnID)
	}
}

func TestPipelineBoardTaskUpdateEditsDraft(t *testing.T) {
	env := newPipelineBoardTestEnv(t)
	issue, err := env.board.Create(native.Issue{
		Title:   "Rough draft",
		State:   native.StateInbox,
		Bot:     "review",
		BotArgs: map[string]string{"scope": "old"},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	body := `{"title":"Polished","bot_args":{"scope":"new","extra":"1"},"priority":3}`
	req, _ := http.NewRequest(http.MethodPatch, env.http.URL+"/api/v1/pipeline-board/tasks/"+issue.ID, bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PATCH: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("update status = %d", resp.StatusCode)
	}
	var out native.Issue
	decodeJSONResp(t, resp, &out)
	if out.Title != "Polished" || out.Priority != 3 || out.BotArgs["scope"] != "new" || out.BotArgs["extra"] != "1" {
		t.Errorf("updated issue = %+v", out)
	}
	// The edit shows on the board's Draft card.
	card := findPipelineCard(t, env.projection(t).Cards, "task:"+issue.ID)
	if card.Title != "Polished" || card.ColumnID != pipelineColumnBacklog || card.EntryInput["scope"] != "new" {
		t.Errorf("edited draft card = %+v", card)
	}

	// Empty title is rejected.
	req2, _ := http.NewRequest(http.MethodPatch, env.http.URL+"/api/v1/pipeline-board/tasks/"+issue.ID, bytes.NewBufferString(`{"title":"  "}`))
	req2.Header.Set("Content-Type", "application/json")
	resp2, _ := http.DefaultClient.Do(req2)
	if resp2.StatusCode != http.StatusBadRequest {
		resp2.Body.Close()
		t.Errorf("empty title update status = %d, want 400", resp2.StatusCode)
	} else {
		resp2.Body.Close()
	}
}

func TestPipelineTicketLaunchable(t *testing.T) {
	env := newPipelineBoardTestEnv(t)
	rs := env.runStore(t)
	ctx := context.Background()

	// No run yet → launchable.
	if !pipelineTicketLaunchable(ctx, rs, &native.Issue{}) {
		t.Error("ticket with no run must be launchable")
	}
	seed := func(id string, status store.RunStatus) *native.Issue {
		env.seedRun(t, id, "review", status, nil)
		return &native.Issue{LastRunID: id}
	}
	for _, tc := range []struct {
		status store.RunStatus
		want   bool
	}{
		{store.RunStatusRunning, false},
		{store.RunStatusPausedWaitingHuman, false},
		{store.RunStatusQueued, false},
		{store.RunStatusFinished, false}, // success is not retried
		{store.RunStatusFailed, true},    // failure is retry-able
		{store.RunStatusCancelled, true},
	} {
		iss := seed("run-"+string(tc.status), tc.status)
		if got := pipelineTicketLaunchable(ctx, rs, iss); got != tc.want {
			t.Errorf("launchable(%s) = %v, want %v", tc.status, got, tc.want)
		}
	}
}

func TestPipelineBoardCloudStoreIsResolvedFromActiveTeam(t *testing.T) {
	local, err := native.NewStore(filepath.Join(t.TempDir(), "local"))
	if err != nil {
		t.Fatalf("local store: %v", err)
	}
	t.Cleanup(func() { _ = local.Close() })
	teamBoard, err := native.NewStore(filepath.Join(t.TempDir(), "team-b"))
	if err != nil {
		t.Fatalf("team store: %v", err)
	}
	t.Cleanup(func() { _ = teamBoard.Close() })

	var resolvedTeam string
	server := &Server{cfg: Config{
		Mode:               "cloud",
		NativeTrackerStore: local,
		CloudBoardFor: func(teamID string) native.BoardStore {
			resolvedTeam = teamID
			if teamID == "team-b" {
				return teamBoard
			}
			return nil
		},
	}}
	request := httptest.NewRequest(http.MethodGet, "/api/v1/pipeline-board", nil)
	request = request.WithContext(auth.WithIdentity(request.Context(), auth.Identity{TeamID: "team-b"}))
	got, err := server.resolvePipelineBoardStore(request)
	if err != nil {
		t.Fatalf("resolve cloud board: %v", err)
	}
	if got != teamBoard || resolvedTeam != "team-b" {
		t.Fatalf("resolved store/team = %p/%q, want %p/team-b", got, resolvedTeam, teamBoard)
	}

	// Even when a local store is present on a cloud-configured process, an
	// identity without an active team must fail closed instead of reading it.
	request = httptest.NewRequest(http.MethodGet, "/api/v1/pipeline-board", nil)
	got, err = server.resolvePipelineBoardStore(request)
	if err != nil {
		t.Fatalf("resolve board without team: %v", err)
	}
	if got != nil {
		t.Fatalf("store without active team = %p, want nil", got)
	}
}

func findPipelineCard(t *testing.T, cards []PipelineBoardCard, id string) PipelineBoardCard {
	t.Helper()
	for _, card := range cards {
		if card.ID == id {
			return card
		}
	}
	t.Fatalf("card %q not found in %+v", id, cards)
	return PipelineBoardCard{}
}

func hasPipelineCard(cards []PipelineBoardCard, id string) bool {
	for _, card := range cards {
		if card.ID == id {
			return true
		}
	}
	return false
}
