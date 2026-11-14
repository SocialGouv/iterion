package bots

import (
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
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
// whole surface. The forge-reply skill reads the mounted token file and runs
// its curl GET/POST/VERIFY through the shell, and the scope reads run `git
// diff` there, so the list needs `bash`; `read_file` is how the agent opens
// the skill and the sources it cites. `tools: []`, or a list without either
// one, is declared and still leaves the question unanswered.
func TestReviConverseAgentHoldsTheShellAndTheFileReader(t *testing.T) {
	n := reviConverseAgent(t)
	for _, want := range []string{"bash", "read_file"} {
		if !slices.Contains(n.Tools, want) {
			t.Errorf("converse_agent's tools: %v lack %q — on claw the agent cannot run the forge-reply skill", n.Tools, want)
		}
	}
}

// One answer is a loop of several tool calls: name the changed files, read
// the diff, read the sources it cites, open the forge-reply skill, POST the
// note, then VERIFY it landed by re-fetching the thread. claw bounds an
// unnumbered tool loop at model.DefaultClawToolMaxSteps, which stops the
// loop before the skill's mandatory VERIFY step, so the node must name a
// bound that outlives it; 20 leaves the six steps room for a retry and a
// second source read.
func TestReviConverseAgentToolLoopOutlivesTheClawDefault(t *testing.T) {
	const floor = 20
	if got := reviConverseAgent(t).ToolMaxSteps; got < floor {
		t.Errorf("converse_agent's tool_max_steps = %d, want >= %d — on claw the loop stops at %d steps, before the reply is verified", got, floor, model.DefaultClawToolMaxSteps)
	}
}

// The run's duration budget must outlive the agent's tool loop: a question
// that needs most of tool_max_steps has to finish and post its answer
// before the run is cut. On the team gateway one deepseek step takes
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
