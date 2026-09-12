package parser

import (
	"reflect"
	"strings"
	"testing"
)

// A named resource pool — the one list-valued property the registry does
// not describe, since its keys are the author's — reads the `- item` form
// as the inline one, in every profile.
func TestResourcePoolReadsTheDashList(t *testing.T) {
	inline := "agent a:\n  description: \"x\"\n\nworkflow w:\n  entry: a\n  resources:\n    gpu: [\"gpu-1\", \"gpu-2\"]\n    cpu: 3\n  a -> done\n"
	dash := "agent a:\n  description: \"x\"\n\nworkflow w:\n  entry: a\n  resources:\n    gpu: ## the pool\n      - \"gpu-1\"\n      - \"gpu-2\"\n    cpu: 3\n  a -> done\n"
	for _, profile := range []string{"", "dsl: 2\n"} {
		want := Parse("i.bot", profile+inline)
		got := Parse("d.bot", profile+dash)
		if len(want.Diagnostics) != 0 || len(got.Diagnostics) != 0 {
			t.Fatalf("profile %q: inline=%v dash=%v", profile, want.Diagnostics, got.Diagnostics)
		}
		rw, rd := want.File.Workflows[0].Resources, got.File.Workflows[0].Resources
		if !reflect.DeepEqual(rw.Members, rd.Members) || !reflect.DeepEqual(rw.Capacities, rd.Capacities) || rd.Capacities["gpu"] != 2 || rd.Capacities["cpu"] != 3 {
			t.Fatalf("profile %q: pools differ: %+v vs %+v", profile, rw, rd)
		}
	}
}

// A `-` with nothing after it is not an empty item: it draws one diagnostic
// naming it, and the other items of the list are read — on every list path.
func TestAnEmptyBulletIsRefusedNotIgnored(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want []string
		pick func(res *ParseResult) []string
	}{
		{"tools, middle", "agent a:\n  tools:\n    - bash\n    -\n    - grep\n", []string{"bash", "grep"}, func(r *ParseResult) []string { return r.File.Agents[0].Tools }},
		{"tools, last", "agent a:\n  tools:\n    - bash\n    -\n", []string{"bash"}, func(r *ParseResult) []string { return r.File.Agents[0].Tools }},
		{"tools, last with a comment", "agent a:\n  tools:\n    - bash\n    - ## nothing\n", []string{"bash"}, func(r *ParseResult) []string { return r.File.Agents[0].Tools }},
		{"skills", "agent a:\n  skills:\n    -\n    - house.style\n", []string{"house.style"}, func(r *ParseResult) []string { return r.File.Agents[0].Skills }},
		{"needs", "agent a:\n  needs:\n    - godot\n    -\n", []string{"godot"}, func(r *ParseResult) []string { return r.File.Agents[0].Needs }},
		{"allow", "workflow w:\n  entry: done\n  allow:\n    -\n    - \"Read(**)\"\n", []string{"Read(**)"}, func(r *ParseResult) []string { return r.File.Workflows[0].Allow }},
		{"rules", "workflow w:\n  entry: done\n  sandbox:\n    network:\n      mode: allowlist\n      rules:\n        - github\n        -\n", []string{"github"}, func(r *ParseResult) []string { return r.File.Workflows[0].Sandbox.Network.Rules }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res := Parse("x.bot", c.src)
			if len(res.Diagnostics) != 1 {
				t.Fatalf("want exactly one diagnostic, got %v", res.Diagnostics)
			}
			d := res.Diagnostics[0]
			if d.Code != DiagExpectedToken || !strings.Contains(d.Message, "expected an element after `-`") {
				t.Fatalf("got %s %q", d.Code, d.Message)
			}
			if got := c.pick(res); !reflect.DeepEqual(got, c.want) {
				t.Fatalf("the other items were not read: %v, want %v", got, c.want)
			}
		})
	}
}

// The short form of a secret takes one bare word as any string-valued
// property does: `token: abc` is `token: "abc"`.
func TestASecretShortFormReadsABareWord(t *testing.T) {
	quoted := Parse("q.bot", "secrets:\n  token: \"abc\"\n  other:\n    value: \"x\"\n")
	bare := Parse("b.bot", "secrets:\n  token: abc\n  other:\n    value: \"x\"\n")
	if len(quoted.Diagnostics) != 0 || len(bare.Diagnostics) != 0 {
		t.Fatalf("quoted=%v bare=%v", quoted.Diagnostics, bare.Diagnostics)
	}
	if quoted.File.Secrets.Fields[0].Value != "abc" || bare.File.Secrets.Fields[0].Value != "abc" {
		t.Fatalf("values: %q vs %q", quoted.File.Secrets.Fields[0].Value, bare.File.Secrets.Fields[0].Value)
	}
	if bare.File.Secrets.Fields[1].Value != "x" {
		t.Fatalf("the block form after it was not read: %+v", bare.File.Secrets.Fields[1])
	}
}
