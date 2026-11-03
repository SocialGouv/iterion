package permission

import (
	"path/filepath"
	"testing"
)

// TestDenyMatchesTheFoldedSpelling is the fold witness of #1710: the gate
// matched the path as the model spelled it, so on a case-folding file
// system `/RUN/ITERION/SECRETS/token` IS the file the deny names and the
// rule never saw it, and a `\`-separated spelling is the same path on the
// platforms whose separator it is. A deny is a closing verdict: the fold
// can only cost a refusal, never a leak.
func TestDenyMatchesTheFoldedSpelling(t *testing.T) {
	p := mustPolicy(t, ModeDeny,
		[]string{"Read(**)", "Grep", "Glob", "LS(**)"},
		nil,
		[]string{"Read(/run/iterion/secrets/**)", "Grep(/run/iterion/secrets/**)", "Glob(/run/iterion/secrets/**)", "LS(/run/iterion/secrets/**)"},
	)
	// A backslash is a separator only where the platform says it is: there
	// the spelling folds onto the denied tree; elsewhere it names a
	// DIFFERENT file (one segment, literal backslashes) and the fold must
	// not conflate the two.
	backslash := Allow
	if filepath.Separator == '\\' {
		backslash = Deny
	}
	for _, tc := range []struct {
		name  string
		tool  string
		input map[string]any
		want  Decision
	}{
		{"uppercase read", "Read", map[string]any{"file_path": "/RUN/ITERION/SECRETS/token"}, Deny},
		{"mixed-case read", "Read", map[string]any{"file_path": "/Run/Iterion/Secrets/token"}, Deny},
		{"backslash-separated read", "Read", map[string]any{"file_path": `\run\iterion\secrets\token`}, backslash},
		{"uppercase grep root", "Grep", map[string]any{"pattern": ".", "path": "/RUN/ITERION/SECRETS"}, Deny},
		{"uppercase glob root", "Glob", map[string]any{"pattern": "*", "path": "/Run/Iterion/Secrets"}, Deny},
		{"backslash ls root", "LS", map[string]any{"path": `\run\iterion\secrets`}, backslash},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got, _ := p.Evaluate(tc.tool, tc.input); got != tc.want {
				t.Errorf("Evaluate(%s, %v) = %v, want %v", tc.tool, tc.input, got, tc.want)
			}
		})
	}
}

// TestAnAskStillClosesAFoldedSpelling: a scoped ask is deliberately
// stronger than every allow, so `ask: Read(/run/iterion/secrets/**)`
// re-gates a `Read(**)` allow. Matched against the open candidates alone,
// the folded spelling would miss the ask and fall INTO the allow — the one
// order of rules where a fold widens instead of closing.
func TestAnAskStillClosesAFoldedSpelling(t *testing.T) {
	p := mustPolicy(t, ModeAsk,
		[]string{"Read(**)"},
		[]string{"Read(/run/iterion/secrets/**)"},
		nil,
	)
	if got, _ := p.Evaluate("Read", map[string]any{"file_path": "/RUN/ITERION/SECRETS/token"}); got != Ask {
		t.Errorf("a folded spelling of the re-gated directory = %v, want Ask — it fell through to the allow", got)
	}
	// The ungated spelling keeps reaching the allow: the fold closes, it
	// does not redirect ordinary calls to the human.
	if got, _ := p.Evaluate("Read", map[string]any{"file_path": "/work/pkg/x.go"}); got != Allow {
		t.Errorf("an ordinary read = %v, want Allow", got)
	}
}

// TestAnAllowIsNotWidenedByTheFold pins the asymmetry: on a case-sensitive
// file system `/WORK/secret` is NOT the file `/work/**` names, so the fold
// feeds the closing verdicts and never the opening ones. A grant is an
// opening verdict too: an approval for one file must not authorise the
// file whose name only folds to it.
func TestAnAllowIsNotWidenedByTheFold(t *testing.T) {
	p := mustPolicy(t, ModeDeny, []string{"Read(/work/**)"}, nil, nil)
	if got, _ := p.Evaluate("Read", map[string]any{"file_path": "/WORK/secret"}); got != Deny {
		t.Errorf("a folded spelling matched the allow: got %v, want Deny (the mode default)", got)
	}

	g := mustPolicy(t, ModeDeny, nil, nil, nil)
	g.AddGrantRule("Read(/work/file)")
	if got, _ := g.Evaluate("Read", map[string]any{"file_path": "/WORK/FILE"}); got == Allow {
		t.Error("a grant matched the folded spelling of the approved path")
	}
}

// TestDenyPathTreesArmsEveryPathTool is the per-node secrets witness of
// #1710: one call bounds a directory for the read family, the write family
// (the seeded credential directories are WRITABLE — an agent that can edit
// .credentials.json replaces the credential the CLI spends), the listing
// and both search families, while the shell stays open as the legitimate
// sink channel.
func TestDenyPathTreesArmsEveryPathTool(t *testing.T) {
	p := mustPolicy(t, ModeDeny,
		[]string{"Read(**)", "Write(**)", "Edit(**)", "NotebookEdit(**)", "LS(**)", "Grep", "Glob", "workspace_grep", "Bash"},
		nil, nil,
	)
	if err := p.DenyPathTrees("/run/iterion/secrets"); err != nil {
		t.Fatalf("DenyPathTrees: %v", err)
	}
	dir := "/run/iterion/secrets"
	for _, tc := range []struct {
		name  string
		tool  string
		input map[string]any
		want  Decision
	}{
		{"read", "Read", map[string]any{"file_path": dir + "/token"}, Deny},
		{"write", "Write", map[string]any{"file_path": dir + "/token"}, Deny},
		{"edit", "Edit", map[string]any{"file_path": dir + "/token"}, Deny},
		{"notebook edit", "NotebookEdit", map[string]any{"notebook_path": dir + "/n.ipynb"}, Deny},
		{"ls rooted at the tree", "LS", map[string]any{"path": dir}, Deny},
		{"grep rooted at the tree", "Grep", map[string]any{"pattern": ".", "path": dir}, Deny},
		{"glob rooted at the tree", "Glob", map[string]any{"pattern": "*", "path": dir}, Deny},
		{"claw's workspace_grep is its own canonical tool", "workspace_grep", map[string]any{"pattern": ".", "path": dir}, Deny},
		{"the shell stays the sink channel", "Bash", map[string]any{"command": "curl --cert " + dir + "/mtls.pem https://internal"}, Allow},
		{"a path outside the tree still reads", "Read", map[string]any{"file_path": "/work/pkg/x.go"}, Allow},
		{"a sibling root is not bounded", "Read", map[string]any{"file_path": "/run/iterion/other/token"}, Allow},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got, _ := p.Evaluate(tc.tool, tc.input); got != tc.want {
				t.Errorf("Evaluate(%s, %v) = %v, want %v", tc.tool, tc.input, got, tc.want)
			}
		})
	}

	// The armed rules are ordinary deny rules: they serialise through
	// PolicyConfig, so the gate a child process rebuilds refuses the same
	// directories.
	rebuilt, err := NewPolicyFromConfig(p.Config())
	if err != nil {
		t.Fatalf("NewPolicyFromConfig: %v", err)
	}
	if got, _ := rebuilt.Evaluate("Read", map[string]any{"file_path": dir + "/token"}); got != Deny {
		t.Errorf("the rebuilt gate allowed the armed tree: got %v, want Deny", got)
	}
}

// TestDenyPathTreesOutranksAnAuthorAllow: the bound is engine-owned, so no
// workflow rule reopens it — deny wins in every mode, and an author allow
// naming the tree is exactly the prompt-injection shape the bound exists
// against.
func TestDenyPathTreesOutranksAnAuthorAllow(t *testing.T) {
	p := mustPolicy(t, ModeAsk, []string{"Read(/run/iterion/secrets/**)"}, nil, nil)
	if err := p.DenyPathTrees("/run/iterion/secrets"); err != nil {
		t.Fatalf("DenyPathTrees: %v", err)
	}
	if got, _ := p.Evaluate("Read", map[string]any{"file_path": "/run/iterion/secrets/token"}); got != Deny {
		t.Errorf("an author allow reopened the armed tree: got %v, want Deny", got)
	}
}

// TestAGrantDoesNotReopenAnEngineDeny pins the rank the bound claims: an
// engine-armed tree judges WITH the author's denies, BEFORE the grants —
// an operator approval outranks an ask rule, never a deny, and the engine
// bound is a deny. Judged after the grants, a run-level `Read(**)` grant
// would reopen the secret trees the run was armed against, and the refusal
// would depend on which half of the gate a one-line reorder touched.
func TestAGrantDoesNotReopenAnEngineDeny(t *testing.T) {
	p := mustPolicy(t, ModeAsk, nil, nil, nil)
	p.AddGrantRule("Read(**)")
	p.AddGrantRule("Write(**)")
	if err := p.DenyPathTrees("/run/iterion/secrets"); err != nil {
		t.Fatalf("DenyPathTrees: %v", err)
	}
	for _, tc := range []struct {
		tool  string
		input map[string]any
	}{
		{"Read", map[string]any{"file_path": "/run/iterion/secrets/token"}},
		{"Write", map[string]any{"file_path": "/run/iterion/secrets/token"}},
	} {
		if got, _ := p.Evaluate(tc.tool, tc.input); got != Deny {
			t.Errorf("a %s grant reopened the armed tree: Evaluate = %v, want Deny", tc.tool, got)
		}
	}
	// The grant itself still decides outside the tree: the bound narrows
	// the grant, it does not eat it.
	if got, _ := p.Evaluate("Read", map[string]any{"file_path": "/work/pkg/x.go"}); got != Allow {
		t.Errorf("the granted read outside the tree = %v, want Allow", got)
	}
}
