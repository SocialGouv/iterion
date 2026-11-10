package runview

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

// The bot dimension at the ONE launch fold (ADR-132 §2 D21): a cell for
// another bot drops, a cell for the launch's bot folds as its bare target
// AFTER every unscoped cell — so it wins a tie of equal node specificity —
// and an unscoped cell keeps its place.
func TestFold_TheBotDimensionResolvesAtTheFold(t *testing.T) {
	pol := &store.RunLLMRoutePolicy{Overrides: []store.RunLLMRouteOverride{
		{Selector: "review-pr/reviewer_claude", Model: "openai_compatible/bot"},
		{Selector: "reviewer_claude", Model: "openai_compatible/team"},
		{Selector: "revi-converse/converse_agent#glm", Model: "openai_compatible/other"},
		{Selector: "revi*/supervisor:pacer", Model: "openai_compatible/glob"},
	}}
	got := EffectiveModelOverrides(nil, pol, "review-pr")
	want := []ModelOverrideEntry{
		{Selector: "reviewer_claude", Model: "openai_compatible/team"},
		{Selector: "reviewer_claude", Model: "openai_compatible/bot"},
		{Selector: "supervisor:pacer", Model: "openai_compatible/glob"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("fold for review-pr = %+v, want %+v", got, want)
	}
	if m := toModelOverrides(got).ForNode("reviewer_claude", ir.NodeJudge).Model; m != "openai_compatible/bot" {
		t.Fatalf("reviewer_claude resolves model %q, want the bot-scoped cell's (it wins the tie)", m)
	}
	if got := EffectiveModelOverrides(nil, pol, ""); !reflect.DeepEqual(got, []ModelOverrideEntry{{Selector: "reviewer_claude", Model: "openai_compatible/team"}}) {
		t.Fatalf("fold for a launch naming no bot = %+v, want the unscoped cell alone", got)
	}
}

// The launch's "*" row re-targets nodes only: it shadows the policy's node
// cells on its field, never a route's or a supervisor's own pin — those
// sites are reached by no node row.
func TestFold_AStarLaunchRowShadowsNoScopedCell(t *testing.T) {
	pol := &store.RunLLMRoutePolicy{Overrides: []store.RunLLMRouteOverride{
		{Selector: "review-pr/reviewer_claude", Model: "openai_compatible/node"},
		{Selector: "review-pr/reviewer_claude#glm", Model: "openai_compatible/route"},
		{Selector: "review-pr/supervisor:pacer", Model: "openai_compatible/sup"},
	}}
	got := toModelOverrides(EffectiveModelOverrides([]ModelOverrideEntry{{Selector: "*", Model: "anthropic/launch"}}, pol, "review-pr"))
	if m := got.ForNode("reviewer_claude", ir.NodeJudge).Model; m != "anthropic/launch" {
		t.Fatalf("node model = %q, want the launch's * row", m)
	}
	if m := got.ForRoute("reviewer_claude", 0, "glm").Model; m != "openai_compatible/route" {
		t.Fatalf("route model = %q, want the route cell kept", m)
	}
	if m := got.ForSupervisor("pacer"); m != "openai_compatible/sup" {
		t.Fatalf("supervisor model = %q, want the supervisor cell kept", m)
	}
}

const scopedScreenBot = `
agent work:
  backend: "claude_code"
  model: "claude-opus-5"
  system: p
  tools: [read_file]
  fallbacks:
    glm:
      model: "glm-5.3"
      provider: "zai"

supervisor pacer:
  watches: [work]
  model: "anthropic/claude-opus-5"
  system: p

prompt p:
  Do the work.

workflow scoped:
  entry: work
  work -> done
`

// The zero-match warning: a cell written for THIS bot by name whose target
// reaches nothing in it — at the write screen every time, at the launch once
// per (bot, selector) per process. A glob-scoped or unscoped cell matching
// nothing here is the fold's ordinary reach, never warned.
func TestZeroMatch_ACellNamingTheBotThatReachesNothingIsWarned(t *testing.T) {
	pol := &store.RunLLMRoutePolicy{Overrides: []store.RunLLMRouteOverride{
		{Selector: "scoped/work#gml", Model: "openai_compatible/x"},
		{Selector: "scoped/supervisor:pacr", Model: "openai_compatible/x"},
		{Selector: "scoped/work#glm", Model: "openai_compatible/x"},
		{Selector: "scoped/supervisor:pacer", Model: "openai_compatible/x"},
		{Selector: "scoped/work", Model: "openai_compatible/x"},
		{Selector: "other/nope", Model: "openai_compatible/x"},
		{Selector: "*/nope#glm", Model: "openai_compatible/x"},
		{Selector: "nope", Model: "openai_compatible/x"},
	}}
	path := filepath.Join(t.TempDir(), "main.bot")
	if err := os.WriteFile(path, []byte(scopedScreenBot), 0o644); err != nil {
		t.Fatal(err)
	}
	warnings, err := ScreenPolicyOverrides(path, scopedScreenBot, "", "", "scoped", pol)
	if err != nil {
		t.Fatalf("screen: %v", err)
	}
	joined := strings.Join(warnings, "\n")
	for _, want := range []string{`"scoped/work#gml"`, `"scoped/supervisor:pacr"`} {
		if !strings.Contains(joined, want) {
			t.Errorf("screen warnings = %v, want one naming %s", warnings, want)
		}
	}
	for _, quiet := range []string{`"scoped/work#glm"`, `"scoped/supervisor:pacer"`, `"scoped/work"`, `"other/nope"`, `"*/nope#glm"`, `"nope"`} {
		if strings.Contains(joined, quiet) {
			t.Errorf("screen warns on %s, which reaches its site or is not this bot's by name: %v", quiet, warnings)
		}
	}
	wf, _, _, err := compileForLaunch(path, scopedScreenBot, "", "", "", AnalysisTrust)
	if err != nil {
		t.Fatal(err)
	}
	const bot = "scoped-once-per-process"
	once := &store.RunLLMRoutePolicy{Overrides: []store.RunLLMRouteOverride{{Selector: bot + "/work#gml", Model: "openai_compatible/x"}}}
	warnings = zeroMatchWarnings(wf, once, bot)
	if len(warnings) != 1 {
		t.Fatalf("launch warnings = %v, want one", warnings)
	}
	var logged bytes.Buffer
	logger := iterlog.New(iterlog.LevelInfo, &logged)
	logZeroMatchOnce(logger, warnings)
	logZeroMatchOnce(logger, warnings)
	if n := strings.Count(logged.String(), bot+"/work#gml"); n != 1 {
		t.Fatalf("two launches logged the warning %d times, want once per name:\n%s", n, logged.String())
	}
}

// The composed-pair check reaches a route: a route cell's provider beside
// the backend an override gave the node — the route names none of its own —
// is a pair the harness ignores, refused naming the route.
func TestComposedPair_ARouteCellComposesWithTheNodesBackend(t *testing.T) {
	wf := &ir.Workflow{Name: "w", Nodes: map[string]ir.Node{
		"work": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "work"}, LLMFields: ir.LLMFields{Backend: "claude_code", Model: "claude-opus-5"},
			Fallbacks: []ir.Fallback{{Name: "glm", Model: "glm-5.3"}}},
	}}
	sourced := foldSourcedOverrides(nil, &store.RunLLMRoutePolicy{Overrides: []store.RunLLMRouteOverride{
		{Selector: "w/work", Backend: "codex"},
		{Selector: "w/work#glm", Provider: "zai"},
	}}, "w")
	findings := composedPairFindings(wf, sourced)
	if len(findings) != 1 || findings[0].route != "work#glm" || !strings.Contains(findings[0].err.Error(), "codex") {
		t.Fatalf("findings = %+v, want the glm route's composed codex+zai pair", findings)
	}
}

const cardShapeBot = `
vars:
  m: string = "anthropic/claude-opus-5"

agent work:
  backend: "claw"
  model: "{{vars.m}}"
  system: p
  tools: [read_file]

prompt p:
  Do the work.

workflow card:
  entry: work
  work -> done
`

// The card lanes' shape check judges the field dispatch reads: a model a
// cell pins is the cell's literal, so the node's own `{{vars.m}}` — which
// this card's vars leave undecided — refuses nothing. Without the cell it
// does.
func TestRefuseUndecidedRoutingFields_ACellDecidesTheFieldItPins(t *testing.T) {
	path := filepath.Join(t.TempDir(), "main.bot")
	if err := os.WriteFile(path, []byte(cardShapeBot), 0o644); err != nil {
		t.Fatal(err)
	}
	wf, _, _, err := compileForLaunch(path, cardShapeBot, "", "", "", AnalysisTrust)
	if err != nil {
		t.Fatal(err)
	}
	vars := map[string]string{"m": "openai/gpt-5.5"}
	bare := &LaunchSpec{Vars: vars}
	if err := refuseUndecidedRoutingFields(wf, bare); err == nil {
		t.Fatal("the card's own undecided model launched — the fixture is not the refusing shape")
	}
	pinned := &LaunchSpec{Vars: vars, ModelOverrides: []ModelOverrideEntry{{Selector: "work", Model: "openai_compatible/x"}}}
	if err := refuseUndecidedRoutingFields(wf, pinned); err != nil {
		t.Fatalf("a cell pinning the model still refused on the node's own template: %v", err)
	}
}
