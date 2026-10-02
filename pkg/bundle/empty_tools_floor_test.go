package bundle

import (
	"strings"
	"testing"
)

const emptyToolsMain = `prompt sys:
  """s"""

prompt usr:
  """u"""

judge reviewer:
  model: "openai/gpt-5.5"
  backend: "claw"
  system: sys
  user: usr
  tools: []

workflow w:
  entry: reviewer
  reviewer -> done
`

// An engine below DeclaredEmptyToolsSince parses `tools: []` without error
// and gives it the OPPOSITE meaning: an absent list, which every CLI backend
// reads as "no restriction". A bot that asked for no tools would run with all
// of them. So the bundle asks for the floor and an older engine refuses it
// instead of inverting it in silence.
//
// Reddens on the mutation that stops scanning for the declaration.
func TestABotDeclaringAnEmptyToolListAsksForTheEngineFloorThatReadsIt(t *testing.T) {
	req := MaxSyntaxRequirements(map[string]string{MainBotFile: emptyToolsMain})
	if !req.UsesDeclaredEmptyTools() {
		t.Fatalf("`tools: []` must ask for the floor, got %+v", req)
	}
	release, reason := RequiredRelease(req)
	if release != DeclaredEmptyToolsSince {
		t.Errorf("required release = %q, want %q", release, DeclaredEmptyToolsSince)
	}
	if !strings.Contains(reason, "tools") {
		t.Errorf("the reason must name the syntax, got %q", reason)
	}
	if !strings.Contains(req.Describe(), MainBotFile) {
		t.Errorf("the description must name the file, got %q", req.Describe())
	}
}

// The floor is asked for by the DECLARATION, not by any tools: list: a named
// list means the same thing on every engine, so asking for a floor there
// would pin every bot in the catalogue to a release it does not need.
func TestANamedOrAbsentToolListAsksForNoEmptyToolsFloor(t *testing.T) {
	for _, tc := range []struct{ name, src string }{
		{"named", strings.Replace(emptyToolsMain, "tools: []", "tools: [read_file]", 1)},
		{"absent", strings.Replace(emptyToolsMain, "  tools: []\n", "", 1)},
	} {
		req := MaxSyntaxRequirements(map[string]string{MainBotFile: tc.src})
		if req.UsesDeclaredEmptyTools() {
			t.Errorf("%s: asked for the empty-tools floor", tc.name)
		}
	}
}
