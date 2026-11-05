package model_test

import (
	"context"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/backend/tool"
	"github.com/SocialGouv/iterion/pkg/dispatcher/native/boardops"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// A tool node's `capabilities:` is ITS grant for every recipe that consults
// one (#78=A, ADR-151) — not only for the native action transport. A
// registry recipe naming one of iterion's host tools (`command:
// mcp.iterion_board.create_issue`) is held to the same list: nil inherits the
// workflow's, a declared `capabilities: []` grants nothing (#1801). The board
// tools are registered the way runview registers them — every capability, so
// the whole family exists — and the node's own grant is the only gate.
func TestARegistryRecipeIsHeldToTheToolNodesOwnGrant(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		wfCaps  []string
		nodeCap []string
		allowed bool
	}{
		{"a declared empty list under a granting workflow", []string{"board.read", "board.create"}, []string{}, false},
		{"a declared list without the verb's capability", []string{"board.create"}, []string{"board.read"}, false},
		{"no line inherits the granting workflow", []string{"board.create"}, nil, true},
		{"a declared list opens what the workflow does not", nil, []string{"board.create"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			store := newBoardStore(t)
			reg := tool.NewRegistry()
			if err := tool.RegisterClawBoardTools(reg, &tool.BoardConfig{Store: store, Capabilities: boardops.AllCapabilities()}); err != nil {
				t.Fatalf("RegisterClawBoardTools: %v", err)
			}
			e := model.NewClawExecutor(model.NewRegistry(), &ir.Workflow{Capabilities: tc.wfCaps}, model.WithToolRegistry(reg))
			node := &ir.ToolNode{
				BaseNode:     ir.BaseNode{ID: "file"},
				Command:      "mcp.iterion_board.create_issue",
				Capabilities: tc.nodeCap,
			}
			_, err := e.Execute(context.Background(), node, map[string]any{"title": "filed by a registry recipe"})
			cards := countCards(t, store)
			if tc.allowed {
				if err != nil || cards != 1 {
					t.Fatalf("Execute = %v with %d card(s), want the card filed", err, cards)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), `holds no "board.create" capability`) {
				t.Fatalf("Execute: err = %v, want the grant refusal naming board.create", err)
			}
			if cards != 0 {
				t.Fatalf("a refused node filed %d card(s)", cards)
			}
		})
	}
}
