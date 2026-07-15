package server

import (
	"context"
	"net/http"
	"net/url"
	"testing"

	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
)

// seedChildRun writes a run and stamps its shard-tuple edges
// (ParentRunID + shard coordinates) via SaveRun, so the reverse-tree
// endpoint has a subtree to project.
func seedChildRun(t *testing.T, srv *Server, runID, parentID string, shardIndex, shardCount int, shardLabel string) {
	t.Helper()
	st, err := store.New(srv.cfg.StoreDir)
	if err != nil {
		t.Fatalf("open seed store: %v", err)
	}
	ctx := context.Background()
	if _, err := st.CreateRun(ctx, runID, "wf_child", nil); err != nil {
		t.Fatalf("CreateRun %s: %v", runID, err)
	}
	r, err := st.LoadRun(ctx, runID)
	if err != nil {
		t.Fatalf("LoadRun %s: %v", runID, err)
	}
	r.ParentRunID = parentID
	r.ShardIndex = shardIndex
	r.ShardCount = shardCount
	r.ShardLabel = shardLabel
	if err := st.SaveRun(ctx, r); err != nil {
		t.Fatalf("SaveRun %s: %v", runID, err)
	}
}

// TestListRunChildren exercises GET /api/runs/{id}/children: it returns
// exactly the runs whose ParentRunID is {id}, carrying the projected
// shard tuple, and nothing else (T4b, refs #125).
func TestListRunChildren(t *testing.T) {
	srv, hs := newTestServer(t)
	// Parent + two shards under it, one unrelated run.
	seedRun(t, srv, "parent-1", "wf_parent", store.RunStatusFinished)
	seedChildRun(t, srv, "shard-0", "parent-1", 0, 2, "docs")
	seedChildRun(t, srv, "shard-1", "parent-1", 1, 2, "code")
	seedRun(t, srv, "unrelated", "wf_other", store.RunStatusFinished)

	t.Run("returns the subtree with shard tuple", func(t *testing.T) {
		resp, err := http.Get(hs.URL + "/api/runs/parent-1/children")
		if err != nil {
			t.Fatalf("GET children: %v", err)
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
		var out struct {
			Runs []runview.RunSummary `json:"runs"`
		}
		decodeJSONResp(t, resp, &out)
		if len(out.Runs) != 2 {
			t.Fatalf("len = %d, want 2 (%+v)", len(out.Runs), out.Runs)
		}
		// created_at ascending → shard-0 then shard-1.
		if out.Runs[0].ID != "shard-0" || out.Runs[1].ID != "shard-1" {
			t.Errorf("ids = [%s %s], want [shard-0 shard-1]", out.Runs[0].ID, out.Runs[1].ID)
		}
		s0 := out.Runs[0]
		if s0.ParentRunID != "parent-1" || s0.ShardIndex != 0 || s0.ShardCount != 2 || s0.ShardLabel != "docs" {
			t.Errorf("shard-0 tuple = {parent=%q idx=%d count=%d label=%q}, want {parent-1 0 2 docs}",
				s0.ParentRunID, s0.ShardIndex, s0.ShardCount, s0.ShardLabel)
		}
	})

	t.Run("run with no children yields empty", func(t *testing.T) {
		resp, err := http.Get(hs.URL + "/api/runs/unrelated/children")
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		var out struct {
			Runs []runview.RunSummary `json:"runs"`
		}
		decodeJSONResp(t, resp, &out)
		if len(out.Runs) != 0 {
			t.Errorf("len = %d, want 0", len(out.Runs))
		}
	})
}

// seedChildRunInStore mirrors seedChildRun but writes into an arbitrary
// store (the foreign cross-store), so the ?store= deep-link path has a
// subtree to project that the primary daemon store never sees.
func seedChildRunInStore(t *testing.T, st store.RunStore, runID, parentID string, shardIndex, shardCount int, shardLabel string) {
	t.Helper()
	ctx := context.Background()
	if _, err := st.CreateRun(ctx, runID, "wf_child", nil); err != nil {
		t.Fatalf("CreateRun %s: %v", runID, err)
	}
	r, err := st.LoadRun(ctx, runID)
	if err != nil {
		t.Fatalf("LoadRun %s: %v", runID, err)
	}
	r.ParentRunID = parentID
	r.ShardIndex = shardIndex
	r.ShardCount = shardCount
	r.ShardLabel = shardLabel
	if err := st.SaveRun(ctx, r); err != nil {
		t.Fatalf("SaveRun %s: %v", runID, err)
	}
}

// TestListRunChildren_CrossStore pins the ?store= deep-link parity: a run
// whose child subtree lives in a *foreign* store (another daemon's
// ~/.iterion) must have /children served from that store — not silently
// masked to empty by the primary daemon store, which holds no such runs.
func TestListRunChildren_CrossStore(t *testing.T) {
	_, hs := newTestServer(t)
	xs, xdir := newCrossStore(t)

	// Parent + two shards live ONLY in the foreign store. The primary
	// daemon store has no run with parent_run_id == parent-xs.
	if _, err := xs.CreateRun(context.Background(), "parent-xs", "wf_parent", nil); err != nil {
		t.Fatalf("CreateRun parent: %v", err)
	}
	seedChildRunInStore(t, xs, "xs-shard-0", "parent-xs", 0, 2, "docs")
	seedChildRunInStore(t, xs, "xs-shard-1", "parent-xs", 1, 2, "code")

	resp, err := http.Get(hs.URL + "/api/runs/parent-xs/children?store=" + url.QueryEscape(xdir))
	if err != nil {
		t.Fatalf("GET children: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var out struct {
		Runs []runview.RunSummary `json:"runs"`
	}
	decodeJSONResp(t, resp, &out)
	if len(out.Runs) != 2 {
		t.Fatalf("len = %d, want 2 (%+v)", len(out.Runs), out.Runs)
	}
	if out.Runs[0].ID != "xs-shard-0" || out.Runs[1].ID != "xs-shard-1" {
		t.Errorf("ids = [%s %s], want [xs-shard-0 xs-shard-1]", out.Runs[0].ID, out.Runs[1].ID)
	}
	if s0 := out.Runs[0]; s0.ParentRunID != "parent-xs" || s0.ShardLabel != "docs" {
		t.Errorf("shard-0 tuple = {parent=%q label=%q}, want {parent-xs docs}", s0.ParentRunID, s0.ShardLabel)
	}
}

// TestListRunChildren_ParentNodeID pins contract C3 on the children wire:
// a child run spawned by a subbot node carries parent_node_id (the IR node
// id of the spawning subbot node in the parent workflow) in the JSON.
func TestListRunChildren_ParentNodeID(t *testing.T) {
	srv, hs := newTestServer(t)
	seedRun(t, srv, "parent-sb", "wf_parent", store.RunStatusRunning)

	st, err := store.New(srv.cfg.StoreDir)
	if err != nil {
		t.Fatalf("open seed store: %v", err)
	}
	ctx := context.Background()
	if _, err := st.CreateRun(ctx, "sb-child", "wf_child", nil); err != nil {
		t.Fatalf("CreateRun sb-child: %v", err)
	}
	r, err := st.LoadRun(ctx, "sb-child")
	if err != nil {
		t.Fatalf("LoadRun sb-child: %v", err)
	}
	r.ParentRunID = "parent-sb"
	r.ParentNodeID = "produce_episode"
	if err := st.SaveRun(ctx, r); err != nil {
		t.Fatalf("SaveRun sb-child: %v", err)
	}

	resp, err := http.Get(hs.URL + "/api/runs/parent-sb/children")
	if err != nil {
		t.Fatalf("GET children: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var out struct {
		Runs []runview.RunSummary `json:"runs"`
	}
	decodeJSONResp(t, resp, &out)
	if len(out.Runs) != 1 {
		t.Fatalf("len = %d, want 1 (%+v)", len(out.Runs), out.Runs)
	}
	if got := out.Runs[0].ParentNodeID; got != "produce_episode" {
		t.Errorf("parent_node_id = %q, want %q", got, "produce_episode")
	}
}
