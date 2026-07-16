package permission

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestUpdateToDict_Minimal(t *testing.T) {
	t.Parallel()

	update := &Update{
		Type: UpdateTypeSetMode,
	}

	require.Equal(t, map[string]any{
		"type": string(UpdateTypeSetMode),
	}, update.ToDict())
}

func TestUpdateToDict_Full(t *testing.T) {
	t.Parallel()

	ruleContent := "cwd=/tmp"
	behavior := BehaviorAsk
	mode := ModeBypassPermissions
	dest := UpdateDestProjectSettings
	update := &Update{
		Type: UpdateTypeAddRules,
		Rules: []*RuleValue{
			{ToolName: "Bash", RuleContent: &ruleContent},
			{ToolName: "Read"},
		},
		Behavior:    &behavior,
		Mode:        &mode,
		Directories: []string{"/tmp", "/home/savid"},
		Destination: &dest,
	}

	got := update.ToDict()
	require.Equal(t, string(UpdateTypeAddRules), got["type"])
	require.Equal(t, string(UpdateDestProjectSettings), got["destination"])
	require.Equal(t, string(BehaviorAsk), got["behavior"])
	require.Equal(t, string(ModeBypassPermissions), got["mode"])
	require.Equal(t, []string{"/tmp", "/home/savid"}, got["directories"])

	rules, ok := got["rules"].([]map[string]any)
	require.True(t, ok)
	require.Len(t, rules, 2)
	require.Equal(t, map[string]any{
		"toolName":    "Bash",
		"ruleContent": "cwd=/tmp",
	}, rules[0])
	require.Equal(t, map[string]any{
		"toolName": "Read",
	}, rules[1])
}

func TestResultBehaviors(t *testing.T) {
	t.Parallel()

	allow := &ResultAllow{}
	deny := &ResultDeny{}

	require.Equal(t, "allow", allow.GetBehavior())
	require.Equal(t, "deny", deny.GetBehavior())
}
