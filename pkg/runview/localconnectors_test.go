package runview

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/secrets"
)

// A `tool … action:` node resolves through a connector catalog the LAUNCH
// SURFACE hands it. Only `iterion run` and `iterion resume` did: the studio,
// the launch API, a subbot and the dispatcher built their ExecutorSpec without
// it, so the same `.bot`, on the same machine, with the same catalog and the
// same sealer, ran from the CLI and failed at its first action node
// everywhere else.
//
// These pin the answer that is now shared by all of them.

func actionWorkflow() *ir.Workflow {
	return &ir.Workflow{Nodes: map[string]ir.Node{
		"t": &ir.ToolNode{BaseNode: ir.BaseNode{ID: "t"}, Action: "forgejo.issue.comment", Connection: "main"},
	}}
}

func plainWorkflow() *ir.Workflow {
	return &ir.Workflow{Nodes: map[string]ir.Node{
		"t": &ir.ToolNode{BaseNode: ir.BaseNode{ID: "t"}, Command: "go test ./..."},
	}}
}

func TestLocalConnectorsAreWiredForAWorkflowThatCallsOne(t *testing.T) {
	workspace := t.TempDir()
	if err := os.MkdirAll(filepath.Join(workspace, "connectors", "probe"), 0o755); err != nil {
		t.Fatal(err)
	}
	sealer := secrets.NewLazyLocalSealer(t.TempDir(), nil)

	r, client, err := LocalConnectors(actionWorkflow(), workspace, t.TempDir(), sealer)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if r == nil {
		t.Fatal("a workflow declaring `action:` must reach the local catalog from every launch surface, not only the CLI")
	}
	if client == nil {
		t.Error("the resolver must come with its GUARDED client — without one the executor would invent the unguarded default")
	}
}

func TestLocalConnectorsAreNotBuiltWhenNothingWouldUseThem(t *testing.T) {
	workspace := t.TempDir()
	if err := os.MkdirAll(filepath.Join(workspace, "connectors", "probe"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		wf     *ir.Workflow
		sealer secrets.Sealer
	}{
		// Reading the catalog and the connection store for a run that cannot
		// call one is work nobody asked for — the same gate the local secret
		// store follows.
		{"no action node", plainWorkflow(), secrets.NewLazyLocalSealer(t.TempDir(), nil)},
		// No sealer is a CLOUD surface: its connections come from the
		// tenant's store, and a local catalog there would be the pod's.
		{"no sealer", actionWorkflow(), nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, client, err := LocalConnectors(tc.wf, workspace, t.TempDir(), tc.sealer)
			if err != nil {
				t.Fatalf("build: %v", err)
			}
			if r != nil || client != nil {
				t.Errorf("resolver = %v, client = %v, want neither built", r, client)
			}
		})
	}
}

// TestNoCatalogYieldsANilINTERFACE.
//
// The executor asks `e.connectors == nil` to answer "this process has no
// connector catalog wired", which names the fix. A nil *connection.Resolver
// assigned into that interface field is NOT nil, so the run would instead get
// the resolver's own "not wired (catalog or store missing)" — a message that
// points at the connector rather than at the install.
func TestNoCatalogYieldsANilINTERFACE(t *testing.T) {
	// A workspace and a home with no `connectors/` at all, and no store file.
	r, _, err := LocalConnectors(actionWorkflow(), t.TempDir(), t.TempDir(), secrets.NewLazyLocalSealer(t.TempDir(), nil))
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if r != nil {
		t.Errorf("resolver = %#v, want an untyped nil so the executor's own diagnostic speaks", r)
	}
}
