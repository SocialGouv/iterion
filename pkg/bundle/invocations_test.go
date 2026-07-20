package bundle

import "testing"

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
	if fi.EffectiveMode() != ExecutionDirect {
		t.Errorf("forge mode: want direct, got %q", fi.EffectiveMode())
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
	if ci.EffectiveMode() != ExecutionBoard {
		t.Errorf("command mode: want board, got %q", ci.EffectiveMode())
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
