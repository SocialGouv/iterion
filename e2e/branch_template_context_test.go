package e2e

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/store"
)

// promptRecorder is a delegate backend that answers every LLM node with a
// fixed schema-valid output and keeps the prompt the executor RESOLVED for
// it. The executor renders the prompt, so what lands here is the real
// rendering — a literal `{{run.id}}` in the recording is a literal the
// model would have been asked to read.
//
// It also records the ctx run ID each dispatch carried, which is the other
// half of the contract: see assertBranchDispatchHasNoRunID.
type promptRecorder struct {
	mu      sync.Mutex
	prompts map[string][]string
	ctxRuns map[string][]string
}

func newPromptRecorder() *promptRecorder {
	return &promptRecorder{
		prompts: make(map[string][]string),
		ctxRuns: make(map[string][]string),
	}
}

func (p *promptRecorder) Execute(ctx context.Context, task delegate.Task) (delegate.Result, error) {
	p.mu.Lock()
	p.prompts[task.NodeID] = append(p.prompts[task.NodeID], task.UserPrompt)
	p.ctxRuns[task.NodeID] = append(p.ctxRuns[task.NodeID], model.RunIDFromContext(ctx))
	p.mu.Unlock()
	return delegate.Result{
		BackendName: delegate.BackendClaudeCode,
		Output:      map[string]any{"text": "ok"},
	}, nil
}

func (p *promptRecorder) rendered(t *testing.T, nodeID string, want int) []string {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	got := append([]string(nil), p.prompts[nodeID]...)
	if len(got) != want {
		t.Fatalf("node %q rendered %d prompts, want %d: %q", nodeID, len(got), want, got)
	}
	return got
}

func (p *promptRecorder) ctxRunIDs(nodeID string) []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.ctxRuns[nodeID]...)
}

// nodeOutputsFor returns every output a node published on a node_finished
// event, in order. A fan-out node publishes one per branch.
func nodeOutputsFor(events []*store.Event, nodeID string) []map[string]any {
	var out []map[string]any
	for _, e := range events {
		if e.Type != store.EventNodeFinished || e.NodeID != nodeID {
			continue
		}
		if o, ok := e.Data["output"].(map[string]any); ok {
			out = append(out, o)
		}
	}
	return out
}

// TestTemplateContextReachesFanOutBranches is the #763 regression: the
// identity + template snapshot the executor renders prompts and tool
// commands from was attached on the TRUNK dispatch path only, so the same
// probe rendered a run id on the trunk and a silent empty constant one edge
// later, inside a fan-out branch.
//
// The fixture runs one tool probe and one agent probe on all three dispatch
// paths of a single run — trunk, `fan_out_each` body (twice, one per item),
// `fan_out_all` branch — and this asserts they render IDENTICALLY. The
// per-item agent additionally reads `{{outputs.spread.it}}`, which exists
// only in the branch's own outputs view, so it also pins whose view the
// snapshot was taken from.
func TestTemplateContextReachesFanOutBranches(t *testing.T) {
	wf := compileFixture(t, "branch_template_context_mini.bot")
	s := tmpStore(t)
	const runID = "e2e-branch-template-ctx"

	recorder := newPromptRecorder()
	backends := delegate.NewRegistry()
	backends.Register(delegate.BackendClaudeCode, recorder)
	exec := model.NewClawExecutor(model.NewRegistry(), wf,
		model.WithBackendRegistry(backends),
		model.WithWorkDir(t.TempDir()),
	)

	eng := runtime.New(wf, s, exec)
	if err := eng.Run(context.Background(), runID, nil); err != nil {
		t.Fatalf("run: %v", err)
	}
	r, err := s.LoadRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("load run: %v", err)
	}
	if r.Status != store.RunStatusFinished {
		t.Fatalf("status = %s (%s), want finished", r.Status, r.Error)
	}
	events, err := s.LoadEvents(context.Background(), runID)
	if err != nil {
		t.Fatalf("load events: %v", err)
	}

	// --- tool `command:` — the silent-constant half -----------------------
	//
	// A `{{run.id}}` a tool node cannot resolve does not survive as visible
	// braces: resolveRunRefs substitutes it with the empty string, so the
	// command runs with an argument nobody notices is missing.
	for _, probe := range []struct {
		node  string
		calls int
	}{
		{"trunk_probe", 1}, // the reference: the trunk always had the snapshot
		{"each_probe", 2},  // fan_out_each body, once per item
		{"all_probe", 1},   // fan_out_all branch
	} {
		outs := nodeOutputsFor(events, probe.node)
		if len(outs) != probe.calls {
			t.Fatalf("tool %q produced %d outputs, want %d", probe.node, len(outs), probe.calls)
		}
		for i, out := range outs {
			if got := out["run_id"]; got != runID {
				t.Errorf("tool %q call %d: command rendered run.id = %q, want %q", probe.node, i, got, runID)
			}
			assertElapsed(t, probe.node, out["elapsed"])
		}
	}

	// --- agent `user:` prompt — the literal-braces half -------------------
	trunk := recorder.rendered(t, "trunk_agent", 1)[0]
	assertPromptResolved(t, "trunk_agent", trunk, runID)
	if !strings.Contains(trunk, "seed=SEEDED") {
		t.Errorf("trunk_agent prompt did not resolve {{outputs.seed.value}}: %q", trunk)
	}

	// trunk_agent and all_agent share ONE prompt body, so everything but
	// the live elapsed reading must render identically across the trunk /
	// fan_out_all boundary.
	branch := recorder.rendered(t, "all_agent", 1)[0]
	assertPromptResolved(t, "all_agent", branch, runID)
	if !strings.Contains(branch, "seed=SEEDED") {
		t.Errorf("all_agent prompt did not resolve {{outputs.seed.value}} inside a fan_out_all branch: %q", branch)
	}
	if dropElapsed(trunk) != dropElapsed(branch) {
		t.Errorf("one prompt body rendered two ways:\n  trunk:  %q\n  branch: %q", trunk, branch)
	}

	// The per-item ref: `{{outputs.spread.it}}` is stamped into the BRANCH's
	// outputs view by the fan_out_each router, never into the trunk's. It
	// resolves only if the snapshot was built from the branch scope — the
	// same scope the expr path already resolved `outputs.*` against.
	items := map[string]bool{}
	for _, p := range recorder.rendered(t, "each_agent", 2) {
		assertPromptResolved(t, "each_agent", p, runID)
		if !strings.Contains(p, "seed=SEEDED") {
			t.Errorf("each_agent prompt did not resolve {{outputs.seed.value}}: %q", p)
		}
		switch {
		case strings.Contains(p, "item=alpha"):
			items["alpha"] = true
		case strings.Contains(p, "item=beta"):
			items["beta"] = true
		default:
			t.Errorf("each_agent prompt resolved no per-item {{outputs.spread.it}}: %q", p)
		}
	}
	if len(items) != 2 {
		t.Errorf("the two fan_out_each bodies rendered %v, want one prompt per item", items)
	}

	assertBranchDispatchHasNoRunID(t, recorder, runID)
}

// assertBranchDispatchHasNoRunID pins the other half of the contract: the
// branch dispatch carries the template snapshot but NOT the ctx run ID.
//
// The two are not interchangeable. A run ID on the executor's context is a
// capability switch — it arms the per-node claw session store, the operator
// inbox drain and the async-ask binder, all keyed `(runID, nodeID)` with no
// branch discriminator. A `fan_out_each` runs N branches concurrently under
// ONE node id, so a run ID there makes item N's generation inherit item M's
// stored messages and lets one arbitrary item swallow a steering message.
// The `run=<id>` assertions above prove the snapshot is enough on its own,
// so nothing is lost by keeping the ID on the trunk — and this is what fails
// if someone "simplifies" the branch path back onto plain execContext.
func assertBranchDispatchHasNoRunID(t *testing.T, recorder *promptRecorder, runID string) {
	t.Helper()
	for _, node := range []string{"trunk_agent", "all_agent", "each_agent"} {
		got := recorder.ctxRunIDs(node)
		if len(got) == 0 {
			t.Fatalf("node %q never dispatched, cannot check its ctx run id", node)
		}
		want := ""
		if node == "trunk_agent" {
			want = runID // the trunk keeps every run-ID-gated feature
		}
		for i, id := range got {
			if id != want {
				t.Errorf("node %q dispatch %d carried ctx run id %q, want %q", node, i, id, want)
			}
		}
	}
}

// dropElapsed blanks the one field that legitimately differs between two
// renderings of the same body: `run.elapsed_seconds` is read when the node
// starts, so two nodes never see the same number.
func dropElapsed(prompt string) string {
	head, rest, ok := strings.Cut(prompt, "elapsed=")
	if !ok {
		return prompt
	}
	_, tail, _ := strings.Cut(rest, " ")
	return head + "elapsed=<n> " + tail
}

func assertPromptResolved(t *testing.T, nodeID, prompt, runID string) {
	t.Helper()
	if strings.Contains(prompt, "{{") {
		t.Errorf("%s prompt kept an unresolved reference: %q", nodeID, prompt)
	}
	if !strings.Contains(prompt, "run="+runID) {
		t.Errorf("%s prompt did not resolve {{run.id}}: %q", nodeID, prompt)
	}
	if _, _, ok := strings.Cut(prompt, "elapsed="); !ok {
		t.Fatalf("%s prompt lost the elapsed field entirely: %q", nodeID, prompt)
	}
	_, rest, _ := strings.Cut(prompt, "elapsed=")
	field, _, _ := strings.Cut(rest, " ")
	assertElapsed(t, nodeID, field)
}

// assertElapsed accepts any non-negative number: `run.elapsed_seconds` is a
// live measurement, so the assertion is that it was MEASURED, not that it
// equals a value.
func assertElapsed(t *testing.T, nodeID string, v any) {
	t.Helper()
	s, ok := v.(string)
	if !ok {
		t.Errorf("%s: run.elapsed_seconds rendered as %T (%v), want a string", nodeID, v, v)
		return
	}
	if s == "" {
		t.Errorf("%s: run.elapsed_seconds rendered empty — the template snapshot never reached this dispatch path", nodeID)
		return
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		t.Errorf("%s: run.elapsed_seconds = %q, want a number", nodeID, s)
		return
	}
	if f < 0 {
		t.Errorf("%s: run.elapsed_seconds = %v, want >= 0", nodeID, f)
	}
}
