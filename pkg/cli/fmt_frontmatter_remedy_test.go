package cli

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

// The notice `fmt --to yaml` prints for a frontmatter block the catalogue's
// reader refuses carries the remedy of that message — the real site, where
// the author reads it: the block's reading resolves the tags, aliases and
// merge keys the author document's node tree leaves alone, so these
// messages reach an author here and nowhere else.
func TestAFrontmatterRefusalNoticeCarriesItsRemedy(t *testing.T) {
	inTempWorkspace(t)
	const body = "\ndsl: 2\n\nprompt ask:\n  Say hello.\n\nagent hello:\n  model: \"m\"\n  system: ask\n\nworkflow hello:\n  entry: hello\n  hello -> done\n"
	for name, tc := range map[string]struct{ head, read, want string }{
		"a merge key": {
			"## ---\n## name: probe\n## <<: 3\n## ---\n",
			"map merge requires map or sequence of maps as the value",
			"`<<` is YAML's merge key",
		},
		"a !!binary value that is not base64": {
			"## ---\n## name: !!binary nope!\n## ---\n",
			"!!binary value contains invalid base64 data",
			"reads as a YAML tag",
		},
		"a value tagged !!int that is not one": {
			"## ---\n## name: !!int notanint\n## ---\n",
			"cannot decode !!str `notanint` as a !!int",
			"write it whole in single quotes",
		},
		"an anchor whose value holds its alias": {
			"## ---\n## x: &a {b: *a}\n## ---\n",
			"anchor 'a' value contains itself",
			"is a cycle the resolving has no end to",
		},
		"a key that is itself a list": {
			"## ---\n## x:\n##   ? [a, b]\n##   : 1\n## ---\n",
			"invalid map key:",
			"is no key at all",
		},
		// The shapes the four keys take: the block's reading decodes into
		// their Go types, so a value of another shape is refused by the
		// decoder itself rather than by an error constructor of the reader
		// — the family the problem-string scan cannot see.
		"a scalar where a list is expected": {
			"## ---\n## name: probe\n## triggers: just-one\n## ---\n",
			"cannot unmarshal !!str `just-one` into []string",
			"one `- item` per line",
		},
		"a list where one line of text is expected": {
			"## ---\n## name: [a, b]\n## ---\n",
			"cannot unmarshal !!seq into string",
			"one line of text",
		},
		"a mapping where a list is expected": {
			"## ---\n## name: probe\n## triggers:\n##   a: 1\n## ---\n",
			"cannot unmarshal !!map into []string",
			"`[a, b]` on one line",
		},
		// The remedy answers the READER's words: a value the reader writes
		// back into its own message is the author's text, and a backtick of
		// its own used to split a span paired delimiter to delimiter —
		// leaving the tail of the value to choose the remedy of a message
		// no reader had raised (the colon one, the tab one).
		"a tagged value whose text holds a backtick and a message": {
			"## ---\n## name: !!int \"a`mapping values are not allowed\"\n## ---\n",
			"cannot decode !!str `a`mapping values are not allowed` as a !!int",
			"reads as a YAML tag",
		},
		"a tagged value whose text holds a backtick and a clause": {
			"## ---\n## name: !!int \"a`found a tab character\"\n## ---\n",
			"cannot decode !!str `a`found a tab character` as a !!int",
			"reads as a YAML tag",
		},
		"a complex key whose text holds a quote and a message": {
			"## ---\n## x:\n##   ? [\"a\\\"mapping values are not allowed\"]\n##   : 1\n## ---\n",
			"invalid map key:",
			"is no key at all",
		},
	} {
		t.Run(name, func(t *testing.T) {
			bot := writeBot(t, "c/b.bot", tc.head+body)
			jp, _ := jsonPrinter()
			res, err := RunFmt(FmtOptions{Paths: []string{bot}, To: "yaml", Printer: jp})
			if err != nil {
				t.Fatalf("fmt refused the file: %v", err)
			}
			var notice string
			for _, n := range res.Notices {
				if strings.Contains(n, "the frontmatter is not YAML") {
					notice = n
					break
				}
			}
			switch {
			case notice == "":
				t.Fatalf("no notice says the frontmatter is not YAML, got %v", res.Notices)
			case !strings.Contains(notice, tc.read):
				t.Errorf("the notice does not carry %q: %s", tc.read, notice)
			case !strings.Contains(notice, tc.want):
				t.Errorf("the message reaches the author bare of %q: %s", tc.want, notice)
			}
		})
	}
}

// The line a frontmatter refusal names is a line of the BLOCK: the
// catalogue's reader is given the text between the fences alone, so its
// "line N" is the Nth of the lines that reading was given. The notice says
// so and quotes that line, rather than telling the author how to count to
// it: the block's lines are not the file's lines with an offset — a run of
// blank lines inside the block stands for ONE line of the reading, and a
// `dsl:` or `import` line written between two of its comment lines for none
// — so no count from the fence is true of every file. The number is the
// author's only way back to the construct, and the line's own text is one
// they can read off their file.
func TestAFrontmatterRefusalSaysWhichLinesItsNumberCounts(t *testing.T) {
	inTempWorkspace(t)
	const body = "\ndsl: 2\n\nprompt ask:\n  Say hello.\n\nagent hello:\n  model: \"m\"\n  system: ask\n\nworkflow hello:\n  entry: hello\n  hello -> done\n"
	const bodyNoHeader = "\nprompt ask:\n  Say hello.\n\nagent hello:\n  model: \"m\"\n  system: ask\n\nworkflow hello:\n  entry: hello\n  hello -> done\n"
	for name, tc := range map[string]struct {
		head string
		// body replaces the shared one where the head carries the `dsl:`
		// header itself.
		body string
		// fileLines are the lines of the file the mistakes are written on,
		// with the opening fence on line 1; blockLines the numbers the
		// reader names, which count the lines that reading was given.
		fileLines, blockLines []int
		// texts are the block lines blockLines name, as the notice quotes
		// them and in the reader's own order; empty where the reader's
		// words name no line of its own and the notice owes no clause.
		texts []string
		// forbid is what this file's notice may not say.
		forbid []string
	}{
		"a value holding a colon":  {head: "## ---\n## name: probe\n## description: a: b\n## ---\n", fileLines: []int{3}, blockLines: []int{2}, texts: []string{"description: a: b"}},
		"a tab indenting a line":   {head: "## ---\n## name: probe\n## x:\n## \t- a\n## ---\n", fileLines: []int{4}, blockLines: []int{3}, texts: []string{"\t- a"}},
		"a flow list never closed": {head: "## ---\n## name: probe\n## triggers: [a,\n##  b\n## ---\n", fileLines: []int{4}, blockLines: []int{3}, texts: []string{" b"}},
		// Two blank lines in a row: the reading is given ONE empty line for
		// the run (parser.Frontmatter), so the file's line 5 is the
		// reading's line 3 — a count from the fence lands on line 4, the
		// blank one, and the author reads a line they did not write.
		"a run of two blank lines": {head: "## ---\n## name: probe\n\n\n## triggers: just-one\n## ---\n", fileLines: []int{5}, blockLines: []int{3}, texts: []string{"triggers: just-one"}},
		// The other collapse, the other way: the `dsl:` header does not end
		// the head, so a block written around it loses that line altogether
		// — the file's line 4 is the reading's line 2.
		"the dsl header inside the block": {head: "## ---\n## name: probe\ndsl: 2\n## triggers: just-one\n## ---\n", body: bodyNoHeader, fileLines: []int{4}, blockLines: []int{2}, texts: []string{"triggers: just-one"}},
		// `# ---` and `## ---` are the same fence (pkg/bundle/frontmatter.go):
		// a clause naming one spelling is false for the file that holds the
		// other.
		"a single-hash fence": {head: "# ---\n# name: probe\n# triggers: just-one\n# ---\n", fileLines: []int{3}, blockLines: []int{2}, texts: []string{"triggers: just-one"}, forbid: []string{"## ---"}},
		// The reading collects a refusal per key, so one message names
		// several lines: a clause about "the line it names" would leave the
		// author to guess which, and the second mistake unanswered.
		"two keys of the wrong shape": {head: "## ---\n## name: [a]\n## triggers: x\n## ---\n", fileLines: []int{2, 3}, blockLines: []int{1, 2}, texts: []string{"name: [a]", "triggers: x"}},
		// The number is read off the reader's own words, never off a value
		// the reader quoted back: `cannot decode` carries no line of its
		// own, so the author's text is the only number in the notice.
		"a value whose text names a line": {head: "## ---\n## name: !!int \"line 9: \"\n## ---\n"},
		// The same forgery inside the block's own range: `line 9` is out of
		// a one-line block and a bound on the number refuses it by itself,
		// while `line 1` is a line the block holds — only reading the
		// reader's words alone, the value it quoted taken out, refuses this
		// one.
		"a value whose text names a line of the block": {head: "## ---\n## name: !!int \"line 1: \"\n## ---\n"},
		// The same forgery with a backtick of the author's own in the
		// value: a span paired delimiter to delimiter breaks on it and the
		// tail — the `line N:` — survives to feed the numbers. One
		// character is the whole difference with the row above, so the
		// value's text is bounded by the reader's own format or by nothing.
		"a value whose text names a line of the block, with a backtick": {head: "## ---\n## name: !!int \"a`line 1: \"\n## ---\n"},
		// The other two formats that write an author's value back: the Go
		// rendering of a complex key, whose strings carry double quotes
		// the renderer escapes inside them, and a value longer than the
		// `cannot decode` reader truncates to — it truncates nothing.
		"a complex key whose text names a line of the block":         {head: "## ---\n## x:\n##   ? [\"a\\\"line 4: b\"]\n##   : 1\n## k5: v5\n## ---\n"},
		"a value whose text names a line of the block at any length": {head: "## ---\n## name: !!int \"a`line 5: a tail of any length, the reader truncates nothing\"\n## k3: v3\n## k4: v4\n## k5: v5\n## k6: v6\n## ---\n"},
		// A forged number BESIDE one the reader really wrote: the notice
		// owes the author the reader's line and no other, so a value
		// naming a second line of the block must not add a quote.
		"a value whose text names a line beside the reader's": {
			head:       "## ---\n## name: probe\n## triggers: \"a`line 9:\"\n## k3: v3\n## k4: v4\n## k5: v5\n## k6: v6\n## k7: v7\n## k8: v8\n## k9: v9\n## k10: v10\n## ---\n",
			fileLines:  []int{3},
			blockLines: []int{2},
			texts:      []string{"triggers: \"a`line 9:\""},
		},
	} {
		t.Run(name, func(t *testing.T) {
			if tc.body == "" {
				tc.body = body
			}
			bot := writeBot(t, "ln/"+strings.ReplaceAll(name, " ", "_")+"/b.bot", tc.head+tc.body)
			jp, _ := jsonPrinter()
			res, err := RunFmt(FmtOptions{Paths: []string{bot}, To: "yaml", Printer: jp})
			if err != nil {
				t.Fatalf("fmt refused the file: %v", err)
			}
			var notice string
			for _, n := range res.Notices {
				if strings.Contains(n, "the frontmatter is not YAML") {
					notice = n
					break
				}
			}
			if notice == "" {
				t.Fatalf("no notice says the frontmatter is not YAML, got %v", res.Notices)
			}
			for _, f := range tc.forbid {
				if strings.Contains(notice, f) {
					t.Errorf("the notice names %q, which this file does not hold: %s", f, notice)
				}
			}
			if strings.Contains(notice, "count from") {
				t.Errorf("the notice tells the author to count to the line themselves, which the two collapses make false: %s", notice)
			}
			if len(tc.texts) == 0 {
				if strings.Contains(notice, "the block's own") {
					t.Errorf("the notice claims a line of the block where the reader named none — the only number in it is the author's own quoted text: %s", notice)
				}
				return
			}
			for i, n := range tc.blockLines {
				if want := fmt.Sprintf("line %d:", n); !strings.Contains(notice, want) {
					t.Errorf("the notice does not name %q — the number no longer counts the lines the reading was given, so the clause below names the wrong line: %s", want, notice)
				}
				if unwanted := fmt.Sprintf("line %d:", tc.fileLines[i]); !slices.Contains(tc.blockLines, tc.fileLines[i]) && strings.Contains(notice, unwanted) {
					t.Errorf("the notice names the file's own line %q: the clause saying the number is the block's is then false: %s", unwanted, notice)
				}
			}
			if len(tc.texts) == 1 {
				if want := "the line it names is the block's own, not the file's — `" + tc.texts[0] + "`"; !strings.Contains(notice, want) {
					t.Errorf("the notice does not quote the line its number names (%s), leaving the author to count the block's lines, which are not the file's with an offset: %s", want, notice)
				}
				return
			}
			if want := "the lines it names are the block's own, not the file's"; !strings.Contains(notice, want) {
				t.Errorf("the notice speaks of one line where the reader named %d: %s", len(tc.texts), notice)
			}
			for i, n := range tc.blockLines {
				if want := fmt.Sprintf("line %d `%s`", n, tc.texts[i]); !strings.Contains(notice, want) {
					t.Errorf("the notice does not quote %s: a mistake the reader named reaches the author with no line to read: %s", want, notice)
				}
			}
		})
	}
}

// The fence a notice names is one the author's file holds: `# ---` and
// `## ---` are the same fence (pkg/bundle/frontmatter.go), and the block
// the reading hands over carries only the comment text the two spellings
// share, so the one notice written before the block is read owes the author
// both — a notice naming `## ---` alone sends the author of a `# ---` head
// looking for a line they never wrote.
func TestTheUnclosedFrontmatterNoticeNamesBothFenceSpellings(t *testing.T) {
	inTempWorkspace(t)
	const body = "\ndsl: 2\n\nprompt ask:\n  Say hello.\n\nagent hello:\n  model: \"m\"\n  system: ask\n\nworkflow hello:\n  entry: hello\n  hello -> done\n"
	for name, head := range map[string]string{
		"a double-hash fence never closed": "## ---\n## name: probe\n",
		"a single-hash fence never closed": "# ---\n# name: probe\n",
	} {
		t.Run(name, func(t *testing.T) {
			bot := writeBot(t, "fence/"+strings.ReplaceAll(name, " ", "_")+"/b.bot", head+body)
			jp, _ := jsonPrinter()
			res, err := RunFmt(FmtOptions{Paths: []string{bot}, To: "yaml", Printer: jp})
			if err != nil {
				t.Fatalf("fmt --to yaml: %v", err)
			}
			var notice string
			for _, n := range res.Notices {
				if strings.Contains(n, "not closed") {
					notice = n
				}
			}
			if notice == "" {
				t.Fatalf("no notice says the block is not closed, got %v", res.Notices)
			}
			for _, want := range []string{"`# ---`", "`## ---`"} {
				if !strings.Contains(notice, want) {
					t.Errorf("the notice does not name %s, which an author writing that spelling must add: %s", want, notice)
				}
			}
		})
	}
}
