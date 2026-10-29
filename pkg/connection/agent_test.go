package connection_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/connection"
)

// The `agent` capability is WIRED (#2758): the MCP facade exists, so the
// chokepoint's two answers are now "this connection may serve an agent" and
// "this connection may not serve a deterministic node" — and the inert-grant
// sentence is owed to NEITHER side of an `agent` grant anymore.

// The chokepoint's unchanged half: an agent-only connection asked to serve a
// deterministic node is refused with a TYPED error naming the missing grant —
// and must NOT claim the build cannot honour the grant it holds, because it
// now can.
func TestAnAgentOnlyGrantIsRefusedTypedForANode(t *testing.T) {
	store := connection.NewMemoryStore()
	r := resolver(t, store)
	c := conn("c1", "tenant-a", "main")
	c.Capabilities = []connection.Capability{connection.CapAgent}
	c.GrantedScopes, c.ScopesKnown = []string{"read:issue"}, true
	seed(t, r, store, c)

	_, _, _, _, err := r.ResolveAction(context.Background(), "probe.issue.get", "main")
	if err == nil {
		t.Fatal("an agent-only connection must not serve a deterministic node")
	}
	var capErr *connection.CapabilityError
	if !errors.As(err, &capErr) {
		t.Fatalf("the chokepoint's refusal must be typed *CapabilityError, so a caller can tell a widenable grant from a capability this build cannot honour: %T %v", err, err)
	}
	if capErr.Want != connection.CapAction {
		t.Errorf("Want = %q, want %q", capErr.Want, connection.CapAction)
	}
	if errors.Is(err, connection.ErrAgentFacadeUnwired) {
		t.Errorf("the facade is wired (#2758): an agent grant is HONOURED, so the refusal must not read as a dead end: %v", err)
	}
	if strings.Contains(err.Error(), "inert") {
		t.Errorf("the message must not call an honoured grant inert: %v", err)
	}
}

// The facade's half of the same chokepoint: an `agent` connection resolves
// through ResolveAgent, an `action`-only one is refused with the same typed
// shape and the want flipped — one path, one error vocabulary, two grants.
func TestTheFacadeHalfOfTheChokepoint(t *testing.T) {
	store := connection.NewMemoryStore()
	r := resolver(t, store)
	c := conn("c1", "tenant-a", "main")
	c.Capabilities = []connection.Capability{connection.CapAgent}
	c.GrantedScopes, c.ScopesKnown = []string{"read:issue"}, true
	seed(t, r, store, c)

	_, op, cred, baseURL, err := r.ResolveAgent(context.Background(), "probe.issue.get", "main")
	if err != nil {
		t.Fatalf("an agent-capable connection must resolve for the facade: %v", err)
	}
	if op.ID != "probe.issue.get" || cred.Value == "" || baseURL == "" {
		t.Errorf("resolved op=%q cred-empty=%v baseURL=%q", op.ID, cred.Value == "", baseURL)
	}

	// The reverse refusal: action-only does not serve an agent.
	a := conn("c2", "tenant-a", "node-only")
	a.Capabilities = []connection.Capability{connection.CapAction}
	a.GrantedScopes, a.ScopesKnown = []string{"read:issue"}, true
	seed(t, r, store, a)
	_, _, _, _, err = r.ResolveAgent(context.Background(), "probe.issue.get", "node-only")
	if err == nil {
		t.Fatal("an action-only connection must not serve the facade")
	}
	var capErr *connection.CapabilityError
	if !errors.As(err, &capErr) || capErr.Want != connection.CapAgent {
		t.Errorf("the facade's refusal must be the same typed shape with Want=agent: %T %v", err, err)
	}
}

// A connection that merely declares the WRONG capability is an ordinary grant
// mismatch: it must NOT claim the build cannot honour what it holds. Both
// orders of the capability list, because the predicate walks it.
func TestAGrantMismatchIsNotTheFacadeBeingUnwired(t *testing.T) {
	for _, caps := range [][]connection.Capability{
		{connection.CapAgent, connection.Capability("future")},
		{connection.Capability("future"), connection.CapAgent},
	} {
		err := (&connection.CapabilityError{Alias: "main", Want: connection.CapAction, Have: caps}).Unwrap()
		if errors.Is(err, connection.ErrAgentFacadeUnwired) {
			t.Errorf("Have=%v holds a capability other than agent, so the refusal is a grant mismatch, not an unwired facade", caps)
		}
	}
	// And a connection with no capability at all is a mismatch too, not an
	// inert agent grant.
	if err := (&connection.CapabilityError{Alias: "main", Want: connection.CapAction}).Unwrap(); errors.Is(err, connection.ErrAgentFacadeUnwired) {
		t.Error("a connection declaring nothing holds no agent grant to be inert")
	}
}

// The CLI's half reads the SAME answer: one place decides whether a grant is
// honoured, so no surface can keep saying otherwise after a flip. `agent`
// stopped being owed the sentence when the facade landed (#2758); `trigger`
// is still owed it.
func TestGrantAdviceIsOwedExactlyWhenTheGrantIsInert(t *testing.T) {
	// A TRIPWIRE, not a skip: the day either predicate flips, this reddens
	// and whoever flipped it has to revisit the two CLI readers
	// (`connections add` / `connections list`) together with this test —
	// which is exactly what #2758's Done-when demanded for `agent`.
	if !connection.AgentFacadeWired() {
		t.Fatal("AgentFacadeWired() is false: the MCP facade is gone from this build — `connections add` / `connections list` must warn about `agent` again, and this test's rows flip back")
	}
	if connection.TriggerIngressWired() {
		t.Fatal("TriggerIngressWired() is true: the connector webhook ingress landed — revisit the same two surfaces and this test together")
	}
	for _, tc := range []struct {
		name string
		caps []connection.Capability
		want bool
	}{
		{"agent alone — honoured since the facade landed", []connection.Capability{connection.CapAgent}, false},
		{"action and agent", []connection.Capability{connection.CapAction, connection.CapAgent}, false},
		{"action alone", []connection.Capability{connection.CapAction}, false},
		{"nothing", nil, false},
		{"trigger alone", []connection.Capability{connection.CapTrigger}, true},
		{"action and trigger", []connection.Capability{connection.CapAction, connection.CapTrigger}, true},
		{"agent and trigger — one of the two is still inert", []connection.Capability{connection.CapAgent, connection.CapTrigger}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			advice := connection.GrantAdvice(tc.caps)
			if tc.want && advice == "" {
				t.Fatal("a grant holding `trigger` is owed the sentence that says it is inert")
			}
			if !tc.want && advice != "" {
				t.Fatalf("a grant whose capabilities are all honoured is owed nothing, got %q", advice)
			}
			if tc.want && !strings.Contains(advice, "inert") {
				t.Errorf("the advice must say the grant is inert: %q", advice)
			}
		})
	}
}
