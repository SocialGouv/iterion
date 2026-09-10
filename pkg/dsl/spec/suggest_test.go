package spec

import (
	"strings"
	"testing"
)

func TestSuggestFindsTheClosestNames(t *testing.T) {
	cases := []struct {
		kind, name string
		want       []string
	}{
		{"tool", "comand", []string{"command"}},
		{"budget", "max_cost", []string{"max_cost_usd"}},
		{"agent", "modle", []string{"model"}},
		{"workflow", "budjet", []string{"budget"}},
		{"agent", "toolz", []string{"tools"}},
		{"agent", "model", nil}, // an exact match is not a suggestion
		{"agent", "zzzzzzzz", nil},
		{"nokind", "model", nil},
	}
	for _, c := range cases {
		got := Suggest(c.kind, c.name)
		if strings.Join(got, ",") != strings.Join(c.want, ",") {
			t.Errorf("Suggest(%q, %q) = %v, want %v", c.kind, c.name, got, c.want)
		}
	}
}

func TestUnknownPropertyHintNamesTheRemedy(t *testing.T) {
	cases := []struct {
		kind, name string
		wants      []string
	}{
		// a typo: the closest accepted name
		{"tool", "comand", []string{"Did you mean `command`?", "tool accepts: description, command"}},
		// a child block's property written on its host: indent it
		{"sandbox", "rules", []string{"`rules` is a property of the `network:` block", "indent it under `network:`"}},
		{"sandbox", "preset", []string{"`network:` block"}},
		{"tool", "max_repair_attempts", []string{"`recovery:` block"}},
		// the host's property written inside its child: outdent it
		{"budget", "entry", []string{"belongs to the enclosing `workflow`", "outdent"}},
		{"sandbox.network", "image", []string{"belongs to the enclosing `sandbox`"}},
		// another kind's property: named, with the kind's own list
		{"compute", "command", []string{"`command` is a property of agent/judge/mcp_server/tool", "compute does not take it", "compute accepts: description, input, output"}},
		{"tool", "readonly", []string{"`readonly` is a property of agent/judge"}},
		// nothing close: the list alone
		{"emit", "zzzz", []string{"emit accepts: description, event, with."}},
	}
	for _, c := range cases {
		got := UnknownPropertyHint(c.kind, c.name)
		for _, w := range c.wants {
			if !strings.Contains(got, w) {
				t.Errorf("UnknownPropertyHint(%q, %q) = %q, want it to contain %q", c.kind, c.name, got, w)
			}
		}
	}
	if got := UnknownPropertyHint("nokind", "x"); got != "" {
		t.Errorf("an unknown kind must yield no hint, got %q", got)
	}
}

// A block with several possible hosts names the enclosing kind only when the
// caller says which one the block sits in: "outdent it to the agent's level"
// is a false claim under a workflow.
func TestEnclosingKindRemedyNeedsTheRealHost(t *testing.T) {
	cases := []struct {
		host, name    string
		want, wantNot string
	}{
		{"agent", "model", "belongs to the enclosing `agent`", ""},
		{"judge", "model", "belongs to the enclosing `judge`", "`agent`"},
		{"workflow", "model", "`model` is a property of agent/fallback/human/judge/", "enclosing"},
		{"", "model", "`model` is a property of agent/fallback/human/judge/", "enclosing"},
		{"agent", "entry", "`entry` is a property of workflow", "enclosing"},
		{"workflow", "entry", "belongs to the enclosing `workflow`", ""},
	}
	for _, c := range cases {
		got := UnknownPropertyHintIn("mcp", c.host, c.name)
		if !strings.Contains(got, c.want) {
			t.Errorf("mcp in %q, %q: %q lacks %q", c.host, c.name, got, c.want)
		}
		if c.wantNot != "" && strings.Contains(got, c.wantNot) {
			t.Errorf("mcp in %q, %q: %q must not contain %q", c.host, c.name, got, c.wantNot)
		}
	}
	// A single-host block keeps its remedy with no host named.
	if got := UnknownPropertyHint("budget", "entry"); !strings.Contains(got, "belongs to the enclosing `workflow`") {
		t.Errorf("budget: %q", got)
	}
}

func TestOwnersAndHostedBy(t *testing.T) {
	if got := strings.Join(Owners("threshold"), ","); got != "compaction" {
		t.Errorf("Owners(threshold) = %q", got)
	}
	var names []string
	for _, k := range HostedBy("sandbox") {
		names = append(names, k.Name)
	}
	if got := strings.Join(names, ","); got != "sandbox.build,sandbox.network" {
		t.Errorf("HostedBy(sandbox) = %q", got)
	}
}
