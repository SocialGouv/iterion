package bundle

import (
	"errors"
	"strings"
	"testing"
)

func TestLoadManifest_ParsesInvocations(t *testing.T) {
	body := `name: featurly
schema_version: 1
invocations:
  - kind: forge
    mode: direct
    forge:
      event: pull_request
      actions: [opened, reopened]
  - kind: command
    mode: board
    args_var: feature_prompt
    command:
      name: featurly
      aliases: [feature-dev]
      scope: any
      min_replier_role: maintainer
    context_vars:
      post_to_board: "false"
  - kind: schedule
    mode: board
    schedule:
      suggested_cron: "0 2 * * 1"
      default_vars:
        depth: deep
  - kind: board
`
	m, err := LoadManifest(writeManifestForTest(t, body))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := len(m.Invocations); got != 4 {
		t.Fatalf("invocations: want 4, got %d", got)
	}

	fi := m.Invocations[0]
	if fi.Kind != InvocationKindForge || fi.Forge == nil || fi.Forge.Event != ForgeEventPullRequest {
		t.Errorf("forge invocation: %+v", fi)
	}
	if mode, err := fi.EffectiveMode(); err != nil || mode != ExecutionDirect {
		t.Errorf("forge mode: want direct, got %q (%v)", mode, err)
	}

	ci := m.Invocations[1]
	if ci.Kind != InvocationKindCommand || ci.Command == nil {
		t.Fatalf("command invocation: %+v", ci)
	}
	if ci.Command.Name != "featurly" || len(ci.Command.Aliases) != 1 || ci.Command.Aliases[0] != "feature-dev" {
		t.Errorf("command name/aliases: %+v", ci.Command)
	}
	if ci.Command.Scope != "any" || ci.Command.MinReplierRole != "maintainer" {
		t.Errorf("command scope/role: %+v", ci.Command)
	}
	if ci.ArgsVar != "feature_prompt" || ci.ContextVars["post_to_board"] != "false" {
		t.Errorf("command args/context: args_var=%q context=%v", ci.ArgsVar, ci.ContextVars)
	}
	if mode, err := ci.EffectiveMode(); err != nil || mode != ExecutionBoard {
		t.Errorf("command mode: want board, got %q (%v)", mode, err)
	}

	si := m.Invocations[2]
	if si.Kind != InvocationKindSchedule || si.Schedule == nil || si.Schedule.SuggestedCron != "0 2 * * 1" {
		t.Errorf("schedule invocation: %+v", si)
	}

	if m.Invocations[3].Kind != InvocationKindBoard {
		t.Errorf("board invocation: %+v", m.Invocations[3])
	}
}

func TestLoadManifest_RejectsInvocationErrors(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{
			name: "two schedules one cron",
			body: "name: b\nschema_version: 1\ninvocations:\n  - kind: schedule\n    schedule:\n      suggested_cron: \"0 2 * * 1\"\n  - kind: schedule\n    schedule:\n      suggested_cron: \"0 2 * * 1\"\n",
			want: "derives the same subscription",
		},
		{
			name: "two connectors one trigger",
			body: "name: b\nschema_version: 1\ninvocations:\n  - kind: connector\n    connector:\n      trigger: slack.message.posted\n  - kind: connector\n    connector:\n      trigger: slack.message.posted\n",
			want: "derives the same subscription",
		},
		{
			name: "unknown kind",
			body: "name: b\nschema_version: 1\ninvocations:\n  - kind: webhook\n",
			want: `unknown kind "webhook"`,
		},
		{
			name: "invalid mode",
			body: "name: b\nschema_version: 1\ninvocations:\n  - kind: board\n    mode: async\n",
			want: `invalid mode "async"`,
		},
		{
			name: "forge missing block",
			body: "name: b\nschema_version: 1\ninvocations:\n  - kind: forge\n",
			want: "kind=forge requires a forge: block",
		},
		{
			name: "forge unknown event",
			body: "name: b\nschema_version: 1\ninvocations:\n  - kind: forge\n    forge:\n      event: push\n",
			want: `unknown event "push"`,
		},
		{
			name: "command missing block",
			body: "name: b\nschema_version: 1\ninvocations:\n  - kind: command\n",
			want: "kind=command requires a command: block",
		},
		{
			name: "command bad name",
			body: "name: b\nschema_version: 1\ninvocations:\n  - kind: command\n    command:\n      name: Featurly!\n",
			want: `invalid name "Featurly!"`,
		},
		{
			name: "command bad scope",
			body: "name: b\nschema_version: 1\ninvocations:\n  - kind: command\n    command:\n      name: x\n      scope: everywhere\n",
			want: `invalid scope "everywhere"`,
		},
		{
			name: "command bad disambiguator",
			body: "name: b\nschema_version: 1\ninvocations:\n  - kind: command\n    command:\n      name: x\n      disambiguator: maybe\n",
			want: `invalid disambiguator "maybe"`,
		},
		{
			name: "duplicate command name intra-bot",
			body: "name: b\nschema_version: 1\ninvocations:\n  - kind: command\n    command:\n      name: dup\n  - kind: command\n    command:\n      name: dup\n",
			want: `duplicate command name "dup"`,
		},
		{
			name: "wrong payload on kind",
			body: "name: b\nschema_version: 1\ninvocations:\n  - kind: forge\n    forge:\n      event: pull_request\n    command:\n      name: x\n",
			want: "kind=forge must not set a command:/schedule:/keepalive: block",
		},
		{
			name: "keepalive missing block",
			body: "name: b\nschema_version: 1\ninvocations:\n  - kind: keepalive\n",
			want: "kind=keepalive requires a keepalive: block",
		},
		{
			name: "keepalive missing interval",
			body: "name: b\nschema_version: 1\ninvocations:\n  - kind: keepalive\n    keepalive:\n      stale_after: 10m\n",
			want: "interval is required",
		},
		{
			name: "keepalive interval below floor",
			body: "name: b\nschema_version: 1\ninvocations:\n  - kind: keepalive\n    keepalive:\n      interval: 1s\n",
			want: "below the 5s floor",
		},
		{
			name: "keepalive bad interval",
			body: "name: b\nschema_version: 1\ninvocations:\n  - kind: keepalive\n    keepalive:\n      interval: nope\n",
			want: "invalid interval",
		},
		{
			name: "keepalive with disallowed payload",
			body: "name: b\nschema_version: 1\ninvocations:\n  - kind: keepalive\n    keepalive:\n      interval: 30s\n    board: {}\n",
			want: "kind=keepalive must not set a forge:/command:/schedule:/board: block",
		},
		{
			name: "schedule bad cron",
			body: "name: b\nschema_version: 1\ninvocations:\n  - kind: schedule\n    schedule:\n      suggested_cron: \"0 2 *\"\n",
			want: "must be a 5-field cron expression",
		},
		{
			name: "board with disallowed payload",
			body: "name: b\nschema_version: 1\ninvocations:\n  - kind: board\n    command:\n      name: x\n",
			want: "kind=board takes only an optional board: block",
		},
		{
			name: "board unknown on kind",
			body: "name: b\nschema_version: 1\ninvocations:\n  - kind: board\n    board:\n      on: [card.exploded]\n",
			want: "unknown on \"card.exploded\"",
		},
		{
			name: "board consume_labels without all_labels",
			body: "name: b\nschema_version: 1\ninvocations:\n  - kind: board\n    mode: direct\n    board:\n      on: [card.created]\n      consume_labels: true\n",
			want: "consume_labels requires a non-empty all_labels",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadManifest(writeManifestForTest(t, tc.body))
			errContains(t, err, tc.want)
		})
	}
}

func TestSyntheticInvocations_FromForgeBlock(t *testing.T) {
	m := &Manifest{Forge: &ForgeRequirements{Events: []string{ForgeEventPullRequest, ForgeEventPullRequestComment}}}
	got := SyntheticInvocations(m)
	if len(got) != 2 {
		t.Fatalf("want 2 synthetic invocations, got %d (%+v)", len(got), got)
	}
	if got[0].Kind != InvocationKindForge || got[0].Forge.Event != ForgeEventPullRequest {
		t.Errorf("first synthetic: %+v", got[0])
	}
	if len(got[0].Forge.Actions) != 2 {
		t.Errorf("pull_request should carry open/reopen actions, got %v", got[0].Forge.Actions)
	}
	// No command is ever synthesised from a forge block.
	for _, inv := range got {
		if inv.Kind == InvocationKindCommand {
			t.Errorf("unexpected synthesised command: %+v", inv)
		}
	}

	if SyntheticInvocations(&Manifest{}) != nil {
		t.Error("no forge block should yield nil synthetic invocations")
	}
}

func TestEffectiveInvocations_ExplicitWins(t *testing.T) {
	explicit := []Invocation{{Kind: InvocationKindBoard}}
	m := &Manifest{
		Invocations: explicit,
		Forge:       &ForgeRequirements{Events: []string{ForgeEventPullRequest}},
	}
	got := EffectiveInvocations(m)
	if len(got) != 1 || got[0].Kind != InvocationKindBoard {
		t.Errorf("explicit invocations should win over synthetic, got %+v", got)
	}

	// Falls back to synthetic when no explicit block.
	m2 := &Manifest{Forge: &ForgeRequirements{Events: []string{ForgeEventPullRequest}}}
	if got := EffectiveInvocations(m2); len(got) != 1 || got[0].Kind != InvocationKindForge {
		t.Errorf("want synthetic fallback, got %+v", got)
	}
}

// A kind=connector invocation carries its trigger, suggested filters and var
// map; each malformed shape is refused alone, and a connector: block on any
// other kind is refused.
func TestConnectorInvocationsAreValidated(t *testing.T) {
	ok := Invocation{Kind: InvocationKindConnector, Connector: &InvocationConnector{
		Trigger: "slack.message.posted",
		Filters: map[string][]string{"channel": {"C1"}},
		Vars:    map[string]string{"prompt": "event.text"},
	}}
	if err := validateInvocations([]Invocation{ok}); err != nil {
		t.Fatalf("a well-formed connector invocation = %v", err)
	}
	with := func(f func(*Invocation)) Invocation {
		inv := ok
		c := *ok.Connector
		inv.Connector = &c
		f(&inv)
		return inv
	}
	for name, inv := range map[string]Invocation{
		"no connector block":  {Kind: InvocationKindConnector},
		"another block too":   with(func(i *Invocation) { i.Schedule = &InvocationSchedule{} }),
		"an args_var":         with(func(i *Invocation) { i.ArgsVar = "prompt" }),
		"a bare trigger id":   with(func(i *Invocation) { i.Connector.Trigger = "slack" }),
		"a filter id":         with(func(i *Invocation) { i.Connector.Filters = map[string][]string{"Bad-Id": {"x"}} }),
		"an empty filter":     with(func(i *Invocation) { i.Connector.Filters = map[string][]string{"channel": nil} }),
		"a var name":          with(func(i *Invocation) { i.Connector.Vars = map[string]string{"1bad": "event.text"} }),
		"an empty expression": with(func(i *Invocation) { i.Connector.Vars = map[string]string{"prompt": " "} }),
		"a block on forge":    {Kind: InvocationKindForge, Forge: &InvocationForge{Event: ForgeEventPullRequest}, Connector: &InvocationConnector{Trigger: "slack.message.posted"}},
	} {
		if err := validateInvocations([]Invocation{inv}); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// deliver parses (every builder refuses it, typed); an unknown mode does not.
	deliver := with(func(i *Invocation) { i.Mode = ExecutionDeliver })
	if err := validateInvocations([]Invocation{deliver}); err != nil {
		t.Fatalf("deliver at parse = %v, want accepted", err)
	}
	if _, err := deliver.EffectiveMode(); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("deliver's effective mode = %v, want ErrUnsupported", err)
	}
	if err := validateInvocations([]Invocation{with(func(i *Invocation) { i.Mode = "twice" })}); err == nil {
		t.Fatal("an unknown mode parsed")
	}
}

// ResolveMode is the one reading of a mode: "" and direct launch, board
// materialises a card except on a connector invocation, and deliver or any
// other value is a typed refusal naming its site — never a launch.
func TestResolveMode(t *testing.T) {
	for _, tc := range []struct {
		kind InvocationKind
		mode ExecutionMode
		want ExecutionMode
	}{
		{InvocationKindCommand, "", ExecutionDirect},
		{InvocationKindCommand, ExecutionDirect, ExecutionDirect},
		{InvocationKindBoard, ExecutionBoard, ExecutionBoard},
		{InvocationKindConnector, "", ExecutionDirect},
	} {
		if got, err := ResolveMode(tc.kind, tc.mode, "test"); err != nil || got != tc.want {
			t.Errorf("ResolveMode(%s, %q) = %q, %v; want %q", tc.kind, tc.mode, got, err, tc.want)
		}
	}
	for _, tc := range []struct {
		kind InvocationKind
		mode ExecutionMode
	}{
		{InvocationKindConnector, ExecutionBoard},
		{InvocationKindCommand, ExecutionDeliver},
		{InvocationKindCommand, "launch"},
		{InvocationKindCommand, "Direct"},
	} {
		got, err := ResolveMode(tc.kind, tc.mode, "the-site")
		if !errors.Is(err, ErrUnsupported) || got != "" {
			t.Errorf("ResolveMode(%s, %q) = %q, %v; want a typed refusal", tc.kind, tc.mode, got, err)
			continue
		}
		if !strings.Contains(err.Error(), "the-site") {
			t.Errorf("refusal %q does not name its site", err)
		}
	}
}

// The matcher reads the board lists insensible to their order (any-of on
// kinds and states, all-of on labels): a cosmetic reorder of the manifest
// must not change the invocation's identity, and a real change must.
func TestInvocationKeyIgnoresAListReorder(t *testing.T) {
	a := Invocation{Kind: InvocationKindBoard, Board: &InvocationBoard{
		On: []string{"card.moved", "card.created"}, ToStates: []string{"ready", "done"}, AllLabels: []string{"triage", "urgent"}}}
	b := Invocation{Kind: InvocationKindBoard, Board: &InvocationBoard{
		On: []string{"card.created", "card.moved"}, ToStates: []string{"done", "ready"}, AllLabels: []string{"urgent", "triage"}}}
	if InvocationKey([]Invocation{a}, 0) != InvocationKey([]Invocation{b}, 0) {
		t.Fatal("a cosmetic reorder of the board lists changed the invocation's identity")
	}
	b.Board.On = []string{"card.created"}
	if InvocationKey([]Invocation{a}, 0) == InvocationKey([]Invocation{b}, 0) {
		t.Fatal("a changed board kept the identity")
	}
}

// The digest reads a copy: the manifest's board block is never reordered in
// place — manifests are shared between readers.
func TestInvocationKeyDoesNotMutateItsInput(t *testing.T) {
	b := &InvocationBoard{On: []string{"b", "a"}, ToStates: []string{"y", "x"}, AllLabels: []string{"l2", "l1"}}
	in := []Invocation{{Kind: InvocationKindBoard, Board: b}}
	_ = InvocationKey(in, 0)
	if b.On[0] != "b" || b.ToStates[0] != "y" || b.AllLabels[0] != "l2" {
		t.Fatalf("the digest reordered its input in place: %+v", b)
	}
}
