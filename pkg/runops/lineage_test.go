package runops

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/store"
)

// A subbot's output is awaited by its parent. `resumable` answers "will the
// store accept a resume of THIS run", which stays true for a leaf whose whole
// ancestry died days ago — and that is exactly the reading that sent an
// assistant to relaunch an abandoned grandchild while the card it was asked
// to unblock never moved. `orphaned` is the fact that was missing.
func TestRunGet_ReportsLineageAndOrphanhood(t *testing.T) {
	ctx := context.Background()
	rs, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	mk := func(id, parent string, status store.RunStatus, wf string) {
		t.Helper()
		if _, err := rs.CreateRun(ctx, id, wf, nil); err != nil {
			t.Fatalf("CreateRun %s: %v", id, err)
		}
		r, err := rs.LoadRun(ctx, id)
		if err != nil {
			t.Fatalf("LoadRun %s: %v", id, err)
		}
		r.ParentRunID = parent
		r.Status = status
		if err := rs.SaveRun(ctx, r); err != nil {
			t.Fatalf("SaveRun %s: %v", id, err)
		}
	}
	get := func(id string) runDiagnosis {
		t.Helper()
		raw, err := callRunGet(ctx, rs, json.RawMessage(`{"run_id":"`+id+`"}`))
		if err != nil {
			t.Fatalf("run_get %s: %v", id, err)
		}
		var out runDiagnosis
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatalf("decode %s: %v", id, err)
		}
		return out
	}

	// The shape observed in the field: a cancelled root, a failed_resumable
	// middle, and a leaf that resumes happily into the void.
	mk("root", "", store.RunStatusCancelled, "town_planner")
	mk("mid", "root", store.RunStatusFailedResumable, "hierarchical_planning")
	mk("leaf", "mid", store.RunStatusFailedResumable, "hierarchy_feature_author")

	leaf := get("leaf")
	if !leaf.Resumable {
		t.Fatal("the leaf IS resumable on its own — that must not change")
	}
	if leaf.ParentRunID != "mid" {
		t.Fatalf("parent_run_id = %q, want mid", leaf.ParentRunID)
	}
	if len(leaf.Ancestors) != 2 || leaf.Ancestors[0].ID != "mid" || leaf.Ancestors[1].ID != "root" {
		t.Fatalf("ancestors = %+v, want [mid root] nearest first", leaf.Ancestors)
	}
	if leaf.Ancestors[0].WorkflowName != "hierarchical_planning" {
		t.Fatalf("ancestor workflow = %q", leaf.Ancestors[0].WorkflowName)
	}
	if !leaf.Orphaned {
		t.Fatal("no ancestor is running, so nothing awaits the leaf's output — orphaned must be true")
	}
	if leaf.RootID != "root" {
		t.Fatalf("root_run_id = %q, want root — it is the run a caller should resume instead", leaf.RootID)
	}

	// A live parent means the child's output still reaches someone: this is
	// an ordinary subbot, not an abandoned one.
	mk("live-parent", "", store.RunStatusRunning, "town_planner")
	mk("live-child", "live-parent", store.RunStatusFailedResumable, "hierarchy_feature_author")
	if child := get("live-child"); child.Orphaned {
		t.Fatal("a running parent is still waiting for this child — orphaned must stay false")
	}

	// A root has no lineage to report and is never orphaned: there is no
	// parent that could have abandoned it.
	root := get("root")
	if root.ParentRunID != "" || len(root.Ancestors) != 0 || root.Orphaned || root.RootID != "" {
		t.Fatalf("a root must report no lineage, got %+v", root)
	}

	// …but the ABSENCE must stay legible on the wire. Every other lineage
	// field is legitimately empty on a root, so without an always-present
	// `orphaned` a caller cannot tell "no parent" from "this server does not
	// report lineage" — and the careful ones refuse to guess, which is
	// exactly the reader this field exists to serve.
	raw, err := callRunGet(ctx, rs, json.RawMessage(`{"run_id":"root"}`))
	if err != nil {
		t.Fatalf("run_get root: %v", err)
	}
	if !strings.Contains(string(raw), `"orphaned":false`) {
		t.Fatalf("a root must still WITNESS that lineage was evaluated: %s", raw)
	}
}

// An unreadable ancestor (deleted, or another tenant) must degrade to "no
// lineage known" rather than assert orphanhood — claiming a run is abandoned
// on the strength of a failed read would send a caller to resume the wrong
// thing, or nothing at all.
func TestRunGet_UnreadableAncestorDoesNotClaimOrphanhood(t *testing.T) {
	ctx := context.Background()
	rs, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	if _, err := rs.CreateRun(ctx, "child", "wf", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	r, err := rs.LoadRun(ctx, "child")
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	r.ParentRunID = "vanished"
	r.Status = store.RunStatusFailedResumable
	if err := rs.SaveRun(ctx, r); err != nil {
		t.Fatalf("SaveRun: %v", err)
	}

	raw, err := callRunGet(ctx, rs, json.RawMessage(`{"run_id":"child"}`))
	if err != nil {
		t.Fatalf("run_get must still answer when an ancestor is unreadable: %v", err)
	}
	var out runDiagnosis
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.ParentRunID != "vanished" {
		t.Fatalf("the known parent id must still be reported, got %q", out.ParentRunID)
	}
	if out.Orphaned {
		t.Fatal("orphanhood was asserted from a failed read")
	}
	if out.Status != store.RunStatusFailedResumable {
		t.Fatalf("the diagnosis itself must survive: status = %q", out.Status)
	}
}
