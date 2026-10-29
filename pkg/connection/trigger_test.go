package connection_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/connection"
)

// The `trigger` capability is what may be BELIEVED about an inbound delivery,
// decided when the connection is entrusted (#2761). Nothing reads it for an
// inbound event yet — the ingress is the next lot — so it lands the way
// `agent` did: accepted, never silently narrowed, and READ at the two places
// that exist, so the grant cannot be configuration that looks like a
// guarantee.

// TestATriggerOnlyGrantIsRefusedTypedAndSaysTheGrantIsInert is the
// chokepoint's half: the refusal says both halves of the truth — this use
// needs `action`, and the `trigger` grant you do hold buys nothing yet.
func TestATriggerOnlyGrantIsRefusedTypedAndSaysTheGrantIsInert(t *testing.T) {
	store := connection.NewMemoryStore()
	r := resolver(t, store)
	c := conn("c1", "tenant-a", "main")
	c.Capabilities = []connection.Capability{connection.CapTrigger}
	c.GrantedScopes, c.ScopesKnown = []string{"read:issue"}, true
	seed(t, r, store, c)

	_, _, _, _, err := r.ResolveAction(context.Background(), "probe.issue.get", "main")
	if err == nil {
		t.Fatal("a trigger-only connection must not serve a deterministic node")
	}
	var capErr *connection.CapabilityError
	if !errors.As(err, &capErr) {
		t.Fatalf("the refusal must be typed *CapabilityError: %T %v", err, err)
	}
	if capErr.Want != connection.CapAction {
		t.Errorf("Want = %q, want %q", capErr.Want, connection.CapAction)
	}
	if !errors.Is(err, connection.ErrTriggerIngressUnwired) {
		t.Errorf("a grant that is trigger-only must unwrap to ErrTriggerIngressUnwired — a release fixes it, not the operator: %v", err)
	}
	if errors.Is(err, connection.ErrAgentFacadeUnwired) {
		t.Errorf("it holds no `agent` grant, so the agent sentinel must not travel: %v", err)
	}
	// The sentence an operator ACTS on: the grant, and what is missing — which
	// is what tells them whether to wait for a release or widen the record.
	for _, want := range []string{"inert", "`trigger`", "no connector webhook ingress reads it"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the message must contain %q, got: %v", want, err)
		}
	}
}

// TestTwoInertGrantsBothTravel.
//
// A connection granting `agent` AND `trigger` holds two facts a caller may
// branch on, and which one it reads must not depend on the order the record
// happens to list them in. Both orders, because the predicate walks the list.
func TestTwoInertGrantsBothTravel(t *testing.T) {
	for _, caps := range [][]connection.Capability{
		{connection.CapAgent, connection.CapTrigger},
		{connection.CapTrigger, connection.CapAgent},
	} {
		err := (&connection.CapabilityError{Alias: "main", Want: connection.CapAction, Have: caps}).Unwrap()
		if !errors.Is(err, connection.ErrAgentFacadeUnwired) {
			t.Errorf("Have=%v: the agent fact must travel whatever the order", caps)
		}
		if !errors.Is(err, connection.ErrTriggerIngressUnwired) {
			t.Errorf("Have=%v: the trigger fact must travel whatever the order", caps)
		}
	}
	// And the falsifier the class rests on: a capability this build does not
	// RECOGNISE is not inert, it is unknown, so the record is an ordinary
	// grant mismatch and neither sentinel may travel.
	err := (&connection.CapabilityError{
		Alias: "main", Want: connection.CapAction,
		Have: []connection.Capability{connection.CapTrigger, connection.Capability("future")},
	}).Unwrap()
	if errors.Is(err, connection.ErrTriggerIngressUnwired) {
		t.Error("a record holding a capability from a newer replica is a mismatch, not a build that cannot honour what it holds")
	}
}

// TestAnUnknownCapabilityIsRefusedNamingWhatThisBuildUnderstands.
//
// The enumeration an operator reads must BE the set ValidCapability admits: a
// message listing two of three sends them to widen a record with a value this
// build would have taken.
func TestAnUnknownCapabilityIsRefusedNamingWhatThisBuildUnderstands(t *testing.T) {
	c := conn("c1", "tenant-a", "main")
	c.Capabilities = []connection.Capability{connection.Capability("webhook")}
	err := c.Validate()
	if err == nil {
		t.Fatal("a capability this build does not know must be refused rather than ignored")
	}
	for _, want := range connection.CapabilityNames() {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal = %v, want it to name %q — the list an operator reads is the list the build admits", err, want)
		}
	}

	// And the grant it DOES admit validates, or the assertion above would
	// pass on a build that refuses everything.
	c.Capabilities = []connection.Capability{connection.CapTrigger}
	if err := c.Validate(); err != nil {
		t.Fatalf("`trigger` must be a capability this build accepts on a record: %v", err)
	}
}
