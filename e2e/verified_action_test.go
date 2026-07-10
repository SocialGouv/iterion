package e2e

import (
	"context"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/store"
)

// compileSource compiles an inline .bot source for engine-level tests.
func compileSource(t *testing.T, name, src string) *ir.Workflow {
	t.Helper()
	pr := parser.Parse(name, src)
	cr := ir.Compile(pr.File)
	if cr.HasErrors() {
		for _, d := range cr.Diagnostics {
			t.Logf("compile diagnostic: %s", d.Error())
		}
		t.Fatalf("compilation errors for %s", name)
	}
	return cr.Workflow
}

// Engine integration for the Verified Action ladder (ADR-044): when the
// executor returns the private `_verified_action` metadata, the engine emits
// a node_verified_action event and strips the key before the output reaches
// the persisted artifact / downstream refs. The ladder itself is unit-tested
// in pkg/backend/model; this pins the engine glue with the stub executor.
func TestVerifiedActionEngineEmitsAndStrips(t *testing.T) {
	src := `tool commit_changes:
  command: "echo wip"
  postcondition: "git rev-parse HEAD"
  policy: recover
  publish: commit_result

workflow w:
  entry: commit_changes
  commit_changes -> done
`
	wf := compileSource(t, "verified_action.bot", src)
	exec := newScenarioExecutor()
	// Simulate the real executor: the node escalated to self-repair and the
	// postcondition then held.
	exec.on("commit_changes", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{
			"sha":       "abc123",
			"_tokens":   10,
			"_cost_usd": 0.0,
			"_verified_action": map[string]any{
				"rung":              "self_repair",
				"postcondition_met": true,
				"policy":            "recover",
			},
		}, nil
	})

	s := tmpStore(t)
	eng := runtime.New(wf, s, exec)
	if err := eng.Run(context.Background(), "e2e-va", nil); err != nil {
		t.Fatalf("Run: %v", err)
	}

	r, _ := s.LoadRun(context.Background(), "e2e-va")
	if r.Status != store.RunStatusFinished {
		t.Fatalf("status = %s, want finished", r.Status)
	}

	events, _ := s.LoadEvents(context.Background(), "e2e-va")
	if !hasEvent(events, store.EventNodeVerifiedAction) {
		t.Fatal("missing node_verified_action event")
	}
	// The event must carry the satisfying rung.
	var found bool
	for _, ev := range events {
		if ev.Type == store.EventNodeVerifiedAction {
			if ev.Data["rung"] == "self_repair" && ev.Data["postcondition_met"] == true {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("node_verified_action event missing rung/postcondition_met fields")
	}

	// The published artifact must NOT contain the private _verified_action key.
	art, err := s.LoadArtifact(context.Background(), "e2e-va", "commit_changes", 0)
	if err != nil {
		t.Fatalf("load artifact: %v", err)
	}
	if _, present := art.Data["_verified_action"]; present {
		t.Fatal("_verified_action leaked into the persisted artifact (not stripped)")
	}
	if art.Data["sha"] != "abc123" {
		t.Fatalf("artifact sha = %v, want abc123", art.Data["sha"])
	}
}
