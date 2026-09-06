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
type promptRecorder struct {
	mu      sync.Mutex
	prompts map[string][]string
}

func newPromptRecorder() *promptRecorder {
	return &promptRecorder{prompts: make(map[string][]string)}
}

func (p *promptRecorder) Execute(_ context.Context, task delegate.Task) (delegate.Result, error) {
	p.mu.Lock()
	p.prompts[task.NodeID] = append(p.prompts[task.NodeID], task.UserPrompt)
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
	// command runs with an argument nobody notices is missing. An
	// `{{outputs.*}}` ref the command resolver did not know (#797) survived
	// the other way — as the literal braces, handed to sh -c as an argument.
	// Both are asserted on every dispatch path.
	toolItems := map[string]bool{}
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
			if got := out["seed"]; got != "SEEDED" {
				t.Errorf("tool %q call %d: command rendered {{outputs.seed.value}} as %q, want SEEDED", probe.node, i, got)
			}
			if probe.node == "each_probe" {
				// The per-item binding lives only in the branch's own
				// outputs view: resolving it proves the command was
				// rendered from the branch snapshot, like the prompt is.
				switch it := out["item"]; it {
				case "alpha", "beta":
					toolItems[it.(string)] = true
				default:
					t.Errorf("each_probe call %d: command rendered {{outputs.spread.it}} as %q, want alpha or beta", i, it)
				}
			}
		}
	}
	if len(toolItems) != 2 {
		t.Errorf("the two fan_out_each tool bodies rendered %v, want one command per item", toolItems)
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
