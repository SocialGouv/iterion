package runner

import (
	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/usagecap"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/queue"
)

// The ladder's escapes: a node whose PRIMARY rides the capped wire but
// whose computed ladder carries an open stage OFF it is not park-forcing
// — the capped call fails (usage_window, the stage's On carries it), the
// chain falls to the open stage. A claude_code stage or a claw
// anthropic-wire stage is no escape (the same wire, the same cap).
func TestLadderEscapes(t *testing.T) {
	node := func(id, backend, model string) ir.Node {
		return &ir.AgentNode{
			BaseNode:  ir.BaseNode{ID: id},
			LLMFields: ir.LLMFields{Backend: backend, Model: model},
		}
	}
	wf := &ir.Workflow{Nodes: map[string]ir.Node{
		"capped":    node("capped", "claude_code", "claude-opus-5-5"),
		"escaped":   node("escaped", "claude_code", "claude-opus-5-5"),
		"modelless": node("modelless", "claude_code", ""),
		"clawed":    node("clawed", "claw", "anthropic/claude-opus-5-5"),
	}}
	msg := &queue.RunMessage{Fallback: queue.RunFallback{
		{Backend: "codex", Provider: "chatgpt_forfait", Policy: true, On: []string{"usage_window", "auth"}},
		{Backend: "claw", Provider: "zai_key", Policy: true, On: []string{"usage_window"}},
	}}

	got := ladderEscapes(wf, msg, false)
	// The ladder is run-level: every eligible node WITH a servable model
	// carries the codex stage and escapes. A MODELLESS node does not: the
	// materializer drops its stages (StageModel maps nothing modelless),
	// so the park stands for it (R9917f6).
	for _, id := range []string{"capped", "escaped", "clawed"} {
		if !got[id] {
			t.Fatalf("%s did not escape — the codex stage takes every servable node off the capped wire", id)
		}
	}
	if got["modelless"] {
		t.Fatal("modelless escaped — its ladder stages are dropped by the materializer; the park stands")
	}

	// A ladder whose only stages ride the anthropic wire escapes nothing:
	// the capped wire is exactly what those stages spend.
	onWire := &queue.RunMessage{Fallback: queue.RunFallback{
		{Backend: "claude_code", Provider: "anthropic_key", Policy: true},
		{Backend: "claw", Provider: "zai_key", Policy: true},
	}}
	if got := ladderEscapes(wf, onWire, false); len(got) != 0 {
		t.Fatalf("on-wire stages escaped %v — they spend the very wire the cap protects", got)
	}
}

// The F6 acceptance: a hard-capped anthropic wire + a policy ladder stage
// on another wire = the run STARTS (the capped primary fails usage_window,
// the stage's On carries it, the chain serves the open stage) — parking it
// would defeat the selection's own scenario. The SAME stage flagged as an
// OPERATOR rescue stays excluded: the pre-flight's rescue carve-out is
// about operator routes, not about the policy's spend plan.
func TestUsageCapPreflight_PolicyLadderEscapesThePark(t *testing.T) {
	ctx := t.Context()
	caps := usagecap.NewMemStore()
	key := usagecap.Key(delegate.BackendClaudeCode, usagecap.ScopePlatform, "")
	if err := caps.Record(ctx, key, usagecap.Reading{
		Window:      usagecap.WindowSevenDay,
		Utilization: 0.92,
		Status:      usagecap.StatusWarning,
		ResetsAt:    time.Now().UTC().Add(30 * time.Hour),
		ObservedAt:  time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	r := capRunner(capTestPolicy(), caps, &capStatusStore{})
	policyStage := queue.RunFallbackEntry{Backend: "codex", Provider: "chatgpt_forfait", Policy: true, On: []string{"usage_window", "auth"}}
	// A DECLARED-model node: the escape requires the stage's crossing to
	// map (a modelless node's stages are dropped by the materializer — the
	// park stands for it, R9917f6).
	servedWf := func() *ir.Workflow {
		return &ir.Workflow{
			Name:  "served",
			Entry: "think",
			Nodes: map[string]ir.Node{
				"think": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "think"}, LLMFields: ir.LLMFields{Model: "claude-opus-5-5"}},
				"done":  &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
			},
			Edges: []*ir.Edge{{From: "think", To: "done"}},
		}
	}

	if err := r.usageCapPreflight(ctx, servedWf(), &queue.RunMessage{RunID: "run-esc", Fallback: queue.RunFallback{policyStage}}, iterlog.Nop()); err != nil {
		t.Fatalf("the ladder escapes the capped wire — the run must start, got %v", err)
	}
	if err := r.usageCapPreflight(ctx, servedWf(), &queue.RunMessage{RunID: "run-op", Fallback: queue.RunFallback{
		{Backend: "codex", Provider: "chatgpt_forfait", On: []string{"usage_window"}},
	}}, iterlog.Nop()); err == nil {
		t.Fatal("the same stage as an OPERATOR rescue stays excluded — the carve-out is about operator routes")
	}
}
