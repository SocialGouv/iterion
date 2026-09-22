package unparse_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ast"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
	"github.com/SocialGouv/iterion/pkg/dsl/unparse"
)

// commentPositions is one `.bot` in its canonical form carrying a comment at
// every place the grammar lets one be written. The name of each comment is
// the place it holds, so a failure names the position that moved, and the
// file being canonical makes the round trip a BYTE comparison: anything the
// writer does to a comment shows.
const commentPositions = `## HEAD-1 the file's head
## HEAD-2 second head line

dsl: 2

## LEADS-VARS
vars:
  ## LEADS-FIELD
  topic: string = "x"
  n: int = 1 ## TRAILS-FIELD
  ## ENDS-VARS

prompt sys:
  Be brief.

schema out:
  ok: bool

## LEADS-AGENT
agent plan:
  ## LEADS-MODEL
  model: "sonnet"
  output: out
  system: sys
  ## LEADS-SANDBOX
  sandbox:
    image: "alpine"
    ## LEADS-NETWORK
    network:
      mode: open
      ## ENDS-NETWORK
    ## ENDS-SANDBOX
  ## ENDS-AGENT

## LEADS-WORKFLOW
workflow main:

  entry: plan

  ## LEADS-EDGE
  plan -> done ## TRAILS-EDGE

## FILE-TAIL
`

// TestEveryCommentPositionSurvivesTheWriter: a comment written anywhere in a
// canonical file comes back byte-identical. Before comment provenance the
// writer kept the ones leading the file and dropped every other (#1282); the
// fixture names eleven other places, and dropping any of them reddens here
// on the diff of that line.
func TestEveryCommentPositionSurvivesTheWriter(t *testing.T) {
	pr := parser.Parse("positions.bot", commentPositions)
	assertNoErrors(t, pr)
	got := unparse.Unparse(pr.File)
	if got != commentPositions {
		t.Fatalf("the writer did not give the file back:\n%s", lineDiff(commentPositions, got))
	}
	if err := unparse.Verify(pr.File, got); err != nil {
		t.Fatalf("Verify: %v", err)
	}
}

// TestEveryCommentPositionSurvivesTheTransport: the same, through the JSON
// the studio sends back — the document a save is actually written from. A
// comment the transport drops, or whose address it loses, moves the text.
func TestEveryCommentPositionSurvivesTheTransport(t *testing.T) {
	pr := parser.Parse("positions.bot", commentPositions)
	assertNoErrors(t, pr)
	raw, err := ast.MarshalFile(pr.File)
	if err != nil {
		t.Fatal(err)
	}
	back, err := ast.UnmarshalFile(raw)
	if err != nil {
		t.Fatal(err)
	}
	got := unparse.Unparse(back)
	if got != commentPositions {
		t.Fatalf("the document did not come back through the transport:\n%s", lineDiff(commentPositions, got))
	}
}

// TestCommentsAreOnTheirDeclaration: the comments of the fixture sit on the
// declaration they were written around, not in one flat list at the file's
// head — which is what makes them survive an edit that moves a declaration.
func TestCommentsAreOnTheirDeclaration(t *testing.T) {
	pr := parser.Parse("positions.bot", commentPositions)
	assertNoErrors(t, pr)
	f := pr.File
	head := commentTextsOf(f.Comments, ast.CommentBefore)
	if want := []string{"HEAD-1 the file's head", "HEAD-2 second head line"}; !equalStrings(head, want) {
		t.Errorf("the file's head carries %q, want %q", head, want)
	}
	if tail := commentTextsOf(f.Comments, ast.CommentAtEnd); !equalStrings(tail, []string{"FILE-TAIL"}) {
		t.Errorf("the file's tail carries %q, want [FILE-TAIL]", tail)
	}
	cases := []struct {
		where  string
		got    []*ast.Comment
		anchor []string // "<text> @<anchor>/<place>"
	}{
		{"vars", f.Vars.Comments, []string{"LEADS-VARS @/before", "LEADS-FIELD @topic/before", "TRAILS-FIELD @n/trailing", "ENDS-VARS @/end"}},
		{"agent plan", f.Agents[0].Comments, []string{
			"LEADS-AGENT @/before", "LEADS-MODEL @model/before", "LEADS-SANDBOX @sandbox/before",
			"LEADS-NETWORK @sandbox.network/before", "ENDS-NETWORK @sandbox.network/end",
			"ENDS-SANDBOX @sandbox/end", "ENDS-AGENT @/end",
		}},
		{"workflow main", f.Workflows[0].Comments, []string{"LEADS-WORKFLOW @/before"}},
		{"workflow main, edge 1", f.Workflows[0].Edges[0].Comments, []string{"LEADS-EDGE @/before", "TRAILS-EDGE @/trailing"}},
	}
	for _, c := range cases {
		got := describeComments(c.got)
		if !equalStrings(got, c.anchor) {
			t.Errorf("%s carries\n  %q\nwant\n  %q", c.where, got, c.anchor)
		}
	}
}

// TestACommentWhoseAnchorIsGoneStaysInItsDeclaration: the canvas clears the
// property a comment led. The comment is not dropped — it is written at the
// end of the block it was in, and the text still says what it said.
func TestACommentWhoseAnchorIsGoneStaysInItsDeclaration(t *testing.T) {
	pr := parser.Parse("x.bot", commentPositions)
	assertNoErrors(t, pr)
	f := pr.File
	f.Agents[0].Sandbox = nil // the block LEADS-SANDBOX and its two ENDS named
	out := unparse.Unparse(f)
	for _, want := range []string{"LEADS-SANDBOX", "LEADS-NETWORK", "ENDS-NETWORK", "ENDS-SANDBOX"} {
		if !strings.Contains(out, "## "+want) {
			t.Errorf("%q was dropped with the block it named:\n%s", want, out)
		}
	}
	back := parser.Parse("x.bot", out)
	assertNoErrors(t, back)
	if got := len(back.File.Agents[0].Comments); got != len(f.Agents[0].Comments) {
		t.Fatalf("the agent carries %d comments after the rewrite, %d before", got, len(f.Agents[0].Comments))
	}
	if err := unparse.Verify(f, out); err != nil {
		t.Fatalf("Verify: %v", err)
	}
}

// TestVerifySeesACommentLost: the guard the studio save and `iterion fmt`
// both call refuses a text that lost a comment. Until #1282 it could not:
// SameProgram does not carry comments and the mirror comparison left them
// out, so a save dropped every comment inside a declaration and reported
// success.
func TestVerifySeesACommentLost(t *testing.T) {
	pr := parser.Parse("x.bot", commentPositions)
	assertNoErrors(t, pr)
	text := unparse.Unparse(pr.File)
	lost := removeLine(text, "## LEADS-MODEL")
	if lost == text {
		t.Fatal("the fixture no longer holds the line the test removes")
	}
	err := unparse.Verify(pr.File, lost)
	if err == nil {
		t.Fatal("Verify accepted a text that lost a comment")
	}
	if !strings.Contains(err.Error(), "comment") {
		t.Fatalf("Verify refused for another reason: %v", err)
	}
}

// TestACommentMovedToAnotherDeclarationIsRefused: a text that keeps every
// comment but hangs one on ANOTHER declaration is not the document either —
// which is the shape of the defect #1282 named, every comment hoisted to
// the file's head.
func TestACommentMovedToAnotherDeclarationIsRefused(t *testing.T) {
	pr := parser.Parse("x.bot", commentPositions)
	assertNoErrors(t, pr)
	text := unparse.Unparse(pr.File)
	moved := strings.Replace(text, "  ## LEADS-MODEL\n", "", 1)
	moved = strings.Replace(moved, "## LEADS-WORKFLOW\n", "## LEADS-WORKFLOW\n## LEADS-MODEL\n", 1)
	if moved == text {
		t.Fatal("the fixture no longer holds the lines the test moves")
	}
	err := unparse.Verify(pr.File, moved)
	if err == nil {
		t.Fatal("Verify accepted a comment hung on another declaration")
	}
	if !strings.Contains(err.Error(), "comment") {
		t.Fatalf("Verify refused for another reason: %v", err)
	}
}

// TestAHumanNodeKeepsWhatItsInteractionSays holds both halves of a guard
// the writer gets wrong in either direction. A human node with NO
// `interaction:` parses to the ZERO of the mode, which the compiler reads
// as the node's default: writing that zero back as `interaction: none`
// made the writer's own output parse to a document it would then write
// differently — one rewrite dropped the line, the next added
// `interaction: none`, and `fmt --check` could never go green. An explicit
// `interaction: human` is NOT that zero: under a workflow-level
// `interaction:` default it PINS the node, and dropping it changes the
// program (the round trip is then refused by name, which is how a save
// turns into a 422 on a legal bot).
func TestAHumanNodeKeepsWhatItsInteractionSays(t *testing.T) {
	bot := func(nodeInteraction, workflowInteraction string) string {
		return "dsl: 2\n\nprompt ask:\n  What is it?\n\nschema q:\n  a: string\n\nhuman gate:\n  input: q\n  output: q\n" +
			nodeInteraction + "  instructions: ask\n\nworkflow main:\n" + workflowInteraction + "\n  entry: gate\n\n  gate -> done\n"
	}
	for name, tc := range map[string]struct {
		src  string
		want string // the `interaction:` line the writer must produce, "" for none
	}{
		"no interaction at all":             {bot("", ""), ""},
		"an explicit human":                 {bot("  interaction: human\n", ""), "interaction: human"},
		"an explicit human under a default": {bot("  interaction: human\n", "  interaction: llm_or_human\n"), "interaction: human"},
		"a mode that is not the default":    {bot("  interaction: human_or_host\n", ""), "interaction: human_or_host"},
	} {
		t.Run(name, func(t *testing.T) {
			pr := parser.Parse("h.bot", tc.src)
			assertNoErrors(t, pr)
			first := unparse.Unparse(pr.File)
			if err := unparse.Verify(pr.File, first); err != nil {
				t.Fatalf("Verify refused the writer's own text: %v\n%s", err, first)
			}
			has := strings.Contains(first, "  "+tc.want+"\n") && tc.want != ""
			none := !strings.Contains(first, "interaction:")
			if tc.want == "" && !none {
				t.Fatalf("the writer wrote an interaction the document does not have:\n%s", first)
			}
			if tc.want != "" && !has {
				t.Fatalf("the writer dropped %q:\n%s", tc.want, first)
			}
			again := parser.Parse("h.bot", first)
			assertNoErrors(t, again)
			if second := unparse.Unparse(again.File); second != first {
				t.Fatalf("the writer is not a fixed point on a human node:\n%s", lineDiff(first, second))
			}
		})
	}
}

// ---- helpers ----

func assertNoErrors(t *testing.T, pr *parser.ParseResult) {
	t.Helper()
	for _, d := range pr.Diagnostics {
		if d.Severity == parser.SeverityError {
			t.Fatalf("the fixture does not parse: %s", d.Error())
		}
	}
}

func commentTextsOf(cs []*ast.Comment, place ast.CommentPlace) []string {
	var out []string
	for _, c := range cs {
		if c.Place == place {
			out = append(out, c.Text)
		}
	}
	return out
}

func describeComments(cs []*ast.Comment) []string {
	places := map[ast.CommentPlace]string{ast.CommentBefore: "before", ast.CommentAtEnd: "end", ast.CommentTrailing: "trailing"}
	out := make([]string, 0, len(cs))
	for _, c := range cs {
		out = append(out, c.Text+" @"+c.Anchor+"/"+places[c.Place])
	}
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func removeLine(text, prefix string) string {
	var kept []string
	for _, l := range strings.Split(text, "\n") {
		if strings.TrimSpace(l) == prefix {
			continue
		}
		kept = append(kept, l)
	}
	return strings.Join(kept, "\n")
}

// lineDiff names the first line at which two texts diverge, with both sides.
func lineDiff(want, got string) string {
	a, b := strings.Split(want, "\n"), strings.Split(got, "\n")
	for i := 0; i < len(a) || i < len(b); i++ {
		x, y := "", ""
		if i < len(a) {
			x = a[i]
		}
		if i < len(b) {
			y = b[i]
		}
		if x != y {
			return fmt.Sprintf("line %d:\n  want %q\n   got %q", i+1, x, y)
		}
	}
	return "(identical)"
}

// TestTheWriterGivesTheFileBack holds one canonical `.bot` per shape a
// round of adversarial review broke. Each fixture is already in canonical
// form, so the assertion is a BYTE comparison of parse → unparse: the
// writer that moves, welds, splits or swallows a comment fails on the line
// it touched, and `iterion fmt --check` would go red on a user's file the
// same way.
func TestTheWriterGivesTheFileBack(t *testing.T) {
	for name, src := range map[string]string{
		// The file's own head and the comment leading its first
		// declaration are written one after the other; a blank line is
		// all that tells them apart, and without one the head grew by a
		// comment on every rewrite.
		"a head comment and a leading one, no `dsl:` header": `## my-bot.bot — what this bot does.

## The prompt bob runs.
prompt p:
  do the thing

agent bob:
  model: "sonnet"
  user: p

workflow main:

  entry: bob

  bob -> done
`,
		// Two `- item` lines fold into one `tools: [...]`: their two
		// trailing comments cannot both end that line, and joining them
		// read back as ONE comment.
		"two trailing comments the writer folds onto one line": `dsl: 2

schema s:
  ok: bool

agent bob:
  model: "sonnet"
  output: s
  tools: [bash, grep] ## the shell
  ## the search

workflow main:

  entry: bob

  bob -> done
`,
		// A chain is one line and as many edges as it has arrows; an
		// ordinal counted per LINE put every comment below a chain on an
		// edge too early.
		"a comment under a chained edge": `dsl: 2

schema s:
  ok: bool

compute a:
  output: s
  expr:
    ok: "true"

compute b:
  output: s
  expr:
    ok: "true"

workflow main:

  entry: a

  ## ABOVE-THE-CHAIN
  a -> b

  b -> done

  ## ABOVE-THE-LAST-EDGE
  b -> done
`,
		// A profile-2 `"a\nb"` decodes to two lines and occupies one:
		// measuring the value made the property swallow the comment
		// written below it, and the comment walked a line per rewrite.
		"a comment under an escaped newline": `dsl: 2

schema s:
  ok: bool

tool t:
  command: "echo a\necho b"
  ## THIS-IS-ABOUT-THE-TIMEOUT
  timeout: 5m
  output: s

workflow main:

  entry: t

  t -> done
`,
		// The left margin is not the file's tail while code is still
		// below it: such a comment was teleported past every
		// declaration.
		"a comment at the left margin inside a body": `dsl: 2

schema s:
  ok: bool

agent bob:
  model: "sonnet"
  output: s
  tools: [bash, grep]
  ## BETWEEN-THE-ITEMS

workflow main:

  entry: bob

  bob -> done
`,
		// The indentation an author writes INSIDE a comment is text: a
		// wrapped line aligned under a bullet lost it and read as a new
		// bullet.
		"a comment with its own indentation": `## Modes:
##   addendum -> append a dated note, then ask the human
##               commit or skip
##   skip     -> do nothing

dsl: 2

schema s:
  ok: bool

compute c:
  output: s
  expr:
    ok: "true"

workflow main:

  entry: c

  c -> done
`,
		// A `prompt` header reaches to the end of its body, where a `#`
		// is TEXT: a trailing comment written at that extent became part
		// of the prompt, or ate the blank line between two declarations.
		"a trailing comment on a prompt header": `vars:
  d: string = "x"

prompt ps: ## about the system prompt
  You are a Go developer.
  Use the tools.

prompt pu:
  Task: do it.

agent a:
  system: ps
  user: pu

workflow demo:

  entry: a
`,
		// The closing brace of a `with { … }` is written at column 1: it
		// is not the declaration's body indentation, and taking it put
		// the comment at the left margin, where the next read gives it
		// to whatever declaration follows.
		"a comment under a `use … with { … }`": `group g(label):
  tool t:
    command: "echo {{params.label}}"

use g as r1 with {
  label: "x",
}
  ## note about r1

workflow demo:

  entry: r1.t
`,
		// A block whose entries the grammar gives no key — a `cursor`'s
		// `bands:`, keyed by quoted strings — still ENDS somewhere. The
		// index did not record it, so the comment went above the entries
		// at the header's column, where the next read called it the
		// header's and the file never settled.
		"a comment inside a block with quoted-string keys": `dsl: 2

schema s:
  ok: bool

cursor depth:
  description: "how deep"
  bands:
    "0.0..0.5": "Skim."
    "0.51..1.0": "Dig."
    ## LEADS-FIRST-BAND

compute c:
  output: s
  expr:
    ok: "true"

workflow main:

  entry: c

  c -> done
`,
		// The paragraph break an author writes inside a run of `##`
		// lines: without it the writer welds the run into one block.
		"paragraph breaks inside a comment run": `## Why this bot exists.
##
## The long version.

## A second paragraph, a blank line below the first.

dsl: 2

schema s:
  ok: bool

compute c:
  ## What it computes.

  ## And why that is the shape.
  output: s
  expr:
    ok: "true"

workflow main:

  entry: c

  c -> done
`,
	} {
		t.Run(name, func(t *testing.T) {
			pr := parser.Parse("x.bot", src)
			assertNoErrors(t, pr)
			got := unparse.Unparse(pr.File)
			if got != src {
				t.Fatalf("the writer did not give the file back:\n%s", lineDiff(src, got))
			}
			if err := unparse.Verify(pr.File, got); err != nil {
				t.Fatalf("Verify: %v", err)
			}
			// And through the transport the studio saves from.
			raw, err := ast.MarshalFile(pr.File)
			if err != nil {
				t.Fatal(err)
			}
			back, err := ast.UnmarshalFile(raw)
			if err != nil {
				t.Fatal(err)
			}
			if got := unparse.Unparse(back); got != src {
				t.Fatalf("the document did not come back through the transport:\n%s", lineDiff(src, got))
			}
		})
	}
}

// TestTwoDeclarationsUnderOneNameKeepTheirCommentsAtTheHead: the address of
// a comment is ambiguous when two declarations of a kind share a name
// (E010 refuses the program later). Neither takes it — not the declaration
// and not its edges — so nothing is claimed by the wrong one.
func TestTwoDeclarationsUnderOneNameKeepTheirCommentsAtTheHead(t *testing.T) {
	const src = `dsl: 2

schema s:
  ok: bool

compute a:
  output: s
  expr:
    ok: "true"

workflow main:

  entry: a

  ## FIRST-MAIN-EDGE
  a -> done

workflow main:

  entry: a

  ## SECOND-MAIN-EDGE
  a -> done
`
	pr := parser.Parse("x.bot", src)
	for _, w := range pr.File.Workflows {
		for _, e := range w.Edges {
			if len(e.Comments) > 0 {
				t.Fatalf("an edge of a duplicated workflow claimed %q", e.Comments[0].Text)
			}
		}
	}
	head := commentTextsOf(pr.File.Comments, ast.CommentBefore)
	if !equalStrings(head, []string{"FIRST-MAIN-EDGE", "SECOND-MAIN-EDGE"}) {
		t.Fatalf("the head carries %q", head)
	}
}

// TestTheWriterKeepsCommentsWhenItRewritesTheText holds the shapes the
// writer does NOT give back byte for byte — a `- item` list folded inline,
// a chained edge split into one line per edge, a comment at the left margin
// inside a body. The round trip cannot be the assertion there, so each case
// says where the comment must land, and the text must be a fixed point
// afterwards.
func TestTheWriterKeepsCommentsWhenItRewritesTheText(t *testing.T) {
	const head = "dsl: 2\n\nschema s:\n  ok: bool\n\n"
	for name, tc := range map[string]struct {
		src string
		// want must appear in the output, in this order, before `stop`.
		want []string
		stop string
	}{
		"two trailing comments on a folded list": {
			src: head + "agent bob:\n  model: \"sonnet\"\n  output: s\n  tools:\n    - bash ## the shell\n    - grep ## the search\n\nworkflow main:\n  entry: bob\n  bob -> done\n",
			// Both survive: joined on one line they read back as ONE.
			want: []string{"## the shell", "## the search"},
			stop: "workflow main:",
		},
		"a comment under a chained edge": {
			src: head + "compute a:\n  output: s\n  expr:\n    ok: \"true\"\n\ncompute b:\n  output: s\n  expr:\n    ok: \"true\"\n\ncompute c:\n  output: s\n  expr:\n    ok: \"true\"\n\nworkflow main:\n  entry: a\n  ## ABOVE-THE-CHAIN\n  a -> b -> c\n  ## ABOVE-THE-LAST-EDGE\n  c -> done\n",
			// The chain is ONE line and TWO edges; the comment written
			// above the edge below it must still lead THAT edge. The
			// three edges have different targets on purpose — with two
			// alike, a comment landing on the wrong one reads the same.
			want: []string{"## ABOVE-THE-CHAIN\n  a -> b\n", "## ABOVE-THE-LAST-EDGE\n  c -> done\n"},
			stop: "",
		},
		"a comment glued to the first declaration, beside an inline prompt": {
			src: "workflow demo:\n  entry: a\n\n## leads the agent\nagent a:\n  user: \"hi\"\n",
			// An INLINE prompt — one written as the text of a property —
			// has no header line and is not a carrier. Listed, it was
			// "the first declaration" of nearly every file, which is
			// where the head is pooled and where the head is closed: the
			// comment came back as a head comment that grew by one and
			// the save was refused.
			want: []string{"## leads the agent\n\nagent a:\n"},
			stop: "",
		},
		"a frontmatter block with a paragraph break, glued to the first declaration": {
			src: "## ---\n## name: catalogue-identity\n## description: what the catalogue shows\n\n## triggers:\n##   - nightly\n## ---\nworkflow demo:\n  entry: n\n\nagent n:\n  user: \"x\"\n",
			// A bot's catalogue identity is read between an opening
			// `## ---` and its closing one. The head split must not cut
			// the block at the blank line inside it: the second half
			// travelled with the first declaration, leaving code between
			// the two fences and the bot with no name.
			want: []string{"## ---\n## name: catalogue-identity\n## description: what the catalogue shows\n\n## triggers:\n##   - nightly\n## ---\n"},
			stop: "",
		},
		"a comment under a prompt body": {
			src: "dsl: 2\n\nschema s:\n  ok: bool\n\ncompute g:\n  output: s\n  expr:\n    ok: \"true\"\n\nworkflow w:\n  entry: g\n  g -> done\n\nprompt p:\n    the declared body\n  ## a comment under the prompt body\n",
			// Below the header there is nowhere to write it: inside a
			// prompt body a `#` is TEXT, and the comment became part of
			// the prompt — a program change, refused at the save.
			want: []string{"## a comment under the prompt body\nprompt p:\n"},
			stop: "",
		},
		"a comment indented deeper than the property below it": {
			src: "dsl: 2\n\nschema s:\n  ok: bool\n\ncompute g:\n  output: s\n  expr:\n    ok: \"true\"\n\nworkflow w:\n  worktree: none\n    ## a note written too deep\n  budget:\n    max_iterations: 3\n  entry: g\n  g -> done\n",
			// `worktree:` opens no block, so there is no end of it to sit
			// at: the comment belongs to the workflow's body. Taking the
			// property for a block put it back between two siblings,
			// where the next read called it the second's.
			want: []string{"entry: g", "## a note written too deep"},
			stop: "",
		},
		"a comment at the left margin inside a body": {
			src: head + "agent bob:\n  model: \"sonnet\"\n  output: s\n  tools:\n    - bash\n## BETWEEN-THE-ITEMS\n    - grep\n\nworkflow main:\n  entry: bob\n  bob -> done\n",
			// It belongs to the agent, not to the end of the file.
			want: []string{"## BETWEEN-THE-ITEMS"},
			stop: "workflow main:",
		},
	} {
		t.Run(name, func(t *testing.T) {
			pr := parser.Parse("x.bot", tc.src)
			assertNoErrors(t, pr)
			out := unparse.Unparse(pr.File)
			if err := unparse.Verify(pr.File, out); err != nil {
				t.Fatalf("Verify refused the writer's own text: %v\n%s", err, out)
			}
			scope := out
			if tc.stop != "" {
				i := strings.Index(out, tc.stop)
				if i < 0 {
					t.Fatalf("the fixture no longer holds %q:\n%s", tc.stop, out)
				}
				scope = out[:i]
			}
			at := 0
			for _, w := range tc.want {
				j := strings.Index(scope[at:], w)
				if j < 0 {
					t.Fatalf("%q is not where it was written:\n%s", w, out)
				}
				at += j + len(w)
			}
			// …and what the writer wrote reads back the same way.
			again := parser.Parse("x.bot", out)
			assertNoErrors(t, again)
			if twice := unparse.Unparse(again.File); twice != out {
				t.Fatalf("not a fixed point:\n%s", lineDiff(out, twice))
			}
		})
	}
}

// TestAnAnchorOnAPropertyWithNoBlockIsWrittenAboveIt: the transport takes
// an address the parser never produces — `place: end` on a property that
// opens no block — and the writer must still produce a text that reads
// back the same. Written BELOW such a line, the next read gives the
// comment to whatever follows: on a workflow's `entry:` that is the first
// edge, and the save is then refused on a document the canvas built.
func TestAnAnchorOnAPropertyWithNoBlockIsWrittenAboveIt(t *testing.T) {
	const src = `dsl: 2

schema s:
  ok: bool

compute a:
  output: s
  expr:
    ok: "true"

workflow demo:

  entry: a

  a -> done
`
	pr := parser.Parse("x.bot", src)
	assertNoErrors(t, pr)
	w := pr.File.Workflows[0]
	w.Comments = []*ast.Comment{{Text: "ABOUT-THE-ENTRY", Anchor: "entry", Place: ast.CommentAtEnd}}
	out := unparse.Unparse(pr.File)
	if err := unparse.Verify(pr.File, out); err != nil {
		t.Fatalf("Verify refused a document the transport accepts: %v\n%s", err, out)
	}
	if !strings.Contains(out, "  ## ABOUT-THE-ENTRY\n  entry: a\n") {
		t.Fatalf("the comment is not above the line it names:\n%s", out)
	}
	again := parser.Parse("x.bot", out)
	assertNoErrors(t, again)
	if twice := unparse.Unparse(again.File); twice != out {
		t.Fatalf("not a fixed point:\n%s", lineDiff(out, twice))
	}
}

// TestTheStrictEscapeDirectiveIsWrittenOnceWhereverItIsCarried: the
// directive is the file's lexing MODE, not a note. It is read among the
// first lines of the text, so the writer places it at the head — and since
// a comment now travels on the declaration it was written around, it can be
// carried by one. Reading only the file's head for it rendered every value
// unescaped and then added a directive above them: the file gained a line
// its author never wrote, on every rewrite.
func TestTheStrictEscapeDirectiveIsWrittenOnceWhereverItIsCarried(t *testing.T) {
	const src = `## probe bot — a note about the file

## strict-escape: on
vars:
  k: string = "a\"b` + "`" + `c"

schema s:
  ok: bool

compute a:
  output: s
  expr:
    ok: "'v'"

workflow w:
  entry: a
  a -> done
`
	pr := parser.Parse("d.bot", src)
	assertNoErrors(t, pr)
	if len(pr.File.Comments) != 1 {
		t.Fatalf("the fixture no longer carries the directive on a declaration: head=%d", len(pr.File.Comments))
	}
	out := unparse.Unparse(pr.File)
	if n := strings.Count(out, "strict-escape"); n != 1 {
		t.Fatalf("the directive is written %d times:\n%s", n, out)
	}
	if !strings.HasPrefix(out, "## strict-escape: on\n") {
		t.Fatalf("the directive is not at the head, where the lexer reads it:\n%s", out)
	}
	again := parser.Parse("d.bot", out)
	assertNoErrors(t, again)
	if twice := unparse.Unparse(again.File); twice != out {
		t.Fatalf("not a fixed point:\n%s", lineDiff(out, twice))
	}
	if err := unparse.Verify(pr.File, out); err != nil {
		t.Fatalf("Verify: %v", err)
	}
}

// TestACommentInsideAContractDoesNotRefuseTheSave: sameContracts compares
// contracts as documents. With the comments in that comparison, a comment
// the writer legitimately re-anchored — the property it named being one the
// writer does not emit — refused the whole save, while sameComments (which
// compares what the comments SAY) was content. One owner for the comment
// invariant, and it is sameComments.
func TestACommentInsideAContractDoesNotRefuseTheSave(t *testing.T) {
	const src = `dsl: 2

contract k:
  version: 1
  inputs:
    goal: string
  outputs:
    b: string
      description: "the output"
        ## a note indented deeper than the attributes around it
      nullable: true
      from: g.x

schema s:
  x: string

vars:
  goal: string

compute g:
  output: s
  expr:
    x: "'v'"

workflow w:
  contract: k
  entry: g
  g -> done
`
	pr := parser.Parse("c.bot", src)
	assertNoErrors(t, pr)
	out := unparse.Unparse(pr.File)
	if err := unparse.Verify(pr.File, out); err != nil {
		t.Fatalf("Verify refused a file `validate` accepts: %v\n%s", err, out)
	}
	if !strings.Contains(out, "## a note indented deeper than the attributes around it") {
		t.Fatalf("the comment was dropped:\n%s", out)
	}
}
