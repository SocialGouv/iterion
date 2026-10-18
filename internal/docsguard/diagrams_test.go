package docsguard

import (
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// The production run. One parse of the tree, one verdict per property, so a
// page that owes entities and a page that owes gates redden separately and a
// reader of the failure knows which half to go and write.
//
// The floors come first and they STOP: a guard that read no collection, or
// one whose ident rule resolved nothing, would report an empty disagreement
// and look like agreement. Any subtest after that point would be certifying
// nothing.
func TestCheckDiagramsAcceptsThisTree(t *testing.T) {
	root := filepath.Join("..", "..")
	rep, err := CheckDiagrams(root, IterionSpec())
	if err != nil {
		t.Fatal(err)
	}
	if problem := collectionFloor(rep); problem != "" {
		t.Fatalf("the guard read nothing and would pass on anything: %s", problem)
	}
	if got, want := len(IterionSpec().Gates), gateFloor; got < want {
		t.Fatalf("the gate table is down to %d rows from %d — a table that shrinks is a page that stops being held; if the code dropped a gate, delete its row and lower gateFloor in the same change, saying which gate went", got, want)
	}
	t.Logf("read %d collections (%d ident, %d selector, %d literal sites) + %d allowlisted, %d dynamic sites, %d entities across %d pages",
		len(rep.Collections), rep.Resolutions[ByIdent], rep.Resolutions[BySelector], rep.Resolutions[ByLiteral],
		len(rep.Allowlisted), len(rep.DynamicSites), len(rep.Entities), len(rep.ERPages))

	t.Run("every known collection is drawn", func(t *testing.T) {
		if names := rep.Subjects(UnmappedCollection); len(names) > 0 {
			t.Errorf("%d collections the code reaches are in no erDiagram block under docs/diagrams/data-model-*.md:\n  %s\n\n%s",
				len(names), strings.Join(names, "\n  "), rep.Lines(MissingCorpus))
		}
	})

	t.Run("every entity drawn answers for something", func(t *testing.T) {
		if problems := rep.Lines(UnknownEntity); problems != "" {
			t.Errorf("the ER pages draw entities nothing answers for:\n  %s", problems)
		}
	})

	t.Run("every ER line is in a form the guard reads", func(t *testing.T) {
		if problems := rep.Lines(UnreadERLine); problems != "" {
			t.Errorf("an erDiagram line is in no form the guard reads, so an entity on it would go unchecked:\n  %s", problems)
		}
	})

	t.Run("every dynamic site is named by a row", func(t *testing.T) {
		if problems := rep.Lines(UnlistedDynamicSite, StaleDynamicRow); problems != "" {
			t.Errorf("the dynamic-site table and the tree disagree:\n  %s", problems)
		}
	})

	t.Run("every element of the purger's swept lists is a literal naming a collection a call site resolves to", func(t *testing.T) {
		if problems := rep.Lines(UnknownSweptCollection); problems != "" {
			t.Errorf("the org purger's swept lists hold an element no `.Collection(` call site resolves, or one that is not a literal:\n  %s", problems)
		}
	})

	t.Run("the access-control page's source names every gate of the table", func(t *testing.T) {
		if problems := rep.Lines(UndeclaredGate, UndrawnGate); problems != "" {
			t.Errorf("the gate table, the code and docs/diagrams/access-control.md disagree:\n  %s", problems)
		}
	})

	t.Run("no stateDiagram line outside a %% comment holds a semicolon", func(t *testing.T) {
		if problems := rep.Lines(SemicolonInStateLabel); problems != "" {
			t.Errorf("a state diagram under docs/diagrams holds a `;`:\n  %s", problems)
		}
	})

	t.Run("every corpus the guard reads is there", func(t *testing.T) {
		if problems := rep.Lines(MissingCorpus); problems != "" {
			t.Errorf("the guard could not read part of what it checks, so the verdicts above cover less than the tree:\n  %s", problems)
		}
	})

	// A refusal the run produced that no subtest above reads would be invisible
	// — a green verdict over a red report. This catches such a refusal by its
	// kind; it cannot see a kind the run did not produce, so a new check must
	// still add its own subtest above and its kind here.
	t.Run("every refusal kind the run produced is read by a subtest", func(t *testing.T) {
		read := map[ProblemKind]bool{
			UnmappedCollection: true, UnknownEntity: true, UnreadERLine: true, UnlistedDynamicSite: true, StaleDynamicRow: true,
			UnknownSweptCollection: true, UndeclaredGate: true, UndrawnGate: true, SemicolonInStateLabel: true,
			MissingCorpus: true,
		}
		for _, kind := range rep.KindsSeen() {
			if !read[kind] {
				t.Errorf("the run refused on %q and no subtest reads that kind:\n  %s", kind, rep.Lines(kind))
			}
		}
	})

	// A green verdict above is only worth what the run that produced it read.
	// The fixture proves the rules bite on a tree this test wrote; these
	// probes prove four of them bite on THIS one, by moving the only thing a test may
	// move about the real tree — the spec — and watching the real corpus
	// refuse it. Each is red for a reason no other probe can supply.
	t.Run("the run is reading this tree and not agreeing with itself", func(t *testing.T) {
		for _, probe := range []struct {
			name    string
			spec    func(Spec) Spec
			kind    ProblemKind
			subject string
		}{{
			name: "a gate symbol pkg/server does not declare",
			spec: func(s Spec) Spec {
				s.Gates = append([]GateRef{{"canViewTeamThatIsNotThere", "pkg/server/auth_authz.go"}}, s.Gates...)
				return s
			},
			kind:    UndeclaredGate,
			subject: "canViewTeamThatIsNotThere",
		}, {
			name: "the purger's dynamic site with its row taken away",
			spec: func(s Spec) Spec {
				s.Dynamic = s.Dynamic[1:]
				return s
			},
			kind:    UnlistedDynamicSite,
			subject: "pkg/cloud/orgsweep/sweep.go:deleteMany",
		}, {
			name: "a row naming a collection the pages do not draw",
			spec: func(s Spec) Spec {
				s.Dynamic[0].Names = append(s.Dynamic[0].Names, "a_collection_no_page_draws")
				return s
			},
			kind:    UnmappedCollection,
			subject: "a_collection_no_page_draws",
		}, {
			name: "a swept list the purger no longer declares",
			spec: func(s Spec) Spec {
				s.Swept = []SliceRef{{"pkg/cloud/orgsweep/sweep.go", "collectionsSweptLongAgo"}}
				return s
			},
			kind:    MissingCorpus,
			subject: "pkg/cloud/orgsweep/sweep.go:collectionsSweptLongAgo",
		}} {
			t.Run(probe.name, func(t *testing.T) {
				probed, err := CheckDiagrams(root, probe.spec(IterionSpec()))
				if err != nil {
					t.Fatal(err)
				}
				if got := probed.Subjects(probe.kind); !equal(got, []string{probe.subject}) {
					t.Errorf("the real tree refused %v on %s, want exactly [%s]\n  %s",
						got, probe.kind, probe.subject, probed.Lines())
				}
			})
		}
	})
}

// gateFloor is the number of gates the table held when it was written. The
// count may only grow: a row removed because the page was easier to write
// without it is the one change this guard cannot see any other way.
const gateFloor = 40

// collectionFloor reports why a run cannot be trusted, or "" when it can.
//
// It is a predicate rather than an assertion so that both sides can hold it:
// the production run fails on it, and the test below drives a tree it must
// fire on. An assertion written twice is an assertion tested nowhere.
//
// The ident rule is the only one the floor can demand of THIS tree: every
// store under pkg/ names its collection with a constant of its own package,
// and the selector and literal forms appear only in _test.go files, which
// this guard does not read (pkg/dispatcher/boardmongo's conformance tests
// pass boardmongo.IssuesCollection; pkg/internal/storekit's passes "tickets"
// and "recs" — names no production store owns). Those two rules are held by
// the fixture instead, which reaches one collection through each.
func collectionFloor(rep Report) string {
	const want = 70
	// Both halves, always: reporting the first and returning would leave the
	// second untestable, and an assertion nothing can drive is decoration.
	var reasons []string
	if len(rep.Collections) < want {
		reasons = append(reasons, "only "+strconv.Itoa(len(rep.Collections))+
			" collection names resolved, want at least "+strconv.Itoa(want))
	}
	if rep.Resolutions[ByIdent] == 0 {
		reasons = append(reasons, "no site resolved by ident — the one rule every store under pkg/ uses "+
			"matched nothing, so the pass read names by luck or not at all")
	}
	return strings.Join(reasons, "; ")
}

// The end state the guard drives toward: a tree whose pages draw exactly what
// its code reaches. A guard that refused this would punish the fix.
func TestTheDiagramGuardAcceptsATreeThatAgrees(t *testing.T) {
	root := syntheticDiagramTree(t)
	rep, err := CheckDiagrams(root, syntheticSpec())
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Problems) != 0 {
		t.Fatalf("a tree whose pages agree with its code was refused:\n  %s", rep.Lines())
	}
	// Each resolution rule carries a collection of its OWN here — no other
	// rule could have produced that name — which is what keeps a rule from
	// becoming decoration. The real tree cannot hold this: every store under
	// pkg/ names its collection with a constant of its own package, so the
	// selector and literal rules are exercised nowhere but here, and a break
	// in either turns its name into an unlisted dynamic site above.
	for name, want := range map[string]Resolution{"widgets": ByIdent, "gizmos": BySelector, "sprockets": ByLiteral} {
		var kinds []string
		for _, site := range rep.Sites[name] {
			kinds = append(kinds, string(site.How))
		}
		if !equal(kinds, []string{string(want)}) {
			t.Errorf("%s was reached by %v, want exactly [%s]", name, kinds, want)
		}
	}
	if want := []string{"cogs", "gadgets", "gizmos", "sprockets", "widgets"}; !equal(rep.Known(), want) {
		t.Errorf("read %v, want %v", rep.Known(), want)
	}
	if want := []string{"S3: artifacts", "cogs", "gadgets", "gizmos", "sprockets", "widgets"}; !equal(rep.Entities, want) {
		t.Errorf("drew %v, want %v", rep.Entities, want)
	}
}

// At least one mutant per refusal arm a fixture tree can reach — the two
// directory-listing arms are held by TestADiagramsDirectoryThatCannotBeListedIsRefused —
// each of which must move exactly the verdicts its row names and no other.
//
// Comparing the whole SET of kinds the report refuses on, rather than looking
// for one of them, is what makes the set a witness: a rule that fired on
// every mutant — a resolution that silently returned nothing, a page scanner
// that read no entity — would pass a "contains" assertion on every row
// here and prove none of them.
func TestTheDiagramGuardBites(t *testing.T) {
	for _, tc := range []struct {
		name     string
		mutate   func(t *testing.T, root string, spec *Spec)
		kinds    []ProblemKind
		subjects []string // of the first kind
		detail   string   // when set, a substring of the first kind's lines
	}{{
		name: "an entity dropped from the page",
		mutate: func(t *testing.T, root string, _ *Spec) {
			writePage(t, root, withoutBlock(syntheticERLines, "gizmos {"))
		},
		kinds:    []ProblemKind{UnmappedCollection},
		subjects: []string{"gizmos"},
	}, {
		name: "an entity drawn only as the far end of a relationship, dropped with its line",
		mutate: func(t *testing.T, root string, _ *Spec) {
			writePage(t, root, withoutLine(syntheticERLines, "widgets ||--o{ cogs"))
		},
		kinds:    []ProblemKind{UnmappedCollection},
		subjects: []string{"cogs"},
	}, {
		name: "an entity drawn only bare, dropped with its line",
		mutate: func(t *testing.T, root string, _ *Spec) {
			writePage(t, root, withoutLine(syntheticERLines, "sprockets"))
		},
		kinds:    []ProblemKind{UnmappedCollection},
		subjects: []string{"sprockets"},
	}, {
		name: "an ER line in a form the guard does not read",
		mutate: func(t *testing.T, root string, _ *Spec) {
			// Mermaid parses a word cardinality; an entity drawn only on such
			// a line would be invisible to a reader that skipped it.
			writePage(t, root, append(append([]string{}, syntheticERLines...), `widgets only one to zero or more gizmos : "a word form"`))
		},
		kinds:    []ProblemKind{UnreadERLine},
		subjects: []string{"docs/diagrams/data-model-core.md"},
		detail:   "only one to zero or more",
	}, {
		name: "a statement after a relationship's quoted label",
		mutate: func(t *testing.T, root string, _ *Spec) {
			writePage(t, root, append(append([]string{}, syntheticERLines...), `widgets ||--o{ cogs : "holds" typo`))
		},
		kinds:    []ProblemKind{UnreadERLine},
		subjects: []string{"docs/diagrams/data-model-core.md"},
		detail:   `"holds" typo`,
	}, {
		name: "a statement after the brace that closes an entity block",
		mutate: func(t *testing.T, root string, _ *Spec) {
			lines := append([]string{}, syntheticERLines...)
			for i, line := range lines {
				if line == "}" {
					lines[i] = "} typo"
					break
				}
			}
			writePage(t, root, lines)
		},
		kinds:    []ProblemKind{UnreadERLine},
		subjects: []string{"docs/diagrams/data-model-core.md"},
		detail:   "} typo",
	}, {
		name: "a statement on the erDiagram header line",
		mutate: func(t *testing.T, root string, _ *Spec) {
			writeIn(t, root, "docs/diagrams/data-model-h.md", "# H\n\n```mermaid\nerDiagram typo\n    widgets {\n        string _id\n    }\n```\n")
		},
		kinds:    []ProblemKind{UnreadERLine},
		subjects: []string{"docs/diagrams/data-model-h.md"},
		detail:   "typo",
	}, {
		name: "a keyword before a directive on the ER header line",
		mutate: func(t *testing.T, root string, _ *Spec) {
			writeIn(t, root, "docs/diagrams/data-model-k.md", "# K\n\n```mermaid\nerDiagram %%{init: {\"theme\":\"base\"}}%%\n    widgets {\n        string _id\n    }\n```\n")
		},
		kinds:    []ProblemKind{UnreadERLine},
		subjects: []string{"docs/diagrams/data-model-k.md"},
		detail:   "%%{init",
	}, {
		name: "a directive followed by a statement inside an ER block",
		mutate: func(t *testing.T, root string, _ *Spec) {
			writePage(t, root, append(append([]string{}, syntheticERLines...), `%%{init: {"theme":"base"}}%% typo`))
		},
		kinds:    []ProblemKind{UnreadERLine},
		subjects: []string{"docs/diagrams/data-model-core.md"},
		detail:   "}%% typo",
	}, {
		name: "a quoted comment left open on its line",
		mutate: func(t *testing.T, root string, _ *Spec) {
			writeIn(t, root, "docs/diagrams/data-model-q.md", "# Q\n\n```mermaid\nerDiagram\n    widgets {\n        string _id \"opens here\n        closes here\"\n    }\n```\n")
		},
		kinds:    []ProblemKind{UnreadERLine},
		subjects: []string{"docs/diagrams/data-model-q.md", "docs/diagrams/data-model-q.md"},
		detail:   "opens here",
	}, {
		name: "YAML front matter before the ER header is read past",
		mutate: func(t *testing.T, root string, _ *Spec) {
			writeIn(t, root, "docs/diagrams/data-model-f.md", "# F\n\n```mermaid\n---\ntitle: F\n---\nerDiagram\n    typo {\n        string _id\n    }\n```\n")
		},
		kinds:    []ProblemKind{UnknownEntity},
		subjects: []string{"typo"},
	}, {
		name: "a diagram inside a blockquote is read without its quote marks",
		mutate: func(t *testing.T, root string, _ *Spec) {
			writeIn(t, root, "docs/diagrams/data-model-b.md", "# B\n\n> ```mermaid\n> erDiagram\n>     typo {\n>         string _id\n>     }\n> ```\n")
		},
		kinds:    []ProblemKind{UnknownEntity},
		subjects: []string{"typo"},
	}, {
		name: "an erDiagram header behind a directive is read",
		mutate: func(t *testing.T, root string, _ *Spec) {
			writeIn(t, root, "docs/diagrams/data-model-d.md", "# D\n\n```mermaid\n%%{init: {\"theme\":\"base\"}}%% erDiagram\n    typo {\n        string _id\n    }\n```\n")
		},
		kinds:    []ProblemKind{UnknownEntity},
		subjects: []string{"typo"},
	}, {
		name: "a collection the code renames under the page's feet",
		mutate: func(t *testing.T, root string, _ *Spec) {
			// Renamed onto a name the page DOES draw, so the only thing
			// wrong with the tree is the entity left without an owner.
			writeIn(t, root, "pkg/x/store.go",
				strings.Replace(syntheticStoreX, `colWidgets = "widgets"`, `colWidgets = "gizmos"`, 1))
		},
		kinds:    []ProblemKind{UnknownEntity},
		subjects: []string{"widgets"},
	}, {
		name: "a collection site whose argument no constant holds",
		mutate: func(t *testing.T, root string, _ *Spec) {
			writeIn(t, root, "pkg/w/pick.go", "package w\n\n"+
				"func pick(db database, name string) collection { return db.Collection(name) }\n")
		},
		kinds:    []ProblemKind{UnlistedDynamicSite},
		subjects: []string{"pkg/w/pick.go:pick"},
		detail:   "its argument (name)",
	}, {
		name: "a variadic Collection(name, opts) call is a site too",
		mutate: func(t *testing.T, root string, _ *Spec) {
			// The driver's Collection takes options after the name; a walker
			// that demanded exactly one argument let this store pass unseen.
			writeIn(t, root, "pkg/x/opts.go", "package x\n\n"+
				"func openOpts(db database) collection { return db.Collection(\"opts_probe\", nil) }\n")
		},
		kinds:    []ProblemKind{UnmappedCollection},
		subjects: []string{"opts_probe"},
	}, {
		name: "a local of a function declaration shadowing a package constant is a dynamic site",
		mutate: func(t *testing.T, root string, _ *Spec) {
			// colWidgets is the package's constant; the local of the same
			// name holds something else, and resolving it as the constant
			// would record a collection the code never reaches.
			writeIn(t, root, "pkg/x/shadow.go", "package x\n\n"+
				"func shadow(db database) collection {\n\tcolWidgets := \"a_collection_nothing_draws\"\n\treturn db.Collection(colWidgets)\n}\n")
		},
		kinds:    []ProblemKind{UnlistedDynamicSite},
		subjects: []string{"pkg/x/shadow.go:shadow"},
	}, {
		name: "a gate symbol named only inside a longer identifier is still omitted",
		mutate: func(t *testing.T, root string, _ *Spec) {
			writeIn(t, root, "docs/diagrams/access-control.md",
				strings.Replace(syntheticAccessControl, "`requireRoot`", "`requireRootV2`", 1))
		},
		kinds:    []ProblemKind{UndrawnGate},
		subjects: []string{"requireRoot"},
	}, {
		name: "a gate symbol with a letter before it is still omitted",
		mutate: func(t *testing.T, root string, _ *Spec) {
			writeIn(t, root, "docs/diagrams/access-control.md",
				strings.Replace(syntheticAccessControl, "`requireRoot`", "`xrequireRoot`", 1))
		},
		kinds:    []ProblemKind{UndrawnGate},
		subjects: []string{"requireRoot"},
	}, {
		name: "a gate symbol with a digit before it is still omitted",
		mutate: func(t *testing.T, root string, _ *Spec) {
			writeIn(t, root, "docs/diagrams/access-control.md",
				strings.Replace(syntheticAccessControl, "`requireRoot`", "`2requireRoot`", 1))
		},
		kinds:    []ProblemKind{UndrawnGate},
		subjects: []string{"requireRoot"},
	}, {
		name: "a gate symbol with an underscore before it is still omitted",
		mutate: func(t *testing.T, root string, _ *Spec) {
			writeIn(t, root, "docs/diagrams/access-control.md",
				strings.Replace(syntheticAccessControl, "`requireRoot`", "`_requireRoot`", 1))
		},
		kinds:    []ProblemKind{UndrawnGate},
		subjects: []string{"requireRoot"},
	}, {
		name: "a gate symbol with a digit after it is still omitted",
		mutate: func(t *testing.T, root string, _ *Spec) {
			writeIn(t, root, "docs/diagrams/access-control.md",
				strings.Replace(syntheticAccessControl, "`requireRoot`", "`requireRoot2`", 1))
		},
		kinds:    []ProblemKind{UndrawnGate},
		subjects: []string{"requireRoot"},
	}, {
		name: "a gate symbol with an underscore after it is still omitted",
		mutate: func(t *testing.T, root string, _ *Spec) {
			writeIn(t, root, "docs/diagrams/access-control.md",
				strings.Replace(syntheticAccessControl, "`requireRoot`", "`requireRoot_`", 1))
		},
		kinds:    []ProblemKind{UndrawnGate},
		subjects: []string{"requireRoot"},
	}, {
		name: "a constant of a multi-name spec",
		mutate: func(t *testing.T, root string, _ *Spec) {
			writeIn(t, root, "pkg/x/multi.go", "package x\n\nconst colMultiA, colMultiB = \"multi_a\", \"multi_b\"\n\n"+
				"func multi(db database) collection { return db.Collection(colMultiB) }\n")
		},
		kinds:    []ProblemKind{UnmappedCollection},
		subjects: []string{"multi_b"},
	}, {
		name: "a constant that is not a string names no collection",
		mutate: func(t *testing.T, root string, _ *Spec) {
			// A rune unquotes as cleanly as a string; only the literal's kind
			// tells them apart.
			writeIn(t, root, "pkg/x/num.go", "package x\n\nconst colRune = 'r'\n\nfunc num(db database) collection { return db.Collection(colRune) }\n")
		},
		kinds:    []ProblemKind{UnlistedDynamicSite},
		subjects: []string{"pkg/x/num.go:num"},
	}, {
		name: "a constant build-tagged twins declare with two string literals is a dynamic site in each",
		mutate: func(t *testing.T, root string, _ *Spec) {
			writeIn(t, root, "pkg/bt/a_linux.go", "//go:build linux\n\npackage bt\n\nconst colBT = \"bt_linux\"\n\n"+
				"func openLinux(db database) collection { return db.Collection(colBT) }\n")
			writeIn(t, root, "pkg/bt/a_windows.go", "//go:build windows\n\npackage bt\n\nconst colBT = \"bt_win\"\n\n"+
				"func openWindows(db database) collection { return db.Collection(colBT) }\n")
		},
		kinds:    []ProblemKind{UnlistedDynamicSite},
		subjects: []string{"pkg/bt/a_linux.go:openLinux", "pkg/bt/a_windows.go:openWindows"},
	}, {
		name: "a stateDiagram line holding a semicolon, in every form",
		mutate: func(t *testing.T, root string, _ *Spec) {
			// A transition label, a state description, a note and a quoted
			// state name are refused; the `%%` comment line is not.
			writeIn(t, root, "docs/diagrams/data-model-s.md", "# S\n\n```mermaid\nstateDiagram-v2\n    %% vocabulary: x.Y\n"+
				"    %% a --> b: a comment; never parsed\n"+
				"    a --> b: one; two\n"+
				"    a : desc; more\n"+
				"    note right of a: n; m\n"+
				"    state \"x; y\" as s\n```\n")
			// A directive on the header line still opens a state diagram.
			writeIn(t, root, "docs/diagrams/data-model-t.md", "# T\n\n```mermaid\n%%{init: {\"theme\":\"base\"}}%% stateDiagram-v2\n    a --> b: one; two\n```\n")
			// ... and so does a directive spanning lines, closed on the header's line.
			writeIn(t, root, "docs/diagrams/data-model-u.md", "# U\n\n```mermaid\n%%{init: {\n  \"theme\": \"base\"\n}}%% stateDiagram-v2\n    a --> b: one; two\n```\n")
			// The v1 header opens one too.
			writeIn(t, root, "docs/diagrams/data-model-v.md", "# V\n\n```mermaid\nstateDiagram\n    a --> b: one; two\n```\n")
			// A page whose lines end with a lone carriage return is read line by line.
			writeIn(t, root, "docs/diagrams/data-model-w.md", "# W\r\r```mermaid\rstateDiagram-v2\r    a --> b: one; two\r```\r")
			// A statement on the header line is scanned too.
			writeIn(t, root, "docs/diagrams/data-model-x.md", "# X\n\n```mermaid\nstateDiagram-v2 a --> b: one; two\n```\n")
			// A keyword written before a directive still opens the diagram.
			writeIn(t, root, "docs/diagrams/data-model-y.md", "# Y\n\n```mermaid\nstateDiagram-v2 %%{init: {\"theme\":\"base\"}}%%\n    a --> b: one; two\n```\n")
			// A blockquoted state diagram is scanned like any other.
			writeIn(t, root, "docs/diagrams/data-model-bq.md", "# BQ\n\n> ```mermaid\n> stateDiagram-v2\n>     a --> b: one; two\n> ```\n")
			// A directive followed by a statement is no comment line.
			writeIn(t, root, "docs/diagrams/data-model-z.md", "# Z\n\n```mermaid\nstateDiagram-v2\n    %%{init: {}}%% b --> c: one; two\n```\n")
		},
		kinds:    []ProblemKind{SemicolonInStateLabel},
		subjects: []string{"docs/diagrams/data-model-s.md:10", "docs/diagrams/data-model-s.md:7", "docs/diagrams/data-model-s.md:8", "docs/diagrams/data-model-s.md:9", "docs/diagrams/data-model-t.md:5", "docs/diagrams/data-model-u.md:7", "docs/diagrams/data-model-v.md:5", "docs/diagrams/data-model-w.md:5", "docs/diagrams/data-model-x.md:4", "docs/diagrams/data-model-bq.md:5", "docs/diagrams/data-model-y.md:5", "docs/diagrams/data-model-z.md:5"},
	}, {
		name: "a row naming a collection no page draws",
		mutate: func(_ *testing.T, _ string, spec *Spec) {
			spec.Dynamic[0].Names = append(spec.Dynamic[0].Names, "phantoms")
		},
		kinds:    []ProblemKind{UnmappedCollection},
		subjects: []string{"phantoms"},
	}, {
		name: "a row naming a site that is not dynamic any more",
		mutate: func(_ *testing.T, _ string, spec *Spec) {
			spec.Dynamic = append(spec.Dynamic, DynamicRow{
				Site:     "pkg/z/sweep.go:deleteManyRenamed",
				Names:    []string{"gadgets"},
				Evidence: "a row whose site was refactored away",
			})
		},
		kinds:    []ProblemKind{StaleDynamicRow},
		subjects: []string{"pkg/z/sweep.go:deleteManyRenamed"},
	}, {
		name: "a gate symbol its file no longer declares",
		mutate: func(t *testing.T, root string, _ *Spec) {
			writeIn(t, root, "pkg/server/gates.go",
				strings.Replace(syntheticGates, "func (s *Server) requireRoot(", "func (s *Server) requireRootV2(", 1))
		},
		kinds:    []ProblemKind{UndeclaredGate},
		subjects: []string{"requireRoot"},
	}, {
		name: "a gate symbol the access-control page omits",
		mutate: func(t *testing.T, root string, _ *Spec) {
			writeIn(t, root, "docs/diagrams/access-control.md",
				strings.Replace(syntheticAccessControl, "`requireRoot`", "the root check", 1))
		},
		kinds:    []ProblemKind{UndrawnGate},
		subjects: []string{"requireRoot"},
	}, {
		name: "a swept name no store owns",
		mutate: func(t *testing.T, root string, _ *Spec) {
			writeIn(t, root, "pkg/z/sweep.go",
				strings.Replace(syntheticSweep, `[]string{"sprockets"}`, `[]string{"sprockets", "ghosts"}`, 1))
		},
		kinds:    []ProblemKind{UnknownSweptCollection},
		subjects: []string{"ghosts"},
	}, {
		name: "a swept-list element that is not a string literal",
		mutate: func(t *testing.T, root string, _ *Spec) {
			writeIn(t, root, "pkg/z/sweep.go",
				strings.Replace(syntheticSweep, `[]string{"sprockets"}`, `[]string{"sprockets", sweptGhost}`+"\n\nconst sweptGhost = \"ghosts\"", 1))
		},
		kinds:    []ProblemKind{UnknownSweptCollection},
		subjects: []string{"pkg/z/sweep.go:sweptCollections"},
	}, {
		// A dynamic-site row vouches for no swept name — the purger's own row
		// lists exactly what it sweeps — so a collection a store opens only
		// dynamically, and the page draws, is still refused on a swept list.
		name: "a swept name only a dynamic-site row reaches",
		mutate: func(t *testing.T, root string, spec *Spec) {
			writeIn(t, root, "pkg/w/scoped.go", "package w\n\n"+
				"func scoped(db database, name string) collection { return db.Collection(name) }\n")
			spec.Dynamic = append(spec.Dynamic, DynamicRow{
				Site:     "pkg/w/scoped.go:scoped",
				Names:    []string{"routes"},
				Evidence: "the fixture store opens the collection its caller names",
			})
			writePage(t, root, append(append([]string{}, syntheticERLines...), "routes {", "    string _id", "}"))
			writeIn(t, root, "pkg/z/sweep.go",
				strings.Replace(syntheticSweep, `[]string{"sprockets"}`, `[]string{"sprockets", "routes"}`, 1))
		},
		kinds:    []ProblemKind{UnknownSweptCollection},
		subjects: []string{"routes"},
	}, {
		name: "the swept list renamed out from under the table",
		mutate: func(t *testing.T, root string, _ *Spec) {
			writeIn(t, root, "pkg/z/sweep.go",
				strings.Replace(syntheticSweep, "var sweptCollections", "var sweptCollectionNames", 1))
		},
		kinds:    []ProblemKind{MissingCorpus},
		subjects: []string{"pkg/z/sweep.go:sweptCollections"},
	}, {
		name: "the file a gate row names, gone",
		mutate: func(_ *testing.T, _ string, spec *Spec) {
			spec.Gates = append(spec.Gates, GateRef{"requireRoot", "pkg/server/gates_moved.go"})
		},
		kinds:    []ProblemKind{MissingCorpus},
		subjects: []string{"pkg/server/gates_moved.go"},
	}, {
		name: "the access-control page moved away",
		mutate: func(t *testing.T, root string, _ *Spec) {
			moveIn(t, root, "docs/diagrams/access-control.md", "docs/diagrams/access-control-moved.md")
		},
		kinds:    []ProblemKind{MissingCorpus, UndrawnGate},
		subjects: []string{"docs/diagrams/access-control.md"},
	}, {
		name: "a diagram page that cannot be read",
		mutate: func(t *testing.T, root string, _ *Spec) {
			mkdirIn(t, root, "docs/diagrams/flow-unreadable.md")
		},
		kinds:    []ProblemKind{MissingCorpus},
		subjects: []string{"docs/diagrams/flow-unreadable.md"},
	}, {
		// Both readers list it — the state-label scan and the entity scan —
		// and each must say so.
		name: "an ER page that cannot be read",
		mutate: func(t *testing.T, root string, _ *Spec) {
			mkdirIn(t, root, "docs/diagrams/data-model-unreadable.md")
		},
		kinds:    []ProblemKind{MissingCorpus},
		subjects: []string{"docs/diagrams/data-model-unreadable.md", "docs/diagrams/data-model-unreadable.md"},
	}, {
		name: "no ER page at all",
		mutate: func(t *testing.T, root string, _ *Spec) {
			moveIn(t, root, "docs/diagrams/data-model-core.md", "docs/diagrams/core.md")
		},
		kinds:    []ProblemKind{UnmappedCollection, MissingCorpus},
		subjects: []string{"cogs", "gadgets", "gizmos", "sprockets", "widgets"},
	}} {
		t.Run(tc.name, func(t *testing.T) {
			root := syntheticDiagramTree(t)
			spec := syntheticSpec()
			tc.mutate(t, root, &spec)
			rep, err := CheckDiagrams(root, spec)
			if err != nil {
				t.Fatal(err)
			}
			if !equalKinds(rep.KindsSeen(), tc.kinds) {
				t.Fatalf("the mutant moved the wrong verdicts: refused on %v, want exactly %v\n  %s",
					rep.KindsSeen(), tc.kinds, rep.Lines())
			}
			if got := rep.Subjects(tc.kinds[0]); !equal(got, tc.subjects) {
				t.Fatalf("refused about %v, want %v\n  %s", got, tc.subjects, rep.Lines())
			}
			if tc.detail != "" && !strings.Contains(rep.Lines(tc.kinds[0]), tc.detail) {
				t.Fatalf("the refusal does not show %q:\n  %s", tc.detail, rep.Lines(tc.kinds[0]))
			}
		})
	}
}

// A tree with nothing to read is the failure mode every guard in this package
// has to name out loud: zero collections and zero drawings agree perfectly.
func TestATreeThatYieldsNoCollectionIsRefused(t *testing.T) {
	root := t.TempDir()
	writeIn(t, root, "pkg/x/store.go", "package x\n\nfunc load() {}\n")
	rep, err := CheckDiagrams(root, Spec{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Collections) != 0 {
		t.Fatalf("read %v out of a tree with no collection call", rep.Collections)
	}
	problem := collectionFloor(rep)
	if problem == "" {
		t.Fatal("a tree with no collection at all passed the floor: the production run would read nothing and report agreement")
	}
	if !strings.Contains(problem, "want at least 70") {
		t.Fatalf("the floor fired for the wrong reason: %s", problem)
	}
	// And the floor's other half, which the count cannot stand in for: a
	// corpus where the ident rule — the only one every store under pkg/ uses
	// — resolved nothing, while another rule did.
	writeIn(t, root, "pkg/x/store.go", "package x\n\nfunc load(db database) { db.Collection(\"only\") }\n")
	rep, err = CheckDiagrams(root, Spec{})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Resolutions[ByLiteral] != 1 {
		t.Fatalf("the literal rule read %d sites out of one: %v", rep.Resolutions[ByLiteral], rep.Collections)
	}
	if got := collectionFloor(rep); !strings.Contains(got, "no site resolved by ident") {
		t.Fatalf("a corpus the ident rule read nothing of passed that half of the floor: %q", got)
	}
}

// The fixture tree: four collections read three different ways, one dynamic
// site, one swept list, two gates, and one page drawing all of it.
const (
	syntheticStoreX = "package x\n\n" +
		"// colWidgets is read by an ident of its own package.\n" +
		"const colWidgets = \"widgets\"\n\n" +
		"// ColGizmos is read by a selector from pkg/y.\n" +
		"const ColGizmos string = \"gizmos\"\n\n" +
		"func open(db database) collection { return db.Collection(colWidgets) }\n"

	syntheticStoreY = "package y\n\n" +
		"import \"github.com/SocialGouv/iterion/pkg/x\"\n\n" +
		"func gizmos(db database) collection { return db.Collection(x.ColGizmos) }\n\n" +
		"func sprockets(db database) collection { return db.Collection(\"sprockets\") }\n\n" +
		"func cogs(db database) collection { return db.Collection(\"cogs\") }\n"

	syntheticSweep = "package z\n\n" +
		"var sweptCollections = []string{\"sprockets\"}\n\n" +
		"func purge(db database) {\n" +
		"\tfor _, coll := range sweptCollections {\n" +
		"\t\tdeleteMany(db, coll)\n" +
		"\t}\n" +
		"}\n\n" +
		"func deleteMany(db database, coll string) { db.Collection(coll).DeleteMany() }\n"

	syntheticGates = "package server\n\n" +
		"func (s *Server) canViewThing() bool { return true }\n\n" +
		"func (s *Server) requireRoot() bool { return false }\n"

	syntheticAccessControl = "# Access control\n\n" +
		"| gate | what it admits |\n| --- | --- |\n" +
		"| `canViewThing` | a reader of one thing |\n" +
		"| `requireRoot` | the platform operator |\n"
)

// syntheticERLines is the ER body. Each form draws a collection no other line
// draws — gizmos only as a block, cogs only as a far end, gadgets only as a
// near end, sprockets only bare — so a reader that loses one form leaves that
// collection undrawn.
var syntheticERLines = []string{
	"widgets {",
	"    string _id",
	"}",
	"gizmos {",
	"    string _id",
	"}",
	"sprockets",
	"",
	"%% a comment the reader skips",
	`widgets ||--o{ cogs : "holds"`,
	`gadgets ||..o| widgets : "annotates"`,
	`"S3: artifacts" }o--|| widgets : "stores"`,
}

func syntheticSpec() Spec {
	return Spec{
		Dynamic: DynamicAllowlist{{
			Site:     "pkg/z/sweep.go:deleteMany",
			Names:    []string{"gadgets"},
			Evidence: "the fixture's purger takes the name as a parameter; the only caller adds gadgets",
		}},
		Gates: []GateRef{
			{"canViewThing", "pkg/server/gates.go"},
			{"requireRoot", "pkg/server/gates.go"},
		},
		Swept: []SliceRef{{"pkg/z/sweep.go", "sweptCollections"}},
	}
}

func syntheticDiagramTree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	writeSyntheticTree(t, root)
	return root
}

// writeSyntheticTree writes the fixture tree under root.
func writeSyntheticTree(t *testing.T, root string) {
	t.Helper()
	writeIn(t, root, "pkg/x/store.go", syntheticStoreX)
	writeIn(t, root, "pkg/y/store.go", syntheticStoreY)
	writeIn(t, root, "pkg/z/sweep.go", syntheticSweep)
	writeIn(t, root, "pkg/server/gates.go", syntheticGates)
	// Neither of these may be read: a test file and a testdata fixture name
	// collections that exist nowhere, and a guard that walked into them
	// would report entities no page should ever draw.
	writeIn(t, root, "pkg/x/store_test.go", "package x\n\nfunc load(db database) { db.Collection(\"from_a_test\") }\n")
	writeIn(t, root, "pkg/x/testdata/fixture.go", "package fixture\n\nfunc load(db database) { db.Collection(\"from_testdata\") }\n")
	writeIn(t, root, "docs/diagrams/access-control.md", syntheticAccessControl)
	writePage(t, root, syntheticERLines)
}

// A root path is a path. Each of these, read as a glob pattern, misses the
// tree ([ab] matches no directory of that name, x[y is a syntax error) or
// reaches past it (a?b and s*t also match the sibling planted beside them),
// and a guard reading that way reports a page missing, or an entity no
// collection answers for, about a tree that has neither.
func TestARootHoldingGlobSyntaxIsReadAsAPath(t *testing.T) {
	for _, dir := range []string{"x[y", "[ab]", "a?b", "s*t"} {
		t.Run(dir, func(t *testing.T) {
			parent := t.TempDir()
			root := filepath.Join(parent, dir)
			writeSyntheticTree(t, root)
			// The sibling each pattern would also match, holding a page
			// that draws an entity no store owns.
			sibling := strings.NewReplacer("?", "x", "*", "x").Replace(dir)
			if sibling != dir {
				writeIn(t, filepath.Join(parent, sibling), "docs/diagrams/data-model-intruder.md",
					"# Intruder\n\n```mermaid\nerDiagram\n    intruder {\n        string _id\n    }\n```\n")
			}
			rep, err := CheckDiagrams(root, syntheticSpec())
			if err != nil {
				t.Fatal(err)
			}
			if len(rep.Problems) != 0 {
				t.Fatalf("the tree under %q was misread:\n  %s", root, rep.Lines())
			}
			if want := []string{"docs/diagrams/data-model-core.md"}; !equal(rep.ERPages, want) {
				t.Fatalf("read the pages %v, want %v", rep.ERPages, want)
			}
		})
	}
}

// Both page listers name a directory they cannot list, each its own subject:
// with no list there is no page, and a guard that read none must say so
// rather than agree with nothing.
func TestADiagramsDirectoryThatCannotBeListedIsRefused(t *testing.T) {
	root := t.TempDir()
	writeIn(t, root, "docs/diagrams", "a file where the directory belongs\n")
	if got := readStateLabels(root); len(got) != 1 || got[0].Kind != MissingCorpus || got[0].Subject != diagramsDir {
		t.Fatalf("the state-label scan answered %+v, want one %q refusal about %s", got, MissingCorpus, diagramsDir)
	}
	_, pages, problems := readEntities(root)
	if want := diagramsDir + "/" + erPageGlob; len(pages) != 0 || len(problems) != 1 || problems[0].Kind != MissingCorpus || problems[0].Subject != want ||
		!strings.Contains(problems[0].Detail, "cannot be listed") {
		t.Fatalf("the entity scan answered pages %v and %+v, want one %q refusal about %s saying it cannot be listed", pages, problems, MissingCorpus, want)
	}
}

// Only a mermaid fence is a diagram: a page that QUOTES diagram source in a
// text fence draws nothing, and neither reader may take it for a drawing.
func TestATextFenceQuotingAnERDiagramDrawsNothing(t *testing.T) {
	root := syntheticDiagramTree(t)
	writeIn(t, root, "docs/diagrams/data-model-quoted.md",
		"# Quoted\n\n```text\nerDiagram\n    intruder {\n        string _id\n    }\n```\n")
	rep, err := CheckDiagrams(root, syntheticSpec())
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Problems) != 0 {
		t.Fatalf("a text fence was read as an ER diagram:\n  %s", rep.Lines())
	}
}

// A block's header names its kind: a line naming another kind later in the
// block opens nothing, in either reader.
func TestADiagramKeywordInsideAFlowchartOpensNothing(t *testing.T) {
	root := syntheticDiagramTree(t)
	writeIn(t, root, "docs/diagrams/data-model-n.md", "# N\n\n```mermaid\nflowchart LR\n    erDiagram\n    typo {\n        string _id\n    }\n```\n\n"+
		"```mermaid\nflowchart LR\n    stateDiagram-v2\n    a --> b: one; two\n```\n")
	rep, err := CheckDiagrams(root, syntheticSpec())
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Problems) != 0 {
		t.Fatalf("a keyword inside a flowchart opened a diagram:\n  %s", rep.Lines())
	}
}

// A name a function declaration binds itself — however it binds it — is not
// the package constant it is spelled like: the site is dynamic, whatever the
// constant holds.
func TestEveryLocalBindingInAFunctionDeclarationIsADynamicSite(t *testing.T) {
	for _, tc := range []struct{ name, rel, body, site string }{
		{"a method receiver", "pkg/x/rc.go", "package x\n\ntype box struct{}\n\nfunc (colWidgets box) rc(db database) { db.Collection(colWidgets) }\n", "pkg/x/rc.go:rc"},
		{"a plain parameter", "pkg/x/pm.go", "package x\n\nfunc pm(db database, colWidgets string) { db.Collection(colWidgets) }\n", "pkg/x/pm.go:pm"},
		{"a named result", "pkg/x/nr.go", "package x\n\nfunc nr(db database) (colWidgets string) { db.Collection(colWidgets); return }\n", "pkg/x/nr.go:nr"},
		{"a local var declaration", "pkg/x/vr.go", "package x\n\nfunc vr(db database) {\n\tvar colWidgets = \"nothing_draws_this\"\n\tdb.Collection(colWidgets)\n}\n", "pkg/x/vr.go:vr"},
		{"a range variable", "pkg/x/rg.go", "package x\n\nfunc rg(db database, all []string) {\n\tfor _, colWidgets := range all {\n\t\tdb.Collection(colWidgets)\n\t}\n}\n", "pkg/x/rg.go:rg"},
		{"a closure parameter", "pkg/x/cl.go", "package x\n\nfunc cl(db database) {\n\tf := func(colWidgets string) { db.Collection(colWidgets) }\n\t_ = f\n}\n", "pkg/x/cl.go:cl"},
		{"a closure's named result", "pkg/x/cr.go", "package x\n\nfunc cr(db database) {\n\tf := func() (colWidgets string) { db.Collection(colWidgets); return }\n\t_ = f\n}\n", "pkg/x/cr.go:cr"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := syntheticDiagramTree(t)
			writeIn(t, root, tc.rel, tc.body)
			rep, err := CheckDiagrams(root, syntheticSpec())
			if err != nil {
				t.Fatal(err)
			}
			if !equalKinds(rep.KindsSeen(), []ProblemKind{UnlistedDynamicSite}) || !equal(rep.Subjects(UnlistedDynamicSite), []string{tc.site}) {
				t.Fatalf("want exactly one dynamic site, %s:\n  %s", tc.site, rep.Lines())
			}
		})
	}
}

// A name bound in one function shadows nothing in a function of another name.
func TestABindingInAFunctionOfAnotherNameShadowsNothing(t *testing.T) {
	root := syntheticDiagramTree(t)
	writeIn(t, root, "pkg/x/two.go", "package x\n\n"+
		"func reads(db database) collection { return db.Collection(colWidgets) }\n\n"+
		"func binds() {\n\tcolWidgets := \"elsewhere\"\n\t_ = colWidgets\n}\n")
	rep, err := CheckDiagrams(root, syntheticSpec())
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Problems) != 0 {
		t.Fatalf("a binding in binds() made the site in reads() dynamic:\n  %s", rep.Lines())
	}
}

// A page whose lines end with a lone carriage return is read line by line,
// as the link checker reads it (mdcode.Lines).
func TestAnERPageWithLoneCarriageReturnsIsRead(t *testing.T) {
	root := syntheticDiagramTree(t)
	body, err := os.ReadFile(filepath.Join(root, "docs", "diagrams", "data-model-core.md"))
	if err != nil {
		t.Fatal(err)
	}
	writeIn(t, root, "docs/diagrams/data-model-core.md", strings.ReplaceAll(string(body), "\n", "\r"))
	rep, err := CheckDiagrams(root, syntheticSpec())
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Problems) != 0 {
		t.Fatalf("a page with lone carriage returns was misread:\n  %s", rep.Lines())
	}
}

func TestATextFenceQuotingAStateDiagramIsNotScanned(t *testing.T) {
	root := syntheticDiagramTree(t)
	writeIn(t, root, "docs/diagrams/data-model-quoted.md",
		"# Quoted\n\n```text\nstateDiagram-v2\n    a --> b: one; two\n```\n")
	rep, err := CheckDiagrams(root, syntheticSpec())
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Problems) != 0 {
		t.Fatalf("a text fence was scanned as a state diagram:\n  %s", rep.Lines())
	}
}

// writePage writes the ER page from its body lines, inside a mermaid fence —
// built by concatenation because a Go raw string cannot hold a backtick.
func writePage(t *testing.T, root string, lines []string) {
	t.Helper()
	const fence = "```"
	body := "# The fixture data model\n\nProse above the fence, with a `](quoted)` link form in it.\n\n" +
		fence + "mermaid\nerDiagram\n"
	for _, line := range lines {
		body += "    " + line + "\n"
	}
	body += fence + "\n\nAnd a flowchart, which holds no entity:\n\n" +
		fence + "mermaid\nflowchart LR\n    a --> b\n" + fence + "\n"
	writeIn(t, root, "docs/diagrams/data-model-core.md", body)
}

// withoutBlock drops an entity block: its opening line through the brace that
// closes it.
func withoutBlock(lines []string, opening string) []string {
	var out []string
	skipping := false
	for _, line := range lines {
		switch {
		case line == opening:
			skipping = true
		case skipping:
			skipping = line != "}"
		default:
			out = append(out, line)
		}
	}
	return out
}

func withoutLine(lines []string, prefix string) []string {
	var out []string
	for _, line := range lines {
		if strings.HasPrefix(line, prefix) {
			continue
		}
		out = append(out, line)
	}
	return out
}

func writeIn(t *testing.T, root, rel, body string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// moveIn renames a file of the tree the test built, so a reader looking for
// it by its old name finds nothing.
func moveIn(t *testing.T, root, from, to string) {
	t.Helper()
	if err := os.Rename(filepath.Join(root, filepath.FromSlash(from)), filepath.Join(root, filepath.FromSlash(to))); err != nil {
		t.Fatal(err)
	}
}

// mkdirIn plants a directory where a reader expects a page: a glob lists it,
// and reading it as a file fails.
func mkdirIn(t *testing.T, root, rel string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(root, filepath.FromSlash(rel)), 0o700); err != nil {
		t.Fatal(err)
	}
}

func equal(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	g, w := append([]string(nil), got...), append([]string(nil), want...)
	sort.Strings(g)
	sort.Strings(w)
	for i := range g {
		if g[i] != w[i] {
			return false
		}
	}
	return true
}

func equalKinds(got, want []ProblemKind) bool {
	g, w := make([]string, len(got)), make([]string, len(want))
	for i, k := range got {
		g[i] = string(k)
	}
	for i, k := range want {
		w[i] = string(k)
	}
	return equal(g, w)
}
