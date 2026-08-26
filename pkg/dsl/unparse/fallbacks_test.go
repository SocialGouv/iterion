package unparse

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

const fallbacksRoundtripSrc = `agent implement:
  backend: "claude_code"
  model: "claude-opus-5"
  system: work
  tools: [read_file, run_command]
  fallbacks:
    api:
      backend: "claw"
      model: "anthropic/claude-opus-5"
      provider: "anthropic"
      on: [usage_window]
    gpt:
      backend: "claw"
      model: "openai/gpt-5.5"
      on: [usage_window, unavailable]
      metered: true

judge gate:
  backend: "claw"
  model: "anthropic/claude-opus-5"
  system: work
  tools: [read_file]
  fallbacks:
    second_opinion:
      model: "openai/gpt-5.5"
    give_up:
      action: skip
      when: "vars.policy == 'skip'"
      on: [usage_window, unavailable, auth]

prompt work:
  Do the thing.

workflow w:
  entry: implement
  implement -> gate
  gate -> done
`

// TestFallbacksRoundtrip guards the failure mode that makes an
// unserialised DSL block WORSE than an unimplemented one: the studio
// saves every edit through parse → unparse, so a block the unparser
// does not know is silently DELETED from the .bot the next time anyone
// touches an unrelated field.
func TestFallbacksRoundtrip(t *testing.T) {
	pr1 := parser.Parse("t.bot", fallbacksRoundtripSrc)
	if len(pr1.Diagnostics) > 0 {
		for _, d := range pr1.Diagnostics {
			t.Logf("first parse diag: %+v", d)
		}
		t.Fatalf("first parse produced diagnostics")
	}
	out1 := Unparse(pr1.File)

	pr2 := parser.Parse("t.bot", out1)
	if len(pr2.Diagnostics) > 0 {
		for _, d := range pr2.Diagnostics {
			t.Logf("second parse diag: %+v", d)
		}
		t.Fatalf("re-parse of unparsed source produced diagnostics:\n%s", out1)
	}
	out2 := Unparse(pr2.File)
	if out1 != out2 {
		t.Fatalf("round-trip drift:\n--- pass 1 ---\n%s\n--- pass 2 ---\n%s", out1, out2)
	}

	for _, want := range []string{
		"fallbacks:",
		"api:",
		`backend: "claw"`,
		`model: "anthropic/claude-opus-5"`,
		`provider: "anthropic"`,
		"on: [usage_window]",
		"on: [usage_window, unavailable]",
		"metered: true",
		"second_opinion:",
	} {
		if !strings.Contains(out1, want) {
			t.Errorf("unparsed source missing %q\n---\n%s", want, out1)
		}
	}

	// Order is the try order — it must survive verbatim.
	if strings.Index(out1, "api:") > strings.Index(out1, "gpt:") {
		t.Errorf("route order inverted by the unparser:\n%s", out1)
	}

	// A judge's block must survive too: forgetting one of the two write
	// sites is the easy half-implementation.
	agents := pr2.File.Agents
	if len(agents) != 1 || len(agents[0].Fallbacks) != 2 {
		t.Fatalf("agent routes lost on re-parse: %+v", agents)
	}
	judges := pr2.File.Judges
	if len(judges) != 1 || len(judges[0].Fallbacks) != 2 {
		t.Fatalf("judge routes lost on re-parse: %+v", judges)
	}
	if !agents[0].Fallbacks[1].Metered {
		t.Error("metered: true lost in the round-trip — the author's spend acknowledgement must survive an editor save")
	}
	// The skip route's action + when gate must survive an editor save too:
	// losing either silently turns "skip on usage_window when the operator
	// chose skip" into an ordinary malformed route.
	skip := judges[0].Fallbacks[1]
	if skip.Action != "skip" {
		t.Errorf("action: skip lost in the round-trip (got %q)", skip.Action)
	}
	if skip.When != "vars.policy == 'skip'" {
		t.Errorf("when: gate lost in the round-trip (got %q)", skip.When)
	}
}
