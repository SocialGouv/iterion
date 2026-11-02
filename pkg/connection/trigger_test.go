package connection_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/connection"
)

// The `trigger` capability is what may be BELIEVED about an inbound delivery,
// decided when the connection is entrusted (#2761). The ingress now reads it
// (the connector webhook lane, pkg/server/webhooks_connector.go), so the
// grant is honoured — and these tests keep the chokepoint honest about what
// that changes: a trigger-only connection asked for an ACTION is an ordinary
// grant mismatch, not a build defect.

// TestATriggerOnlyGrantIsAnOrdinaryMismatchForANode is the chokepoint's half
// after the ingress landed: the refusal names the missing grant, and NOTHING
// about waiting for a release — the ingress exists, so "inert" would send the
// operator to wait for what is already shipped.
func TestATriggerOnlyGrantIsAnOrdinaryMismatchForANode(t *testing.T) {
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
	if errors.Is(err, connection.ErrTriggerIngressUnwired) {
		t.Errorf("the ingress is wired (#2761): the grant is honoured, so the release-fixed sentinel must not travel: %v", err)
	}
	if errors.Is(err, connection.ErrAgentFacadeUnwired) {
		t.Errorf("it holds no `agent` grant, so the agent sentinel must not travel: %v", err)
	}
	// The operator's sentence: widen the grant. No "inert", no waiting on a
	// release — the use exists and the record simply does not grant it.
	if !strings.Contains(err.Error(), "an operator grants that use deliberately") {
		t.Errorf("the refusal must direct the operator at the grant: %v", err)
	}
	if strings.Contains(err.Error(), "inert") {
		t.Errorf("nothing is inert in this build — the word must not appear: %v", err)
	}
}

// TestAnHonouredGrantMakesTheMismatchOrdinary.
//
// Before either surface landed, [agent, trigger] held TWO inert facts and
// both sentinels travelled. Since #2758 (facade) and #2761 (ingress) BOTH
// are honoured, so the record is an ordinary grant mismatch — the refusal
// for a node must say "widen the grant", not "wait for a release". Both
// orders, because the predicate walks the list: which answer a caller reads
// must not depend on the field order.
func TestAnHonouredGrantMakesTheMismatchOrdinary(t *testing.T) {
	for _, caps := range [][]connection.Capability{
		{connection.CapAgent, connection.CapTrigger},
		{connection.CapTrigger, connection.CapAgent},
	} {
		err := (&connection.CapabilityError{Alias: "main", Want: connection.CapAction, Have: caps}).Unwrap()
		if errors.Is(err, connection.ErrAgentFacadeUnwired) {
			t.Errorf("Have=%v: the agent grant is honoured (#2758), so no agent sentinel may travel", caps)
		}
		if errors.Is(err, connection.ErrTriggerIngressUnwired) {
			t.Errorf("Have=%v: with `agent` honoured the record is not all-inert, so the release-fixed reading must not replace the widen-the-grant one", caps)
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
