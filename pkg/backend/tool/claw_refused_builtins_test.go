package tool

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/toolcatalog"
)

// TestTheRefusedClawCatalogHoldsOnlyNamesTheRegistryServes binds the
// compile-time list to the registry from the side where the registry is real.
//
// toolcatalog.ClawRefusedBuiltins is what C159 reads to warn an author, and
// the list is EMPTY: `agent` left it when iterion wired the subagent runner at
// both hosts (#1940). An empty list makes every loop over it vacuous — "N
// entries seen refusing N examples" with N = 0 — so the invariant a future
// entry must satisfy is exercised through an injected one: the name must be a
// catalog builtin and must RESOLVE in the registry every run builds, because
// a refusal is for a tool the registry HOLDS (otherwise C135 rejects the name
// and C159 has nothing to add).
func TestTheRefusedClawCatalogHoldsOnlyNamesTheRegistryServes(t *testing.T) {
	production := toolcatalog.ClawRefusedBuiltins()
	if len(production) != 0 {
		t.Errorf("the refused list carries %v; #1940 emptied it, and a name here warns about a tool that works", production)
	}

	const probe = "web_search"
	restore := toolcatalog.SetClawRefusedBuiltinForTest(probe, toolcatalog.ClawRefusal{
		Reason: "a probe reason", Remedy: "A probe remedy",
	})
	defer restore()

	reg := NewRegistry()
	if err := RegisterClawAll(reg, ClawDefaults{IncludeWebSearch: true}); err != nil {
		t.Fatalf("RegisterClawAll: %v", err)
	}
	names := toolcatalog.ClawRefusedBuiltins()
	if len(names) == 0 {
		t.Fatal("the injection did not take, so this guard would pass on an empty list, proving nothing")
	}
	for _, name := range names {
		if !toolcatalog.IsBuiltin(name) {
			t.Errorf("%q is listed as refused but is not in the catalog — C159 would stay silent and C135 would reject the name", name)
			continue
		}
		if _, resolveErr := reg.Resolve(name); resolveErr != nil {
			t.Errorf("%q is listed as refused but does not resolve: %v — a refusal is for a tool the registry HOLDS", name, resolveErr)
		}
	}
}

// TestTheUnwiredSubagentRefusalCarriesTheHeadlineTheGrantReadsOn is the
// run-time half, and the one case that exists: a host that wires no subagent
// runner. The ultracode grant tells the model a refusal is final where the
// tool ANSWERS the headline
// (delegate.ultracodeOrchestrationInstruction); a refusal whose error omits it
// leaves that instruction keyed on words the model never receives, and the
// retry loop the grant exists to kill comes back.
func TestTheUnwiredSubagentRefusalCarriesTheHeadlineTheGrantReadsOn(t *testing.T) {
	reg := NewRegistry()
	if err := RegisterClawAll(reg, ClawDefaults{}); err != nil {
		t.Fatalf("RegisterClawAll: %v", err)
	}
	td, err := reg.Resolve("agent")
	if err != nil {
		t.Fatalf("resolve agent: %v", err)
	}
	out, execErr := td.Execute(context.Background(), json.RawMessage(`{}`))
	var unsupported *delegate.ErrCapabilityUnsupported
	if !errors.As(execErr, &unsupported) {
		t.Fatalf("an unwired `agent` returned (%q, %v); want the typed capability refusal", out, execErr)
	}
	refusal := toolcatalog.ClawUnwiredSubagentRefusal
	if !strings.Contains(unsupported.Error(), refusal.Remedy) {
		t.Errorf("the refusal does not carry the remedy %q: %s", refusal.Remedy, unsupported.Error())
	}
	if !strings.Contains(unsupported.Error(), toolcatalog.ClawRefusedToolHeadline) {
		t.Errorf("the refusal does not carry the headline %q the ultracode grant conditions on: %s",
			toolcatalog.ClawRefusedToolHeadline, unsupported.Error())
	}
}
