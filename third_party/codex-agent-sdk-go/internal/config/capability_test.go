package config

import (
	"reflect"
	"testing"

	"github.com/ethpandaops/codex-agent-sdk-go/internal/hook"
	"github.com/ethpandaops/codex-agent-sdk-go/internal/mcp"
	"github.com/stretchr/testify/require"
)

func TestOptionCapabilities_CoversAllOptionsFields(t *testing.T) {
	t.Parallel()

	optsType := reflect.TypeFor[Options]()
	caps := OptionCapabilities()

	capByField := make(map[string]OptionCapability, len(caps))
	for _, capability := range caps {
		if _, exists := capByField[capability.Field]; exists {
			t.Fatalf("duplicate capability entry for field %q", capability.Field)
		}

		capByField[capability.Field] = capability
	}

	require.Equal(t, optsType.NumField(), len(caps), "capability list must classify every options field")

	for field := range optsType.Fields() {
		capability, ok := capByField[field.Name]
		require.True(t, ok, "missing capability entry for field %q", field.Name)
		require.NotEmpty(t, capability.OptionName, "missing option constructor name for field %q", field.Name)
		require.NotEmpty(t, capability.Exec, "missing exec support level for field %q", field.Name)
		require.NotEmpty(t, capability.AppServer, "missing app-server support level for field %q", field.Name)
	}
}

func TestSelectQueryBackend(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		options *Options
		want    QueryBackend
	}{
		{name: "default options", options: &Options{}, want: QueryBackendExec},
		{name: "exec-compatible option keeps exec", options: &Options{Model: "gpt-5.4"}, want: QueryBackendExec},
		{name: "system prompt requires app-server", options: &Options{SystemPrompt: "be helpful"}, want: QueryBackendAppServer},
		{name: "resume requires app-server", options: &Options{Resume: "thread_1"}, want: QueryBackendAppServer},
		{
			name: "hooks require app-server",
			options: &Options{
				Hooks: map[hook.Event][]*hook.Matcher{
					hook.EventPreToolUse: {},
				},
			},
			want: QueryBackendAppServer,
		},
		{
			name: "SDK tools require app-server",
			options: &Options{
				SDKTools: []*DynamicTool{
					{Name: "add", Description: "Add numbers"},
				},
			},
			want: QueryBackendAppServer,
		},
		{
			name: "SDK MCP server requires app-server",
			options: &Options{
				MCPServers: map[string]mcp.ServerConfig{
					"sdk": &mcp.SdkServerConfig{Type: mcp.ServerTypeSDK, Name: "sdk"},
				},
			},
			want: QueryBackendAppServer,
		},
		{
			name: "non-SDK MCP server keeps exec",
			options: &Options{
				MCPServers: map[string]mcp.ServerConfig{
					"stdio": &mcp.StdioServerConfig{Command: "echo"},
				},
			},
			want: QueryBackendExec,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := SelectQueryBackend(tt.options)
			require.Equal(t, tt.want, got)
		})
	}
}

func TestValidateOptionsForBackend(t *testing.T) {
	t.Parallel()

	t.Run("supported app-server options pass", func(t *testing.T) {
		t.Parallel()

		opts := &Options{
			Model:        "gpt-5.4",
			SystemPrompt: "be helpful",
			Resume:       "thread_1",
			ForkSession:  true,
			Config:       map[string]string{"model": "gpt-5.4"},
		}

		err := ValidateOptionsForBackend(opts, QueryBackendAppServer)
		require.NoError(t, err)
	})

	t.Run("unsupported options fail fast", func(t *testing.T) {
		t.Parallel()

		opts := &Options{
			AddDirs: []string{"/tmp"},
		}

		err := ValidateOptionsForBackend(opts, QueryBackendAppServer)
		require.Error(t, err)
		require.ErrorContains(t, err, "AddDirs")
	})

	t.Run("continue conversation requires resume", func(t *testing.T) {
		t.Parallel()

		opts := &Options{
			ContinueConversation: true,
		}

		err := ValidateOptionsForBackend(opts, QueryBackendAppServer)
		require.Error(t, err)
		require.ErrorContains(t, err, "requires WithResume")
	})

	t.Run("permission prompt tool only allows stdio", func(t *testing.T) {
		t.Parallel()

		opts := &Options{
			PermissionPromptToolName: "custom",
		}

		err := ValidateOptionsForBackend(opts, QueryBackendAppServer)
		require.Error(t, err)
		require.ErrorContains(t, err, "only supports value \"stdio\"")
	})

	t.Run("exec backend rejects SDK MCP servers", func(t *testing.T) {
		t.Parallel()

		opts := &Options{
			MCPServers: map[string]mcp.ServerConfig{
				"sdk": &mcp.SdkServerConfig{Type: mcp.ServerTypeSDK, Name: "sdk"},
			},
		}

		err := ValidateOptionsForBackend(opts, QueryBackendExec)
		require.Error(t, err)
		require.ErrorContains(t, err, "MCPServers")
	})
}
