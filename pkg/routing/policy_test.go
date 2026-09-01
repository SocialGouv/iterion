package routing

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/store"
)

func runWith(outputs map[string]map[string]any, p *store.RoutingPolicy) *store.Run {
	return &store.Run{
		ID:            "r1",
		Status:        store.RunStatusFinished,
		RoutingPolicy: p,
		Checkpoint:    &store.Checkpoint{Outputs: outputs},
	}
}

func policy(success string, block []string, actions ...string) *store.RoutingPolicy {
	p := &store.RoutingPolicy{
		Version:        1,
		SuccessWhen:    success,
		BlockWhen:      block,
		AllowedActions: actions,
	}
	p.Hash = p.ComputeHash()
	return p
}

func TestEvaluate_StrictAndFailClosed(t *testing.T) {
	gateTrue := map[string]map[string]any{"gate": {"converged": true, "needs_reanchor": false}}

	cases := []struct {
		name      string
		run       *store.Run
		want      Decision
		reasonHas string
	}{
		{
			name: "success and merge allowed",
			run:  runWith(gateTrue, policy("outputs.gate.converged", []string{"outputs.gate.needs_reanchor"}, "merge")),
			want: DecisionMerge, reasonHas: "success_when held",
		},
		{
			name: "no policy escalates",
			run:  runWith(gateTrue, nil),
			want: DecisionEscalate, reasonHas: "no routing policy",
		},
		{
			// The measured incident class: a converged run carrying an
			// explicit blocker must NEVER auto-merge.
			name: "blocker outranks success",
			run: runWith(map[string]map[string]any{"gate": {"converged": true, "needs_reanchor": true}},
				policy("outputs.gate.converged", []string{"outputs.gate.needs_reanchor"}, "merge")),
			want: DecisionEscalate, reasonHas: "block_when[0] held",
		},
		{
			// F2's core rule: an ABSENT path must not read as false and
			// green-light anything — reading only "converged" on a bot
			// that also publishes a blocker the contract forgot would
			// reproduce the forbidden landing.
			name: "absent success path escalates, never merges",
			run:  runWith(map[string]map[string]any{"other": {"x": 1}}, policy("outputs.gate.converged", nil, "merge")),
			want: DecisionEscalate, reasonHas: "success_when unreadable",
		},
		{
			name: "absent blocker path escalates (blocker unreadable ≠ blocker false)",
			run: runWith(map[string]map[string]any{"gate": {"converged": true}},
				policy("outputs.gate.converged", []string{"outputs.gate.needs_reanchor"}, "merge")),
			want: DecisionEscalate, reasonHas: "block_when[0] unreadable",
		},
		{
			name: "non-bool success value escalates",
			run: runWith(map[string]map[string]any{"gate": {"converged": "yes"}},
				policy("outputs.gate.converged", nil, "merge")),
			want: DecisionEscalate, reasonHas: "not a bool",
		},
		{
			name: "success but merge not allowed",
			run:  runWith(gateTrue, policy("outputs.gate.converged", nil)),
			want: DecisionEscalate, reasonHas: "not an allowed action",
		},
		{
			name: "failure with relaunch allowed",
			run: runWith(map[string]map[string]any{"gate": {"converged": false}},
				policy("outputs.gate.converged", nil, "merge", "relaunch")),
			want: DecisionRelaunch, reasonHas: "did not hold",
		},
		{
			name: "failure without relaunch escalates",
			run: runWith(map[string]map[string]any{"gate": {"converged": false}},
				policy("outputs.gate.converged", nil, "merge")),
			want: DecisionEscalate, reasonHas: "relaunch is not an allowed action",
		},
		{
			name: "run namespace resolves the typed outcome",
			run:  runWith(gateTrue, policy(`outputs.gate.converged && run.terminal_code == ""`, nil, "merge")),
			want: DecisionMerge, reasonHas: "success_when held",
		},
		{
			name: "nil checkpoint escalates",
			run:  &store.Run{ID: "r2", RoutingPolicy: policy("outputs.gate.converged", nil, "merge")},
			want: DecisionEscalate, reasonHas: "unreadable",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			v := Evaluate(c.run)
			if v.Decision != c.want {
				t.Fatalf("Decision = %q (reason %q), want %q", v.Decision, v.Reason, c.want)
			}
			if !strings.Contains(v.Reason, c.reasonHas) {
				t.Errorf("Reason = %q, want it to mention %q", v.Reason, c.reasonHas)
			}
			if c.run.RoutingPolicy != nil && v.PolicyHash != c.run.RoutingPolicy.Hash {
				t.Errorf("PolicyHash = %q, want the contract's %q", v.PolicyHash, c.run.RoutingPolicy.Hash)
			}
		})
	}
}

func TestValidate(t *testing.T) {
	ok := policy("outputs.gate.converged", []string{"outputs.gate.blocked"}, "merge")
	if err := Validate(ok); err != nil {
		t.Fatalf("valid policy refused: %v", err)
	}
	cases := []struct {
		name string
		p    *store.RoutingPolicy
		want string
	}{
		{"empty success_when", &store.RoutingPolicy{}, "success_when is required"},
		{"malformed success_when", &store.RoutingPolicy{SuccessWhen: "outputs.gate.converged &&"}, "success_when"},
		{"malformed blocker", &store.RoutingPolicy{SuccessWhen: "outputs.a.b", BlockWhen: []string{"(("}}, "block_when[0]"},
		{"unknown action", &store.RoutingPolicy{SuccessWhen: "outputs.a.b", AllowedActions: []string{"deploy"}}, "unknown action"},
		{"negative cap", &store.RoutingPolicy{SuccessWhen: "outputs.a.b", MaxRelaunches: -1}, "max_relaunches"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := Validate(c.p)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("Validate = %v, want error mentioning %q", err, c.want)
			}
		})
	}
	if err := Validate(nil); err != nil {
		t.Fatalf("nil policy is 'no contract', not an error: %v", err)
	}
}

func TestComputeHash_CanonicalAndSelfExcluding(t *testing.T) {
	a := policy("outputs.g.ok", nil, "merge")
	b := policy("outputs.g.ok", nil, "merge")
	if a.Hash == "" || a.Hash != b.Hash {
		t.Fatalf("identical contracts must hash identically: %q vs %q", a.Hash, b.Hash)
	}
	c := policy("outputs.g.ok2", nil, "merge")
	if c.Hash == a.Hash {
		t.Fatal("different contracts must hash differently")
	}
	// Hash excludes itself: recomputing over a hashed policy is stable.
	if got := a.ComputeHash(); got != a.Hash {
		t.Fatalf("hash not self-excluding: %q vs %q", got, a.Hash)
	}
}
