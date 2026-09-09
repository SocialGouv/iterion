package store

import (
	"testing"
)

func TestExecutionContextNormalizeSortsAndRejectsDuplicates(t *testing.T) {
	c := &ExecutionContext{
		RunStore: ContextRef{ID: "run", Kind: "filesystem"},
		BusinessStores: []ContextRef{
			{ID: "z", Kind: "mongo", Namespace: "tenant-b"},
			{ID: "a", Kind: "mongo", Namespace: "tenant-a"},
		},
		Workspace: WorkspaceContext{Mode: WorkspaceIsolated},
	}
	if err := c.Normalize(); err != nil {
		t.Fatal(err)
	}
	if c.Version != ExecutionContextVersion || c.Policy != ContextPolicyLegacy {
		t.Fatalf("defaults not applied: version=%d policy=%q", c.Version, c.Policy)
	}
	if got := c.BusinessStores[0].ID; got != "a" {
		t.Fatalf("business stores not canonicalized: first id=%q", got)
	}

	c.BusinessStores = append(c.BusinessStores, c.BusinessStores[0])
	if err := c.Normalize(); err == nil {
		t.Fatal("duplicate business store accepted")
	}
}

func TestExecutionContextNormalizeRejectsUnknownValues(t *testing.T) {
	tests := []struct {
		name string
		ctx  ExecutionContext
	}{
		{name: "version", ctx: ExecutionContext{Version: 99, RunStore: ContextRef{ID: "r", Kind: "fs"}}},
		{name: "policy", ctx: ExecutionContext{Policy: "unknown", RunStore: ContextRef{ID: "r", Kind: "fs"}}},
		{name: "workspace", ctx: ExecutionContext{RunStore: ContextRef{ID: "r", Kind: "fs"}, Workspace: WorkspaceContext{Mode: "unknown"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.ctx.Normalize(); err == nil {
				t.Fatal("invalid context accepted")
			}
		})
	}
}

func TestExecutionContextCloneDetachesStores(t *testing.T) {
	original := &ExecutionContext{
		RunStore:       ContextRef{ID: "run", Kind: "fs"},
		BusinessStores: []ContextRef{{ID: "biz", Kind: "mongo"}},
	}
	clone := original.Clone()
	clone.BusinessStores[0].ID = "changed"
	if original.BusinessStores[0].ID != "biz" {
		t.Fatalf("clone shares business store slice: %+v", original.BusinessStores)
	}
}
