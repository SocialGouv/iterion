package bundle

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// aboveFloor is the floor one MAJOR above the constant.
//
// Derived rather than written: the case it covers ("a higher floor enables
// too") used to be the literal 99.0.0, which silently became a floor BELOW the
// constant the day the constant moved. A test whose meaning depends on a number
// staying bigger than another number is the same rot the constant itself has.
func aboveFloor(t *testing.T) string {
	t.Helper()
	major, _, ok := strings.Cut(ToolAliasesSince, ".")
	n, err := strconv.Atoi(major)
	if !ok || err != nil {
		t.Fatalf("ToolAliasesSince = %q: no numeric major to build an upper case from", ToolAliasesSince)
	}
	return fmt.Sprintf(">= %d.0.0", n+1)
}

func TestToolAliasesRequireSufficientDeclaredEngineFloor(t *testing.T) {
	if AllowsToolAliases(nil) || AllowsToolAliases(&Manifest{}) {
		t.Fatal("bare/legacy workflow enabled aliases")
	}
	for _, tc := range []struct {
		floor string
		want  bool
	}{{"", false}, {">= 3.143.0", false}, {">= " + ToolAliasesSince, true}, {aboveFloor(t), true}, {"garbage", false}} {
		if got := AllowsToolAliases(&Manifest{Requires: &Requires{Iterion: tc.floor}}); got != tc.want {
			t.Errorf("%q: %v, want %v", tc.floor, got, tc.want)
		}
	}
}

// TestTheUnsetFloorFailsClosed is what the sentinel is FOR: while the real
// release is unknown, no manifest anyone could plausibly write may switch the
// resolver on. Asserted against versions that actually exist rather than a
// made-up one, so it keeps meaning something as the project ships.
func TestTheUnsetFloorFailsClosed(t *testing.T) {
	if ToolAliasesSince != "9999.0.0" {
		t.Skip("the floor names a real release; the deadlock this guards is over")
	}
	for _, floor := range []string{">= 3.143.0", ">= 3.146.6", ">= 4.0.0", ">= 100.0.0"} {
		if AllowsToolAliases(&Manifest{Requires: &Requires{Iterion: floor}}) {
			t.Errorf("%q enabled the resolver while its release is still unknown — the floor must fail CLOSED, not merely be documented as provisional", floor)
		}
	}
}

// aliasFloorBot is a bundle source that spells an alias in two of the lists
// the resolver reads: a node's `tools:` and a node `tool_policy:`.
func aliasFloorBot() map[string]string {
	return map[string]string{
		"main.bot": "workflow w:\n  entry: a\n\nagent a:\n  backend: claw\n  model: test/test-model\n  tools: [Read]\n  tool_policy: [Grep]\n",
	}
}

// A bundle whose sources spell an alias in a tool list asks for the alias
// floor by the ONE floor predicate — the same ask a profile-2, `import` or
// `contract` bundle makes — and CheckSyntaxFloor holds the manifest to it.
// The runtime gate (AllowsToolAliases) keeps its meaning beside this: the
// declared floor is the opt-in.
func TestAliasSpellingsAskForTheAliasFloorByTheOnePredicate(t *testing.T) {
	req := MaxSyntaxRequirements(aliasFloorBot())
	if !req.UsesToolAliases() || !slices.Equal(req.AliasBy, []string{"main.bot"}) {
		t.Fatalf("requirements: %+v", req)
	}
	release, reason := RequiredRelease(req)
	if release != ToolAliasesSince || reason != "the Claw tool alias" {
		t.Fatalf("RequiredRelease = %q (%q), want %q (the Claw tool alias)", release, reason, ToolAliasesSince)
	}
	if pf := CheckSyntaxFloor(nil, req); pf.OK || pf.Need != ToolAliasesSince || pf.Reason != "the Claw tool alias" {
		t.Fatalf("no manifest: %+v", pf)
	}
	if pf := CheckSyntaxFloor(&Manifest{Requires: &Requires{Iterion: ">= " + ToolAliasesSince}}, req); !pf.OK {
		t.Fatalf("the alias release declared: %+v", pf)
	}
	if pf := CheckSyntaxFloor(&Manifest{Requires: &Requires{Iterion: ">= 3.176.0"}}, req); pf.OK {
		t.Fatal("a floor below the alias release passed")
	}
	if got := req.Describe(); got != "the Claw tool alias (main.bot)" {
		t.Fatalf("describe: %q", got)
	}
	// The canonical spelling and a wildcard-shaped entry never ask: the
	// detection is the exact alias table, not a substring.
	canonical := map[string]string{
		"main.bot": "workflow w:\n  entry: a\n\nagent a:\n  backend: claw\n  model: test/test-model\n  tools: [read_file, grep]\n",
	}
	if req := MaxSyntaxRequirements(canonical); req.UsesToolAliases() || req.Asks() {
		t.Fatalf("canonical names ask for a floor: %+v", req)
	}
}

// A workflow-level `tool_policy:`, a Verified Action's rung-4
// `agent_tools:`, and a tool node's `command:` spelled as a bare alias are
// the other lists the resolver reads; each ask for the alias floor the same
// way. A command that is not exactly an alias spelling (a shell command, a
// template ref) never asks.
func TestAliasDetectionCoversTheOtherToolLists(t *testing.T) {
	policy := map[string]string{
		"main.bot": "workflow w:\n  entry: a\n  tool_policy: [Bash]\n\nagent a:\n  backend: claw\n  model: test/test-model\n",
	}
	if req := MaxSyntaxRequirements(policy); !req.UsesToolAliases() {
		t.Fatalf("workflow tool_policy: %+v", req)
	}
	recovery := map[string]string{
		"main.bot": "workflow w:\n  entry: t\n\ntool t:\n  command: echo hi\n  policy: recover\n  recovery:\n    agent_tools: [Read]\n",
	}
	if req := MaxSyntaxRequirements(recovery); !req.UsesToolAliases() {
		t.Fatalf("recovery agent_tools: %+v", req)
	}
	registryCommand := map[string]string{
		"main.bot": "workflow w:\n  entry: t\n\ntool t:\n  command: Read\n",
	}
	if req := MaxSyntaxRequirements(registryCommand); !req.UsesToolAliases() {
		t.Fatalf("tool node command: %+v", req)
	}
	// An unquoted `command: Bash -c "ls"` parses as the bare word `Bash` —
	// exactly what the resolver aliases at runtime — so it asks, like the
	// bare spelling it is. What never asks: a quoted multi-word command (an
	// exact match against the whole string fails), a template ref, a
	// canonical name.
	for name, src := range map[string]string{
		"quoted multi-word": "workflow w:\n  entry: t\n\ntool t:\n  command: \"Read this file\"\n",
		"template ref":      "workflow w:\n  entry: t\n\ntool t:\n  command: ${TOOL}\n",
		"canonical":         "workflow w:\n  entry: t\n\ntool t:\n  command: read_file\n",
	} {
		if req := MaxSyntaxRequirements(map[string]string{"main.bot": src}); req.UsesToolAliases() {
			t.Errorf("%s asks for the alias floor: %+v", name, req)
		}
	}
}
