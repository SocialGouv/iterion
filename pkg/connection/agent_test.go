package connection_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/connection"
)

// The `agent` capability stops being inert (#1072 governance). It is not
// removed and not silently narrowed — it is READ, at the two places that
// exist: the resolution chokepoint, and the CLI where the grant is made.
//
// The chokepoint's half: an agent-only connection asked to serve a
// deterministic node is refused with a TYPED error that says both halves of
// the truth — this use needs `action`, and the `agent` grant you do hold buys
// nothing in this build.
func TestAnAgentOnlyGrantIsRefusedTypedAndSaysTheGrantIsInert(t *testing.T) {
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
	if !errors.Is(err, connection.ErrAgentFacadeUnwired) {
		t.Errorf("a grant that is agent-only must unwrap to ErrAgentFacadeUnwired — that is what tells the operator the agent path is not the way round: %v", err)
	}
	if !strings.Contains(err.Error(), "inert") {
		t.Errorf("the message must say the agent grant is inert, or the operator reads the refusal as 'use the agent path': %v", err)
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

// The CLI's half reads the SAME answer: one place decides whether `agent` is
// honoured, so the day the facade lands no surface keeps saying otherwise.
func TestAgentGrantAdviceIsOwedExactlyWhenTheGrantIsInert(t *testing.T) {
	// A TRIPWIRE, not a skip: the day AgentFacadeWired() flips, this reddens
	// and whoever flipped it has to come back for the two readers that stop
	// owing the warning (#2758 says so in its own Done-when).
	if connection.AgentFacadeWired() {
		t.Fatal("AgentFacadeWired() is true: the MCP facade landed — revisit `connections add` / `connections list` and this test together")
	}
	for _, tc := range []struct {
		name string
		caps []connection.Capability
		want bool
	}{
		{"agent alone", []connection.Capability{connection.CapAgent}, true},
		{"action and agent", []connection.Capability{connection.CapAction, connection.CapAgent}, true},
		{"action alone", []connection.Capability{connection.CapAction}, false},
		{"nothing", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			advice := connection.AgentGrantAdvice(tc.caps)
			if tc.want && advice == "" {
				t.Fatal("a grant holding `agent` is owed the sentence that says it is inert")
			}
			if !tc.want && advice != "" {
				t.Fatalf("a grant with no `agent` is owed nothing, got %q", advice)
			}
			if tc.want && !strings.Contains(advice, "inert") {
				t.Errorf("the advice must say the grant is inert: %q", advice)
			}
		})
	}
}
