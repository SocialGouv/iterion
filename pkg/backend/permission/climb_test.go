package permission

import (
	"strings"
	"testing"
)

// TestARawClimbDoesNotWidenANarrowAllow is #2918's headline witness, the
// spelling the issue measured: the string the model sent was offered to the
// OPENING verdicts as it was, so its text met `Read(/run/iterion/secrets/**)`
// while the file it names is /etc/passwd.
func TestARawClimbDoesNotWidenANarrowAllow(t *testing.T) {
	p := mustPolicy(t, ModeDeny, []string{"Read(/run/iterion/secrets/**)"}, nil, nil)
	if got, rule := p.Evaluate("Read", map[string]any{"file_path": "/run/iterion/secrets/../../../etc/passwd"}); got != Deny {
		t.Errorf("Read(/run/iterion/secrets/../../../etc/passwd) under allow Read(/run/iterion/secrets/**) = %v (rule %q), want Deny", got, rule)
	}
	// The allow still opens the tree it names.
	if got, _ := p.Evaluate("Read", map[string]any{"file_path": "/run/iterion/secrets/token"}); got != Allow {
		t.Errorf("a read inside the allowed tree = %v, want Allow", got)
	}
}

// TestNoClimbingSpellingWidensAnOpeningRule is the class around the issue's
// spelling: every shape of a `..` segment, on every opening verdict (a
// workflow allow, an operator grant, the literal allow that supersedes a bare
// ask), for every tool family whose argument is a path. Each row is one
// spelling against one rule that its TEXT satisfies and its FILE does not.
func TestNoClimbingSpellingWidensAnOpeningRule(t *testing.T) {
	for _, tc := range []struct {
		name  string
		allow string
		tool  string
		input map[string]any
	}{
		{"a/../b leaves the tree", "Read(/work/docs/**)", "Read", map[string]any{"file_path": "/work/docs/../secrets/key"}},
		{"a trailing climb lists the parent", "LS(/work/docs/**)", "LS", map[string]any{"path": "/work/docs/.."}},
		{"repeated separators around the climb", "Read(/work/docs/**)", "Read", map[string]any{"file_path": "/work/docs//..//..//etc/passwd"}},
		{"a dot segment before the climb", "Read(/work/docs/**)", "Read", map[string]any{"file_path": "/work/docs/./../../etc/passwd"}},
		{"claw's read_file key", "Read(/work/docs/**)", "read_file", map[string]any{"path": "/work/docs/../../etc/passwd"}},
		{"write", "Write(/work/docs/**)", "Write", map[string]any{"file_path": "/work/docs/../../etc/cron.d/x"}},
		{"edit", "Edit(/work/docs/**)", "Edit", map[string]any{"file_path": "/work/docs/../../etc/hosts"}},
		{"notebook", "NotebookEdit(/work/docs/**)", "NotebookEdit", map[string]any{"notebook_path": "/work/docs/../../etc/x.ipynb"}},
		{"a search root, path: form", "Grep(path:/work/docs/**)", "Grep", map[string]any{"pattern": "key", "path": "/work/docs/../../run/iterion/secrets"}},
		{"a search root, bare form", "Grep(/work/docs/**)", "Grep", map[string]any{"pattern": "key", "path": "/work/docs/../../run/iterion/secrets"}},
		{"a relative climb above the root", "Read(docs/**)", "Read", map[string]any{"file_path": "docs/../../etc/passwd"}},
		{"a relative climb to a sibling", "Read(docs/**)", "Read", map[string]any{"file_path": "docs/../secrets/key"}},
		// The cleanup of a relative path keeps its climb at the head, and a
		// rule that starts with dots reads that head as its own text.
		{"a dotfile rule meets a cleaned climb", "Read(.*)", "Read", map[string]any{"file_path": "../../etc/passwd"}},
		{"a parent rule meets a deeper climb", "Read(../**)", "Read", map[string]any{"file_path": "../../etc/passwd"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			allow := mustPolicy(t, ModeDeny, []string{tc.allow}, nil, nil)
			allow.SetPathRoot("/work")
			if got, rule := allow.Evaluate(tc.tool, tc.input); got != Deny {
				t.Errorf("allow %s: Evaluate(%s, %v) = %v (rule %q), want Deny", tc.allow, tc.tool, tc.input, got, rule)
			}
			// An operator grant opens with the same candidates — the issue's
			// "idem pour un grant opérateur".
			grant := mustPolicy(t, ModeAsk, nil, nil, nil)
			grant.SetPathRoot("/work")
			grant.AddGrantRule(tc.allow)
			if got, rule := grant.Evaluate(tc.tool, tc.input); got != Ask {
				t.Errorf("grant %s: Evaluate(%s, %v) = %v (rule %q), want Ask", tc.allow, tc.tool, tc.input, got, rule)
			}
		})
	}
}

// TestACleanupThatStaysInsideStillOpens: the fix refuses the TEXT of a climb,
// never the file — a `..` that lands back inside the tree, and the anchored
// spelling of a relative path, still meet the allow that names them.
func TestACleanupThatStaysInsideStillOpens(t *testing.T) {
	p := mustPolicy(t, ModeDeny, []string{"Read(/work/docs/**)", "Read(src/**)"}, nil, nil)
	p.SetPathRoot("/work")
	for _, tc := range []struct {
		name string
		path string
	}{
		{"absolute, landing inside", "/work/docs/sub/../x.md"},
		{"relative, landing inside", "src/a/../b.go"},
		{"relative, anchored inside an absolute rule", "pkg/../docs/x.md"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got, _ := p.Evaluate("Read", map[string]any{"file_path": tc.path}); got != Allow {
				t.Errorf("Read(%q) = %v, want Allow", tc.path, got)
			}
		})
	}
}

// TestAnUnboundedScopeStillOpensAClimb: `Read(**)` leaves no call out, so it
// keeps reading the one whose every spelling a pattern may not read — a
// relative climb with no root to anchor it, which offers the opening side no
// candidate at all.
func TestAnUnboundedScopeStillOpensAClimb(t *testing.T) {
	for _, allow := range []string{"Read(**)", "Read(*)", "Read(:*)"} {
		p := mustPolicy(t, ModeDeny, []string{allow}, nil, nil)
		if got, _ := p.Evaluate("Read", map[string]any{"file_path": "../../etc/passwd"}); got != Allow {
			t.Errorf("allow %s: a rootless climb = %v, want Allow", allow, got)
		}
	}
	// A literal empty scope bounds the empty argument alone, not a call the
	// gate refused every spelling of.
	p := mustPolicy(t, ModeDeny, []string{"Read()"}, nil, nil)
	if got, _ := p.Evaluate("Read", map[string]any{"file_path": "../../etc/passwd"}); got != Deny {
		t.Errorf("allow Read(): a rootless climb = %v, want Deny", got)
	}
}

// TestTheGateDecodesNothing: the class has no encoded member because no
// layer of the gate decodes a file path, and neither does the file system —
// `%2e%2e` is a directory NAME under the allowed tree, not a climb.
func TestTheGateDecodesNothing(t *testing.T) {
	p := mustPolicy(t, ModeDeny, []string{"Read(/work/docs/**)"}, nil, nil)
	if got, _ := p.Evaluate("Read", map[string]any{"file_path": "/work/docs/%2e%2e/%2e%2e/x"}); got != Allow {
		t.Errorf("a literal %%2e%%2e name under the allowed tree = %v, want Allow", got)
	}
	if got, _ := p.Evaluate("Read", map[string]any{"file_path": "/work/docs/..x/y"}); got != Allow {
		t.Errorf("a name that starts with dots = %v, want Allow", got)
	}
}

// TestAnExactRuleStillNamesTheSpellingAsSent: an `allow once` quotes what the
// model wrote (grantArg), climb included. An exact rule names one spelling
// whole, which a `..` segment cannot make name another file, so the
// once-grant matches its own retry — and a different climb, naming a
// different file, still asks. Withholding the spelling as sent from exact
// rules too would turn every approved climb into an approval that never
// takes; offering it to patterns is #2918.
func TestAnExactRuleStillNamesTheSpellingAsSent(t *testing.T) {
	for _, tc := range []struct {
		name  string
		root  string
		path  string
		other string
	}{
		{"absolute", "/work", "/work/docs/../../etc/passwd", "/work/docs/../../etc/shadow"},
		{"relative, climbing above the root", "/work", "../sibling/x", "../sibling/y"},
		{"relative, no root captured", "", "../sibling/x", "../sibling/y"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			call := map[string]any{"file_path": tc.path}
			grant := GrantRuleFor("Read", call, false)
			if grant != "Read("+tc.path+")" {
				t.Fatalf("GrantRuleFor(Read, %v, once) = %q, want the spelling the model sent", call, grant)
			}
			p := mustPolicy(t, ModeAsk, nil, nil, nil)
			p.SetPathRoot(tc.root)
			p.AddGrantRule(grant)
			if got, _ := p.Evaluate("Read", call); got != Allow {
				t.Errorf("the approved call's retry = %v, want Allow — the approval never takes", got)
			}
			if got, _ := p.Evaluate("Read", map[string]any{"file_path": tc.other}); got != Ask {
				t.Errorf("another climb under the once-grant = %v, want Ask", got)
			}
		})
	}
}

// TestTheClosingSideStillReadsTheClimb: a deny and an ask read every spelling
// — over-refusing costs a refusal — so a rule written against the climb
// itself keeps biting, and a deny the climb's text satisfies still refuses
// it.
//
// The tree is spelled in mixed case on purpose: the closing side also reads
// the case-folded spellings, which carry the climb too, so a lower-case tree
// would be met by the fold whether or not the spelling as sent is read — two
// guards covering each other. `/Work/Docs/**` is met by the unfolded
// spelling as sent alone.
func TestTheClosingSideStillReadsTheClimb(t *testing.T) {
	p := mustPolicy(t, ModeDeny, []string{"Read(**)"}, nil, []string{"Read(**/../**)"})
	if got, _ := p.Evaluate("Read", map[string]any{"file_path": "/work/docs/../../etc/passwd"}); got != Deny {
		t.Errorf("a deny on the climb itself = %v, want Deny", got)
	}
	climb := map[string]any{"file_path": "/Work/Docs/../../etc/passwd"}
	tree := mustPolicy(t, ModeDeny, []string{"Read(**)"}, nil, []string{"Read(/Work/Docs/**)"})
	if got, _ := tree.Evaluate("Read", climb); got != Deny {
		t.Errorf("a deny the climb's text satisfies = %v, want Deny (the closing side over-refuses)", got)
	}
	ask := mustPolicy(t, ModeDeny, []string{"Read(**)"}, []string{"Read(/Work/Docs/**)"}, nil)
	if got, _ := ask.Evaluate("Read", climb); got != Ask {
		t.Errorf("an ask the climb's text satisfies = %v, want Ask", got)
	}
}

// TestAGlobWhosePatternClimbsIsPlacedByNoOpeningRule: a glob pattern is a
// path under the search root, so one that climbs is the same class twice —
// its text meets a pattern-scoped allow (`Glob(src/**)` against
// `src/../../etc/*`), and the root no longer bounds what it reads
// (`Glob(path:/work/**)` against `../../etc/*` rooted at /work/a). A Grep
// pattern is a regular expression over contents: `..` there is no climb.
func TestAGlobWhosePatternClimbsIsPlacedByNoOpeningRule(t *testing.T) {
	for _, tc := range []struct {
		name  string
		allow string
		input map[string]any
	}{
		{"the pattern's text", "Glob(src/**)", map[string]any{"pattern": "src/../../etc/*"}},
		{"the root, path: form", "Glob(path:/work/**)", map[string]any{"pattern": "../../etc/*", "path": "/work/a"}},
		{"the root, bare form", "Glob(/work/**)", map[string]any{"pattern": "../../etc/*", "path": "/work/a"}},
		{"a climb behind a glob segment", "Glob(path:/work/**)", map[string]any{"pattern": "**/../../../etc/*", "path": "/work/a"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := mustPolicy(t, ModeDeny, []string{tc.allow}, nil, nil)
			if got, rule := p.Evaluate("Glob", tc.input); got != Deny {
				t.Errorf("allow %s: Evaluate(Glob, %v) = %v (rule %q), want Deny", tc.allow, tc.input, got, rule)
			}
		})
	}
	// The same rules still open the searches that stay under their root.
	p := mustPolicy(t, ModeDeny, []string{"Glob(path:/work/**)"}, nil, nil)
	if got, _ := p.Evaluate("Glob", map[string]any{"pattern": "**/*.go", "path": "/work/a"}); got != Allow {
		t.Errorf("a glob that stays under the allowed root = %v, want Allow", got)
	}
	// A bare or unbounded allow vouches for every search, this one included.
	for _, allow := range []string{"Glob", "Glob(**)"} {
		open := mustPolicy(t, ModeDeny, []string{allow}, nil, nil)
		if got, _ := open.Evaluate("Glob", map[string]any{"pattern": "../../etc/*", "path": "/work/a"}); got != Allow {
			t.Errorf("allow %s: a climbing glob = %v, want Allow", allow, got)
		}
	}
	grep := mustPolicy(t, ModeDeny, []string{"Grep(path:/work/**)"}, nil, nil)
	if got, _ := grep.Evaluate("Grep", map[string]any{"pattern": "a/../b", "path": "/work/a"}); got != Allow {
		t.Errorf("a grep whose regular expression holds `..` = %v, want Allow (no climb)", got)
	}
}

// TestADenyMeetsWhereAClimbingGlobLands: the closing side reads the place a
// climbing pattern reaches — the pattern joined onto its root — so the
// engine-armed secret trees bound a glob that climbs into them, from the
// root a call names or from the agent's own directory.
func TestADenyMeetsWhereAClimbingGlobLands(t *testing.T) {
	p := mustPolicy(t, ModeDeny, []string{"Glob"}, nil, nil)
	p.SetPathRoot("/work/a")
	if err := p.DenyPathTrees("/run/iterion/secrets"); err != nil {
		t.Fatalf("DenyPathTrees: %v", err)
	}
	for _, tc := range []struct {
		name  string
		input map[string]any
	}{
		{"from the call's root", map[string]any{"pattern": "../../run/iterion/secrets/*", "path": "/work/a"}},
		{"from the agent's directory", map[string]any{"pattern": "../../run/iterion/secrets/*"}},
		{"an absolute pattern read as written", map[string]any{"pattern": "/run/iterion/x/../secrets/*", "path": "/work/a"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got, rule := p.Evaluate("Glob", tc.input); got != Deny {
				t.Errorf("Evaluate(Glob, %v) = %v (rule %q), want Deny", tc.input, got, rule)
			}
		})
	}
}

// TestTheClosingSideReadsAClimbingGlobPatternAsSent: summarize offers a
// climbing glob no candidate, and the place it lands carries no `..`, so its
// pattern meets a deny or an ask only as the closing side reads it as sent —
// which keeps a rule written against the climb itself biting.
func TestTheClosingSideReadsAClimbingGlobPatternAsSent(t *testing.T) {
	call := map[string]any{"pattern": "../../etc/*", "path": "/work/a"}
	for _, climb := range []string{"Glob(**/../**)", "Glob(../**)"} {
		deny := mustPolicy(t, ModeDeny, []string{"Glob"}, nil, []string{climb})
		if got, rule := deny.Evaluate("Glob", call); got != Deny || rule != climb {
			t.Errorf("deny %s: Evaluate(Glob, %v) = %v (rule %q), want Deny by it", climb, call, got, rule)
		}
		ask := mustPolicy(t, ModeDeny, []string{"Glob"}, []string{climb}, nil)
		if got, rule := ask.Evaluate("Glob", call); got != Ask || rule != climb {
			t.Errorf("ask %s: Evaluate(Glob, %v) = %v (rule %q), want Ask by it", climb, call, got, rule)
		}
	}
}

// TestAnApprovedClimbingGlobGrantsNothingWider: a glob that climbs has no
// directory an approval could bind, so the once-grant quotes its pattern —
// never the root it leaves (which would vouch for the climb) and never an
// empty argument (which would make it a bare, whole-tool grant).
func TestAnApprovedClimbingGlobGrantsNothingWider(t *testing.T) {
	for _, tc := range []struct {
		name string
		call map[string]any
	}{
		{"rooted", map[string]any{"pattern": "../../etc/*", "path": "/work/a"}},
		{"from the agent's directory", map[string]any{"pattern": "../../etc/*"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			grant := GrantRuleFor("Glob", tc.call, false)
			if grant == "Glob" || strings.Contains(grant, "path:") {
				t.Fatalf("GrantRuleFor(Glob, %v, once) = %q — a bare or root-bound grant vouches for more than the call", tc.call, grant)
			}
			p := mustPolicy(t, ModeAsk, nil, nil, nil)
			p.AddGrantRule(grant)
			for _, other := range []map[string]any{
				{"pattern": "../../run/iterion/secrets/*", "path": "/work/a"},
				{"pattern": "*", "path": "/run/iterion/secrets"},
			} {
				if got, rule := p.Evaluate("Glob", other); got != Ask {
					t.Errorf("under grant %q: Evaluate(Glob, %v) = %v (rule %q), want Ask", grant, other, got, rule)
				}
			}
		})
	}
}

// TestTheVerdictTravelsWithThePolicy: claude_code's hook child, the grok and
// kimi hooks and a sandboxed claw runner all judge with a gate rebuilt from
// PolicyConfig, and each backend sends its own spelling of the call —
// claude_code `Read{file_path}`, claw `read_file{path}`. Every one of them
// reaches the engine's verdict on the climb.
func TestTheVerdictTravelsWithThePolicy(t *testing.T) {
	engine := mustPolicy(t, ModeDeny, []string{"Read(docs/**)", "Grep(path:/work/docs/**)"}, nil, nil)
	engine.SetPathRoot("/work")
	child, err := NewPolicyFromConfig(engine.Config())
	if err != nil {
		t.Fatalf("NewPolicyFromConfig: %v", err)
	}
	for _, tc := range []struct {
		tool  string
		input map[string]any
		want  Decision
	}{
		{"Read", map[string]any{"file_path": "docs/../../etc/passwd"}, Deny},
		{"read_file", map[string]any{"path": "docs/../../etc/passwd"}, Deny},
		{"Read", map[string]any{"file_path": "docs/x.md"}, Allow},
		{"read_file", map[string]any{"path": "docs/x.md"}, Allow},
		{"Grep", map[string]any{"pattern": "k", "path": "/work/docs/../../etc"}, Deny},
		{"grep", map[string]any{"pattern": "k", "path": "/work/docs/../../etc"}, Deny},
	} {
		for side, p := range map[string]*Policy{"engine": engine, "rebuilt": child} {
			if got, rule := p.Evaluate(tc.tool, tc.input); got != tc.want {
				t.Errorf("%s gate: Evaluate(%s, %v) = %v (rule %q), want %v", side, tc.tool, tc.input, got, rule, tc.want)
			}
		}
	}
}

// TestAFetchedURLThatClimbsIsReadByNoPattern: a URL's path is a path, and a
// fetcher resolves its dot segments before it asks the server — the encoded
// ones (`%2e%2e`) and, for http(s), the ones a `\` separates included. Such a
// URL holds the text of a full-URL allow and reaches outside it; its host
// still stands for it.
func TestAFetchedURLThatClimbsIsReadByNoPattern(t *testing.T) {
	const tree = "WebFetch(https://api.github.com/repos/SocialGouv/**)"
	p := mustPolicy(t, ModeDeny, []string{tree}, nil, nil)
	for _, u := range []string{
		"https://api.github.com/repos/SocialGouv/../other/secret",
		"https://api.github.com/repos/SocialGouv/%2e%2e/other/secret",
		"https://api.github.com/repos/SocialGouv/%2E%2E/other/secret",
		"https://api.github.com/repos/SocialGouv/.%2e/other/secret",
		`https://api.github.com/repos/SocialGouv/..\other\secret`,
		"https://api.github.com/repos/SocialGouv/iterion/../..",
	} {
		if got, rule := p.Evaluate("WebFetch", map[string]any{"url": u}); got != Deny {
			t.Errorf("allow %s: WebFetch(%q) = %v (rule %q), want Deny", tree, u, got, rule)
		}
	}
	for _, u := range []string{
		"https://api.github.com/repos/SocialGouv/iterion",
		"https://api.github.com/repos/SocialGouv/iterion?path=../x", // a query is no path
		"https://api.github.com/repos/SocialGouv/..x",               // a name holding dots
	} {
		if got, _ := p.Evaluate("WebFetch", map[string]any{"url": u}); got != Allow {
			t.Errorf("allow %s: WebFetch(%q) = %v, want Allow", tree, u, got)
		}
	}
	host := mustPolicy(t, ModeDeny, []string{"WebFetch(domain:api.github.com)"}, nil, nil)
	if got, _ := host.Evaluate("WebFetch", map[string]any{"url": "https://api.github.com/repos/SocialGouv/../other"}); got != Allow {
		t.Errorf("a host allow over a climbing URL = %v, want Allow (a dot segment moves no host)", got)
	}
	exact := mustPolicy(t, ModeDeny, []string{"WebFetch(https://api.github.com/repos/SocialGouv/../other)"}, nil, nil)
	if got, _ := exact.Evaluate("WebFetch", map[string]any{"url": "https://api.github.com/repos/SocialGouv/../other"}); got != Allow {
		t.Errorf("an exact allow naming the climbing URL = %v, want Allow", got)
	}
}

// TestADenyMeetsWhereAFetchedURLLands: the closing side reads the URL a
// fetcher reaches — read as a WHATWG fetcher reads it (readFetchURL), its
// path decoded, dot segments and repeated separators resolved, an http URL
// also upgraded to https as claude_code's WebFetch upgrades it — beside the
// URL as sent, so a deny on a tree is not walked past by a spelling of a URL
// inside it.
func TestADenyMeetsWhereAFetchedURLLands(t *testing.T) {
	p := mustPolicy(t, ModeDeny, []string{"WebFetch"}, nil, []string{"WebFetch(https://e.example/admin/**)"})
	for _, u := range []string{
		"https://e.example/docs/../admin/users",
		"https://e.example/docs/%2e%2e/admin/users",
		"https://e.example/./admin/users",
		"https://e.example//admin/users",
		"https://e.example/%61dmin/users",
		`https://e.example/docs/..\admin\users`,
		"https://e.example/docs/../admin/x/..", // lands on the tree itself, /admin/
		"https:e.example/docs/../admin/users",
		"https:/e.example/admin/users",
		"https:///e.example/admin/users",
		`https://e.example\docs\..\admin\users`,
		"https://e.example/docs/.\t./admin/users",
		" https://e.example/admin/users",
		"HTTPS://E.example/admin/users",
		"https://e.example:443/admin/users",
		"https://e.example./admin/users",
		"http://e.example/admin/users",
	} {
		if got, rule := p.Evaluate("WebFetch", map[string]any{"url": u}); got != Deny {
			t.Errorf("WebFetch(%q) under deny https://e.example/admin/** = %v (rule %q), want Deny", u, got, rule)
		}
	}
	if got, _ := p.Evaluate("WebFetch", map[string]any{"url": "https://e.example/docs/x"}); got != Allow {
		t.Errorf("a fetch outside the denied tree = %v, want Allow", got)
	}
}

// TestAToolArgumentThatClimbsIsReadByNoPattern: an MCP or custom tool is
// scoped through its sorted `k=v` arguments, and the gate does not know which
// value is a path — so a value carrying a `..` segment keeps the call from
// every pattern, as a climbing path spelling does.
func TestAToolArgumentThatClimbsIsReadByNoPattern(t *testing.T) {
	p := mustPolicy(t, ModeDeny, []string{"mcp__fs__get_file(path=/work/**)"}, nil, nil)
	if got, rule := p.Evaluate("mcp__fs__get_file", map[string]any{"path": "/work/../etc/passwd"}); got != Deny {
		t.Errorf("a climbing argument = %v (rule %q), want Deny", got, rule)
	}
	if got, _ := p.Evaluate("mcp__fs__get_file", map[string]any{"path": "/work/x"}); got != Allow {
		t.Errorf("an argument inside the tree = %v, want Allow", got)
	}
}
