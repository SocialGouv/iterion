package bots

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// TestReviewPRGateCarriesTheMergeTelemetryThrough pins the one link of the
// merge step's telemetry that no edge mapping shows: pr_gate, the node both
// merge twins feed, must carry every `ai_*` field of its output through from
// its input unchanged. TestReviewPRPublishEdgeMapsTheServingTelemetry checks that each
// twin maps its engine keys into pr_gate and that publish_review reads them
// back from pr_gate; a field pr_gate computed some other way, or forgot, would
// print an empty or a made-up merge row — its model, its harness, or the
// effort it ran at (#3283).
func TestReviewPRGateCarriesTheMergeTelemetryThrough(t *testing.T) {
	wf := compileBotFile(t, filepath.Join(botsDir(t), "review-pr", "main.bot"))
	gate, ok := wf.Nodes["pr_gate"].(*ir.ComputeNode)
	if !ok {
		t.Fatalf("pr_gate is %T, want a compute node", wf.Nodes["pr_gate"])
	}
	exprs := map[string]string{}
	for _, x := range gate.Exprs {
		exprs[x.Key] = strings.TrimSpace(x.Raw)
	}
	schema := wf.Schemas[gate.OutputSchema]
	if schema == nil {
		t.Fatalf("pr_gate's output schema %q is not declared", gate.OutputSchema)
	}
	judged := 0
	for _, f := range schema.Fields {
		if !strings.HasPrefix(f.Name, "ai_") {
			continue
		}
		judged++
		if want := "input." + f.Name; exprs[f.Name] != want {
			t.Errorf("pr_gate computes %s = %q, want %q — the merge row must be the twin's own telemetry", f.Name, exprs[f.Name], want)
		}
	}
	if judged == 0 {
		t.Fatal("pr_gate's output declares no ai_* field — the check judges nothing")
	}
}
