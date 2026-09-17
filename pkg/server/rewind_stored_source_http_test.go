package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/botsource"
	"github.com/SocialGouv/iterion/pkg/store"
)

// The whole feature, through the endpoint an operator actually calls.
//
// Each half is pinned on its own elsewhere; this is the join, and the join is
// what was missing: disabling the wiring in handleRewindRun — one condition —
// left every other test in the package green while `rewind --auto` went back
// to refusing every stored-bot run.
//
// The run below executed a two-node team bot and the row has moved since, so
// the answer exists only if the handler resolves the row's CURRENT version and
// hands it to the rewind.
func TestRewindHTTP_AutoTargetsAStoredBotAtItsCurrentVersion(t *testing.T) {
	srv, hs := newTestServer(t)
	srv.botSources = botsource.NewMemoryStore()
	srv.cfg.Mode = "cloud"
	ctx := context.Background()

	const launched = "agent draft:\n  model: \"claude-opus-4-7\"\n\n" +
		"agent polish:\n  model: \"claude-opus-4-7\"\n\n" +
		"workflow main:\n  entry: draft\n  draft -> polish\n  polish -> done\n"
	if _, err := srv.botSources.Create(store.WithTenant(ctx, "t1"), botsource.BotSource{
		TenantID: "t1", Slug: "shared",
		Files: map[string]string{botsource.MainBotFile: launched},
	}); err != nil {
		t.Fatalf("create the team bot: %v", err)
	}

	st, err := store.New(srv.cfg.StoreDir)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if _, err := st.CreateRun(ctx, "stored-run", "main", nil); err != nil {
		t.Fatalf("create run: %v", err)
	}
	run, err := st.LoadRun(ctx, "stored-run")
	if err != nil {
		t.Fatalf("load run: %v", err)
	}
	run.Status = store.RunStatusFailedResumable
	run.FilePath = "bots/shared/main.bot"
	run.BotSourceTier = store.BotSourceTierTeam
	run.BotSourceTenant = "t1"
	run.WorkflowSource = launched
	run.Checkpoint = &store.Checkpoint{
		NodeID:  "polish",
		Outputs: map[string]map[string]any{"draft": {"v": 1}, "polish": {"v": 1}},
	}
	if err := st.SaveRun(ctx, run); err != nil {
		t.Fatalf("save run: %v", err)
	}

	// The row moves: the operator edited the team bot in the studio. This is
	// the edit `--auto` has to find, and it exists nowhere on this pod's
	// filesystem — resolveWorkflowPath would answer the baked catalog twin.
	edited := strings.Replace(launched,
		"agent polish:\n  model: \"claude-opus-4-7\"",
		"agent polish:\n  model: \"claude-opus-5\"", 1)
	if edited == launched {
		t.Fatal("the fixture no longer carries the declaration this test edits")
	}
	tctx := store.WithTenant(ctx, "t1")
	row, err := srv.botSources.GetBySlug(tctx, "t1", "shared")
	if err != nil {
		t.Fatalf("reload the team bot: %v", err)
	}
	row.Files = map[string]string{botsource.MainBotFile: edited}
	if _, err := srv.botSources.Update(tctx, row); err != nil {
		t.Fatalf("update the team bot: %v", err)
	}

	resp, err := http.Post(hs.URL+"/api/runs/stored-run/rewind", "application/json",
		bytes.NewReader([]byte(`{"auto":true,"keep_files":true}`)))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	payload, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("rewind --auto on a stored-bot run = %d: %s\n"+
			"the handler did not resolve the bot's current version, so --auto has no NOW side and refuses",
			resp.StatusCode, payload)
	}
	var got struct {
		NodeID       string   `json:"node_id"`
		AutoTargeted bool     `json:"auto_targeted"`
		DroppedNodes []string `json:"dropped_nodes"`
	}
	if err := json.Unmarshal(payload, &got); err != nil {
		t.Fatalf("decode: %v (%s)", err, payload)
	}
	if got.NodeID != "polish" || !got.AutoTargeted {
		t.Errorf("pivot = %q (auto %v), want polish — the edit is in the row's CURRENT version: %s",
			got.NodeID, got.AutoTargeted, payload)
	}
	for _, n := range got.DroppedNodes {
		if n == "draft" {
			t.Errorf("dropped = %v — `draft` is upstream of the edit and must survive it", got.DroppedNodes)
		}
	}
}
