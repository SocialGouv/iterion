package boardops

import (
	"context"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/boardcaps"
	"github.com/SocialGouv/iterion/pkg/connector/spec"
	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
)

// The facade's genericity is a TWO-WAY matrix (docs/adr/151 §4): every
// exposed action id maps to a native tool, and every native tool is either
// exposed or exempted WITH its reason. A drift either way fails here rather
// than becoming a catalog hole.

func TestFacadeEveryExposedActionMapsToANativeTool(t *testing.T) {
	pkg := FacadePackage()
	if pkg.Connector.ID != ConnectorID {
		t.Fatalf("facade connector id = %q, want %q", pkg.Connector.ID, ConnectorID)
	}
	ops := pkg.Operations()
	if len(ops) == 0 {
		t.Fatal("facade exposes no operation")
	}
	if len(ops) != len(actionBindings) {
		t.Fatalf("facade exposes %d operations but actionBindings holds %d — the two halves of the facade drifted", len(ops), len(actionBindings))
	}
	for _, op := range ops {
		toolName, ok := actionBindings[op.ID]
		if !ok {
			t.Errorf("operation %q is exposed but actionBindings maps no native tool for it", op.ID)
			continue
		}
		if _, known := toolByName[toolName]; !known {
			t.Errorf("operation %q binds to %q, which allTools does not declare", op.ID, toolName)
		}
		// The action id's first segment is the connector's — an operation
		// published under another package's namespace would be unaddressable
		// and unrefusable at once.
		if !strings.HasPrefix(op.ID, ConnectorID+".") {
			t.Errorf("operation %q sits outside the %q namespace", op.ID, ConnectorID)
		}
		// The executor's own usability gate is what the facade promises:
		// deterministic, at an attachable maturity, with a KNOWN effect —
		// checked here against the real predicate rather than trusted.
		if !op.Deterministic {
			t.Errorf("operation %q is not marked deterministic, and `action:` is the recipe that certifies it is", op.ID)
		}
		if m := pkg.EffectiveMaturity(op); !m.Attachable() {
			t.Errorf("operation %q resolves to maturity %q, which may not be bound to a node", op.ID, m)
		}
		if !op.Effect.Known() {
			t.Errorf("operation %q has unknown effect %q — the act ladder reads it as mutating, so a typo fails closed everywhere but here", op.ID, op.Effect)
		}
		if op.Effect.Mutating() {
			t.Errorf("operation %q is exposed with mutating effect %q — the write verbs are the second lot, gated by the capability opt-in (#78=A)", op.ID, op.Effect)
		}
	}
}

func TestFacadeEveryNativeToolIsExposedOrExemptedWithReason(t *testing.T) {
	exposed := map[string]bool{}
	for _, toolName := range actionBindings {
		exposed[toolName] = true
	}
	for _, tool := range allTools {
		if exposed[tool.Name] {
			if reason, exempted := nativeExemptions[tool.Name]; exempted {
				t.Errorf("native tool %q is BOTH exposed (actionBindings) and exempted (%q) — one tool is one thing", tool.Name, reason)
			}
			continue
		}
		reason, ok := nativeExemptions[tool.Name]
		if !ok {
			t.Errorf("native tool %q is neither exposed as an action nor exempted — add the binding or write the reason", tool.Name)
			continue
		}
		if strings.TrimSpace(reason) == "" {
			t.Errorf("native tool %q carries an EMPTY exemption reason — 'not exposed' without the why is the catalog hole this matrix exists to prevent", tool.Name)
		}
	}
	// And the other direction of the second table: an exemption that names
	// no native tool is a stale entry, kept after its tool was renamed or
	// dropped.
	for name := range nativeExemptions {
		if _, known := toolByName[name]; !known {
			t.Errorf("nativeExemptions names %q, which allTools does not declare — a stale exemption reads as coverage", name)
		}
	}
}

// The facade's verbs gate on the SAME capability the tool names — the one
// authorization surface #78=A decided: an action transport adds no second
// regime beside the MCP ones.
func TestFacadeExposedVerbsKeepTheToolsCapability(t *testing.T) {
	for actionID, toolName := range actionBindings {
		tool := toolByName[toolName]
		if tool == nil {
			t.Fatalf("action %q binds to unknown tool %q", actionID, toolName)
		}
		if boardcaps.ToolCapability[toolName] != tool.Capability {
			t.Errorf("tool %q: boardcaps pins %q but allTools pins %q — the two halves of the vocabulary drifted", toolName, boardcaps.ToolCapability[toolName], tool.Capability)
		}
	}
}

func TestActionTransportCall(t *testing.T) {
	store := newStore(t)
	iss, err := store.Create(native.Issue{Title: "Triaged ticket"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	transport := NewActionTransport(store, CallEnv{})
	if transport.Connector() != ConnectorID {
		t.Fatalf("Connector() = %q, want %q", transport.Connector(), ConnectorID)
	}
	pkg := transport.Package()
	if pkg == nil {
		t.Fatal("Package() = nil")
	}

	t.Run("list with the read grant", func(t *testing.T) {
		op, ok := pkg.Operation("board.issue.list")
		if !ok {
			t.Fatal("facade exposes no board.issue.list")
		}
		data, err := transport.Call(context.Background(), op, map[string]any{}, []string{CapBoardRead})
		if err != nil {
			t.Fatalf("Call: %v", err)
		}
		items, ok := data.([]any)
		if !ok {
			t.Fatalf("list result is %T, want an array", data)
		}
		if len(items) != 1 {
			t.Fatalf("list returned %d issues, want 1", len(items))
		}
	})

	t.Run("get by id", func(t *testing.T) {
		op, _ := pkg.Operation("board.issue.get")
		data, err := transport.Call(context.Background(), op, map[string]any{"id": iss.ID}, []string{CapBoardRead})
		if err != nil {
			t.Fatalf("Call: %v", err)
		}
		card, ok := data.(map[string]any)
		if !ok {
			t.Fatalf("get result is %T, want an object", data)
		}
		if card["title"] != "Triaged ticket" {
			t.Errorf("title = %v", card["title"])
		}
	})

	t.Run("no grant is the same refusal every transport gives", func(t *testing.T) {
		op, _ := pkg.Operation("board.issue.list")
		_, err := transport.Call(context.Background(), op, map[string]any{}, nil)
		if err == nil || !strings.Contains(err.Error(), ErrCapabilityDenied.Error()) {
			t.Fatalf("Call without board.read: err = %v, want the capability denial", err)
		}
	})

	t.Run("an undeclared parameter is refused by name", func(t *testing.T) {
		op, _ := pkg.Operation("board.issue.list")
		_, err := transport.Call(context.Background(), op, map[string]any{"stats": "x"}, []string{CapBoardRead})
		if err == nil || !strings.Contains(err.Error(), `"stats"`) {
			t.Fatalf("Call with an undeclared param: err = %v, want a refusal naming it", err)
		}
	})

	t.Run("an action id with no binding is told, not dispatched", func(t *testing.T) {
		_, err := transport.Call(context.Background(), spec.Operation{ID: "board.issue.create"}, map[string]any{}, []string{CapBoardCreate})
		if err == nil || !strings.Contains(err.Error(), "no native binding") {
			t.Fatalf("Call on an unbound id: err = %v", err)
		}
	})

	t.Run("a nil store fails explicitly", func(t *testing.T) {
		bare := NewActionTransport(nil, CallEnv{})
		op, _ := pkg.Operation("board.issue.list")
		_, err := bare.Call(context.Background(), op, map[string]any{}, []string{CapBoardRead})
		if err == nil || !strings.Contains(err.Error(), "no store") {
			t.Fatalf("Call on a store-less transport: err = %v", err)
		}
	})
}
