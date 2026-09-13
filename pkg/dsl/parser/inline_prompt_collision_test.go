package parser

import (
	"fmt"
	"strings"
	"testing"
)

// An inline prompt's name carries twelve hex digits of its body's digest —
// a literal, not the helper's own output, so a shortened prefix is seen.
func TestInlinePromptNameCarriesTwelveHexDigits(t *testing.T) {
	name := InlinePromptName("Review the diff")
	if !strings.HasPrefix(name, "_inline_") || len(name) != len("_inline_")+12 {
		t.Fatalf("name %q", name)
	}
	for _, r := range name[len("_inline_"):] {
		if !strings.ContainsRune("0123456789abcdef", r) {
			t.Fatalf("name %q is not hex", name)
		}
	}
}

// Two DIFFERENT bodies whose digests share the prefix never share a name:
// the second takes a longer prefix. Forced here by shortening the prefix to
// one hex digit, so that twenty bodies collide among sixteen buckets.
func TestInlinePromptsWithACollidingPrefixKeepDistinctNames(t *testing.T) {
	prev := inlineNameHexLen
	t.Cleanup(func() { inlineNameHexLen = prev })
	inlineNameHexLen = 1

	var b strings.Builder
	const n = 20
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, "agent a%d:\n  system: \"body %d\"\n\n", i, i)
	}
	res := Parse("x.bot", b.String())
	if len(res.Diagnostics) != 0 {
		t.Fatalf("diagnostics: %v", res.Diagnostics)
	}
	if len(res.File.Prompts) != n {
		t.Fatalf("%d prompts for %d distinct bodies", len(res.File.Prompts), n)
	}
	byName := map[string]string{}
	for _, p := range res.File.Prompts {
		if _, dup := byName[p.Name]; dup {
			t.Fatalf("two prompts named %q", p.Name)
		}
		byName[p.Name] = p.Body
	}
	for i, a := range res.File.Agents {
		if byName[a.System] != fmt.Sprintf("body %d", i) {
			t.Fatalf("agent %s refers to %q = %q", a.Name, a.System, byName[a.System])
		}
	}
	// The same body twice is still one prompt, under the colliding regime too.
	same := Parse("y.bot", "agent a:\n  system: \"same\"\n\nagent b:\n  system: \"same\"\n")
	if len(same.File.Prompts) != 1 || same.File.Agents[0].System != same.File.Agents[1].System {
		t.Fatalf("the same text made two prompts: %+v", same.File.Prompts)
	}
}
