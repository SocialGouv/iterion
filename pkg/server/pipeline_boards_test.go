package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
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
	return e.projectionQuery(t, "")
}

// projectionQuery fetches the board with an optional raw query string
// (e.g. "since=1h"). It fails the test unless the response is 200.
func (e *pipelineBoardTestEnv) projectionQuery(t *testing.T, query string) PipelineBoardResponse {
	t.Helper()
	url := e.http.URL + "/api/v1/pipeline-board"
	if query != "" {
		url += "?" + query
	}
	resp, err := http.Get(url)
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

// The four fixed lanes, in order. Order is part of the wire contract: the
// studio renders needs_attention to the RIGHT of in_progress.
func TestPipelineBoardHasFourFixedColumns(t *testing.T) {
	env := newPipelineBoardTestEnv(t)
	projection := env.projection(t)
	want := []struct{ id, title string }{
		{pipelineColumnOpened, "Opened"},
		{pipelineColumnInProgress, "In progress"},
		{pipelineColumnNeedsAttention, "Needs attention"},
		{pipelineColumnClosed, "Closed"},
	}
	if len(projection.Columns) != len(want) {
		t.Fatalf("columns = %+v, want %v", projection.Columns, want)
	}
	for i, w := range want {
		if projection.Columns[i].ID != w.id {
			t.Errorf("column[%d] id = %q, want %q", i, projection.Columns[i].ID, w.id)
		}
		if projection.Columns[i].Title != w.title {
			t.Errorf("column[%d] title = %q, want %q", i, projection.Columns[i].Title, w.title)
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

func TestPipelineBoardPendingReviewsOldestUpdateFirstAndRepauseMovesToBack(t *testing.T) {
	env := newPipelineBoardTestEnv(t)
	base := time.Date(2026, time.July, 22, 10, 0, 0, 0, time.UTC)
	env.seedRun(t, "review-root", "review", store.RunStatusRunning, func(run *store.Run) {
		run.CreatedAt = base.Add(-time.Hour)
		run.UpdatedAt = run.CreatedAt
	})

	seedReview := func(id string, createdAt, requestedAt time.Time) {
		t.Helper()
		interactionID := id + "_gate"
		env.seedRun(t, id, "review", store.RunStatusPausedWaitingHuman, func(run *store.Run) {
			run.ParentRunID = "review-root"
			run.CreatedAt = createdAt
			run.UpdatedAt = requestedAt
			run.Checkpoint = &store.Checkpoint{
				NodeID:               "gate",
				InteractionID:        interactionID,
				InteractionQuestions: map[string]any{"reply": id + "?"},
			}
		})
		if err := env.runStore(t).WriteInteraction(context.Background(), &store.Interaction{
			ID:          interactionID,
			RunID:       id,
			NodeID:      "gate",
			RequestedAt: requestedAt,
			Questions:   map[string]any{"reply": id + "?"},
		}); err != nil {
			t.Fatalf("WriteInteraction(%s): %v", id, err)
		}
	}

	// Tree order is A, B, C by creation time. The review queue initially has
	// the same order for a different reason: the pending-turn update stamps.
	seedReview("ai-a", base.Add(-3*time.Minute), base)
	seedReview("ai-b", base.Add(-2*time.Minute), base.Add(time.Minute))
	seedReview("ai-c", base.Add(-time.Minute), base.Add(2*time.Minute))

	assertQueue := func(wantIDs []string, wantTimes []time.Time) {
		t.Helper()
		card := findPipelineCard(t, env.projection(t).Cards, "run:review-root")
		if len(card.PendingReviews) != len(wantIDs) {
			t.Fatalf("pending_reviews = %+v, want %v", card.PendingReviews, wantIDs)
		}
		for i, wantID := range wantIDs {
			got := card.PendingReviews[i]
			if got.RunID != wantID || !got.UpdatedAt.Equal(wantTimes[i]) {
				t.Errorf("pending_reviews[%d] = (%s, %s), want (%s, %s)",
					i, got.RunID, got.UpdatedAt, wantID, wantTimes[i])
			}
		}
	}

	assertQueue(
		[]string{"ai-a", "ai-b", "ai-c"},
		[]time.Time{base, base.Add(time.Minute), base.Add(2 * time.Minute)},
	)

	// Guided reviews reuse the same run/node/interaction ID. Rewriting A's
	// interaction with a fresh request stamp models its next AI turn: it must
	// join the back instead of reclaiming A's structural position in the tree.
	if err := env.runStore(t).WriteInteraction(context.Background(), &store.Interaction{
		ID:          "ai-a_gate",
		RunID:       "ai-a",
		NodeID:      "gate",
		RequestedAt: base.Add(3 * time.Minute),
		Questions:   map[string]any{"reply": "ai-a next turn?"},
	}); err != nil {
		t.Fatalf("rewrite ai-a interaction: %v", err)
	}
	assertQueue(
		[]string{"ai-b", "ai-c", "ai-a"},
		[]time.Time{base.Add(time.Minute), base.Add(2 * time.Minute), base.Add(3 * time.Minute)},
	)
}

// Root status maps to the five lanes; queued is TODO (waiting for a slot).
func TestPipelineBoardColumnBucketing(t *testing.T) {
	env := newPipelineBoardTestEnv(t)
	cases := []struct {
		id         string
		status     store.RunStatus
		column     string
		wantFailed bool
	}{
		{"r-running", store.RunStatusRunning, pipelineColumnInProgress, false},
		{"r-paused", store.RunStatusPausedWaitingHuman, pipelineColumnInProgress, false},
		{"r-finished", store.RunStatusFinished, pipelineColumnClosed, false},
		// These roots are STANDALONE (no ticket). A standalone failure has no
		// retry / resume-to-ready / close affordance on this board and
		// reserves nothing, so it is filed in Closed rather than collecting in
		// a lane nobody can act on. Ticket-backed failures go to
		// needs_attention — see TestPipelineBoardTicketFailureEntersLane.
		{"r-failed", store.RunStatusFailed, pipelineColumnClosed, true},
		{"r-resumable", store.RunStatusFailedResumable, pipelineColumnClosed, true},
		// CANCELLED stays in Closed. Pin this deliberately: a refactor that
		// sweeps cancelled back in with the failures would make the operator's
		// own Stop button hold a concurrency slot, and would make Close retain
		// the slot it exists to release.
		{"r-cancelled", store.RunStatusCancelled, pipelineColumnClosed, true},
		// An operator soft-pause is resumable mid-flight state, NOT a
		// failure — it must never offer Retry-from-zero (L1, PR review).
		{"r-operator", store.RunStatusPausedOperator, pipelineColumnInProgress, false},
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
		// A failed/cancelled run in CLOSED must carry the Failed flag so the
		// UI offers Retry, filters it as "failed", and shows the reason; a
		// successful finish must not.
		if card.Failed != c.wantFailed {
			t.Errorf("%s Failed = %v, want %v", c.id, card.Failed, c.wantFailed)
		}
		// None of these roots is ticket-backed, so none may reserve a slot:
		// a standalone run has no retry path on the board, and a reservation
		// with no way to release it is a permanent capacity leak.
		if card.ReservesSlot {
			t.Errorf("%s ReservesSlot = true for a standalone run — nothing could ever release it", c.id)
		}
	}
	queued := findPipelineCard(t, projection.Cards, "run:r-queued")
	if queued.ColumnID != pipelineColumnOpened {
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
	if out.ColumnID != pipelineColumnClosed {
		t.Errorf("finished column = %q, want closed", out.ColumnID)
	}
	if out.Failed {
		t.Errorf("finished run must not be flagged Failed")
	}
	if out.ExecutedNodes != out.TotalNodes || out.TotalNodes != 3 {
		t.Errorf("finished progress = %d/%d, want 3/3 (100%% clamp)", out.ExecutedNodes, out.TotalNodes)
	}
	if out.Output != "Shipped v2 cleanly." {
		t.Errorf("output = %q, want the final_answer", out.Output)
	}
}

// TestPipelineBoardFinishedOutputMemoized proves a finished run's DONE-card
// output is computed once and served from the memo on later polls, instead of
// re-probing artifacts every tick (PR #193 M1). We overwrite the artifact
// between polls: because a finished run is terminal, the board keeps serving
// the first-computed value.
func TestPipelineBoardFinishedOutputMemoized(t *testing.T) {
	env := newPipelineBoardTestEnv(t)

	env.seedRun(t, "run-memo", "review", store.RunStatusFinished, func(run *store.Run) {
		run.FilePath = env.botPath
		run.ArtifactIndex = map[string]int{"summary": 1}
	})
	env.seedArtifact(t, "run-memo", "summary", map[string]any{"final_answer": "First answer."})

	first := findPipelineCard(t, env.projection(t).Cards, "run:run-memo")
	if first.Output != "First answer." {
		t.Fatalf("first output = %q, want %q", first.Output, "First answer.")
	}

	// Change the underlying artifact. A non-memoized projection would pick this
	// up on the next poll; a memoized one keeps the first value.
	env.seedArtifact(t, "run-memo", "summary", map[string]any{"final_answer": "MUTATED answer."})

	second := findPipelineCard(t, env.projection(t).Cards, "run:run-memo")
	if second.Output != "First answer." {
		t.Errorf("second output = %q, want memoized %q (re-read the store instead of serving the cache)", second.Output, "First answer.")
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
	longTitle := strings.Repeat("é", pipelineTitleMaxRunes+20)
	if got := create(longTitle); len([]rune(got)) != pipelineTitleMaxRunes {
		t.Errorf("long first title has %d runes, want %d", len([]rune(got)), pipelineTitleMaxRunes)
	}
	if got := create(longTitle); len([]rune(got)) != pipelineTitleMaxRunes || !strings.HasPrefix(got, "#2 - ") {
		t.Errorf("long duplicate = %q, want bounded title with #2 prefix", got)
	}
}

func TestPipelineBoardTaskCreateCompactsLongTitle(t *testing.T) {
	env := newPipelineBoardTestEnv(t)
	rawTitle := "A detailed product idea\n\n" + strings.Repeat("with extensive context ", 20)
	payload, err := json.Marshal(map[string]string{
		"bot":   "review",
		"title": rawTitle,
	})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	resp, err := http.Post(
		env.http.URL+"/api/v1/pipeline-board/tasks",
		"application/json",
		bytes.NewReader(payload),
	)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	if resp.StatusCode != http.StatusCreated {
		resp.Body.Close()
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusCreated)
	}
	var out native.Issue
	decodeJSONResp(t, resp, &out)
	if want := compactPipelineTitle(rawTitle); out.Title != want {
		t.Fatalf("stored title = %q, want %q", out.Title, want)
	}
	if strings.Contains(out.Title, "\n") {
		t.Fatalf("stored title still contains a newline: %q", out.Title)
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
	if findPipelineCard(t, projection.Cards, "run:run-manual").ColumnID != pipelineColumnClosed {
		t.Error("manual finished run should be projected in Closed")
	}
	for _, card := range projection.Cards {
		if card.Kind == "task" {
			t.Errorf("in-flight issue was duplicated as a task: %+v", card)
		}
	}
}

// A fork of a card's dead run replaces it on the card: the fork inherits
// the issue's source edge, and its parent link — the structural mark that
// excludes shards and subbot children — must not exclude IT, since a fork
// replaces the card's run instead of fanning out of it.
func TestPipelineBoardForkReplacesFailedParent(t *testing.T) {
	env := newPipelineBoardTestEnv(t)
	issue, err := env.board.Create(native.Issue{Title: "Dispatch me", Bot: "review"})
	if err != nil {
		t.Fatal(err)
	}
	older := time.Now().Add(-time.Hour)
	env.seedRun(t, "run-parent", "review", store.RunStatusFailed, func(run *store.Run) {
		run.FilePath = env.botPath
		run.Source = &store.RunSource{Kind: store.RunSourceKindDispatcher, IssueID: issue.ID}
		run.CreatedAt = older
	})
	// The dispatcher stamps the attempt end BEFORE any fork exists — the
	// fork is the recovery gesture for a run that can no longer continue,
	// so it is always created after the parent's attempt was registered.
	if err := env.board.SetLastRun(issue.ID, "run-parent", ""); err != nil {
		t.Fatalf("SetLastRun: %v", err)
	}
	env.seedRun(t, "run-fork", "review", store.RunStatusRunning, func(run *store.Run) {
		run.FilePath = env.botPath
		run.Source = &store.RunSource{Kind: store.RunSourceKindDispatcher, IssueID: issue.ID}
		run.ForkedFrom = "run-parent"
		run.ParentRunID = "run-parent"
	})
	// A shard (fan-out child: parent edge, NO fork mark) with a source
	// edge stays excluded even when it is the newest run of the issue.
	env.seedRun(t, "run-shard", "review", store.RunStatusRunning, func(run *store.Run) {
		run.FilePath = env.botPath
		run.Source = &store.RunSource{Kind: store.RunSourceKindDispatcher, IssueID: issue.ID}
		run.ParentRunID = "run-parent"
	})

	projection := env.projection(t)
	card := findPipelineCard(t, projection.Cards, "run:run-fork")
	if card.IssueID != issue.ID {
		t.Errorf("fork card issue = %q, want %q — the fork should carry the card, not the dead parent", card.IssueID, issue.ID)
	}
	if hasPipelineCard(projection.Cards, "run:run-shard") {
		t.Error("shard must not become a card root — only forks replace the parent")
	}
}

// A fork that has ENDED is still the card's latest outcome: nothing else
// ever updates the pointer for it (the dispatcher only knows its own
// attempts), so a finished fork supersedes the failed parent even though
// terminal candidates are otherwise ignored.
func TestPipelineBoardTerminalForkSupersedesFailedParent(t *testing.T) {
	env := newPipelineBoardTestEnv(t)
	issue, err := env.board.Create(native.Issue{Title: "Dispatch me", Bot: "review"})
	if err != nil {
		t.Fatal(err)
	}
	older := time.Now().Add(-time.Hour)
	env.seedRun(t, "run-parent", "review", store.RunStatusFailed, func(run *store.Run) {
		run.FilePath = env.botPath
		run.Source = &store.RunSource{Kind: store.RunSourceKindDispatcher, IssueID: issue.ID}
		run.CreatedAt = older
	})
	// Same ordering as production: the dispatcher stamps the attempt end
	// first; the fork is minted afterwards as the recovery gesture.
	if err := env.board.SetLastRun(issue.ID, "run-parent", ""); err != nil {
		t.Fatalf("SetLastRun: %v", err)
	}
	env.seedRun(t, "run-fork", "review", store.RunStatusFinished, func(run *store.Run) {
		run.FilePath = env.botPath
		run.Source = &store.RunSource{Kind: store.RunSourceKindDispatcher, IssueID: issue.ID}
		run.ForkedFrom = "run-parent"
		run.ParentRunID = "run-parent"
		// A run that really ended carries FinishedAt (stamped by the
		// status transition). A parked, never-resumed fork does not —
		// see TestPipelineBoardParkedForkKeepsFailedParent.
		ended := run.CreatedAt.Add(10 * time.Minute)
		run.FinishedAt = &ended
	})

	projection := env.projection(t)
	card := findPipelineCard(t, projection.Cards, "run:run-fork")
	if card.IssueID != issue.ID {
		t.Errorf("finished fork card issue = %q, want %q — the fork is the card's latest outcome", card.IssueID, issue.ID)
	}
}

// A fork that was created but never resumed is parked in `cancelled` —
// itself a terminal status — and must NOT supersede the failed parent:
// the parent's failure is the very reason the operator is forking, and
// the fork may be abandoned before it ever runs. FinishedAt (stamped
// only by real status transitions, never by Fork's initial SaveRun)
// discriminates the parked shell from a fork that actually ended.
func TestPipelineBoardParkedForkKeepsFailedParent(t *testing.T) {
	env := newPipelineBoardTestEnv(t)
	issue, err := env.board.Create(native.Issue{Title: "Dispatch me", Bot: "review"})
	if err != nil {
		t.Fatal(err)
	}
	older := time.Now().Add(-time.Hour)
	env.seedRun(t, "run-parent", "review", store.RunStatusFailed, func(run *store.Run) {
		run.FilePath = env.botPath
		run.Source = &store.RunSource{Kind: store.RunSourceKindDispatcher, IssueID: issue.ID}
		run.CreatedAt = older
	})
	if err := env.board.SetLastRun(issue.ID, "run-parent", ""); err != nil {
		t.Fatalf("SetLastRun: %v", err)
	}
	// The parked fork: status cancelled (how Fork() parks every child),
	// FinishedAt nil because it never executed.
	env.seedRun(t, "run-fork", "review", store.RunStatusCancelled, func(run *store.Run) {
		run.FilePath = env.botPath
		run.Source = &store.RunSource{Kind: store.RunSourceKindDispatcher, IssueID: issue.ID}
		run.ForkedFrom = "run-parent"
		run.ParentRunID = "run-parent"
	})

	projection := env.projection(t)
	if hasPipelineCard(projection.Cards, "run:run-fork") {
		t.Error("a parked, never-resumed fork must not become the card root")
	}
	// The failed parent stays the card's visible outcome.
	card := findPipelineCard(t, projection.Cards, "task:"+issue.ID)
	if len(card.Attempts) == 0 || card.Attempts[len(card.Attempts)-1].RunID != "run-parent" {
		t.Errorf("attempts = %+v, want the failed parent kept as the card's latest attempt", card.Attempts)
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

// Both a not-yet-ready native task (non-eligible state) and an eligible
// (ready) one live in the TODO lane now; the Ready flag distinguishes them
// (drives the Ready/Draft badge + the Todo lane's readiness filter).
func TestPipelineBoardProjectsTaskDraftVsTodo(t *testing.T) {
	env := newPipelineBoardTestEnv(t)
	draft, err := env.board.Create(native.Issue{
		Title:   "Being prepared",
		State:   native.StateInbox, // non-eligible → Todo, not ready (Draft badge)
		Bot:     "review",
		BotArgs: map[string]string{"scope": "api"},
	})
	if err != nil {
		t.Fatalf("Create draft issue: %v", err)
	}
	ready, err := env.board.Create(native.Issue{
		Title: "Ready to run",
		State: native.StateReady, // eligible → Todo, ready
		Bot:   "review",
	})
	if err != nil {
		t.Fatalf("Create ready issue: %v", err)
	}

	projection := env.projection(t)
	d := findPipelineCard(t, projection.Cards, "task:"+draft.ID)
	if d.Kind != "task" || d.ColumnID != pipelineColumnOpened || d.Ready {
		t.Errorf("draft card = %+v, want kind=task column=todo ready=false", d)
	}
	if d.EntryInput["scope"] != "api" {
		t.Errorf("draft entry_input = %+v, want bot_args", d.EntryInput)
	}
	r := findPipelineCard(t, projection.Cards, "task:"+ready.ID)
	if r.ColumnID != pipelineColumnOpened || !r.Ready {
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
	if card := findPipelineCard(t, env.projection(t).Cards, "task:"+issue.ID); card.ColumnID != pipelineColumnOpened || !card.Ready {
		t.Errorf("after ready: card = %+v, want column=todo ready=true", card)
	}
	if got := post(false); got.State != native.StateBacklog {
		t.Errorf("ready=false state = %q, want %q", got.State, native.StateBacklog)
	}
	// Unstaged, the ticket stays in Todo but loses its Ready flag (Draft badge).
	if card := findPipelineCard(t, env.projection(t).Cards, "task:"+issue.ID); card.ColumnID != pipelineColumnOpened || card.Ready {
		t.Errorf("after unready: card = %+v, want column=todo ready=false", card)
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
	// The edit shows on the board's Todo (draft) card.
	card := findPipelineCard(t, env.projection(t).Cards, "task:"+issue.ID)
	if card.Title != "Polished" || card.ColumnID != pipelineColumnOpened || card.EntryInput["scope"] != "new" {
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

// The `?since=` filter (L5) prunes CLOSED cards that last changed before the
// cutoff so a long-lived local store isn't stuck in the truncation banner —
// while live pipelines stay visible regardless of age, and the pruning is
// reported (never silent).
func TestPipelineBoardSinceFilterHidesOldClosedCards(t *testing.T) {
	env := newPipelineBoardTestEnv(t)
	now := time.Now().UTC()
	old := now.Add(-72 * time.Hour)

	// A finished root that last changed long ago → hidden by `?since=1h`.
	env.seedRun(t, "r-old-done", "review", store.RunStatusFinished, func(run *store.Run) {
		run.FilePath = env.botPath
		run.UpdatedAt = old
	})
	// A finished root that changed just now → kept even though it is closed.
	env.seedRun(t, "r-fresh-done", "review", store.RunStatusFinished, func(run *store.Run) {
		run.FilePath = env.botPath
		run.UpdatedAt = now
	})
	// A running root that started long ago → kept: live pipelines are never
	// pruned by age, only closed ones.
	env.seedRun(t, "r-old-running", "review", store.RunStatusRunning, func(run *store.Run) {
		run.FilePath = env.botPath
		run.UpdatedAt = old
	})

	// No filter: all three render.
	all := env.projection(t)
	for _, id := range []string{"run:r-old-done", "run:r-fresh-done", "run:r-old-running"} {
		if !hasPipelineCard(all.Cards, id) {
			t.Fatalf("unfiltered projection missing %s", id)
		}
	}
	if all.HiddenClosedCount != 0 || all.HiddenClosedBefore != nil {
		t.Errorf("unfiltered projection reports hidden=%d before=%v; want 0/nil", all.HiddenClosedCount, all.HiddenClosedBefore)
	}

	// since=1h: the old finished card drops; the fresh finished and the old
	// running one stay.
	filtered := env.projectionQuery(t, "since=1h")
	if hasPipelineCard(filtered.Cards, "run:r-old-done") {
		t.Errorf("since=1h should have hidden the stale finished card")
	}
	if !hasPipelineCard(filtered.Cards, "run:r-fresh-done") {
		t.Errorf("since=1h wrongly hid a freshly-finished card")
	}
	if !hasPipelineCard(filtered.Cards, "run:r-old-running") {
		t.Errorf("since=1h wrongly hid a live (running) card")
	}
	if filtered.HiddenClosedCount != 1 {
		t.Errorf("HiddenClosedCount = %d, want 1", filtered.HiddenClosedCount)
	}
	if filtered.HiddenClosedBefore == nil {
		t.Errorf("HiddenClosedBefore should be set when a filter is applied")
	}
}

// A malformed `?since=` is an explicit 400, not a silently-ignored filter.
func TestPipelineBoardSinceFilterRejectsBadValue(t *testing.T) {
	env := newPipelineBoardTestEnv(t)
	resp, err := http.Get(env.http.URL + "/api/v1/pipeline-board?since=notaduration")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
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

func TestPipelineBoardWorkspaceImageServesGuardsAndRefuses(t *testing.T) {
	env := newPipelineBoardTestEnv(t)
	refDir := filepath.Join(env.workDir, "assets", "characters", "histoire", "boudicca", "refs")
	if err := os.MkdirAll(refDir, 0o755); err != nil {
		t.Fatal(err)
	}
	payload := []byte("\x89PNG fake-bytes")
	if err := os.WriteFile(filepath.Join(refDir, "master.png"), payload, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(env.workDir, "secret.txt"), []byte("nope"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Nominal: a workdir-relative image is served with its content type.
	resp, err := http.Get(env.http.URL + "/api/v1/pipeline-board/workspace-images/assets/characters/histoire/boudicca/refs/master.png")
	if err != nil {
		t.Fatal(err)
	}
	body := new(bytes.Buffer)
	if _, err := body.ReadFrom(resp.Body); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", resp.StatusCode, body.String())
	}
	if got := resp.Header.Get("Content-Type"); got != "image/png" {
		t.Fatalf("content-type = %q, want image/png", got)
	}
	if !bytes.Equal(body.Bytes(), payload) {
		t.Fatalf("body = %q, want the file bytes", body.Bytes())
	}

	// Image-only allowlist: a non-image workdir file is refused even though
	// it exists — this endpoint must not become a generic file reader.
	resp, err = http.Get(env.http.URL + "/api/v1/pipeline-board/workspace-images/secret.txt")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("non-image status = %d, want 404", resp.StatusCode)
	}

	// Traversal out of the workdir is rejected by safePath. The raw path
	// must bypass Go's client-side dot-segment cleaning to reach the server.
	req, err := http.NewRequest(http.MethodGet, env.http.URL+"/api/v1/pipeline-board/workspace-images/escape.png", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.URL.Path = "/api/v1/pipeline-board/workspace-images/../../../etc/passwd.png"
	req.URL.RawPath = "/api/v1/pipeline-board/workspace-images/..%2F..%2F..%2Fetc%2Fpasswd.png"
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		t.Fatalf("traversal returned 200, want an error status")
	}

	// Missing file: clean 404.
	resp, err = http.Get(env.http.URL + "/api/v1/pipeline-board/workspace-images/assets/absent.png")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("missing file status = %d, want 404", resp.StatusCode)
	}
}

func TestTitleFromContentInputs_ShortsEpisode(t *testing.T) {
	got := titleFromContentInputs(map[string]any{
		"character":     "Boudicca",
		"episode_no":    "1",
		"episode_total": "5",
		"episode_title": "Le Fouet et le Serment",
		"hook":          "Rome croyait frapper une veuve…",
	})
	want := "Boudicca · ÉP 1/5 — Le Fouet et le Serment"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestTitleFromContentInputs_SubjectOnly(t *testing.T) {
	got := titleFromContentInputs(map[string]any{"requested_character": "Boudicca"})
	if got != "Boudicca" {
		t.Fatalf("got %q, want Boudicca", got)
	}
}

func TestTitleFromContentInputs_Empty(t *testing.T) {
	if got := titleFromContentInputs(nil); got != "" {
		t.Fatalf("nil → %q", got)
	}
	if got := titleFromContentInputs(map[string]any{"max_script_rewrites": "2"}); got != "" {
		t.Fatalf("noise-only keys should not yield a title, got %q", got)
	}
}

func TestPipelineDisplayTitle_PrefersContentOverRunCodename(t *testing.T) {
	root := &store.Run{
		Name:         "orbital-plunge-borealroar-707f",
		WorkflowName: "shorts_historical_episode",
		Inputs: map[string]any{
			"character":     "Boudicca",
			"episode_no":    "4",
			"episode_total": "5",
			"episode_title": "Londinium Sacrifiée",
		},
	}
	got := pipelineDisplayTitle(nil, root)
	want := "Boudicca · ÉP 4/5 — Londinium Sacrifiée"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestPipelineDisplayTitle_IssueTitleWhenNoContent(t *testing.T) {
	issue := &native.Issue{Title: "Review the release"}
	root := &store.Run{Name: "comet-bonk-novazap-d859", WorkflowName: "review"}
	got := pipelineDisplayTitle(issue, root)
	if got != "Review the release" {
		t.Fatalf("got %q, want issue title", got)
	}
}

func TestPipelineDisplayTitle_TaskFromBotArgs(t *testing.T) {
	issue := &native.Issue{
		Title: "draft ticket",
		BotArgs: map[string]string{
			"character":     "Boudicca",
			"episode_no":    "2",
			"episode_total": "5",
			"episode_title": "La Ville sans Murailles",
		},
	}
	got := pipelineDisplayTitle(issue, nil)
	want := "Boudicca · ÉP 2/5 — La Ville sans Murailles"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestPipelineDisplayTitle_IgnoresLongFormInput(t *testing.T) {
	root := &store.Run{
		BundleDisplayName: "Ida",
		Inputs: map[string]any{
			"idea": strings.Repeat("A full product brief with Markdown. ", 400),
		},
	}
	if got := pipelineDisplayTitle(nil, root); got != "Ida" {
		t.Fatalf("got %q, want bundle display name", got)
	}
}

func TestPipelineDisplayTitle_CompactsAndTruncatesLongCandidate(t *testing.T) {
	root := &store.Run{
		Inputs: map[string]any{
			"topic": "A detailed product idea " + strings.Repeat("with extensive context ", 10) + "\n\nIgnored details",
		},
	}
	got := pipelineDisplayTitle(nil, root)
	if strings.Contains(got, "\n") {
		t.Fatalf("title still contains a newline: %q", got)
	}
	if len([]rune(got)) > pipelineTitleMaxRunes {
		t.Fatalf("title has %d runes, want at most %d: %q", len([]rune(got)), pipelineTitleMaxRunes, got)
	}
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("truncated title = %q, want ellipsis", got)
	}
}

// Content inputs on a live root must surface on the board projection even
// when no native ticket is linked (the common studio launch path).
func TestPipelineBoardCardTitleFromRunInputs(t *testing.T) {
	env := newPipelineBoardTestEnv(t)
	env.seedRun(t, "run-ep", "shorts_historical_episode", store.RunStatusRunning, func(run *store.Run) {
		run.Name = "orbital-plunge-borealroar-707f"
		run.BotID = "shorts-episode"
		run.Inputs = map[string]any{
			"character":     "Boudicca",
			"episode_no":    "1",
			"episode_total": "5",
			"episode_title": "Le Fouet et le Serment",
		}
	})
	projection := env.projection(t)
	card := findPipelineCard(t, projection.Cards, "run:run-ep")
	want := "Boudicca · ÉP 1/5 — Le Fouet et le Serment"
	if card.Title != want {
		t.Fatalf("card title = %q, want %q", card.Title, want)
	}
}

// A ticket restaged to ready after its last run was cancelled must appear
// as an Opened/Ready TASK card — not stuck in Closed behind the old run.
// Regression for: "episode invisible but not lost: ready, waiting for a slot".
func TestPipelineBoardRestagedReadyAfterCancelShowsOpenedTask(t *testing.T) {
	env := newPipelineBoardTestEnv(t)
	issue, err := env.board.Create(native.Issue{
		Title: "Episode 5",
		State: native.StateReady,
		Bot:   "review",
		BotArgs: map[string]string{
			"character":     "Boudicca",
			"episode_no":    "5",
			"episode_total": "5",
			"episode_title": "Le Dernier Fracas",
		},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	env.seedRun(t, "run-cancelled-ep5", "shorts_historical_episode", store.RunStatusCancelled, func(run *store.Run) {
		run.Name = "void-smash-synthsnarl-0db8"
		run.BotID = "shorts-episode"
		run.Inputs = map[string]any{
			"character":     "Boudicca",
			"episode_no":    "5",
			"episode_total": "5",
			"episode_title": "Le Dernier Fracas",
		}
	})
	if err := env.board.SetLastRun(issue.ID, "run-cancelled-ep5", ""); err != nil {
		t.Fatalf("SetLastRun: %v", err)
	}

	projection := env.projection(t)

	// Must NOT project the cancelled run as the primary closed card.
	if hasPipelineCard(projection.Cards, "run:run-cancelled-ep5") {
		t.Fatalf("cancelled prior run must not be the board card when ticket is restaged ready: %+v", projection.Cards)
	}
	card := findPipelineCard(t, projection.Cards, "task:"+issue.ID)
	if card.ColumnID != pipelineColumnOpened {
		t.Errorf("column = %q, want opened", card.ColumnID)
	}
	if !card.Ready {
		t.Errorf("ready = false, want true (ticket is StateReady)")
	}
	if card.Kind != "task" {
		t.Errorf("kind = %q, want task", card.Kind)
	}
	wantTitle := "Boudicca · ÉP 5/5 — Le Dernier Fracas"
	if card.Title != wantTitle {
		t.Errorf("title = %q, want %q", card.Title, wantTitle)
	}
	// Prior attempt still listed for history / drawer.
	foundAttempt := false
	for _, a := range card.Attempts {
		if a.RunID == "run-cancelled-ep5" {
			foundAttempt = true
			break
		}
	}
	if !foundAttempt {
		t.Errorf("attempts missing cancelled prior run: %+v", card.Attempts)
	}
}

// A cancelled run whose ticket is STILL in_progress (not restaged) stays
// Closed — restaging is what flips projection back to Opened.
func TestPipelineBoardCancelledRunWithoutRestageStaysClosed(t *testing.T) {
	env := newPipelineBoardTestEnv(t)
	issue, err := env.board.Create(native.Issue{
		Title: "Still closed",
		State: native.StateInProgress,
		Bot:   "review",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	env.seedRun(t, "run-still-closed", "review", store.RunStatusCancelled, nil)
	if err := env.board.SetLastRun(issue.ID, "run-still-closed", ""); err != nil {
		t.Fatalf("SetLastRun: %v", err)
	}
	projection := env.projection(t)
	card := findPipelineCard(t, projection.Cards, "run:run-still-closed")
	if card.ColumnID != pipelineColumnClosed {
		t.Errorf("column = %q, want closed", card.ColumnID)
	}
	if !card.Failed {
		t.Error("failed flag should be set for cancelled run")
	}
}

func TestPipelineIssueRestagedForRelaunch(t *testing.T) {
	ready := &native.Issue{State: native.StateReady}
	inProg := &native.Issue{State: native.StateInProgress}
	cancelled := &store.Run{Status: store.RunStatusCancelled}
	finished := &store.Run{Status: store.RunStatusFinished}
	running := &store.Run{Status: store.RunStatusRunning}
	if !pipelineIssueRestagedForRelaunch(ready, cancelled) {
		t.Error("ready+cancelled should restage")
	}
	if pipelineIssueRestagedForRelaunch(ready, finished) {
		t.Error("ready+finished must not restage (success not relaunched)")
	}
	if pipelineIssueRestagedForRelaunch(ready, running) {
		t.Error("ready+running must not restage")
	}
	if pipelineIssueRestagedForRelaunch(inProg, cancelled) {
		t.Error("in_progress+cancelled is not restaged (still owns the attempt)")
	}
}

func TestPipelineBoardParentChildProjection(t *testing.T) {
	env := newPipelineBoardTestEnv(t)
	parent, err := env.board.Create(native.Issue{
		Title:   "Plan series",
		State:   native.StateDone,
		Bot:     "review",
		BotArgs: map[string]string{native.BotArgRole: "planner"},
	})
	if err != nil {
		t.Fatal(err)
	}
	child, err := env.board.Create(native.Issue{
		Title:    "Episode 1",
		State:    native.StateReady,
		Bot:      "review",
		ParentID: parent.ID,
		BotArgs: map[string]string{
			native.BotArgSpawnedFrom: parent.ID,
			native.BotArgRole:        "producer",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	proj := env.projection(t)
	pCard := findPipelineCard(t, proj.Cards, "task:"+parent.ID)
	if pCard.Role != "planner" {
		t.Errorf("parent role = %q", pCard.Role)
	}
	if pCard.ChildrenSummary == nil || pCard.ChildrenSummary.Total != 1 {
		t.Fatalf("children summary = %+v", pCard.ChildrenSummary)
	}
	if len(pCard.Children) != 1 || pCard.Children[0].IssueID != child.ID {
		t.Fatalf("children = %+v", pCard.Children)
	}
	cCard := findPipelineCard(t, proj.Cards, "task:"+child.ID)
	if cCard.ParentIssueID != parent.ID {
		t.Errorf("child parent = %q", cCard.ParentIssueID)
	}
	if cCard.ParentTitle != "Plan series" {
		t.Errorf("parent title = %q", cCard.ParentTitle)
	}
}

// A planner parent is an ORDINARY card: once its own run finishes it lands
// in Closed, even while spawned children are still open. (It used to be
// pinned to Opened with launch_blocked_reason=awaiting_children until every
// child closed — that made the parent's lane contradict its own run status.)
// The relation survives as data: the children summary on the parent, and
// parent_id / parent_title on each child.
func TestPipelineBoardParentClosesIndependentlyOfChildren(t *testing.T) {
	env := newPipelineBoardTestEnv(t)
	parent, err := env.board.Create(native.Issue{
		Title:   "Plan series",
		State:   native.StateDone,
		Bot:     "review",
		BotArgs: map[string]string{native.BotArgRole: "planner"},
	})
	if err != nil {
		t.Fatal(err)
	}
	env.seedRun(t, "run-plan", "shorts_series_plan", store.RunStatusFinished, func(run *store.Run) {
		run.BotID = "shorts-series-plan"
	})
	if err := env.board.SetLastRun(parent.ID, "run-plan", ""); err != nil {
		t.Fatal(err)
	}
	child, err := env.board.Create(native.Issue{
		Title:    "Episode 1",
		State:    native.StateReady,
		Bot:      "review",
		ParentID: parent.ID,
		BotArgs: map[string]string{
			native.BotArgSpawnedFrom: parent.ID,
			native.BotArgRole:        "producer",
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	proj := env.projection(t)
	pCard := findPipelineCard(t, proj.Cards, "run:run-plan")
	if pCard.ColumnID != pipelineColumnClosed {
		t.Fatalf("parent column = %q, want closed (its own run finished)", pCard.ColumnID)
	}
	if pCard.LaunchBlockedReason == "awaiting_children" {
		t.Fatal("awaiting_children must no longer be emitted")
	}
	if pCard.ChildrenSummary == nil || pCard.ChildrenSummary.Total != 1 {
		t.Fatalf("parent lost its children counter: %+v", pCard.ChildrenSummary)
	}
	cCard := findPipelineCard(t, proj.Cards, "task:"+child.ID)
	if cCard.ColumnID != pipelineColumnOpened {
		t.Fatalf("child column = %q, want opened", cCard.ColumnID)
	}
	if cCard.ParentIssueID != parent.ID || cCard.ParentTitle != "Plan series" {
		t.Fatalf("child lost its parent link: %q / %q", cCard.ParentIssueID, cCard.ParentTitle)
	}
}

// The card must carry the paused node's resolved `instructions:` text.
// It is the author's operator-facing question and rides ONLY on the
// human_input_requested event — the checkpoint and the interaction record
// don't keep it. Without it the card renders an answer box with nothing
// above it, because the schema-driven form shows the node's OUTPUT fields
// while `questions` holds its INBOUND data (regression: an app-concept
// interview gate whose whole prompt is `instructions: {{input.reply}}`
// showed a blank form on the board while the run console rendered it).
func TestPipelineBoardPendingReviewCarriesResolvedInstructions(t *testing.T) {
	env := newPipelineBoardTestEnv(t)
	env.seedRun(t, "run-gate", "interview", store.RunStatusPausedWaitingHuman, func(run *store.Run) {
		run.FilePath = env.botPath
		run.Checkpoint = &store.Checkpoint{
			NodeID:        "chat",
			InteractionID: "int-chat",
			// Inbound data only: the answer form never renders these.
			InteractionQuestions: map[string]any{"reply": "Which scope do you want?"},
		}
	})
	rs := env.runStore(t)
	// An earlier turn of the SAME gate, then the current one, then a pause
	// on the same node belonging to ANOTHER interaction (what a human node
	// inside a loop produces): the operator must see the turn the
	// checkpoint points at — not the stale one, and not a sibling's. The
	// last entry is what makes the interaction-id key load-bearing: drop
	// it and this test still passes on node id alone.
	for _, turn := range []struct{ id, text string }{
		{"int-chat", "STALE first turn"},
		{"int-chat", "Which scope do you want? **A** or **B**?"},
		{"int-later-loop", "WRONG INTERACTION"},
	} {
		if _, err := rs.AppendEvent(context.Background(), "run-gate", store.Event{
			Type:      store.EventHumanInputRequested,
			RunID:     "run-gate",
			NodeID:    "chat",
			Timestamp: time.Now().UTC(),
			Data: map[string]any{
				"interaction_id": turn.id,
				"instructions":   turn.text,
			},
		}); err != nil {
			t.Fatalf("AppendEvent: %v", err)
		}
	}
	// A pause of a DIFFERENT node must not leak into this review.
	if _, err := rs.AppendEvent(context.Background(), "run-gate", store.Event{
		Type:      store.EventHumanInputRequested,
		RunID:     "run-gate",
		NodeID:    "other_gate",
		Timestamp: time.Now().UTC(),
		Data:      map[string]any{"interaction_id": "int-other", "instructions": "WRONG NODE"},
	}); err != nil {
		t.Fatalf("AppendEvent: %v", err)
	}

	card := findPipelineCard(t, env.projection(t).Cards, "run:run-gate")
	if len(card.PendingReviews) != 1 {
		t.Fatalf("pending_reviews = %+v, want the paused gate", card.PendingReviews)
	}
	got := card.PendingReviews[0].Instructions
	if got != "Which scope do you want? **A** or **B**?" {
		t.Fatalf("instructions = %q, want the current turn's resolved prompt", got)
	}
}

// A node with no `instructions:` prompt emits no such field; the review
// must then carry an empty string rather than borrowing another node's.
func TestPipelineBoardPendingReviewWithoutInstructionsStaysEmpty(t *testing.T) {
	env := newPipelineBoardTestEnv(t)
	env.seedRun(t, "run-bare", "bare", store.RunStatusPausedWaitingHuman, func(run *store.Run) {
		run.FilePath = env.botPath
		run.Checkpoint = &store.Checkpoint{NodeID: "gate", InteractionID: "int-bare"}
	})
	if _, err := env.runStore(t).AppendEvent(context.Background(), "run-bare", store.Event{
		Type:      store.EventHumanInputRequested,
		RunID:     "run-bare",
		NodeID:    "gate",
		Timestamp: time.Now().UTC(),
		Data:      map[string]any{"interaction_id": "int-bare"},
	}); err != nil {
		t.Fatalf("AppendEvent: %v", err)
	}

	card := findPipelineCard(t, env.projection(t).Cards, "run:run-bare")
	if len(card.PendingReviews) != 1 {
		t.Fatalf("pending_reviews = %+v", card.PendingReviews)
	}
	if got := card.PendingReviews[0].Instructions; got != "" {
		t.Fatalf("instructions = %q, want empty", got)
	}
}

// A later turn on the SAME interaction that carries no instructions must
// BLANK the card, not leave the previous turn's question standing. Two
// pause paths legitimately emit no text — pauseForRecovery, and
// humanInstructionsExtra returning nil when the template renders empty —
// and interactionIDForPause hands every pause of a non-loop node the same
// `<runID>_<nodeID>` id, so the stale text would otherwise sit as markdown
// directly above a form asking something else: this PR's own failure mode,
// in reverse.
func TestPipelineBoardPendingReviewLaterTurnWithoutInstructionsBlanks(t *testing.T) {
	env := newPipelineBoardTestEnv(t)
	env.seedRun(t, "run-recovery", "interview", store.RunStatusPausedWaitingHuman, func(run *store.Run) {
		run.FilePath = env.botPath
		run.Checkpoint = &store.Checkpoint{NodeID: "chat", InteractionID: "int-chat"}
	})
	rs := env.runStore(t)
	for _, turn := range []map[string]any{
		{"interaction_id": "int-chat", "instructions": "The gate question"},
		// A recovery pause on the same node/interaction: no instructions key.
		{"interaction_id": "int-chat", "recovery_code": "E_TOOL", "recovery_reason": "boom"},
	} {
		if _, err := rs.AppendEvent(context.Background(), "run-recovery", store.Event{
			Type:      store.EventHumanInputRequested,
			RunID:     "run-recovery",
			NodeID:    "chat",
			Timestamp: time.Now().UTC(),
			Data:      turn,
		}); err != nil {
			t.Fatalf("AppendEvent: %v", err)
		}
	}

	card := findPipelineCard(t, env.projection(t).Cards, "run:run-recovery")
	if len(card.PendingReviews) != 1 {
		t.Fatalf("pending_reviews = %+v", card.PendingReviews)
	}
	if got := card.PendingReviews[0].Instructions; got != "" {
		t.Fatalf("instructions = %q, want empty: the current turn carries none", got)
	}
}

// With the parent's record gone (pruned/deleted), the current==nil early
// return must apply the same shell gate: a never-resumed fork is not the
// card's outcome.
func TestPipelineBoardParkedForkWithoutParentRecord(t *testing.T) {
	env := newPipelineBoardTestEnv(t)
	issue, err := env.board.Create(native.Issue{Title: "Dispatch me", Bot: "review"})
	if err != nil {
		t.Fatal(err)
	}
	// The pointer names a run record the store no longer serves.
	if err := env.board.SetLastRun(issue.ID, "run-gone", ""); err != nil {
		t.Fatalf("SetLastRun: %v", err)
	}
	env.seedRun(t, "run-fork", "review", store.RunStatusCancelled, func(run *store.Run) {
		run.FilePath = env.botPath
		run.Source = &store.RunSource{IssueID: issue.ID}
		run.ForkedFrom = "run-gone"
		run.ParentRunID = "run-gone"
		// No FinishedAt: created by Fork() and never resumed.
	})

	projection := env.projection(t)
	if hasPipelineCard(projection.Cards, "run:run-fork") {
		t.Error("a parked fork shell must not become the card root even when the parent's record is gone")
	}
}

// A CHAT SESSION — a run of a bot whose manifest declares a chat: surface
// (the assistant dock's Copi / Nexie) — is not a pipeline: the dock is its
// surface and it parks on a human turn for days. It never becomes a card,
// whether resolved by bot id or (legacy run, no bot id) by the manifest
// beside its workflow file; its own children stay folded under it; every
// ordinary run keeps its card. The rule is the manifest block, never a bot
// id — the engine stays bot-agnostic.
func TestPipelineBoardHidesChatSessions(t *testing.T) {
	env := newPipelineBoardTestEnv(t)
	bundleDir := filepath.Join(env.workDir, "bots", "chatty")
	if err := os.MkdirAll(bundleDir, 0o755); err != nil {
		t.Fatal(err)
	}
	chatBotPath := filepath.Join(bundleDir, "main.bot")
	if err := os.WriteFile(chatBotPath, []byte(pipelineBoardTestBot), 0o644); err != nil {
		t.Fatal(err)
	}
	manifest := "name: chatty\ndisplay_name: Chatty\nchat:\n  nodes:\n    approval: {kind: human, text_field: feedback}\n"
	if err := os.WriteFile(filepath.Join(bundleDir, "manifest.yaml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}

	env.seedRun(t, "chat-open", "review", store.RunStatusPausedWaitingHuman, func(run *store.Run) {
		run.FilePath = chatBotPath
		run.BotID = "chatty"
	})
	env.seedRun(t, "chat-legacy", "review", store.RunStatusFinished, func(run *store.Run) {
		run.FilePath = chatBotPath // no BotID: resolved from the manifest beside the file
	})
	env.seedRun(t, "chat-child", "review", store.RunStatusRunning, func(run *store.Run) {
		run.FilePath = env.botPath
		run.ParentRunID = "chat-open"
	})
	env.seedRun(t, "run-manual", "review", store.RunStatusRunning, func(run *store.Run) {
		run.FilePath = env.botPath
	})

	projection := env.projection(t)
	for _, hidden := range []string{"run:chat-open", "run:chat-legacy", "run:chat-child"} {
		if hasPipelineCard(projection.Cards, hidden) {
			t.Errorf("chat session %s was projected as a pipeline card", hidden)
		}
	}
	if findPipelineCard(t, projection.Cards, "run:run-manual").ColumnID != pipelineColumnInProgress {
		t.Error("ordinary running run should keep its In-progress card")
	}
	if len(projection.Cards) != 1 {
		t.Errorf("cards = %d, want exactly the ordinary run: %+v", len(projection.Cards), projection.Cards)
	}
}
