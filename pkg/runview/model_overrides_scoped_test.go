package runview

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/llmroute"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

// The bot dimension at the ONE launch fold (ADR-132 §2 D21): a cell for
// another bot drops, a cell for the launch's bot folds as its bare target
// in its bot-match class — every bot, then glob, then the bot's name — so
// the more specific class wins a tie of equal node specificity, and an
// unscoped cell keeps its place among the every-bot cells.
func TestFold_TheBotDimensionResolvesAtTheFold(t *testing.T) {
	pol := &store.RunLLMRoutePolicy{Overrides: []store.RunLLMRouteOverride{
		{Selector: "review-pr/reviewer_claude", Model: "openai_compatible/bot"},
		{Selector: "reviewer_claude", Model: "openai_compatible/team"},
		{Selector: "revi-converse/converse_agent#glm", Model: "openai_compatible/other"},
		{Selector: "revi*/supervisor:pacer", Model: "openai_compatible/glob"},
	}}
	got := EffectiveModelOverrides(nil, pol, llmroute.BotIdentity{ID: "review-pr"})
	want := []ModelOverrideEntry{
		{Selector: "reviewer_claude", Model: "openai_compatible/team"},
		{Selector: "supervisor:pacer", Model: "openai_compatible/glob"},
		{Selector: "reviewer_claude", Model: "openai_compatible/bot"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("fold for review-pr = %+v, want %+v", got, want)
	}
	if m := toModelOverrides(got).ForNode("reviewer_claude", ir.NodeJudge).Model; m != "openai_compatible/bot" {
		t.Fatalf("reviewer_claude resolves model %q, want the bot-scoped cell's (it wins the tie)", m)
	}
	if got := EffectiveModelOverrides(nil, pol, llmroute.BotIdentity{}); !reflect.DeepEqual(got, []ModelOverrideEntry{{Selector: "reviewer_claude", Model: "openai_compatible/team"}}) {
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
	got := toModelOverrides(EffectiveModelOverrides([]ModelOverrideEntry{{Selector: "*", Model: "anthropic/launch"}}, pol, llmroute.BotIdentity{ID: "review-pr"}))
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
	warnings, err := ScreenPolicyOverrides(path, scopedScreenBot, "", "", llmroute.BotIdentity{ID: "scoped"}, pol)
	if err != nil {
		t.Fatalf("screen: %v", err)
	}
	// The zero-match warnings alone: the screen's other warnings (a gateway
	// crossing on this claude_code bot) name the cell they read too.
	var zeroMatch []string
	for _, w := range warnings {
		if strings.Contains(w, "matches no node") {
			zeroMatch = append(zeroMatch, w)
		}
	}
	joined := strings.Join(zeroMatch, "\n")
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
	warnings = zeroMatchWarnings(wf, once, llmroute.BotIdentity{ID: bot})
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
	}}, llmroute.BotIdentity{ID: "w"})
	findings := composedPairFindings(wf, sourced, "", llmroute.BotIdentity{}, nil)
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

// precedenceSnapshot resolves layers, highest priority first, the way
// resolveRunLLMRoutePolicy does — each validated, then llmroute.Resolve —
// so the snapshot carries the per-cell provenance the fold reads.
func precedenceSnapshot(t *testing.T, layers ...llmroute.Layer) *store.RunLLMRoutePolicy {
	t.Helper()
	for _, l := range layers {
		if err := llmroute.Validate(l.Policy); err != nil {
			t.Fatalf("layer %s refused: %v", l.Source, err)
		}
	}
	pol, src := llmroute.Resolve(layers...)
	out := &store.RunLLMRoutePolicy{Sources: src}
	for _, o := range pol.Overrides {
		out.Overrides = append(out.Overrides, store.RunLLMRouteOverride{Selector: o.Selector, Backend: o.Backend, Model: o.Model, Provider: o.Provider, Effort: o.Effort})
	}
	return out
}

func precedenceLayer(source string, cells ...llmroute.Override) llmroute.Layer {
	return llmroute.Layer{Source: source, Policy: llmroute.Policy{Overrides: cells}}
}

// foldFor folds a snapshot for a launch of bot.
func foldFor(snap *store.RunLLMRoutePolicy, bot string) model.ModelOverrides {
	return toModelOverrides(EffectiveModelOverrides(nil, snap, llmroute.BotIdentity{ID: bot}))
}

// The precedence of the cells reaching one site (ADR-132 §2 D21): the
// node target's specificity first, then the bot part — unscoped and `*/`
// (every bot) < a glob < the bot's name — then the level, the one that
// writes nearer the launch winning. Each case reads the field the way
// dispatch reads it (ForNode, ForRoute, ForSupervisor) on the folded list.
func TestFold_BotScopedPrecedence_BotPartThenLevel(t *testing.T) {
	ov := func(sel, m string) llmroute.Override {
		return llmroute.Override{Selector: sel, Model: "openai_compatible/" + m}
	}
	node := func(o model.ModelOverrides) string { return o.ForNode("converge", ir.NodeAgent).Model }
	route := func(o model.ModelOverrides) string { return o.ForRoute("converge", 0, "glm").Model }
	sup := func(o model.ModelOverrides) string { return o.ForSupervisor("pacer") }
	for _, tc := range []struct {
		name   string
		layers []llmroute.Layer
		bot    string
		read   func(model.ModelOverrides) string
		want   string
	}{
		{"a binding's exact route cell beats the team's */ cell", []llmroute.Layer{
			precedenceLayer(llmroute.SourceWebhook, ov("review-pr/converge#glm", "BINDING")),
			precedenceLayer(llmroute.SourceTeam, ov("*/converge#glm", "TEAM")),
		}, "review-pr", route, "BINDING"},
		{"a binding's exact node cell beats the platform's */ cell", []llmroute.Layer{
			precedenceLayer(llmroute.SourceWebhook, ov("review-pr/converge", "BINDING")),
			precedenceLayer(llmroute.SourcePlatform, ov("*/converge", "PLATFORM")),
		}, "review-pr", node, "BINDING"},
		{"*/x and x are one selector: the binding's level wins", []llmroute.Layer{
			precedenceLayer(llmroute.SourceWebhook, ov("converge", "BINDING")),
			precedenceLayer(llmroute.SourcePlatform, ov("*/converge", "PLATFORM")),
		}, "review-pr", node, "BINDING"},
		{"one level writing exact then */: the exact bot part wins", []llmroute.Layer{
			precedenceLayer(llmroute.SourceTeam, ov("review-pr/converge#glm", "EXACT"), ov("*/converge#glm", "STAR")),
		}, "review-pr", route, "EXACT"},
		{"every-bot class: the cell's own level decides, not the order its selector was first seen", []llmroute.Layer{
			precedenceLayer(llmroute.SourceWebhook, llmroute.Override{Selector: "converge", Backend: "claw"}),
			precedenceLayer(llmroute.SourceTeam, ov("*/converge", "TEAM")),
			precedenceLayer(llmroute.SourcePlatform, ov("converge", "PLATFORM")),
		}, "review-pr", node, "TEAM"},
		{"glob class: two globs at two levels, the nearer level wins", []llmroute.Layer{
			precedenceLayer(llmroute.SourceTeam, ov("rev*/converge", "TEAM")),
			precedenceLayer(llmroute.SourcePlatform, ov("*-pr/converge", "PLATFORM")),
		}, "review-pr", node, "TEAM"},
		{"glob class: the cell's own level decides, not the order its selector was first seen", []llmroute.Layer{
			precedenceLayer(llmroute.SourceWebhook, llmroute.Override{Selector: "rev*/converge", Backend: "claw"}),
			precedenceLayer(llmroute.SourceTeam, ov("*-pr/converge", "TEAM")),
			precedenceLayer(llmroute.SourcePlatform, ov("rev*/converge", "PLATFORM")),
		}, "review-pr", node, "TEAM"},
		{"the bot's name at a farther level beats an unscoped cell at a nearer one", []llmroute.Layer{
			precedenceLayer(llmroute.SourceWebhook, ov("converge", "BINDING")),
			precedenceLayer(llmroute.SourceTeam, ov("review-pr/converge", "TEAM")),
		}, "review-pr", node, "TEAM"},
		{"the bot's name beats a glob whatever the levels", []llmroute.Layer{
			precedenceLayer(llmroute.SourceWebhook, ov("rev*/converge", "GLOB")),
			precedenceLayer(llmroute.SourcePlatform, ov("review-pr/converge", "EXACT")),
		}, "review-pr", node, "EXACT"},
		{"a glob beats */ and an unscoped cell", []llmroute.Layer{
			precedenceLayer(llmroute.SourceWebhook, ov("converge", "UNSCOPED"), ov("*/converge", "STAR")),
			precedenceLayer(llmroute.SourcePlatform, ov("rev*/converge", "GLOB")),
		}, "review-pr", node, "GLOB"},
		{"a binding's exact supervisor cell beats the team's */ cell", []llmroute.Layer{
			precedenceLayer(llmroute.SourceWebhook, ov("review-pr/supervisor:pacer", "BINDING")),
			precedenceLayer(llmroute.SourceTeam, ov("*/supervisor:pacer", "TEAM")),
		}, "review-pr", sup, "BINDING"},
		{"*/ reaches a launch that names no bot, as an unscoped cell does", []llmroute.Layer{
			precedenceLayer(llmroute.SourceTeam, ov("*/converge", "STAR")),
		}, "", node, "STAR"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			snap := precedenceSnapshot(t, tc.layers...)
			if got := tc.read(foldFor(snap, tc.bot)); got != "openai_compatible/"+tc.want {
				t.Errorf("resolved %q, want %q (folded %+v)", got, "openai_compatible/"+tc.want, EffectiveModelOverrides(nil, snap, llmroute.BotIdentity{ID: tc.bot}))
			}
		})
	}
}

// A glob, a literal or a route cell scoped to a bot never reaches a launch
// that names none: only `*/` (and an unscoped cell) does.
func TestFold_AStarCellReachesALaunchNamingNoBot(t *testing.T) {
	pol := &store.RunLLMRoutePolicy{Overrides: []store.RunLLMRouteOverride{
		{Selector: "*/work#glm", Model: "openai_compatible/STAR-ROUTE"},
		{Selector: "*/supervisor:pacer", Model: "openai_compatible/STAR-SUP"},
		{Selector: "scoped/work#glm", Model: "openai_compatible/SCOPED"},
		{Selector: "revi*/supervisor:pacer", Model: "openai_compatible/GLOB"},
		{Selector: "**/work", Model: "openai_compatible/DOUBLE-STAR"},
	}}
	got := EffectiveModelOverrides(nil, pol, llmroute.BotIdentity{})
	want := []ModelOverrideEntry{
		{Selector: "work#glm", Model: "openai_compatible/STAR-ROUTE"},
		{Selector: "supervisor:pacer", Model: "openai_compatible/STAR-SUP"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("fold for a launch naming no bot = %+v, want the */ cells alone", got)
	}
}

const recoveryAgentBot = `
tool create_done:
  goal: "Create a file named done.txt in the workspace root."
  postcondition: "test -f done.txt"
  policy: recover
  recovery:
    max_repair_attempts: 0
    max_agent_attempts: 1
    model: "anthropic/claude-sonnet-4-6"
  command: "true"

workflow vabot:
  entry: create_done
  create_done -> done
`

// A Verified Action's rung-4 agent `<tool>__recover` is a node dispatch
// applies the folded overrides to: a cell reaching it pins something, so
// the zero-match warning stays quiet — while the tool node itself, and the
// rung of a tool whose ladder never reaches rung 4, pin nothing.
func TestZeroMatch_ARecoveryAgentCellReachesItsRung(t *testing.T) {
	t.Setenv("ITERION_VERIFIED_ACTION_MODEL", "")
	screen := func(src, selector string) []string {
		t.Helper()
		path := filepath.Join(t.TempDir(), "main.bot")
		if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
		pol := &store.RunLLMRoutePolicy{Overrides: []store.RunLLMRouteOverride{{Selector: selector, Model: "zai/glm-5.3"}}}
		warnings, err := ScreenPolicyOverrides(path, src, "", "", botIdentityFor("vabot"), pol)
		if err != nil {
			t.Fatalf("screen %s: %v", selector, err)
		}
		return warnings
	}
	for _, sel := range []string{"vabot/create_done__recover", "vabot/*__recover", "vabot/agent"} {
		if w := screen(recoveryAgentBot, sel); len(w) != 0 {
			t.Errorf("%s reaches the rung-4 agent, yet the screen warns: %v", sel, w)
		}
	}
	if w := screen(recoveryAgentBot, "vabot/create_done"); len(w) != 1 {
		t.Errorf("vabot/create_done pins a tool node no override reaches: warnings %v, want one", w)
	}
	noAgentRung := strings.Replace(strings.Replace(recoveryAgentBot, "max_repair_attempts: 0", "max_repair_attempts: 1", 1), "max_agent_attempts: 1", "max_agent_attempts: 0", 1)
	if w := screen(noAgentRung, "vabot/create_done__recover"); len(w) != 1 {
		t.Errorf("a ladder with no rung 4 dispatches no agent: warnings %v, want one", w)
	}
}

// A launch's own rows carry no bot dimension and obey the per-class field
// rules a routing cell obeys: foldAndScreen — the preview's body too —
// refuses a row the grammar cannot honour instead of folding a dead pin.
func TestFoldAndScreen_RefusesALaunchRowTheGrammarCannotHonour(t *testing.T) {
	path := filepath.Join(t.TempDir(), "main.bot")
	if err := os.WriteFile(path, []byte(scopedScreenBot), 0o644); err != nil {
		t.Fatal(err)
	}
	wf, _, _, err := compileForLaunch(path, scopedScreenBot, "", "", "", AnalysisTrust)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range []ModelOverrideEntry{
		{Selector: "scoped/work", Model: "anthropic/x"},
		{Selector: "work#glm", Effort: "high"},
		{Selector: "supervisor:pacer", Backend: "claw"},
	} {
		spec := &LaunchSpec{BotID: "scoped", ModelOverrides: []ModelOverrideEntry{row}}
		if _, err := foldAndScreen(wf, spec, ""); err == nil {
			t.Errorf("foldAndScreen accepted the launch row %+v", row)
		}
	}
	spec := &LaunchSpec{BotID: "scoped", ModelOverrides: []ModelOverrideEntry{{Selector: "work#glm", Model: "zai/glm-5.3"}, {Selector: "supervisor:pacer", Model: "anthropic/claude-opus-5"}}}
	if _, err := foldAndScreen(wf, spec, ""); err != nil {
		t.Errorf("foldAndScreen refused honoured launch rows: %v", err)
	}
}

// botIdentityFor is the identity a launch of a catalogue bot folds under:
// its id is its manifest name.
func botIdentityFor(id string) llmroute.BotIdentity { return llmroute.BotIdentity{ID: id} }

// A team fork answers to its slug and to its manifest name: on one site the
// slug's cell beats the name's, the name's beats a glob's, a glob's beats
// the every-bot cell's — whichever order the cells were written in — and a
// cell for another bot never folds.
func TestFold_BotRankIDOverNameOverGlobOverAll(t *testing.T) {
	fork := llmroute.BotIdentity{ID: "review-pr-pool", Name: "review-pr"}
	cells := []store.RunLLMRouteOverride{
		{Selector: "review-pr-pool/work", Model: "openai_compatible/SLUG"},
		{Selector: "review-pr/work", Model: "openai_compatible/NAME"},
		{Selector: "review-p?/work", Model: "openai_compatible/GLOB"},
		{Selector: "*/work", Model: "openai_compatible/STAR"},
		{Selector: "other/work", Model: "openai_compatible/OTHER"},
	}
	resolve := func(cs []store.RunLLMRouteOverride) string {
		pol := &store.RunLLMRoutePolicy{Overrides: cs}
		return toModelOverrides(EffectiveModelOverrides(nil, pol, fork)).ForNode("work", ir.NodeAgent).Model
	}
	for i, want := range []string{"SLUG", "NAME", "GLOB", "STAR"} {
		rest := append([]store.RunLLMRouteOverride{}, cells[i:]...)
		reversed := make([]store.RunLLMRouteOverride, len(rest))
		for j, c := range rest {
			reversed[len(rest)-1-j] = c
		}
		for name, cs := range map[string][]store.RunLLMRouteOverride{"as written": rest, "reversed": reversed} {
			if got := resolve(cs); got != "openai_compatible/"+want {
				t.Errorf("cells %d.. %s: work resolves %q, want %s", i, name, got, want)
			}
		}
	}
	if got := resolve(cells[4:]); got != "" {
		t.Errorf("another bot's cell folded: work resolves %q", got)
	}
}

// The zero-match warning speaks for a cell naming the bot by its manifest
// name too, and says which spelling it read.
func TestZeroMatch_ACellNamingTheManifestNameIsWarned(t *testing.T) {
	wf := &ir.Workflow{Name: "w", Nodes: map[string]ir.Node{
		"work": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "work"}, LLMFields: ir.LLMFields{Model: "claude-opus-5"}},
	}}
	pol := &store.RunLLMRoutePolicy{Overrides: []store.RunLLMRouteOverride{
		{Selector: "review-pr/wrok", Model: "openai_compatible/x"},
		{Selector: "review-pr/work", Model: "openai_compatible/x"},
	}}
	warnings := zeroMatchWarnings(wf, pol, llmroute.BotIdentity{ID: "review-pr-pool", Name: "review-pr"})
	if len(warnings) != 1 || !strings.Contains(warnings[0], `"review-pr/wrok"`) || !strings.Contains(warnings[0], `the manifest name of "review-pr-pool"`) {
		t.Fatalf("warnings = %v, want one for review-pr/wrok naming the manifest-name spelling", warnings)
	}
}
