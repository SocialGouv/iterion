package config

import (
	"context"
	"testing"

	"github.com/ethpandaops/codex-agent-sdk-go/internal/permission"
	"github.com/stretchr/testify/require"
)

func TestConfigureToolPermissionPolicy_CallbackConflict(t *testing.T) {
	t.Parallel()

	opts := &Options{
		CanUseTool: func(
			_ context.Context,
			_ string,
			_ map[string]any,
			_ *permission.Context,
		) (permission.Result, error) {
			return &permission.ResultAllow{Behavior: "allow"}, nil
		},
		PermissionPromptToolName: "stdio",
	}

	err := ConfigureToolPermissionPolicy(opts)
	require.Error(t, err)
}

func TestConfigureToolPermissionPolicy_AutoWrapsAllowedDisallowedTools(t *testing.T) {
	t.Parallel()

	opts := &Options{
		AllowedTools:    []string{"Read", "Write"},
		DisallowedTools: []string{"Bash"},
	}

	err := ConfigureToolPermissionPolicy(opts)
	require.NoError(t, err)
	require.NotNil(t, opts.CanUseTool)
	require.Equal(t, "stdio", opts.PermissionPromptToolName)

	ctx := context.Background()

	allowResult, err := opts.CanUseTool(ctx, "Read", nil, nil)
	require.NoError(t, err)
	require.Equal(t, "allow", allowResult.GetBehavior())

	denyResult, err := opts.CanUseTool(ctx, "Bash", nil, nil)
	require.NoError(t, err)
	require.Equal(t, "deny", denyResult.GetBehavior())

	denyOtherResult, err := opts.CanUseTool(ctx, "Glob", nil, nil)
	require.NoError(t, err)
	require.Equal(t, "deny", denyOtherResult.GetBehavior())
}

func TestConfigureToolPermissionPolicy_CombinesWithExistingCallback(t *testing.T) {
	t.Parallel()

	called := false
	opts := &Options{
		AllowedTools: []string{"Read"},
		CanUseTool: func(
			_ context.Context,
			toolName string,
			_ map[string]any,
			_ *permission.Context,
		) (permission.Result, error) {
			called = true

			require.Equal(t, "Read", toolName)

			return &permission.ResultAllow{Behavior: "allow"}, nil
		},
	}

	err := ConfigureToolPermissionPolicy(opts)
	require.NoError(t, err)
	require.NotNil(t, opts.CanUseTool)

	_, err = opts.CanUseTool(context.Background(), "Read", nil, nil)
	require.NoError(t, err)
	require.True(t, called)

	called = false

	result, err := opts.CanUseTool(context.Background(), "Bash", nil, nil)
	require.NoError(t, err)
	require.Equal(t, "deny", result.GetBehavior())
	require.False(t, called)
}

func TestConfigureToolPermissionPolicy_IncludesToolsListInAllowSet(t *testing.T) {
	t.Parallel()

	opts := &Options{
		Tools: ToolsList{"Read"},
	}

	err := ConfigureToolPermissionPolicy(opts)
	require.NoError(t, err)
	require.NotNil(t, opts.CanUseTool)

	allowed, err := opts.CanUseTool(context.Background(), "Read", nil, nil)
	require.NoError(t, err)
	require.Equal(t, "allow", allowed.GetBehavior())

	denied, err := opts.CanUseTool(context.Background(), "Write", nil, nil)
	require.NoError(t, err)
	require.Equal(t, "deny", denied.GetBehavior())
}

func TestConfigureToolPermissionPolicy_TreatsEditAndWriteAsEquivalent(t *testing.T) {
	t.Parallel()

	opts := &Options{
		AllowedTools: []string{"Write"},
	}

	err := ConfigureToolPermissionPolicy(opts)
	require.NoError(t, err)
	require.NotNil(t, opts.CanUseTool)

	allowed, err := opts.CanUseTool(context.Background(), "Edit", nil, nil)
	require.NoError(t, err)
	require.Equal(t, "allow", allowed.GetBehavior())

	opts = &Options{
		DisallowedTools: []string{"Edit"},
	}

	err = ConfigureToolPermissionPolicy(opts)
	require.NoError(t, err)
	require.NotNil(t, opts.CanUseTool)

	denied, err := opts.CanUseTool(context.Background(), "Write", nil, nil)
	require.NoError(t, err)
	require.Equal(t, "deny", denied.GetBehavior())
}

func TestConfigureToolPermissionPolicy_PermissionsBypassesToolFilter(t *testing.T) {
	t.Parallel()

	var called bool

	opts := &Options{
		AllowedTools: []string{"Write"},
		CanUseTool: func(
			_ context.Context,
			toolName string,
			_ map[string]any,
			_ *permission.Context,
		) (permission.Result, error) {
			called = true

			require.Equal(t, "Permissions", toolName)

			return &permission.ResultAllow{Behavior: "allow"}, nil
		},
	}

	err := ConfigureToolPermissionPolicy(opts)
	require.NoError(t, err)
	require.NotNil(t, opts.CanUseTool)

	result, err := opts.CanUseTool(context.Background(), "Permissions", nil, nil)
	require.NoError(t, err)
	require.Equal(t, "allow", result.GetBehavior())
	require.True(t, called)
}
