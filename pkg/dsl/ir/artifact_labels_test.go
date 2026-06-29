package ir

import "testing"

const artifactLabelsSrc = `
schema empty:
  ok: bool

prompt sys:
  System.

prompt usr:
  Produce a plan.

agent planner:
  model: "test-model"
  input: empty
  output: empty
  system: sys
  user: usr
  publish: the_plan
  artifact_labels: [plan, strategy]

workflow demo:
  entry: planner
  planner -> done
`

// TestCompileArtifactLabels verifies the DSL artifact_labels list compiles
// onto the node's IR PublishLabels.
func TestCompileArtifactLabels(t *testing.T) {
	w := mustCompile(t, artifactLabelsSrc)
	n := w.Nodes["planner"]
	if n == nil {
		t.Fatal("planner node missing")
	}
	labels := NodePublishLabels(n)
	if len(labels) != 2 || labels[0] != "plan" || labels[1] != "strategy" {
		t.Fatalf("PublishLabels = %v, want [plan strategy]", labels)
	}
}

const artifactLabelsNoPublishSrc = `
schema empty:
  ok: bool

prompt sys:
  System.

prompt usr:
  Do work.

agent worker:
  model: "test-model"
  input: empty
  output: empty
  system: sys
  user: usr
  artifact_labels: [plan]

workflow demo:
  entry: worker
  worker -> done
`

// TestArtifactLabelsWithoutPublish warns (C049) when artifact_labels is set
// but the node has no publish: to attach them to.
func TestArtifactLabelsWithoutPublish(t *testing.T) {
	r := compileFile(t, artifactLabelsNoPublishSrc)
	if !hasDiag(r.Diagnostics, DiagArtifactLabelsNoPublish) {
		t.Fatalf("expected C049 warning, got diagnostics: %+v", r.Diagnostics)
	}
}
