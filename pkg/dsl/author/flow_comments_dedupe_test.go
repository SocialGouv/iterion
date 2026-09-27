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
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := Comments([]byte(tc.src))
			if len(got) != tc.want {
				t.Fatalf("got %d comment(s) %v, want %d:\n%s", len(got), got, tc.want, tc.src)
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
