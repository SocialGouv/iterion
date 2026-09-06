package e2e

import (
	"context"
	"sync"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/runtime"
)

// ctxProbeExecutor records, per node id, the run identity and the template
// snapshot the engine put on the context for that execution.
type ctxProbeExecutor struct {
	mu   sync.Mutex
	seen map[string][]ctxProbe
}

type ctxProbe struct {
	ctxRunID string // model.RunIDFromContext — the KEY per-node state is filed under
	tdRunID  string // model.TemplateData.RunID — what {{run.id}} renders from
	haveTD   bool
}

func (e *ctxProbeExecutor) Execute(ctx context.Context, node ir.Node, _ map[string]any) (map[string]any, error) {
	p := ctxProbe{ctxRunID: model.RunIDFromContext(ctx)}
	if td := model.TemplateDataFromContext(ctx); td != nil {
		p.haveTD = true
		p.tdRunID = td.RunID
	}
	e.mu.Lock()
	e.seen[node.NodeID()] = append(e.seen[node.NodeID()], p)
	e.mu.Unlock()

	switch node.NodeID() {
	case "seed":
		return map[string]any{"value": "SEEDED", "items": []any{"alpha", "beta"}}, nil
	case "trunk_probe", "each_probe", "all_probe":
		return map[string]any{"run_id": "stub", "elapsed": "0"}, nil
	default:
		return map[string]any{"text": "ok"}, nil
	}
}

func (e *ctxProbeExecutor) probes(t *testing.T, nodeID string, want int) []ctxProbe {
	t.Helper()
	e.mu.Lock()
	defer e.mu.Unlock()
	got := append([]ctxProbe(nil), e.seen[nodeID]...)
	if len(got) != want {
		t.Fatalf("node %q executed %d times, want %d", nodeID, len(got), want)
	}
	return got
}

// TestFanOutBranchesCarryTheSnapshotNotTheIdentity separates the two things
// the engine puts on a node's context, because only ONE of them is safe to
// hand a fan-out branch.
//
// The template snapshot is a VALUE: every branch gets its own, and it carries
// the run id, which is what keeps `{{run.id}}` rendering there. The ctx run
// identity is a KEY — the per-node compaction session store files under
// `(runID, nodeID)`, as do the operator-chat inbox and the ADR-081
// async-question binder — and `fan_out_each` replays ONE node id per item, so
// that key is not unique inside a fan-out. Sharing it makes a failed item's
// transcript the next item's history; pkg/backend/model's
// TestPerNodeSessionFollowsTheRunIdentity is that mechanism, isolated.
//
// So: branch dispatch carries the snapshot and NOT the identity; the trunk
// carries both.
func TestFanOutBranchesCarryTheSnapshotNotTheIdentity(t *testing.T) {
	wf := compileFixture(t, "branch_template_context_mini.bot")
	s := tmpStore(t)
	const runID = "e2e-branch-ctx-identity"

	exec := &ctxProbeExecutor{seen: map[string][]ctxProbe{}}
	if err := runtime.New(wf, s, exec).Run(context.Background(), runID, nil); err != nil {
		t.Fatalf("run: %v", err)
	}

	assertProbes := func(nodeID string, want int, wantCtxRunID string) {
		t.Helper()
		for i, p := range exec.probes(t, nodeID, want) {
			if !p.haveTD {
				t.Errorf("node %q exec %d: no template snapshot on ctx", nodeID, i)
				continue
			}
			// The snapshot always names the run — that is what keeps
			// `{{run.id}}` rendering on a branch with no ctx identity.
			if p.tdRunID != runID {
				t.Errorf("node %q exec %d: TemplateData.RunID = %q, want %q", nodeID, i, p.tdRunID, runID)
			}
			if p.ctxRunID != wantCtxRunID {
				t.Errorf("node %q exec %d: ctx run id = %q, want %q", nodeID, i, p.ctxRunID, wantCtxRunID)
			}
		}
	}

	// Trunk: the node id is unique per run, so the identity is a valid key.
	assertProbes("seed", 1, runID)
	assertProbes("trunk_probe", 1, runID)
	assertProbes("trunk_agent", 1, runID)

	// Fan-out bodies: no identity. `each_*` runs twice under ONE node id —
	// exactly the aliasing the withheld key prevents.
	assertProbes("each_probe", 2, "")
	assertProbes("each_agent", 2, "")
	assertProbes("all_probe", 1, "")
	assertProbes("all_agent", 1, "")
}
