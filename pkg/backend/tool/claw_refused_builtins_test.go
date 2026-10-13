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

// TestEveryRefusedClawBuiltinActuallyRefuses binds the compile-time list to
// the run-time behaviour, from the side where the registry is real.
//
// toolcatalog.ClawRefusedBuiltins is what C159 reads to warn an author. Both
// directions of drift are silent in production: a name left on the list after
// its capability became real makes the compiler warn about a tool that works,
// and a capability that quietly stopped working leaves the list saying
// nothing. The registry is built the way every run builds it — RegisterClawAll
// over a ClawDefaults no host filled in.
func TestEveryRefusedClawBuiltinActuallyRefuses(t *testing.T) {
	names := toolcatalog.ClawRefusedBuiltins()
	if len(names) == 0 {
		t.Fatal("no refused builtin: this guard would pass on an empty list, proving nothing")
	}
	reg := NewRegistry()
	if err := RegisterClawAll(reg, ClawDefaults{}); err != nil {
		t.Fatalf("RegisterClawAll: %v", err)
	}
	for _, name := range names {
		if !toolcatalog.IsBuiltin(name) {
			t.Errorf("%q is listed as refused but is not in the catalog — C159 would stay silent and C135 would reject the name", name)
			continue
		}
		td, resolveErr := reg.Resolve(name)
		if resolveErr != nil {
			t.Errorf("%q is listed as refused but does not resolve: %v — a refusal is for a tool the registry HOLDS", name, resolveErr)
			continue
		}
		out, err := td.Execute(context.Background(), json.RawMessage(`{}`))
		var unsupported *delegate.ErrCapabilityUnsupported
		if !errors.As(err, &unsupported) {
			t.Errorf("%q is listed as refused but Execute returned (%q, %v); want the typed capability refusal", name, out, err)
			continue
		}
		refusal, _ := toolcatalog.ClawRefusesBuiltin(name)
		if !strings.Contains(unsupported.Error(), refusal.Remedy) {
			t.Errorf("%q: the refusal does not carry the catalog's remedy %q: %s", name, refusal.Remedy, unsupported.Error())
		}
		// The ultracode grant tells the model a refusal is final where the
		// tool ANSWERS the headline (delegate.ultracodeOrchestrationInstruction);
		// a refused tool whose error omits it leaves that instruction keyed on
		// words the model never receives, and the retry loop the grant exists
		// to kill comes back.
		if !strings.Contains(unsupported.Error(), toolcatalog.ClawRefusedToolHeadline) {
			t.Errorf("%q: the refusal does not carry the headline %q the ultracode grant conditions on: %s",
				name, toolcatalog.ClawRefusedToolHeadline, unsupported.Error())
		}
	}
}
