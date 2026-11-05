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
// The rows are the three the issue reproduces, plus the no-over-refusal
// row, run against BOTH gates.
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
	rows := []struct {
		name  string
		tool  string
		input map[string]any
		want  Decision
	}{
		{"grep from the tree's parent", "Grep", map[string]any{"pattern": ".", "path": "/run/iterion"}, Deny},
		{"grep from the filesystem root", "Grep", map[string]any{"pattern": ".", "path": "/"}, Deny},
		{"glob into the tree from its parent", "Glob", map[string]any{"pattern": "secrets/*", "path": "/run/iterion"}, Deny},
		{"a search elsewhere stays allowed", "Grep", map[string]any{"pattern": ".", "path": "/work/pkg"}, Allow},
	}
	for side, p := range map[string]*Policy{"engine": engine, "rebuilt": child} {
		for _, tc := range rows {
			if got, rule := p.Evaluate(tc.tool, tc.input); got != tc.want {
				t.Errorf("%s gate: %s = %v (rule %q), want %v", side, tc.name, got, rule, tc.want)
			}
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

// TestAPathlessSearchReadsTheAgentsDirectory is the omitted-root half of
// #3086, in the review's probe shape: a search that spells no `path` is
// not rootless — the backend runs it from the agent's directory, so a deny
// on a tree under that directory must bind the DEFAULT spelling of a
// search, the one agents emit most. Before, the pathless call carried no
// root spelling at all: the containment had nothing to read, and the
// inside-the-tree matching missed a search run from the tree itself.
func TestAPathlessSearchReadsTheAgentsDirectory(t *testing.T) {
	newPolicy := func(root string) *Policy {
		p := mustPolicy(t, ModeDeny, []string{"Grep", "Glob", "LS"}, nil, nil)
		if root != "" {
			p.SetPathRoot(root)
		}
		if err := p.DenyPathTrees("/work/secrets"); err != nil {
			t.Fatalf("DenyPathTrees: %v", err)
		}
		return p
	}
	for _, tc := range []struct {
		name  string
		root  string
		tool  string
		input map[string]any
		want  Decision
	}{
		{"a pathless grep from the tree's parent", "/work", "Grep", map[string]any{"pattern": "api_key"}, Deny},
		{"a pathless glob from the tree's parent", "/work", "Glob", map[string]any{"pattern": "*"}, Deny},
		{"a pathless listing of the tree's parent", "/work", "LS", map[string]any{}, Deny},
		{"a pathless grep from inside the tree", "/work/secrets", "Grep", map[string]any{"pattern": "api_key"}, Deny},
		{"a pathless grep from a deeper room of the tree", "/work/secrets/tls", "Grep", map[string]any{"pattern": "api_key"}, Deny},
		{"a pathless grep from a sibling stays allowed", "/work/public", "Grep", map[string]any{"pattern": "api_key"}, Allow},
		{"a pathless glob from a sibling stays allowed", "/work/public", "Glob", map[string]any{"pattern": "*"}, Allow},
		// No captured root, nothing to anchor on: the gate invents no
		// directory, and the call reads as it always did.
		{"no captured root, no implied one", "", "Grep", map[string]any{"pattern": "api_key"}, Allow},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newPolicy(tc.root)
			if got, rule := p.Evaluate(tc.tool, tc.input); got != tc.want {
				t.Errorf("Evaluate(%s, %v) with root %q = %v (rule %q), want %v", tc.tool, tc.input, tc.root, got, rule, tc.want)
			}
		})
	}

	// The author's own tree rule reads the omission too: a relative tree
	// anchors on the agent's directory, and a pathless search of that
	// directory walks it.
	rel := mustPolicy(t, ModeDeny, []string{"Grep"}, nil, []string{"Grep(secrets/**)"})
	rel.SetPathRoot("/work")
	if got, rule := rel.Evaluate("Grep", map[string]any{"pattern": "api_key"}); got != Deny {
		t.Errorf("a pathless search over a relative tree deny = %v (rule %q), want Deny", got, rule)
	}

	// The omission feeds every closing verdict: a scoped ask naming the
	// agent's directory re-gates the pathless search before the bare allow.
	ask := mustPolicy(t, ModeAsk, []string{"Grep"}, []string{"Grep(/work/**)"}, nil)
	ask.SetPathRoot("/work")
	if got, rule := ask.Evaluate("Grep", map[string]any{"pattern": "api_key"}); got != Ask {
		t.Errorf("a scoped ask did not re-gate the pathless search = %v (rule %q), want Ask", got, rule)
	}
}

// TestAnMCPSearchToolIsBoundByAPathDeny is the MCP half of #3086: an MCP
// tool keeps its FQN as its canonical name, so the search-family tables
// never named it — a deny on a tree met nothing the call carried, and even
// a search rooted INSIDE the tree passed. The filesystem-shaped servers
// carry their root in `path`, and the closing side reads it now: the plain
// inside-the-tree matching and the containment both bind, while the
// opening side stays the k=v reading (an allow must name what it opens).
func TestAnMCPSearchToolIsBoundByAPathDeny(t *testing.T) {
	p := mustPolicy(t, ModeDeny,
		[]string{"mcp__fs__search_files", "mcp__fs__list_directory"},
		nil,
		[]string{"mcp__fs__search_files(/run/iterion/secrets/**)", "mcp__fs__list_directory(/run/iterion/secrets/**)"},
	)
	for _, tc := range []struct {
		name  string
		tool  string
		input map[string]any
		want  Decision
	}{
		{"rooted inside the tree", "mcp__fs__search_files", map[string]any{"path": "/run/iterion/secrets", "pattern": "."}, Deny},
		{"rooted above the tree", "mcp__fs__search_files", map[string]any{"path": "/run/iterion", "pattern": "."}, Deny},
		{"rooted at the filesystem root", "mcp__fs__search_files", map[string]any{"path": "/", "pattern": "."}, Deny},
		{"a listing of the tree's parent", "mcp__fs__list_directory", map[string]any{"path": "/run/iterion"}, Deny},
		{"a search elsewhere stays allowed", "mcp__fs__search_files", map[string]any{"path": "/work/pkg", "pattern": "."}, Allow},
		{"a sibling of the tree stays allowed", "mcp__fs__search_files", map[string]any{"path": "/run/iterion/other", "pattern": "."}, Allow},
		// No `path` key, nothing to read: a call the gate cannot place
		// keeps the k=v reading alone.
		{"a call with no path key", "mcp__fs__search_files", map[string]any{"pattern": "."}, Allow},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got, rule := p.Evaluate(tc.tool, tc.input); got != tc.want {
				t.Errorf("Evaluate(%s, %v) = %v (rule %q), want %v", tc.tool, tc.input, got, rule, tc.want)
			}
		})
	}

	// The per-server glob is the recipe for a tree an attached MCP server
	// must never read: one rule covers every verb the server ships.
	g := mustPolicy(t, ModeDeny, []string{"mcp__fs__search_files"}, nil, []string{"mcp__fs__*(/run/iterion/secrets/**)"})
	if got, rule := g.Evaluate("mcp__fs__search_files", map[string]any{"path": "/run/iterion", "pattern": "."}); got != Deny {
		t.Errorf("a server-wide tree deny did not bind the parent-rooted search = %v (rule %q), want Deny", got, rule)
	}

	// The single-underscore spelling a backend may send canonicalises to
	// itself, and a rule written in that spelling binds it the same way.
	c := mustPolicy(t, ModeDeny, []string{"mcp_fs__search_files"}, nil, []string{"mcp_fs__search_files(/run/iterion/secrets/**)"})
	if got, rule := c.Evaluate("mcp_fs__search_files", map[string]any{"path": "/run/iterion", "pattern": "."}); got != Deny {
		t.Errorf("the single-underscore FQN escaped its own tree deny = %v (rule %q), want Deny", got, rule)
	}

	// An omitted root reads the agent's directory for an MCP tool too.
	o := mustPolicy(t, ModeDeny, []string{"mcp__fs__search_files"}, nil, []string{"mcp__fs__search_files(/work/secrets/**)"})
	o.SetPathRoot("/work")
	if got, rule := o.Evaluate("mcp__fs__search_files", map[string]any{"pattern": "api_key"}); got != Deny {
		t.Errorf("a pathless MCP search over the tree's parent = %v (rule %q), want Deny", got, rule)
	}
}

// TestAnAskedTreeReGatesTheParentSearch is the ask half of #3086: a scoped
// ask naming a tree is deliberately stronger than every allow, but it met
// only the spellings of a search rooted INSIDE the tree — rooting the same
// search at the tree's parent walked the tree whole without the pause the
// operator wrote. The containment's ask half pauses it, at the scoped ask's
// rank.
func TestAnAskedTreeReGatesTheParentSearch(t *testing.T) {
	p := mustPolicy(t, ModeAsk, []string{"Grep", "Glob"}, []string{"Grep(/run/iterion/secrets/**)"}, nil)
	for _, tc := range []struct {
		name  string
		tool  string
		input map[string]any
		want  Decision
	}{
		{"grep from the tree's parent asks", "Grep", map[string]any{"pattern": ".", "path": "/run/iterion"}, Ask},
		{"grep from the filesystem root asks", "Grep", map[string]any{"pattern": ".", "path": "/"}, Ask},
		{"grep rooted inside the tree asks", "Grep", map[string]any{"pattern": ".", "path": "/run/iterion/secrets"}, Ask},
		// No over-asking: an unrelated tree, and a sibling of the asked
		// one, keep the allow the author wrote.
		{"a search elsewhere stays allowed", "Grep", map[string]any{"pattern": ".", "path": "/work/pkg"}, Allow},
		{"a sibling of the asked tree stays allowed", "Grep", map[string]any{"pattern": ".", "path": "/run/iterion/other"}, Allow},
		// The ask names Grep's tree only: a Glob over the same parent is
		// no call the operator re-gated.
		{"a glob over the parent keeps its allow", "Glob", map[string]any{"pattern": "*", "path": "/run/iterion"}, Allow},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got, rule := p.Evaluate(tc.tool, tc.input); got != tc.want {
				t.Errorf("Evaluate(%s, %v) = %v (rule %q), want %v", tc.tool, tc.input, got, rule, tc.want)
			}
		})
	}

	// The omitted root is the agent's directory on the closing side: a
	// pathless search from the asked tree's parent pauses too.
	o := mustPolicy(t, ModeAsk, []string{"Grep"}, []string{"Grep(/work/secrets/**)"}, nil)
	o.SetPathRoot("/work")
	if got, rule := o.Evaluate("Grep", map[string]any{"pattern": "api_key"}); got != Ask {
		t.Errorf("a pathless search over the asked tree's parent = %v (rule %q), want Ask", got, rule)
	}
}

// TestAServerWideTreeDenyBindsEveryPathKey: the documented recipe for an
// attached filesystem server — one `mcp__<server>__*(tree/**)` deny covering
// it whole — must bind the verbs that carry their targets under keys other
// than `path`. A move's two endpoints and a multi-read's list walked the
// denied tree with the gate reading only the k=v text, whose leading
// `destination=`/`paths=` the anchored tree regex never meets.
func TestAServerWideTreeDenyBindsEveryPathKey(t *testing.T) {
	p := mustPolicy(t, ModeDeny,
		[]string{"mcp__fs__move_file", "mcp__fs__read_multiple_files"},
		nil,
		[]string{"mcp__fs__*(/run/iterion/secrets/**)"},
	)
	for _, tc := range []struct {
		name  string
		tool  string
		input map[string]any
		want  Decision
	}{
		{"a move out of the tree", "mcp__fs__move_file", map[string]any{"source": "/run/iterion/secrets/key.pem", "destination": "/work/key.pem"}, Deny},
		{"a move into the tree", "mcp__fs__move_file", map[string]any{"source": "/work/key.pem", "destination": "/run/iterion/secrets/key.pem"}, Deny},
		{"a move of the tree's parent", "mcp__fs__move_file", map[string]any{"source": "/run/iterion", "destination": "/work/iterion"}, Deny},
		{"a multi-read with one file in the tree", "mcp__fs__read_multiple_files", map[string]any{"paths": []any{"/work/ok.md", "/run/iterion/secrets/a"}}, Deny},
		{"a multi-read as a caller-built string slice", "mcp__fs__read_multiple_files", map[string]any{"paths": []string{"/run/iterion/secrets/a"}}, Deny},
		{"a multi-read outside the tree", "mcp__fs__read_multiple_files", map[string]any{"paths": []any{"/work/ok.md", "/work/other.md"}}, Allow},
		{"a move between outside paths", "mcp__fs__move_file", map[string]any{"source": "/work/a", "destination": "/work/b"}, Allow},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got, rule := p.Evaluate(tc.tool, tc.input); got != tc.want {
				t.Errorf("Evaluate(%s, %v) = %v (rule %q), want %v", tc.tool, tc.input, got, rule, tc.want)
			}
		})
	}
}
