package author

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"
)

// The fuzz shapes of #1814's items 8 and 9: a comment the yaml reader
// hangs on a node AND reads off the source was counted twice; a same-text
// comment on ANOTHER line was eaten by the text dedupe. The dedupe is by
// position now - the sourced read carries its line, the head's lines are
// derived from the source - so each physical comment counts once and a
// same-text comment elsewhere still counts.
func TestCommentsCountEachPhysicalCommentOnce(t *testing.T) {
	for _, tc := range []struct {
		name string
		src  string
		want int
	}{
		{"a flow seq entry comment then a break", "vars:\n  l: [a: # c\n  ]\nworkflow:\n  name: w\n  entry: done\n", 1},
		{"a flow map entry comment then a break", "vars:\n  m: {a: # c\n  }\nworkflow:\n  name: w\n  entry: done\n", 1},
		{"a tagged flow with the head between tag and bracket", "!!seq\n# same\n[a, broken]\n", 1},
		{"a tagged multiline flow with the head between tag and bracket", "!!seq\n# c\n[a,\n b]\n", 1},
		{"a blank inside the head block", "!\n# c1\n\n# c2\n[a]\n", 2},
		{"the head above and an inside same-text comment", "# c\n[\n # c\n]\n", 2},
		{"a foot on the closing line", "[a, # c\n b] # c\n", 2},
		{"a foot after the close", "[a, # c\n b]\n# c\n", 2},
		{"a foot on a closing line whose value holds a hash", "[\"a # b\", # c\n \"c # d\"] # c\n", 2},
		{"a bare hash foot on the closing line", "[a, #\n b] #\n", 2},
		// A DANGLING key inside the collection is the shape yaml.v3 MOVES
		// the inside comment for, hanging it on the node beside whatever
		// the closing line or the block below it carries: the moved copy
		// is the one the sourced read already listed, and a same text
		// written anywhere else is its own comment, counted where it is
		// written and never a second time here.
		{"a dangling key, the same text on a content line below", "vars:\n  l: [a: # c\n  ]\n  w: v # c\nworkflow:\n  name: w\n  entry: done\n", 2},
		{"a dangling key, a line between the close and the same text", "vars:\n  l: [a: # c\n  ]\n  z: y # q\n  w: v # c\nworkflow:\n  name: w\n  entry: done\n", 3},
		{"a dangling key, the same text in a block scalar's body below", "vars:\n  l: [a: # c\n  ]\n  s: |\n    # c\nworkflow:\n  name: w\n  entry: done\n", 1},
		{"a dangling key, the same text on its own closing line", "vars:\n  l: [a: # c\n  ] # c\nworkflow:\n  name: w\n  entry: done\n", 2},
		{"a dangling key, a bare hash on its own closing line", "vars:\n  l: [a: #\n  ] #\nworkflow:\n  name: w\n  entry: done\n", 2},
		{"a dangling key, an own-line same text below the close", "vars:\n  l: [a: # c\n  ]\n  # c\n  w: v\nworkflow:\n  name: w\n  entry: done\n", 2},
		{"a dangling key at the root, the same text on its foot", "[a: # c\n]\n# c\n", 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := Comments([]byte(tc.src))
			if len(got) != tc.want {
				t.Fatalf("got %d comment(s) %v, want %d:\n%s", len(got), got, tc.want, tc.src)
			}
		})
	}
}

// The two slots yaml.v3 hangs a flow collection's comments on are filled
// from two regions of the source and no other: its LINE comment from the
// closing bracket's own line, its FOOT comment from the own-line comment
// block below that line, the blank lines inside the block included. The
// first line that is neither blank nor an own-line comment ends the foot
// block — what is written below it belongs to another node, and counting it
// here would hand a moved comment a second listing.
//
// The foot region is read for a slot no document measured fills with a
// MOVED comment — yaml.v3 hangs that one on the node's line — so this is
// where its bound is exercised.
func TestCommentsAfterCloseCountsItsOwnRegion(t *testing.T) {
	// `  ] # c` closes the collection on line 2; two own-line comments
	// follow, a blank between them, then a content line carrying the same
	// text again.
	lines := sourceLines([]byte("a: [x: # c\n  ] # c\n# c\n\n# c\nb: 2 # c\n"))
	for _, tc := range []struct {
		name string
		text string
		foot bool
		want int
	}{
		{"the closing line's own comment", "# c", false, 1},
		{"the foot block below it", "# c", true, 2},
		{"a text written nowhere near", "# z", false, 0},
		{"a text written nowhere near, foot", "# z", true, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := commentsAfterClose([]string{tc.text}, map[string]bool{tc.text: true}, lines, 2, tc.foot)[tc.text]; got != tc.want {
				t.Fatalf("commentsAfterClose(%q, foot=%v) = %d, want %d", tc.text, tc.foot, got, tc.want)
			}
		})
	}
}

// A same-text comment on ANOTHER line is the author's own: the head's copy
// and the bracket's comment both count.
func TestASameTextCommentOnAnotherLineStillCounts(t *testing.T) {
	src := "# same\nvars:\n  x: !tag [\n    # same\n    a, b]\nworkflow:\n  name: w\n  entry: done\n"
	got := Comments([]byte(src))
	if len(got) != 2 {
		t.Fatalf("got %d comment(s) %v, want both physical comments", len(got), got)
	}
	for _, c := range got {
		if !strings.Contains(c, "# same") {
			t.Errorf("unexpected comment %q", c)
		}
	}
}

// The sourced comments stay in the order they are written in (the head's
// copy aside): a before comment precedes an inside one.
func TestTheSourcedCommentsStayInWrittenOrder(t *testing.T) {
	nl := string(rune(10))
	src := strings.Join([]string{
		"dsl: 2",
		"vars:",
		"  x: ! # 1 the tools, see [docs]",
		"    [a, # 2",
		"    b]",
		"workflow:",
		"  name: w",
		"  entry: done",
	}, nl) + nl
	got := Comments([]byte(src))
	one := -1
	two := -1
	for i, c := range got {
		switch {
		case strings.Contains(c, "# 1"):
			one = i
		case strings.Contains(c, "# 2"):
			two = i
		}
	}
	if one == -1 || two == -1 {
		t.Fatalf("the sourced comments are missing: %v", got)
	}
	if one > two {
		t.Errorf("the before comment came after the inside one: %v", got)
	}
}

// A seeded generator builds flow-heavy documents whose physical comment
// count is known by construction (every generated comment marker is
// counted as it is placed; no scalar ever carries a `#`): Comments lists
// exactly that many, on every document of the corpus.
func TestCommentsMatchThePhysicalCountOnAGeneratedCorpus(t *testing.T) {
	rng := rand.New(rand.NewSource(1814))
	texts := []string{"# c1", "# c2", "# c3"}
	for doc := 0; doc < 400; doc++ {
		lines := []string{"dsl: 2", "vars:"}
		physical := 0
		if doc%3 == 0 {
			lines = append(lines, "  # head "+fmt.Sprint(doc))
			physical++
		}
		flow := []string{}
		if doc%2 == 0 {
			flow = append(flow, "  l: [")
		} else {
			flow = append(flow, "  l: {k: [")
		}
		for e := 0; e < 1+rng.Intn(3); e++ {
			switch rng.Intn(6) {
			case 0: // an own-line comment before an entry, inside the flow
				flow = append(flow, "    "+texts[rng.Intn(3)])
				physical++
			case 1: // an entry with a trailing comment
				flow = append(flow, "    v"+fmt.Sprint(e)+", "+texts[rng.Intn(3)])
				physical++
			default:
				flow = append(flow, "    v"+fmt.Sprint(e)+",")
			}
		}
		lines = append(lines, flow...)
		if doc%2 == 0 {
			lines = append(lines, "  ]")
		} else {
			lines = append(lines, "  ]}")
		}
		if doc%4 == 0 {
			lines = append(lines, "  # foot "+fmt.Sprint(doc))
			physical++
		}
		lines = append(lines, "workflow:", "  name: w", "  entry: done")
		src := strings.Join(lines, "\n") + "\n"
		got := Comments([]byte(src))
		if len(got) != physical {
			t.Fatalf("doc %d: got %d comment(s) %v, want the %d physical ones\n%s", doc, len(got), got, physical, src)
		}
	}
}
