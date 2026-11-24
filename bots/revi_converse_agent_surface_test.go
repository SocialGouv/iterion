package bots

import (
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/unit"
	"github.com/SocialGouv/iterion/pkg/dsl/unit/unittest"
)

// reviConverseAgent is revi-converse's converse_agent as the compiler reads
// it, imports included.
func reviConverseAgent(t *testing.T) *ir.AgentNode {
	t.Helper()
	wf := compileBotFile(t, filepath.Join(botsDir(t), "revi-converse", "main.bot"))
	raw, ok := wf.Nodes["converse_agent"]
	if !ok {
		t.Fatal("revi-converse has no converse_agent node")
	}
	n, ok := raw.(*ir.AgentNode)
	if !ok {
		t.Fatalf("converse_agent is %T, want *ir.AgentNode", raw)
	}
	return n
}

// The converse agent answers through tools it must actually hold, not merely
// declare: a routing cell moves it onto claw, where the declared list is the
// whole surface. The scope reads run `git diff` through the shell, as does
// the read-only thread GET when the transcript is missing, so the list needs
// `bash`; `read_file` is how the agent opens the sources it cites. Posting
// is not the agent's any more (publish_reply posts the draft), so neither
// tool is held for a write. `tools: []`, or a list without either one, is
// declared and still leaves the question answered from nothing.
func TestReviConverseAgentHoldsTheShellAndTheFileReader(t *testing.T) {
	n := reviConverseAgent(t)
	for _, want := range []string{"bash", "read_file"} {
		if !slices.Contains(n.Tools, want) {
			t.Errorf("converse_agent's tools: %v lack %q — on claw the agent cannot read the diff it answers about", n.Tools, want)
		}
	}
}

// One answer is a loop of several tool calls — reads the SCOPE block
// (converse_scope, the entry node) does not cover: surrounding code, the
// sources a claim cites, the thread when the transcript is missing. claw
// bounds an unnumbered tool loop at model.DefaultClawToolMaxSteps, which a
// question needing a few source reads outruns, so the node names its own
// bound: 20, the measured converse shape with the scope precomputed (M2a:
// 13-14 steps on a small thread, 49-61 on a question the scope now answers
// up front), kept inside the [20,24] window the cost plan re-cuts within —
// a raise past 24 quietly re-inflates the per-question cost the bound
// exists to cap. Running short costs the answer some grounding, never its
// publication: the draft is posted by the node after, and whatever the
// bound cut short rides converse_output.unchecked into the thread.
func TestReviConverseAgentToolLoopOutlivesTheClawDefault(t *testing.T) {
	const floor, ceiling = 20, 24
	if got := reviConverseAgent(t).ToolMaxSteps; got < floor || got > ceiling {
		t.Errorf("converse_agent's tool_max_steps = %d, want within [%d,%d] — below %d the reads outrun the loop (claw's default is %d), above %d the per-question cost cap quietly inflates", got, floor, ceiling, floor, model.DefaultClawToolMaxSteps, ceiling)
	}
}

// The system prompt is the byte-stable half of the cross-run prefix (#3283
// S3): the provider's prefix cache serves the next run from this one's
// bytes, which cut at the first run-specific byte — a {{vars.*}} in the
// system prompt IS that byte, on every run. The workspace path, the base
// ref and every per-run fact live in the user message; the system prompt
// carries the mission only. (The assembled-request byte comparison runs in
// pkg/backend/model; this guard reads the template the way the author
// wrote it, so a var that sneaks back in is named at its source.)
func TestReviConverseSystemPromptCarriesNoRunSpecificText(t *testing.T) {
	u := unittest.Whole(unit.LoadDir(filepath.Join(botsDir(t), "revi-converse", "main.bot")))
	var systemBody string
	for _, f := range u.Files {
		if m := regexp.MustCompile(`(?s)prompt converse_system:
(.*?)

prompt `).FindSubmatch(f.Source); m != nil {
			systemBody = string(m[1])
			break
		}
	}
	if systemBody == "" {
		t.Fatal("converse_system not found in the bundle source")
	}
	if hits := regexp.MustCompile(`\{\{[^}]*\}\}`).FindAllString(systemBody, -1); len(hits) > 0 {
		for _, hit := range hits {
			t.Errorf("converse_system carries the template %s — run-specific bytes cut the cross-run prefix cache; move it to the user message", hit)
		}
	}
	if !strings.Contains(systemBody, "SCOPE") {
		t.Error("converse_system no longer teaches the precomputed SCOPE block")
	}
}

// The run's duration budget must outlive the agent's tool loop: a question
// that needs most of tool_max_steps has to finish its draft, and the
// publish step after it has to run, before the run is cut. On the team gateway one deepseek step takes
// ~10-15 s and a step with its tool calls up to ~30 s; at 10 minutes, a
// question that needed ~50 steps died on its budget with nothing posted
// (VAO !48, 2026-11-14). The floor is the step bound at 30 s a step.
func TestReviConverseBudgetOutlivesTheToolLoop(t *testing.T) {
	const perStep = 30 * time.Second
	wf := compileBotFile(t, filepath.Join(botsDir(t), "revi-converse", "main.bot"))
	if wf.Budget == nil || wf.Budget.MaxDuration == "" {
		t.Fatal("revi-converse declares no max_duration")
	}
	got, err := time.ParseDuration(wf.Budget.MaxDuration)
	if err != nil {
		t.Fatalf("revi-converse max_duration %q: %v", wf.Budget.MaxDuration, err)
	}
	steps := reviConverseAgent(t).ToolMaxSteps
	if want := time.Duration(steps) * perStep; got < want {
		t.Errorf("revi-converse max_duration = %s, want >= %s (tool_max_steps %d at %s a step) — a long question dies on its budget before it posts", got, want, steps, perStep)
	}
}
