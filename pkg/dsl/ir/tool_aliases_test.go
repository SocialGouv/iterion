package ir

import (
	"reflect"
	"strings"
	"testing"
)

func TestToolAliasesAreNotLoweredBeforeAmbientMCPDiscovery(t *testing.T) {
	cr := compileToolsSrc(t, toolsWorkflow("  backend: claw\n  tools: [Read, Bash, Grep]\n  tool_policy: [Read, Bash, Grep]\n"))
	if cr.HasErrors() {
		t.Fatalf("alias authoring blocked before registry: %v", cr.Diagnostics)
	}
	node := cr.Workflow.Nodes["x"].(*AgentNode)
	want := []string{"Read", "Bash", "Grep"}
	if !reflect.DeepEqual(node.Tools, want) || !reflect.DeepEqual(node.ToolPolicy, want) {
		t.Fatalf("premature lowering: %+v", node)
	}
	for _, d := range cr.Diagnostics {
		if d.Code == DiagUnknownTool && !strings.Contains(d.Message, "requires.iterion") {
			t.Errorf("missing opt-in remedy: %s", d.Message)
		}
	}
}

func TestCapabilitiesRemainRightsRatherThanToolAliases(t *testing.T) {
	cr := compileToolsSrc(t, toolsWorkflow("  backend: claw\n  tools: [read_file]\n  capabilities: [Read]\n"))
	if len(diagsFor(cr, DiagMalformedCapability)) != 1 {
		t.Fatalf("Read became an invented capability: %v", cr.Diagnostics)
	}
	cr = compileToolsSrc(t, toolsWorkflow("  backend: claw\n  tools: [Read]\n  capabilities: [board.read, runs.read]\n"))
	if cr.HasErrors() {
		t.Fatalf("host rights changed: %v", cr.Diagnostics)
	}
}
