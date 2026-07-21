package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	"github.com/SocialGouv/iterion/pkg/store"
)

func TestPipelineBoardTaskUpsertByInputPath(t *testing.T) {
	env := newPipelineBoardTestEnv(t)
	// First create.
	body := `{
		"bot":"review",
		"title":"Asset fort",
		"bot_args":{"input_path":"requests/fort.json","asset_id":"fort","pipeline_kind":"mesh"},
		"upsert":true
	}`
	resp, err := http.Post(env.http.URL+"/api/v1/pipeline-board/tasks", "application/json", bytes.NewBufferString(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create status = %d", resp.StatusCode)
	}
	var first native.Issue
	if err := json.NewDecoder(resp.Body).Decode(&first); err != nil {
		t.Fatal(err)
	}

	// Second create with upsert + same key → update, not duplicate.
	body2 := `{
		"bot":"review",
		"title":"Asset fort (rev2)",
		"bot_args":{"input_path":"requests/fort.json","asset_id":"fort","pipeline_kind":"mesh","revision_id":"2"},
		"blockers":[],
		"upsert":true
	}`
	resp2, err := http.Post(env.http.URL+"/api/v1/pipeline-board/tasks", "application/json", bytes.NewBufferString(body2))
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("upsert status = %d (want 200)", resp2.StatusCode)
	}
	var second native.Issue
	if err := json.NewDecoder(resp2.Body).Decode(&second); err != nil {
		t.Fatal(err)
	}
	if second.ID != first.ID {
		t.Fatalf("upsert created new id %s, want %s", second.ID, first.ID)
	}
	if second.Title != "Asset fort (rev2)" {
		t.Fatalf("title = %q", second.Title)
	}
	if second.BotArgs["revision_id"] != "2" {
		t.Fatalf("bot_args = %+v", second.BotArgs)
	}

	all, err := env.board.List(native.ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, iss := range all {
		if iss != nil && iss.Bot == "review" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("want 1 review ticket after upsert, got %d", n)
	}
}

func TestPipelineBoardBulkReady(t *testing.T) {
	env := newPipelineBoardTestEnv(t)
	mesh, err := env.board.Create(native.Issue{
		Title: "mesh", Bot: "review", State: native.StateDone,
		BotArgs: map[string]string{native.BotArgInputPath: "m.json", native.BotArgFamilyID: "f1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	feat, err := env.board.Create(native.Issue{
		Title: "feature", Bot: "review", State: native.StateBacklog,
		Blockers: []string{mesh.ID},
		BotArgs: map[string]string{
			native.BotArgInputPath:    "f.json",
			native.BotArgFamilyID:     "f1",
			native.BotArgPipelineKind: "feature",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	// Open-blocker ticket in same family — should be skipped when only_satisfied.
	_, _ = env.board.Create(native.Issue{
		Title: "blocked feat", Bot: "review", State: native.StateBacklog,
		Blockers: []string{"native:missing"},
		BotArgs: map[string]string{
			native.BotArgInputPath: "g.json",
			native.BotArgFamilyID:  "f1",
		},
	})

	body := `{"family_id":"f1"}`
	resp, err := http.Post(env.http.URL+"/api/v1/pipeline-board/bulk/ready", "application/json", bytes.NewBufferString(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var out struct {
		Ready   []string `json:"ready"`
		Skipped []string `json:"skipped"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if len(out.Ready) != 1 || out.Ready[0] != feat.ID {
		t.Fatalf("ready = %v, want [%s]", out.Ready, feat.ID)
	}
	got, _ := env.board.Get(feat.ID)
	if got.State != native.StateReady {
		t.Fatalf("feature state = %q", got.State)
	}
}

func TestPipelineBoardDependencyGraph(t *testing.T) {
	env := newPipelineBoardTestEnv(t)
	a, _ := env.board.Create(native.Issue{Title: "A", Bot: "review", State: native.StateDone})
	b, _ := env.board.Create(native.Issue{
		Title: "B", Bot: "review", State: native.StateWaitingDeps, Blockers: []string{a.ID},
	})
	resp, err := http.Get(env.http.URL + "/api/v1/pipeline-board/tasks/" + b.ID + "/dependency-graph")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var g DependencyGraphResponse
	if err := json.NewDecoder(resp.Body).Decode(&g); err != nil {
		t.Fatal(err)
	}
	if g.Root.ID != b.ID {
		t.Fatalf("root = %s", g.Root.ID)
	}
	if len(g.Root.Blockers) != 1 || !g.Root.Blockers[0].Satisfied {
		t.Fatalf("blockers = %+v", g.Root.Blockers)
	}
	foundA := false
	for _, n := range g.Nodes {
		if n.ID == a.ID {
			foundA = true
		}
	}
	if !foundA {
		t.Fatalf("nodes missing A: %+v", g.Nodes)
	}
}

func TestPipelineBoardBulkDelete(t *testing.T) {
	env := newPipelineBoardTestEnv(t)
	a, err := env.board.Create(native.Issue{Title: "draft A", Bot: "review", State: native.StateInbox})
	if err != nil {
		t.Fatal(err)
	}
	b, err := env.board.Create(native.Issue{Title: "draft B", Bot: "review", State: native.StateReady})
	if err != nil {
		t.Fatal(err)
	}
	// Ticket with active run — must be skipped.
	live, err := env.board.Create(native.Issue{Title: "live", Bot: "review", State: native.StateInProgress})
	if err != nil {
		t.Fatal(err)
	}
	env.seedRun(t, "run-live-bulk", "review", store.RunStatusRunning, nil)
	if err := env.board.SetLastRun(live.ID, "run-live-bulk", ""); err != nil {
		t.Fatal(err)
	}

	body := `{"ids":["` + a.ID + `","` + b.ID + `","` + live.ID + `","native:missing"]}`
	resp, err := http.Post(env.http.URL+"/api/v1/pipeline-board/bulk/delete", "application/json", bytes.NewBufferString(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var out struct {
		Deleted    []string          `json:"deleted"`
		Skipped    []string          `json:"skipped"`
		SkippedWhy map[string]string `json:"skipped_why"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if len(out.Deleted) != 2 {
		t.Fatalf("deleted = %v, want 2", out.Deleted)
	}
	del := map[string]bool{}
	for _, id := range out.Deleted {
		del[id] = true
	}
	if !del[a.ID] || !del[b.ID] {
		t.Fatalf("deleted = %v, want %s and %s", out.Deleted, a.ID, b.ID)
	}
	// live + missing skipped
	if len(out.Skipped) < 2 {
		t.Fatalf("skipped = %v", out.Skipped)
	}
	if _, err := env.board.Get(a.ID); err == nil {
		t.Fatal("A should be gone")
	}
	if _, err := env.board.Get(live.ID); err != nil {
		t.Fatal("live must remain")
	}
}
