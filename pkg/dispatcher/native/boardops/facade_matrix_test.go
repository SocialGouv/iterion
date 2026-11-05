package boardops

import (
	"context"
	"errors"
	"fmt"
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
	}
	// The act ladder, pinned per verb rather than derived: the retry and
	// resume decisions read the effect, so a verb quietly reclassified is a
	// behaviour change nobody reviewed.
	wantEffects := map[string]spec.Effect{
		"board.issue.get":        spec.EffectRead,
		"board.issue.list":       spec.EffectRead,
		"board.issue.create":     spec.EffectCreate,
		"board.issue.label":      spec.EffectUpdate,
		"board.issue.transition": spec.EffectUpdate,
	}
	for id, want := range wantEffects {
		op, ok := pkg.Operation(id)
		if !ok {
			t.Errorf("the facade no longer exposes %q — a verb's removal is a facade Version bump and a matrix edit, not a silent diff", id)
			continue
		}
		if op.Effect != want {
			t.Errorf("%s: effect = %q, want %q", id, op.Effect, want)
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
// regime beside the MCP ones. The compile-time half of that gate reads
// boardcaps.ActionCapability (C334), so this test holds ALL THREE tables to
// each other: binding, capability vocabulary, native registry.
func TestFacadeExposedVerbsKeepTheToolsCapability(t *testing.T) {
	for actionID, toolName := range actionBindings {
		tool := toolByName[toolName]
		if tool == nil {
			t.Fatalf("action %q binds to unknown tool %q", actionID, toolName)
		}
		if boardcaps.ToolCapability[toolName] != tool.Capability {
			t.Errorf("tool %q: boardcaps pins %q but allTools pins %q — the two halves of the vocabulary drifted", toolName, boardcaps.ToolCapability[toolName], tool.Capability)
		}
		cap, ok := boardcaps.ActionCapability[actionID]
		if !ok {
			t.Errorf("action %q has no entry in boardcaps.ActionCapability — the compiler cannot warn when its grant is missing (C334)", actionID)
			continue
		}
		if cap != tool.Capability {
			t.Errorf("action %q: boardcaps.ActionCapability pins %q but the native tool gates on %q — C334 would name the wrong grant", actionID, cap, tool.Capability)
		}
	}
	// And the loop BACK: a table entry naming no exposed action is a stale
	// one — left behind by a verb the facade dropped — and C334 would then
	// name a grant for an action nothing serves. The same discipline as the
	// exemptions above: one table is one truth in BOTH directions.
	for actionID := range boardcaps.ActionCapability {
		if _, exposed := actionBindings[actionID]; !exposed {
			t.Errorf("boardcaps.ActionCapability names %q, which the facade does not expose — a stale entry survives in silence and C334 warns for a verb that does not exist", actionID)
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
		_, err := transport.Call(context.Background(), spec.Operation{ID: "board.issue.nuke"}, map[string]any{}, []string{CapBoardCreate})
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

// failStore wraps a real store and fails each mutating method on demand —
// the witness of the facade's effect honesty: a RETURNED ERROR means NO
// EFFECT, which is what makes the engine's one generic retry of a failed
// native action safe against a duplicated card.
type failStore struct {
	native.BoardStore
	failCreate, failSetState, failAdjust bool
}

func (f *failStore) Create(in native.Issue) (*native.Issue, error) {
	if f.failCreate {
		return nil, errors.New("injected store failure")
	}
	return f.BoardStore.Create(in)
}

func (f *failStore) SetState(id, newState string) (*native.Issue, error) {
	if f.failSetState {
		return nil, errors.New("injected store failure")
	}
	return f.BoardStore.SetState(id, newState)
}

func (f *failStore) AdjustLabels(id string, add, remove []string) (*native.Issue, bool, error) {
	if f.failAdjust {
		return nil, false, errors.New("injected store failure")
	}
	return f.BoardStore.AdjustLabels(id, add, remove)
}

// boardSnapshot is the board's content reduced to what a mutating verb can
// change: the cards' ids, states and labels.
func boardSnapshot(t *testing.T, store native.BoardStore) string {
	t.Helper()
	issues, err := store.List(native.ListFilter{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	var b strings.Builder
	for _, iss := range issues {
		fmt.Fprintf(&b, "%s|%s|%v;", iss.ID, iss.State, iss.Labels)
	}
	return b.String()
}

func TestAFailedMutatingVerbHasNoEffect(t *testing.T) {
	pkg := FacadePackage()
	caps := []string{CapBoardRead, CapBoardCreate, CapBoardLabel, CapBoardMove}

	t.Run("create", func(t *testing.T) {
		base := newStore(t)
		transport := NewActionTransport(&failStore{BoardStore: base, failCreate: true}, CallEnv{})
		op, _ := pkg.Operation("board.issue.create")
		_, err := transport.Call(context.Background(), op, map[string]any{"title": "never filed"}, caps)
		if err == nil {
			t.Fatal("want the store's failure")
		}
		if got := boardSnapshot(t, base); got != "" {
			t.Fatalf("a failed create left an effect: %s", got)
		}
	})

	t.Run("transition", func(t *testing.T) {
		base := newStore(t)
		iss, err := base.Create(native.Issue{Title: "stays put"})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		before := boardSnapshot(t, base)
		transport := NewActionTransport(&failStore{BoardStore: base, failSetState: true}, CallEnv{})
		op, _ := pkg.Operation("board.issue.transition")
		_, err = transport.Call(context.Background(), op, map[string]any{"id": iss.ID, "to": "done"}, caps)
		if err == nil {
			t.Fatal("want the store's failure")
		}
		if got := boardSnapshot(t, base); got != before {
			t.Fatalf("a failed transition moved the card: %s → %s", before, got)
		}
	})

	t.Run("label", func(t *testing.T) {
		base := newStore(t)
		iss, err := base.Create(native.Issue{Title: "stays unlabeled"})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		before := boardSnapshot(t, base)
		transport := NewActionTransport(&failStore{BoardStore: base, failAdjust: true}, CallEnv{})
		op, _ := pkg.Operation("board.issue.label")
		_, err = transport.Call(context.Background(), op, map[string]any{"id": iss.ID, "labels": []any{"never"}}, caps)
		if err == nil {
			t.Fatal("want the store's failure")
		}
		if got := boardSnapshot(t, base); got != before {
			t.Fatalf("a failed label adjusted the card: %s → %s", before, got)
		}
	})
}

// And the happy path of each mutating verb through the facade: the capability
// the verb names is the opt-in, and the call reaches the store verbatim.
func TestMutatingVerbsThroughTheFacade(t *testing.T) {
	base := newStore(t)
	transport := NewActionTransport(base, CallEnv{})
	pkg := FacadePackage()
	ctx := context.Background()

	t.Run("create then label then transition", func(t *testing.T) {
		create, _ := pkg.Operation("board.issue.create")
		data, err := transport.Call(ctx, create, map[string]any{
			"title":  "Filed by the facade",
			"body":   "the body",
			"labels": []any{"source:rgaa-audit"},
		}, []string{CapBoardCreate})
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		card := data.(map[string]any)
		id, _ := card["id"].(string)
		if id == "" {
			t.Fatalf("created card carries no id: %v", card)
		}

		label, _ := pkg.Operation("board.issue.label")
		if _, err := transport.Call(ctx, label, map[string]any{"id": id, "labels": []any{"criterion:1.1"}}, []string{CapBoardLabel}); err != nil {
			t.Fatalf("label: %v", err)
		}

		transition, _ := pkg.Operation("board.issue.transition")
		if _, err := transport.Call(ctx, transition, map[string]any{"id": id, "to": "done"}, []string{CapBoardMove}); err != nil {
			t.Fatalf("transition: %v", err)
		}

		got, err := base.Get(id)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if got.State != "done" {
			t.Errorf("state = %q, want done", got.State)
		}
		labels := strings.Join(got.Labels, ",")
		for _, want := range []string{"source:rgaa-audit", "criterion:1.1"} {
			if !strings.Contains(labels, want) {
				t.Errorf("labels %q miss %q", labels, want)
			}
		}
	})

	t.Run("each mutating verb is denied without its own capability", func(t *testing.T) {
		// A15's check-only default: a read grant opens no write.
		create, _ := pkg.Operation("board.issue.create")
		if _, err := transport.Call(ctx, create, map[string]any{"title": "x"}, []string{CapBoardRead}); err == nil ||
			!strings.Contains(err.Error(), "board.create") {
			t.Fatalf("create under board.read: err = %v, want the denial naming board.create", err)
		}
	})
}
