package bundle

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// labelled builds a kind=forge invocation on issue_labeled.
func labelled(mode ExecutionMode, argsVar string, labels ...string) Invocation {
	return Invocation{
		Kind:    InvocationKindForge,
		Mode:    mode,
		ArgsVar: argsVar,
		Forge:   &InvocationForge{Event: ForgeEventIssueLabeled, Labels: labels},
	}
}

// command builds a kind=command invocation carrying context vars, so a route
// that borrowed them from it shows.
func command(name, argsVar string, opensMR bool) Invocation {
	return Invocation{
		Kind:        InvocationKindCommand,
		Mode:        ExecutionBoard,
		ArgsVar:     argsVar,
		ContextVars: map[string]string{"post_to_board": "false"},
		Command:     &InvocationCommand{Name: name, Scope: "any", OpensMR: opensMR},
	}
}

// syntheticRoute is the label route of a bot that claims issue_labeled
// through its `forge:` block alone, spelled out field by field: board mode,
// the args var and opens_mr given, the block's labels, and NO context vars.
func syntheticRoute(argsVar string, opensMR bool, labels []string) ForgeRoutingInvocation {
	return ForgeRoutingInvocation{
		Inv: Invocation{
			Kind:    InvocationKindForge,
			Mode:    ExecutionBoard,
			ArgsVar: argsVar,
			Forge:   &InvocationForge{Event: ForgeEventIssueLabeled, Labels: labels},
		},
		Key:       "legacy:issue_labeled",
		Labels:    labels,
		Synthetic: true,
		ArgsVar:   argsVar,
		OpensMR:   opensMR,
	}
}

// A bot's own kind=forge invocations on issue_labeled each derive a route
// that keeps what the manifest says — its mode, its labels, its context
// vars — and borrows from the bot's first command only what it does not
// say itself: the args var, and whether a merge request is opened.
func TestForgeIssueInvocations_Explicit(t *testing.T) {
	block := &ForgeRequirements{Events: []string{ForgeEventIssueLabeled}, Labels: []string{"block-label"}}
	withVars := labelled(ExecutionDirect, "issue_text", "nestor", "*-chaos")
	withVars.ContextVars = map[string]string{"force": "true"}

	for name, tc := range map[string]struct {
		fr          *ForgeRequirements
		invs        []Invocation
		wantIdx     int
		wantMode    ExecutionMode
		wantLabels  []string
		wantArgsVar string
		wantOpensMR bool
	}{
		"its own args var, a command that opens no merge request": {
			fr: block, invs: []Invocation{command("probe", "command_text", false), withVars},
			wantIdx: 1, wantMode: ExecutionDirect, wantLabels: []string{"nestor", "*-chaos"}, wantArgsVar: "issue_text", wantOpensMR: false,
		},
		"its own args var, the command declared after it": {
			fr: block, invs: []Invocation{withVars, command("probe", "command_text", false)},
			wantIdx: 0, wantMode: ExecutionDirect, wantLabels: []string{"nestor", "*-chaos"}, wantArgsVar: "issue_text", wantOpensMR: false,
		},
		"no args var: the first command's": {
			invs:    []Invocation{labelled(ExecutionBoard, "", "saboteur"), command("probe", "app_prompt", true), command("other", "other_prompt", false)},
			wantIdx: 0, wantMode: ExecutionBoard, wantLabels: []string{"saboteur"}, wantArgsVar: "app_prompt", wantOpensMR: true,
		},
		"no args var: the first command's, the commands swapped": {
			invs:    []Invocation{labelled(ExecutionBoard, "", "saboteur"), command("other", "other_prompt", false), command("probe", "app_prompt", true)},
			wantIdx: 0, wantMode: ExecutionBoard, wantLabels: []string{"saboteur"}, wantArgsVar: "other_prompt", wantOpensMR: false,
		},
		"no args var, a command that names none: the default": {
			invs:    []Invocation{command("probe", "", false), labelled("", "")},
			wantIdx: 1, wantMode: ExecutionDirect, wantLabels: nil, wantArgsVar: ForgeIssueDefaultArgsVar, wantOpensMR: false,
		},
		"no command at all: the default args var, and it opens a merge request": {
			invs:    []Invocation{labelled("", "", "nestor")},
			wantIdx: 0, wantMode: ExecutionDirect, wantLabels: []string{"nestor"}, wantArgsVar: ForgeIssueDefaultArgsVar, wantOpensMR: true,
		},
	} {
		t.Run(name, func(t *testing.T) {
			got, warnings := ForgeIssueInvocations(tc.fr, tc.invs)
			if len(warnings) != 0 {
				t.Errorf("warned about a manifest that derives every route it declares: %q", warnings)
			}
			if len(got) != 1 {
				t.Fatalf("derived %d routes, want the one declared invocation (the block is not consulted beside it): %+v", len(got), got)
			}
			route, declared := got[0], tc.invs[tc.wantIdx]
			if route.Synthetic || route.Aliased {
				t.Errorf("a declared invocation reads as synthetic=%v aliased=%v", route.Synthetic, route.Aliased)
			}
			if !reflect.DeepEqual(route.Inv, declared) {
				t.Errorf("the route carries %+v, want the declared invocation %+v", route.Inv, declared)
			}
			if want := InvocationKey(tc.invs, tc.wantIdx); route.Key != want || !strings.HasPrefix(route.Key, "forge:issue_labeled|") {
				t.Errorf("key = %q, want the invocation's own %q", route.Key, want)
			}
			if mode, err := route.Inv.EffectiveMode(); err != nil || mode != tc.wantMode {
				t.Errorf("mode = %q (%v), want %q", mode, err, tc.wantMode)
			}
			if !reflect.DeepEqual(route.Labels, tc.wantLabels) {
				t.Errorf("labels = %v, want %v", route.Labels, tc.wantLabels)
			}
			if route.ArgsVar != tc.wantArgsVar {
				t.Errorf("args var = %q, want %q", route.ArgsVar, tc.wantArgsVar)
			}
			if route.OpensMR != tc.wantOpensMR {
				t.Errorf("opens_mr = %v, want %v", route.OpensMR, tc.wantOpensMR)
			}
		})
	}

	// A declared invocation keeps its own context vars: they are the
	// manifest's, and the route is what carries them to the run.
	got, _ := ForgeIssueInvocations(block, []Invocation{command("probe", "command_text", false), withVars})
	if want := map[string]string{"force": "true"}; !reflect.DeepEqual(got[0].Inv.ContextVars, want) {
		t.Errorf("a declared invocation's context vars = %v, want its own %v", got[0].Inv.ContextVars, want)
	}
	// The labels handed out are a copy of the manifest's.
	got[0].Labels[0] = "rewritten"
	if withVars.Forge.Labels[0] != "nestor" {
		t.Errorf("the route handed out the manifest's own label slice: it now reads %q", withVars.Forge.Labels[0])
	}
}

// Two declared invocations derive two routes, in manifest order, whichever
// is declared first: the set is the same in both orders.
func TestForgeIssueInvocations_BothInvocationOrders(t *testing.T) {
	nestor, saboteur := labelled(ExecutionDirect, "", "nestor"), labelled(ExecutionBoard, "", "saboteur")
	for name, invs := range map[string][]Invocation{
		"nestor first":   {nestor, command("probe", "app_prompt", true), saboteur},
		"saboteur first": {saboteur, command("probe", "app_prompt", true), nestor},
	} {
		t.Run(name, func(t *testing.T) {
			got, warnings := ForgeIssueInvocations(nil, invs)
			if len(got) != 2 || len(warnings) != 0 {
				t.Fatalf("derived %d routes with warnings %q, want both invocations and no warning", len(got), warnings)
			}
			byLabel := map[string]ForgeRoutingInvocation{}
			for i, route := range got {
				byLabel[route.Labels[0]] = route
				// Manifest order: the forge invocations sit at 0 and 2.
				if want := InvocationKey(invs, i*2); route.Key != want {
					t.Errorf("route %d has key %q, want the key of invocations[%d], %q", i, route.Key, i*2, want)
				}
			}
			if mode, _ := byLabel["nestor"].Inv.EffectiveMode(); mode != ExecutionDirect {
				t.Errorf("nestor's route runs %q, want its own direct", mode)
			}
			if mode, _ := byLabel["saboteur"].Inv.EffectiveMode(); mode != ExecutionBoard {
				t.Errorf("saboteur's route runs %q, want its own board", mode)
			}
			if byLabel["nestor"].Key == byLabel["saboteur"].Key {
				t.Errorf("two invocations with different labels share the key %q", byLabel["nestor"].Key)
			}
		})
	}
}

// A bot that claims issue_labeled through its `forge:` block alone derives
// one synthetic route: the label route such a bot is started by — board
// mode, the first command's args var or the default, that command's
// opens_mr or true, the block's labels — and no context vars, whatever the
// command carries.
func TestForgeIssueInvocations_Synthetic(t *testing.T) {
	claims := func(labels ...string) *ForgeRequirements {
		return &ForgeRequirements{Events: []string{ForgeEventIssueLabeled, ForgeEventPullRequestComment}, Labels: labels}
	}
	for name, tc := range map[string]struct {
		fr   *ForgeRequirements
		invs []Invocation
		want ForgeRoutingInvocation
	}{
		"a command and a board invocation, the block's labels": {
			claims("implement", "*-feature"), []Invocation{command("featurly", "feature_prompt", true), {Kind: InvocationKindBoard}},
			syntheticRoute("feature_prompt", true, []string{"implement", "*-feature"}),
		},
		"the command's own args var": {
			claims(), []Invocation{{Kind: InvocationKindBoard}, command("appy", "app_prompt", true)},
			syntheticRoute("app_prompt", true, nil),
		},
		"a command that opens no merge request": {
			claims(), []Invocation{command("probe", "notes", false)},
			syntheticRoute("notes", false, nil),
		},
		"a command that names no args var": {
			claims(), []Invocation{command("probe", "", false)},
			syntheticRoute("feature_prompt", false, nil),
		},
		"the first of two commands": {
			claims(), []Invocation{command("first", "first_prompt", false), command("second", "second_prompt", true)},
			syntheticRoute("first_prompt", false, nil),
		},
		"the first of two commands, swapped": {
			claims(), []Invocation{command("second", "second_prompt", true), command("first", "first_prompt", false)},
			syntheticRoute("second_prompt", true, nil),
		},
		"no command: the implementer contract": {
			claims("implement"), nil,
			syntheticRoute("feature_prompt", true, []string{"implement"}),
		},
		"a forge invocation on another event does not speak for labelled issues": {
			claims("implement"), []Invocation{{Kind: InvocationKindForge, Mode: ExecutionDirect, Forge: &InvocationForge{Event: ForgeEventPullRequest, Actions: []string{"opened"}}}},
			syntheticRoute("feature_prompt", true, []string{"implement"}),
		},
	} {
		t.Run(name, func(t *testing.T) {
			got, warnings := ForgeIssueInvocations(tc.fr, tc.invs)
			if len(warnings) != 0 {
				t.Errorf("warned about a block that claims issue_labeled on its own: %q", warnings)
			}
			if len(got) != 1 {
				t.Fatalf("derived %d routes, want the one synthetic route: %+v", len(got), got)
			}
			if !reflect.DeepEqual(got[0], tc.want) {
				t.Fatalf("the synthetic route is not the label route:\n got: %+v (inv %+v, forge %+v)\nwant: %+v (inv %+v, forge %+v)",
					got[0], got[0].Inv, got[0].Inv.Forge, tc.want, tc.want.Inv, tc.want.Inv.Forge)
			}
			if len(got[0].Inv.ContextVars) != 0 {
				t.Fatalf("the synthetic route carries context vars %v: the label route never carries a command's", got[0].Inv.ContextVars)
			}
			if mode, err := got[0].Inv.EffectiveMode(); err != nil || mode != ExecutionBoard {
				t.Fatalf("the synthetic route runs %q (%v), want board", mode, err)
			}
		})
	}

	// The block's labels are copied: a caller that rewrites the route does
	// not rewrite the manifest.
	fr := claims("implement")
	got, _ := ForgeIssueInvocations(fr, nil)
	got[0].Labels[0] = "rewritten"
	got[0].Inv.Forge.Labels[0] = "rewritten"
	if fr.Labels[0] != "implement" {
		t.Errorf("the synthetic route handed out the block's own label slice: it now reads %q", fr.Labels[0])
	}

	// The same route from a manifest's own bytes: a bundle with a `forge:`
	// block and no `invocations:` block declares no invocation, and its
	// block's labels reach the route.
	m, err := DecodeManifest([]byte("name: probe\nforge:\n  events: [issue_labeled]\n  labels: [implement, \"*-feature\"]\n"), "probe")
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	got, warnings := ForgeIssueInvocations(m.Forge, m.Invocations)
	if len(warnings) != 0 || len(got) != 1 || !reflect.DeepEqual(got[0], syntheticRoute("feature_prompt", true, []string{"implement", "*-feature"})) {
		t.Fatalf("a decoded forge-only manifest derived %+v with warnings %q, want its block's label route", got, warnings)
	}

	// A block that does not claim issue_labeled, or no block, derives
	// nothing and warns about nothing.
	for name, fr := range map[string]*ForgeRequirements{
		"no block":                   nil,
		"a block without the event":  {Events: []string{ForgeEventPullRequest, ForgeEventPullRequestComment}, Labels: []string{"implement"}},
		"a block that lists nothing": {},
	} {
		if got, warnings := ForgeIssueInvocations(fr, []Invocation{command("probe", "notes", true), {Kind: InvocationKindBoard}}); len(got) != 0 || len(warnings) != 0 {
			t.Errorf("%s: derived %+v with warnings %q, want nothing", name, got, warnings)
		}
	}
}

// The synthetic route is the one the catalogue's label bots are started by:
// read from their shipped manifests, it is board mode, the bot's own
// command args var, an opened merge request and no context vars.
func TestForgeIssueInvocations_ShippedLabelBots(t *testing.T) {
	for bot, argsVar := range map[string]string{
		"feature-dev": "feature_prompt",
		"app-dev":     "app_prompt",
		"instrument":  "mission_notes",
	} {
		t.Run(bot, func(t *testing.T) {
			m, err := LoadManifest(filepath.Join("..", "..", "bots", bot, "manifest.yaml"))
			if err != nil || m == nil {
				t.Fatalf("load: %v (manifest %v)", err, m)
			}
			got, warnings := ForgeIssueInvocations(m.Forge, m.Invocations)
			if len(warnings) != 0 {
				t.Errorf("warnings on a shipped manifest: %q", warnings)
			}
			if len(got) != 1 {
				t.Fatalf("derived %d routes, want the one label route: %+v", len(got), got)
			}
			if want := syntheticRoute(argsVar, true, nil); !reflect.DeepEqual(got[0], want) {
				t.Fatalf("the label route is %+v (inv %+v), want %+v (inv %+v)", got[0], got[0].Inv, want, want.Inv)
			}
		})
	}
}

// A mode no lane honours derives no route and says so, naming the
// invocation. It owns no key either: an invocation that repeats it derives.
func TestForgeIssueInvocations_Deliver(t *testing.T) {
	deliver := labelled(ExecutionDeliver, "", "nestor")

	got, warnings := ForgeIssueInvocations(nil, []Invocation{command("probe", "notes", true), deliver})
	if len(got) != 0 {
		t.Fatalf("a deliver invocation derived a route: %+v", got)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "invocations[1]:") || !strings.Contains(warnings[0], `"deliver"`) {
		t.Fatalf("the refusal does not name the invocation and its mode: %q", warnings)
	}

	// The same key in a mode a lane honours derives, and is not a duplicate
	// of the invocation that derived nothing — in both orders.
	direct := labelled(ExecutionDirect, "", "nestor")
	for name, tc := range map[string]struct {
		invs       []Invocation
		refusedIdx string
	}{
		"deliver first": {[]Invocation{deliver, direct}, "invocations[0]:"},
		"direct first":  {[]Invocation{direct, deliver}, "invocations[1]:"},
	} {
		t.Run(name, func(t *testing.T) {
			got, warnings := ForgeIssueInvocations(nil, tc.invs)
			if len(got) != 1 {
				t.Fatalf("derived %d routes, want the direct invocation alone: %+v", len(got), got)
			}
			if mode, err := got[0].Inv.EffectiveMode(); err != nil || mode != ExecutionDirect {
				t.Errorf("the derived route runs %q (%v), want direct", mode, err)
			}
			if len(warnings) != 1 || !strings.Contains(warnings[0], tc.refusedIdx) || !strings.Contains(warnings[0], `"deliver"`) {
				t.Errorf("warnings = %q, want the deliver invocation alone, at %s", warnings, tc.refusedIdx)
			}
			if dup := ForgeDuplicateKeyWarnings(tc.invs); len(dup) != 0 {
				t.Errorf("an invocation that derives nothing made its twin a duplicate: %q", dup)
			}
		})
	}

	// With every declared invocation refused, a block that claims
	// issue_labeled speaks for the bot, as it does when its webhook rule is
	// built: the refusal is warned and the label route derives.
	block := &ForgeRequirements{Events: []string{ForgeEventIssueLabeled}, Labels: []string{"implement"}}
	got, warnings = ForgeIssueInvocations(block, []Invocation{deliver})
	if len(got) != 1 || !reflect.DeepEqual(got[0], syntheticRoute("feature_prompt", true, []string{"implement"})) {
		t.Fatalf("derived %+v, want the block's label route", got)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], `"deliver"`) {
		t.Fatalf("the refused invocation is not warned beside the block's route: %q", warnings)
	}
}

// Two declared invocations of one key derive once: the first in manifest
// order answers, the second is named in a warning — whichever comes first.
func TestForgeIssueInvocations_Duplicate(t *testing.T) {
	direct, board := labelled(ExecutionDirect, "", "nestor", "*-chaos"), labelled(ExecutionBoard, "", "*-CHAOS", " Nestor ")
	for name, tc := range map[string]struct {
		invs     []Invocation
		wantMode ExecutionMode
	}{
		"direct first": {[]Invocation{direct, board}, ExecutionDirect},
		"board first":  {[]Invocation{board, direct}, ExecutionBoard},
	} {
		t.Run(name, func(t *testing.T) {
			got, warnings := ForgeIssueInvocations(nil, tc.invs)
			if len(got) != 1 {
				t.Fatalf("one key derived %d routes, want 1: %+v", len(got), got)
			}
			if mode, err := got[0].Inv.EffectiveMode(); err != nil || mode != tc.wantMode {
				t.Errorf("the derived route runs %q (%v), want the first declared invocation's %q", mode, err, tc.wantMode)
			}
			if !reflect.DeepEqual(got[0].Inv, tc.invs[0]) {
				t.Errorf("the derived route carries %+v, want invocations[0] %+v", got[0].Inv, tc.invs[0])
			}
			if len(warnings) != 1 {
				t.Fatalf("got %d warnings, want the one naming the ignored invocation: %q", len(warnings), warnings)
			}
			for _, want := range []string{"invocations[1]:", "two forge invocations derive the same key: the second is ignored", "invocations[0]"} {
				if !strings.Contains(warnings[0], want) {
					t.Errorf("warning %q does not carry %q", warnings[0], want)
				}
			}
		})
	}
}

// The catalogue hands out a bundle's EFFECTIVE invocations, and for a bundle
// with no `invocations:` block those are the migration shim's: one direct
// kind=forge invocation per block event. Read as declared, the shim would
// start the bot direct and on any label; it declares nothing, so the block's
// label route derives — board mode, the block's labels — as the legacy label
// route starts such a bot. Only a set equal to the shim reads so: a declared
// invocation that differs from it in any field keeps its own route.
func TestForgeIssueInvocations_EffectiveShimDeclaresNothing(t *testing.T) {
	for name, tc := range map[string]struct {
		manifest string
		want     ForgeRoutingInvocation
	}{
		"a block claiming issue_labeled, with its labels": {
			manifest: "name: probe\nforge:\n  events: [issue_labeled]\n  labels: [implement, \"*-feature\"]\n",
			want:     syntheticRoute("feature_prompt", true, []string{"implement", "*-feature"}),
		},
		"a block claiming issue_labeled and other events": {
			manifest: "name: probe\nforge:\n  events: [pull_request, issue_labeled, pull_request_comment]\n  labels: [implement]\n",
			want:     syntheticRoute("feature_prompt", true, []string{"implement"}),
		},
		"a block claiming issue_labeled with no label": {
			manifest: "name: probe\nforge:\n  events: [issue_labeled]\n",
			want:     syntheticRoute("feature_prompt", true, nil),
		},
		"declared by hand, exactly the shim's set: read like the shim": {
			manifest: "name: probe\nforge:\n  events: [issue_labeled]\n  labels: [implement]\n" +
				"invocations:\n  - kind: forge\n    mode: direct\n    forge:\n      event: issue_labeled\n",
			want: syntheticRoute("feature_prompt", true, []string{"implement"}),
		},
	} {
		t.Run(name, func(t *testing.T) {
			m, err := DecodeManifest([]byte(tc.manifest), "probe")
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			effective := EffectiveInvocations(m)
			if len(effective) == 0 {
				t.Fatalf("control: the manifest's effective invocations are empty, so the shim is not exercised")
			}
			got, warnings := ForgeIssueInvocations(m.Forge, effective)
			if len(warnings) != 0 {
				t.Errorf("warnings on the shim's set: %q", warnings)
			}
			if len(got) != 1 || !reflect.DeepEqual(got[0], tc.want) {
				t.Fatalf("the effective set derived %+v, want the block's label route %+v (inv %+v)", got, tc.want, tc.want.Inv)
			}
			if mode, err := got[0].Inv.EffectiveMode(); err != nil || mode != ExecutionBoard {
				t.Errorf("the route runs %q (%v), want board", mode, err)
			}
		})
	}

	// A declared set that differs from the shim in one field is the author's:
	// its own direct route, with its own labels.
	block := &ForgeRequirements{Events: []string{ForgeEventIssueLabeled}, Labels: []string{"implement"}}
	for name, invs := range map[string][]Invocation{
		"the shim's invocation with labels of its own": {labelled(ExecutionDirect, "", "nestor")},
		"the shim's invocation with an args var":       {labelled(ExecutionDirect, "issue_text")},
		"the shim's invocation and a command":          {labelled(ExecutionDirect, ""), command("probe", "notes", true)},
	} {
		t.Run(name, func(t *testing.T) {
			got, _ := ForgeIssueInvocations(block, invs)
			if len(got) != 1 || got[0].Synthetic {
				t.Fatalf("a declared set unlike the shim derived %+v, want its own declared route", got)
			}
			if mode, err := got[0].Inv.EffectiveMode(); err != nil || mode != ExecutionDirect {
				t.Errorf("the declared route runs %q (%v), want its own direct", mode, err)
			}
		})
	}
}
