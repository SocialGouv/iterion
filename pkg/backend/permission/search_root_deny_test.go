package permission

import (
	"testing"
)

// TestASearchRootedAboveADeniedTreeIsRefused is the headline witness of
// #3086: a `dir/**` deny only matched a search root INSIDE the tree, so a
// search rooted at the tree's parent — or any ancestor — walked the whole
// denied tree with the gate's blessing. The rows are the ones the issue
// reproduces against allow [Grep, Glob] plus the engine's armed secret
// trees; every one of them used to be ALLOWED.
func TestASearchRootedAboveADeniedTreeIsRefused(t *testing.T) {
	p := mustPolicy(t, ModeDeny, []string{"Grep", "Glob", "LS", "workspace_grep"}, nil, nil)
	if err := p.DenyPathTrees("/run/iterion/secrets"); err != nil {
		t.Fatalf("DenyPathTrees: %v", err)
	}
	for _, tc := range []struct {
		name  string
		tool  string
		input map[string]any
		want  Decision
	}{
		// The issue's rows.
		{"grep from the tree's parent", "Grep", map[string]any{"pattern": ".", "path": "/run/iterion"}, Deny},
		{"grep from the filesystem root", "Grep", map[string]any{"pattern": ".", "path": "/"}, Deny},
		{"glob into the tree from its parent", "Glob", map[string]any{"pattern": "secrets/*", "path": "/run/iterion"}, Deny},
		// The rest of the class.
		{"grep from a higher ancestor", "Grep", map[string]any{"pattern": ".", "path": "/run"}, Deny},
		{"claw's workspace_grep from the parent", "workspace_grep", map[string]any{"pattern": ".", "path": "/run/iterion"}, Deny},
		{"a listing of the parent names the tree", "LS", map[string]any{"path": "/run/iterion"}, Deny},
		// Already refused before the fix; must stay refused.
		{"grep rooted inside the tree", "Grep", map[string]any{"pattern": ".", "path": "/run/iterion/secrets"}, Deny},
		// No over-refusal: an unrelated tree, and a SIBLING of the denied
		// one, still search fine.
		{"a search elsewhere stays allowed", "Grep", map[string]any{"pattern": ".", "path": "/work/pkg"}, Allow},
		{"a sibling of the denied tree stays allowed", "Grep", map[string]any{"pattern": ".", "path": "/run/iterion/other"}, Allow},
		{"a sibling listing stays allowed", "LS", map[string]any{"path": "/run/iterion/other"}, Allow},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got, rule := p.Evaluate(tc.tool, tc.input); got != tc.want {
				t.Errorf("Evaluate(%s, %v) = %v (rule %q), want %v", tc.tool, tc.input, got, rule, tc.want)
			}
		})
	}
}

// TestASearchContainmentDenyIsNotReopened: the tree a search walks through
// is a deny, and a deny wins in every mode — no author allow, no operator
// grant reopens it (the grant/allow rows open searches elsewhere fine).
func TestASearchContainmentDenyIsNotReopened(t *testing.T) {
	p := mustPolicy(t, ModeAsk, []string{"Grep", "Glob"}, nil, nil)
	p.AddGrantRule("Grep(**)")
	if err := p.DenyPathTrees("/run/iterion/secrets"); err != nil {
		t.Fatalf("DenyPathTrees: %v", err)
	}
	for _, tc := range []struct {
		tool  string
		input map[string]any
	}{
		{"Grep", map[string]any{"pattern": ".", "path": "/run/iterion"}},
		{"Glob", map[string]any{"pattern": "secrets/*", "path": "/run/iterion"}},
	} {
		if got, rule := p.Evaluate(tc.tool, tc.input); got != Deny {
			t.Errorf("Evaluate(%s, %v) = %v (rule %q), want Deny — an allow or grant reopened the tree", tc.tool, tc.input, got, rule)
		}
	}
	if got, _ := p.Evaluate("Grep", map[string]any{"pattern": ".", "path": "/work/pkg"}); got != Allow {
		t.Errorf("the granted search outside the tree = %v, want Allow", got)
	}
}

// TestAnAuthorTreeDenyBindsTheParentSearch: the class is not specific to the
// engine-armed trees — an author's own `Grep(dir/**)` deny reads "the agent
// does not search this tree", and a search rooted above it reads it whole.
func TestAnAuthorTreeDenyBindsTheParentSearch(t *testing.T) {
	p := mustPolicy(t, ModeDeny, []string{"Grep"}, nil, []string{"Grep(/work/private/**)"})
	if got, rule := p.Evaluate("Grep", map[string]any{"pattern": ".", "path": "/work"}); got != Deny {
		t.Errorf("a search rooted at the denied tree's parent = %v (rule %q), want Deny", got, rule)
	}
	if got, _ := p.Evaluate("Grep", map[string]any{"pattern": ".", "path": "/work/public"}); got != Allow {
		t.Errorf("a search in a sibling tree = %v, want Allow (no over-refusal)", got)
	}
}

// TestASearchContainmentDenyReadsEverySpelling: the containment comparison
// normalises both sides the way the gate reads any path — a root spelled
// with separators to fold still contains the tree, and a relative rule
// anchors against the directory the agent runs in.
func TestASearchContainmentDenyReadsEverySpelling(t *testing.T) {
	p := mustPolicy(t, ModeDeny, []string{"Grep", "Glob"}, nil, nil)
	if err := p.DenyPathTrees("/run/iterion/secrets"); err != nil {
		t.Fatalf("DenyPathTrees: %v", err)
	}
	for _, tc := range []struct {
		name  string
		input map[string]any
	}{
		{"doubled separator on the root", map[string]any{"pattern": ".", "path": "/run//iterion"}},
		{"trailing separator on the root", map[string]any{"pattern": ".", "path": "/run/iterion/"}},
		{"a folded-case root spelling", map[string]any{"pattern": ".", "path": "/RUN/ITERION"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got, rule := p.Evaluate("Grep", tc.input); got != Deny {
				t.Errorf("Evaluate(Grep, %v) = %v (rule %q), want Deny", tc.input, got, rule)
			}
		})
	}

	// A relative tree rule anchors on the agent's directory: a search of
	// that directory walks the tree the rule names.
	rel := mustPolicy(t, ModeDeny, []string{"Grep"}, nil, []string{"Grep(secrets/**)"})
	rel.SetPathRoot("/work")
	if got, rule := rel.Evaluate("Grep", map[string]any{"pattern": ".", "path": "/work"}); got != Deny {
		t.Errorf("a search of the workspace over a relative tree deny = %v (rule %q), want Deny", got, rule)
	}
	if got, _ := rel.Evaluate("Grep", map[string]any{"pattern": ".", "path": "/work/pkg"}); got != Allow {
		t.Errorf("a search in a subtree without the denied tree = %v, want Allow", got)
	}
}

// TestAnAbsoluteGlobPatternIntoADeniedTree pins the sibling gap the issue
// names: a Glob whose PATTERN is absolute escapes its root on a backend that
// honours it. The pattern dimension already carries the pattern as written,
// so the tree rule meets it — this row keeps that reading from drifting.
func TestAnAbsoluteGlobPatternIntoADeniedTree(t *testing.T) {
	p := mustPolicy(t, ModeDeny, []string{"Glob"}, nil, nil)
	if err := p.DenyPathTrees("/run/iterion/secrets"); err != nil {
		t.Fatalf("DenyPathTrees: %v", err)
	}
	if got, rule := p.Evaluate("Glob", map[string]any{"pattern": "/run/iterion/secrets/*", "path": "/work"}); got != Deny {
		t.Errorf("an absolute glob into the tree from an unrelated root = %v (rule %q), want Deny", got, rule)
	}
}

// TestTheSearchContainmentDenyTravelsWithThePolicy: the child-process gates
// (claude_code's hook, the claw runner in a sandbox) rebuild the policy from
// PolicyConfig, and the containment verdict is derived from the rules
// themselves — the rebuilt gate refuses the same parent-rooted searches.
func TestTheSearchContainmentDenyTravelsWithThePolicy(t *testing.T) {
	engine := mustPolicy(t, ModeDeny, []string{"Grep", "Glob"}, nil, nil)
	engine.SetPathRoot("/work")
	if err := engine.DenyPathTrees("/run/iterion/secrets"); err != nil {
		t.Fatalf("DenyPathTrees: %v", err)
	}
	child, err := NewPolicyFromConfig(engine.Config())
	if err != nil {
		t.Fatalf("NewPolicyFromConfig: %v", err)
	}
	for side, p := range map[string]*Policy{"engine": engine, "rebuilt": child} {
		if got, rule := p.Evaluate("Grep", map[string]any{"pattern": ".", "path": "/run/iterion"}); got != Deny {
			t.Errorf("%s gate: a search rooted above the tree = %v (rule %q), want Deny", side, got, rule)
		}
		if got, _ := p.Evaluate("Grep", map[string]any{"pattern": ".", "path": "/work/pkg"}); got != Allow {
			t.Errorf("%s gate: a search outside the tree = %v, want Allow", side, got)
		}
	}
}

// TestHasExplicitDenySeesTheParentSearch: the backend bridges ask
// HasExplicitDeny before changing a native tool spelling; a search rooted
// above a denied tree is an explicit denial there too, or a bridge would
// treat it as ungated.
func TestHasExplicitDenySeesTheParentSearch(t *testing.T) {
	p := mustPolicy(t, ModeDeny, []string{"Grep"}, nil, nil)
	if err := p.DenyPathTrees("/run/iterion/secrets"); err != nil {
		t.Fatalf("DenyPathTrees: %v", err)
	}
	if !p.HasExplicitDeny("Grep", map[string]any{"pattern": ".", "path": "/run/iterion"}) {
		t.Error("HasExplicitDeny = false for a search rooted above the denied tree")
	}
	if p.HasExplicitDeny("Grep", map[string]any{"pattern": ".", "path": "/work/pkg"}) {
		t.Error("HasExplicitDeny = true for a search outside the denied tree")
	}
}
