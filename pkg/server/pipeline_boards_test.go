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

func (e *pipelineBoardTestEnv) seedRun(t *testing.T, id, workflow string, status store.RunStatus, mutate func(*store.Run)) *store.Run {
	t.Helper()
	runStore, err := store.New(e.storeDir)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	if _, err := runStore.CreateRun(context.Background(), id, workflow, nil); err != nil {
		t.Fatalf("CreateRun(%s): %v", id, err)
	}
	run, err := runStore.LoadRun(context.Background(), id)
	if err != nil {
		t.Fatalf("LoadRun(%s): %v", id, err)
	}
	run.Status = status
	if mutate != nil {
		mutate(run)
	}
	if err := runStore.SaveRun(context.Background(), run); err != nil {
		t.Fatalf("SaveRun(%s): %v", id, err)
	}
	return run
}

func (e *pipelineBoardTestEnv) projection(t *testing.T) PipelineBoardResponse {
	t.Helper()
	resp, err := http.Get(e.http.URL + "/api/v1/pipeline-boards/review")
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

func TestPipelineBoardProjectsTaskRunTreeAndInteractions(t *testing.T) {
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
	if _, err := env.board.Create(native.Issue{Title: "Other bot", Bot: "other"}); err != nil {
		t.Fatalf("Create other issue: %v", err)
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
			Outputs:              map[string]map[string]any{},
			LoopCounters:         map[string]int{},
			ArtifactVersions:     map[string]int{},
			Vars:                 map[string]any{},
		}
	})
	if err := env.board.SetLastRun(issue.ID, "run-root", ""); err != nil {
		t.Fatalf("SetLastRun: %v", err)
	}

	resp, err := http.Get(env.http.URL + "/api/v1/pipeline-boards/review")
	if err != nil {
		t.Fatalf("GET projection: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		var body any
		_ = json.NewDecoder(resp.Body).Decode(&body)
		t.Fatalf("status = %d, body=%v", resp.StatusCode, body)
	}
	var projection PipelineBoardResponse
	decodeJSONResp(t, resp, &projection)

	if projection.Board.ID != "bot:review" || projection.Board.BotID != "review" {
		t.Fatalf("identity = %+v", projection.Board)
	}
	if projection.TopologyError != "" {
		t.Fatalf("topology error = %q", projection.TopologyError)
	}
	if len(projection.Cards) != 2 {
		t.Fatalf("cards = %+v, want root + child only", projection.Cards)
	}
	root := findPipelineCard(t, projection.Cards, "run:run-root")
	if root.ColumnID != pipelineColumnRunning || root.IssueID != issue.ID || root.Title != issue.Title {
		t.Errorf("root = %+v", root)
	}
	if len(root.Attempts) != 1 || root.Attempts[0].RunID != "run-root" {
		t.Errorf("root attempts = %+v", root.Attempts)
	}
	child := findPipelineCard(t, projection.Cards, "run:run-child")
	if child.Depth != 1 || child.ParentRunID != "run-root" || child.RootRunID != "run-root" {
		t.Errorf("child lineage = %+v", child)
	}
	if child.ColumnID != pipelineInteractionColumnID("child_workflow", "child_approval") {
		t.Errorf("child column = %q", child.ColumnID)
	}
	if child.InteractionID != "int-child" || child.Questions["approved"] != "Ship it?" {
		t.Errorf("child interaction = %+v", child)
	}

	staticID := pipelineInteractionColumnID("review", "approval")
	if column := findPipelineColumn(t, projection.Columns, staticID); column.Title != "Approval" || column.InteractionMode != "human" {
		t.Errorf("static interaction = %+v", column)
	}
	dynamicID := pipelineInteractionColumnID("child_workflow", "child_approval")
	dynamicIndex := pipelineColumnIndex(projection.Columns, dynamicID)
	fallbackIndex := pipelineColumnIndex(projection.Columns, pipelineColumnOtherInput)
	if dynamicIndex < 0 || fallbackIndex < 0 || dynamicIndex >= fallbackIndex {
		t.Errorf("dynamic/fallback order = %d/%d; columns=%+v", dynamicIndex, fallbackIndex, projection.Columns)
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

	resp, err := http.Get(env.http.URL + "/api/v1/pipeline-boards/review")
	if err != nil {
		t.Fatal(err)
	}
	var projection PipelineBoardResponse
	decodeJSONResp(t, resp, &projection)
	if len(projection.Cards) != 2 {
		t.Fatalf("cards = %+v, want associated inflight + standalone", projection.Cards)
	}
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

func TestPipelineBoardTaskCreatePinsBotAndSelectsAdmissionState(t *testing.T) {
	env := newPipelineBoardTestEnv(t)
	post := func(body string) *http.Response {
		t.Helper()
		resp, err := http.Post(env.http.URL+"/api/v1/pipeline-boards/review/tasks", "application/json", bytes.NewBufferString(body))
		if err != nil {
			t.Fatalf("POST task: %v", err)
		}
		return resp
	}

	resp := post(`{"title":"Todo task","labels":["demo"],"bot_args":{"scope":"api"}}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("todo status = %d", resp.StatusCode)
	}
	var todo native.Issue
	decodeJSONResp(t, resp, &todo)
	if todo.Bot != "review" || todo.State != native.StateInbox || todo.BotArgs["scope"] != "api" {
		t.Errorf("todo = %+v", todo)
	}

	resp = post(`{"title":"Start task","start":true}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("start status = %d", resp.StatusCode)
	}
	var started native.Issue
	decodeJSONResp(t, resp, &started)
	if started.Bot != "review" || started.State != native.StateReady {
		t.Errorf("started = %+v, want bot=review state=ready", started)
	}

	resp = post(`{"title":""}`)
	if resp.StatusCode != http.StatusBadRequest {
		resp.Body.Close()
		t.Errorf("empty title status = %d, want 400", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
}

func TestPipelineBoardsListAndUnknownBot(t *testing.T) {
	env := newPipelineBoardTestEnv(t)
	resp, err := http.Get(env.http.URL + "/api/v1/pipeline-boards")
	if err != nil {
		t.Fatal(err)
	}
	var list pipelineBoardListResponse
	decodeJSONResp(t, resp, &list)
	if len(list.Boards) != 1 || list.Boards[0].BotID != "review" {
		t.Fatalf("boards = %+v", list.Boards)
	}

	resp, err = http.Get(env.http.URL + "/api/v1/pipeline-boards/missing")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown bot status = %d, want 404", resp.StatusCode)
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
		// The old attempt is deliberately newer by CreatedAt: LastRunID, not
		// run timestamps, is the authoritative current-attempt pointer.
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
	if len(projection.Cards) != 1 {
		t.Fatalf("cards = %+v, want the current attempt only", projection.Cards)
	}
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

func TestPipelineBoardRoutesUnknownRootAndChildInteractionsDifferently(t *testing.T) {
	env := newPipelineBoardTestEnv(t)
	env.seedRun(t, "run-unknown-root", "review", store.RunStatusPausedWaitingHuman, func(run *store.Run) {
		run.FilePath = env.botPath
		run.Checkpoint = &store.Checkpoint{
			NodeID:               "unlisted_root_gate",
			InteractionID:        "int-root",
			InteractionQuestions: map[string]any{"answer": "Root answer?"},
		}
	})
	env.seedRun(t, "run-unknown-child", "child_workflow", store.RunStatusPausedWaitingHuman, func(run *store.Run) {
		run.ParentRunID = "run-unknown-root"
		run.Checkpoint = &store.Checkpoint{
			NodeID:               "unlisted_child_gate",
			InteractionID:        "int-child",
			InteractionQuestions: map[string]any{"answer": "Child answer?"},
		}
	})

	projection := env.projection(t)
	root := findPipelineCard(t, projection.Cards, "run:run-unknown-root")
	if root.ColumnID != pipelineColumnOtherInput {
		t.Errorf("unknown root column = %q, want %q", root.ColumnID, pipelineColumnOtherInput)
	}
	rootDynamicID := pipelineInteractionColumnID("review", "unlisted_root_gate")
	if pipelineColumnIndex(projection.Columns, rootDynamicID) >= 0 {
		t.Errorf("unknown root unexpectedly created dynamic column %q", rootDynamicID)
	}

	child := findPipelineCard(t, projection.Cards, "run:run-unknown-child")
	childDynamicID := pipelineInteractionColumnID("child_workflow", "unlisted_child_gate")
	if child.Depth != 1 || child.ColumnID != childDynamicID {
		t.Errorf("unknown child = %+v, want depth=1 column=%q", child, childDynamicID)
	}
	if pipelineColumnIndex(projection.Columns, childDynamicID) < 0 {
		t.Errorf("unknown child dynamic column %q missing from %+v", childDynamicID, projection.Columns)
	}
}

func TestPipelineBoardDoesNotPromoteMatchingChildOfAnotherBot(t *testing.T) {
	env := newPipelineBoardTestEnv(t)
	env.seedRun(t, "run-other-parent", "other_workflow", store.RunStatusRunning, func(run *store.Run) {
		run.BotID = "other"
	})
	env.seedRun(t, "run-review-child", "review", store.RunStatusRunning, func(run *store.Run) {
		run.BotID = "review"
		run.ParentRunID = "run-other-parent"
	})

	projection := env.projection(t)
	if len(projection.Cards) != 0 {
		t.Fatalf("cards = %+v, want no review root for a child whose existing parent belongs to another bot", projection.Cards)
	}
}

func TestPipelineBoardInteractionColumnIDsPreservePunctuation(t *testing.T) {
	withHyphen := pipelineInteractionColumnID("a-b", "gate")
	withoutHyphen := pipelineInteractionColumnID("ab", "gate")
	if withHyphen == withoutHyphen {
		t.Fatalf("column ID collision: a-b and ab both produced %q", withHyphen)
	}
}

func TestPipelineBoardDoesNotAssociateLooseRunByWorkflowNameAlone(t *testing.T) {
	env := newPipelineBoardTestEnv(t)
	env.seedRun(t, "run-workflow-name-only", "review", store.RunStatusFinished, nil)

	projection := env.projection(t)
	if len(projection.Cards) != 0 {
		t.Fatalf("cards = %+v, want no association based only on WorkflowName", projection.Cards)
	}
}

func TestPipelineBoardTaskCreateRejectsCrossOrigin(t *testing.T) {
	env := newPipelineBoardTestEnv(t)
	req, err := http.NewRequest(
		http.MethodPost,
		env.http.URL+"/api/v1/pipeline-boards/review/tasks",
		bytes.NewBufferString(`{"title":"Must not be created"}`),
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
	request := httptest.NewRequest(http.MethodGet, "/api/v1/pipeline-boards/review", nil)
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
	request = httptest.NewRequest(http.MethodGet, "/api/v1/pipeline-boards/review", nil)
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

func findPipelineColumn(t *testing.T, columns []PipelineBoardColumn, id string) PipelineBoardColumn {
	t.Helper()
	for _, column := range columns {
		if column.ID == id {
			return column
		}
	}
	t.Fatalf("column %q not found in %+v", id, columns)
	return PipelineBoardColumn{}
}

func pipelineColumnIndex(columns []PipelineBoardColumn, id string) int {
	for index, column := range columns {
		if column.ID == id {
			return index
		}
	}
	return -1
}
