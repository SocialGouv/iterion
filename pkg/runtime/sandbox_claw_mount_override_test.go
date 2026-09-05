package runtime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/sandbox"
)

// claudeCodeWorkflow is the shape the override matters for: every node
// declares a CLI backend, so the raw IR says "no claw here".
func claudeCodeWorkflow() *ir.Workflow {
	return &ir.Workflow{Nodes: map[string]ir.Node{
		"implement": &ir.AgentNode{
			BaseNode:  ir.BaseNode{ID: "implement"},
			LLMFields: ir.LLMFields{Backend: "claude_code"},
		},
		"verdict": &ir.JudgeNode{
			BaseNode:  ir.BaseNode{ID: "verdict"},
			LLMFields: ir.LLMFields{Backend: "claude_code"},
		},
	}}
}

// executorWithOverrides builds the REAL executor the run would dispatch
// through, so the test exercises the same EffectiveBackendName chain
// production does instead of a hand-rolled stand-in.
func executorWithOverrides(wf *ir.Workflow, o model.ModelOverrides) effectiveBackendResolver {
	return model.NewClawExecutor(model.NewRegistry(), wf, model.WithModelOverrides(o))
}

// `--backend '*=claw'` is applied at dispatch and never folded into the
// IR, so a mount decision taken on the raw node backend leaves a
// claw-routed run with no in-container iterion binary — every node then
// dies on `exec: /usr/local/bin/iterion: no such file or directory`.
func TestContainsClawNode_HonoursTheLaunchBackendOverride(t *testing.T) {
	wf := claudeCodeWorkflow()
	if containsClawNode(wf, nil) {
		t.Fatal("without an override this workflow reaches no claw node")
	}

	var o model.ModelOverrides
	o.SetBackend("*", "claw")
	if !containsClawNode(wf, executorWithOverrides(wf, o)) {
		t.Fatal("under --backend '*=claw' every node runs on claw: the iterion binary MUST be mounted, or the first node dies with `exec: /usr/local/bin/iterion: no such file or directory`")
	}
}

// A selector narrower than "*" is honoured the same way: one claw node is
// enough to need the binary.
func TestContainsClawNode_HonoursAPerNodeBackendOverride(t *testing.T) {
	wf := claudeCodeWorkflow()
	var o model.ModelOverrides
	o.SetBackend("verdict", "claw")
	if !containsClawNode(wf, executorWithOverrides(wf, o)) {
		t.Fatal("a single overridden node is enough: it runs through the in-container claw runner like any other claw node")
	}
}

// The symmetric case: nodes declare claw, the operator overrides them onto
// claude_code. The mount is KEPT — the resolver reads HOST credentials,
// which need not match what the container resolves, and a missing binary
// is a hard mid-run death while a spare read-only bind costs nothing.
func TestContainsClawNode_KeepsTheMountWhenTheOverrideLeavesClaw(t *testing.T) {
	wf := &ir.Workflow{Nodes: map[string]ir.Node{
		"implement": &ir.AgentNode{
			BaseNode:  ir.BaseNode{ID: "implement"},
			LLMFields: ir.LLMFields{Backend: "claw"},
		},
	}}
	var o model.ModelOverrides
	o.SetBackend("*", "claude_code")
	if !containsClawNode(wf, executorWithOverrides(wf, o)) {
		t.Fatal("a declared claw node keeps its mount under an override away from claw: the decision is a UNION, never a narrowing")
	}
}

// The composition that actually matters: the spec the driver receives must
// carry the bind.
func TestAddClawBinaryMount_MountsUnderTheLaunchBackendOverride(t *testing.T) {
	bin := fakeIterionBinary(t)
	t.Setenv("ITERION_BIN", bin)

	wf := claudeCodeWorkflow()
	var o model.ModelOverrides
	o.SetBackend("*", "claw")

	spec := &sandbox.Spec{}
	addClawBinaryMount(spec, wf, executorWithOverrides(wf, o))

	if !hasIterionBinaryMount(spec.Mounts) {
		t.Fatalf("mounts = %v, want a bind at /usr/local/bin/iterion — the claw runner has no binary to exec inside the container", spec.Mounts)
	}
}

// fakeIterionBinary writes an executable file locateHostIterionBinary can
// find through ITERION_BIN, so the mount assertion does not depend on the
// host having iterion installed.
func fakeIterionBinary(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "iterion")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("write fake iterion binary: %v", err)
	}
	return p
}

func hasIterionBinaryMount(mounts []string) bool {
	for _, m := range mounts {
		if strings.Contains(m, "target=/usr/local/bin/iterion") {
			return true
		}
	}
	return false
}
