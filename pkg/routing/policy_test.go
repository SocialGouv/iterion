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

func withCap(p *store.RoutingPolicy, n int) *store.RoutingPolicy {
	p.MaxRelaunches = n
	p.Hash = p.ComputeHash()
	return p
}

func withStatus(r *store.Run, st store.RunStatus) *store.Run {
	r.Status = st
	return r
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
			name: "failure with relaunch allowed and a granted cap",
			run: runWith(map[string]map[string]any{"gate": {"converged": false}},
				withCap(policy("outputs.gate.converged", nil, "merge", "relaunch"), 1)),
			want: DecisionRelaunch, reasonHas: "did not hold",
		},
		{
			// R6976c3: the omitempty default IS "never relaunch
			// automatically" — listing the action grants nothing while
			// the cap is 0.
			name: "relaunch listed but max_relaunches 0 escalates",
			run: runWith(map[string]map[string]any{"gate": {"converged": false}},
				policy("outputs.gate.converged", nil, "merge", "relaunch")),
			want: DecisionEscalate, reasonHas: "max_relaunches is 0",
		},
		{
			name: "failure without relaunch escalates",
			run: runWith(map[string]map[string]any{"gate": {"converged": false}},
				policy("outputs.gate.converged", nil, "merge")),
			want: DecisionEscalate, reasonHas: "relaunch is not an allowed action",
		},
		{
			// C1 regression: "!" coerces via truthiness inside the DSL —
			// an absent field under negation must escalate, never read
			// as true and merge.
			name: "negated absent field escalates",
			run:  runWith(map[string]map[string]any{"gate": {"converged": true}}, policy("outputs.gate.converged && !outputs.oracle.blind", nil, "merge")),
			want: DecisionEscalate, reasonHas: "path absent",
		},
		{
			// C1 regression, the block_when arm: a disjunction of two
			// absent blockers must not silently disarm.
			name: "blocker disjunction over absent fields escalates",
			run: runWith(gateTrue, policy("outputs.gate.converged",
				[]string{"outputs.gate.rebaseline || outputs.gate.reanchor"}, "merge")),
			want: DecisionEscalate, reasonHas: "block_when[0] unreadable",
		},
		{
			// C1 regression: a truthy non-bool inside a conjunction must
			// not coerce into a verdict.
			name: "non-bool operand inside && escalates",
			run: runWith(map[string]map[string]any{"gate": {"converged": true, "ok": "partial"}},
				policy("outputs.gate.converged && outputs.gate.ok", nil, "merge")),
			want: DecisionEscalate, reasonHas: "not a bool",
		},
		{
			// M6: a contract newer than this reader is never executed.
			name: "newer contract version escalates",
			run: func() *store.Run {
				p := policy("outputs.gate.converged", nil, "merge")
				p.Version = CurrentPolicyVersion + 1
				p.Hash = p.ComputeHash()
				return runWith(gateTrue, p)
			}(),
			want: DecisionEscalate, reasonHas: "newer than this reader",
		},
		{
			// C2 defence in depth: no terminal outputs (a store that
			// destroyed the checkpoint, a run that never produced) —
			// nothing to read a verdict from.
			name: "nil checkpoint escalates",
			run:  &store.Run{ID: "r2", Status: store.RunStatusFinished, RoutingPolicy: policy("outputs.gate.converged", nil, "merge")},
			want: DecisionEscalate, reasonHas: "no terminal outputs",
		},
		{
			// R84df21: the contract describes a TERMINAL run — a run
			// still moving must never decide, whatever its checkpoint
			// already says.
			name: "running run escalates even with a satisfied gate",
			run: withStatus(runWith(map[string]map[string]any{"gate": {"converged": true}},
				policy("outputs.gate.converged", nil, "merge")), store.RunStatusRunning),
			want: DecisionEscalate, reasonHas: "not terminal",
		},
		{
			// R84df21: a cancelled run's checkpoint shows an EARLIER
			// pass; only a finished run may land its work.
			name: "cancelled run with satisfied gate escalates",
			run: withStatus(runWith(map[string]map[string]any{"gate": {"converged": true}},
				policy("outputs.gate.converged", nil, "merge")), store.RunStatusCancelled),
			want: DecisionEscalate, reasonHas: "not finished",
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
		{"missing version", &store.RoutingPolicy{SuccessWhen: "outputs.a.b"}, "version"},
		{"future version", &store.RoutingPolicy{Version: CurrentPolicyVersion + 1, SuccessWhen: "outputs.a.b"}, "version"},
		{"empty success_when", &store.RoutingPolicy{Version: 1}, "success_when is required"},
		{"malformed success_when", &store.RoutingPolicy{Version: 1, SuccessWhen: "outputs.gate.converged &&"}, "success_when"},
		{"malformed blocker", &store.RoutingPolicy{Version: 1, SuccessWhen: "outputs.a.b", BlockWhen: []string{"(("}}, "block_when[0]"},
		// H3: a namespace the routing context cannot resolve is refused
		// at launch — accepted, it would escalate (or worse) forever.
		{"foreign namespace", &store.RoutingPolicy{Version: 1, SuccessWhen: "!input.dry_run"}, "outside the contract's vocabulary"},
		{"vars namespace", &store.RoutingPolicy{Version: 1, SuccessWhen: "vars.x && outputs.a.b"}, "outside the contract's vocabulary"},
		{"shallow ref", &store.RoutingPolicy{Version: 1, SuccessWhen: "outputs.gate"}, "outside the contract's vocabulary"},
		// C1 at launch: comparisons/literals leave the strict grammar.
		{"comparison refused", &store.RoutingPolicy{Version: 1, SuccessWhen: `outputs.a.b == "x"`}, "grammar"},
		{"literal refused", &store.RoutingPolicy{Version: 1, SuccessWhen: "true"}, "grammar"},
		{"unknown action", &store.RoutingPolicy{Version: 1, SuccessWhen: "outputs.a.b", AllowedActions: []string{"deploy"}}, "unknown action"},
		{"negative cap", &store.RoutingPolicy{Version: 1, SuccessWhen: "outputs.a.b", MaxRelaunches: -1}, "max_relaunches"},
		// B8: the landing target feeds a git operation.
		{"hostile strategy", &store.RoutingPolicy{Version: 1, SuccessWhen: "outputs.a.b", MergeStrategy: "rm -rf"}, "merge_strategy"},
		{"hostile branch", &store.RoutingPolicy{Version: 1, SuccessWhen: "outputs.a.b", MergeInto: "-evil"}, "merge_into"},
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
	// B7: the action SET is order-insensitive.
	d1 := policy("outputs.g.ok", nil, "merge", "relaunch")
	d2 := policy("outputs.g.ok", nil, "relaunch", "merge")
	if d1.Hash != d2.Hash {
		t.Fatalf("action order changed the hash: %q vs %q", d1.Hash, d2.Hash)
	}
	// Hash excludes itself: recomputing over a hashed policy is stable.
	if got := a.ComputeHash(); got != a.Hash {
		t.Fatalf("hash not self-excluding: %q vs %q", got, a.Hash)
	}
}

// A consumer that forgets the `err == nil && r != nil` idiom must get
// the default decision, not a panic in a goroutine outside net/http's
// recover.
func TestEvaluateNilRunEscalates(t *testing.T) {
	v := Evaluate(nil)
	if v.Decision != DecisionEscalate {
		t.Fatalf("Evaluate(nil) = %q, want escalate", v.Decision)
	}
}

// The verdict's hash is the audit's claim of WHICH contract decided:
// a stored stamp that does not match the stored contract must refuse,
// never ride the stale stamp onto a merge verdict.
func TestEvaluateTamperedHashEscalates(t *testing.T) {
	r := runWith(map[string]map[string]any{"review": {"approved": true}},
		policy("outputs.review.approved", nil, "merge"))
	r.RoutingPolicy.Hash = r.RoutingPolicy.ComputeHash()
	// Sanity: untampered decides merge.
	if v := Evaluate(r); v.Decision != DecisionMerge {
		t.Fatalf("untampered verdict = %q, want merge", v.Decision)
	}
	// Tamper the contract after stamping.
	r.RoutingPolicy.SuccessWhen = "outputs.review.rubber_stamp"
	v := Evaluate(r)
	if v.Decision != DecisionEscalate {
		t.Fatalf("tampered-contract verdict = %q, want escalate", v.Decision)
	}
	if v.PolicyHash == r.RoutingPolicy.Hash {
		t.Fatalf("verdict rode the stale stamp %q", v.PolicyHash)
	}
}

// The launch must refuse a contract whose refs the workflow cannot
// serve: an author typo would otherwise read "unreadable → escalate"
// at the terminal, forever, silently.
func TestValidateRefs(t *testing.T) {
	p := &store.RoutingPolicy{
		Version:     CurrentPolicyVersion,
		SuccessWhen: "outputs.gate.converged",
		BlockWhen:   []string{"outputs.work.blocked"},
	}
	nodes := map[string]map[string]bool{
		"gate": {"converged": true},
		"work": {"blocked": true},
	}
	hasNode := func(n string) bool { _, ok := nodes[n]; return ok }
	hasField := func(n, f string) bool { return nodes[n][f] }

	if err := ValidateRefs(p, hasNode, hasField); err != nil {
		t.Fatalf("valid refs refused: %v", err)
	}
	// Unknown node.
	bad := *p
	bad.SuccessWhen = "outputs.gone.converged"
	if err := ValidateRefs(&bad, hasNode, hasField); err == nil {
		t.Fatal("unknown node accepted")
	}
	// Known node, unpublished field.
	bad = *p
	bad.BlockWhen = []string{"outputs.work.rubber_stamp"}
	if err := ValidateRefs(&bad, hasNode, hasField); err == nil {
		t.Fatal("unpublished field accepted — the automation would be silently disabled at the terminal")
	}
	// Dynamic output shape: nil hasField only checks nodes.
	bad = *p
	bad.BlockWhen = []string{"outputs.work.anything"}
	if err := ValidateRefs(&bad, hasNode, nil); err != nil {
		t.Fatalf("nil hasField must accept unknown fields: %v", err)
	}
}
