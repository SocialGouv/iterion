package cli

import (
	"context"
	"log/slog"
	"os"
	"slices"
	"testing"

	"github.com/ethpandaops/codex-agent-sdk-go/internal/config"
	"github.com/ethpandaops/codex-agent-sdk-go/internal/errors"
	"github.com/ethpandaops/codex-agent-sdk-go/internal/mcp"
	"github.com/ethpandaops/codex-agent-sdk-go/internal/version"
	"github.com/stretchr/testify/require"
)

const flagImage = "-i"

// TestDiscoverer_NotFound tests that an invalid CLI path returns CLINotFoundError.
func TestDiscoverer_NotFound(t *testing.T) {
	discoverer := NewDiscoverer(&Config{
		CliPath:          "/nonexistent/path/to/codex",
		SkipVersionCheck: true,
		Logger:           slog.Default(),
	})

	_, err := discoverer.Discover(context.Background())

	require.Error(t, err)
	require.IsType(t, &errors.CLINotFoundError{}, err)
}

// TestDiscoverer_ExplicitPath tests discovery with an explicit path.
func TestDiscoverer_ExplicitPath(t *testing.T) {
	// Create a temp file to act as the CLI
	tmpDir := t.TempDir()
	fakeCLI := tmpDir + "/codex"

	// Create the fake CLI file
	err := os.WriteFile(fakeCLI, []byte("#!/bin/sh\necho 2.1.0"), 0o755)
	require.NoError(t, err)

	discoverer := NewDiscoverer(&Config{
		CliPath:          fakeCLI,
		SkipVersionCheck: true,
		Logger:           slog.Default(),
	})

	path, err := discoverer.Discover(context.Background())

	require.NoError(t, err)
	require.Equal(t, fakeCLI, path)
}

// TestBuildExecArgs_Basic tests basic command building with minimal options.
func TestBuildExecArgs_Basic(t *testing.T) {
	options := &config.Options{}
	args := BuildExecArgs("hello", options)

	require.Contains(t, args, "exec")
	require.Contains(t, args, "--json")
	require.Contains(t, args, "--full-auto")
	require.Contains(t, args, "--ephemeral")
	require.Contains(t, args, "--skip-git-repo-check")
	require.Contains(t, args, "hello")
	// Prompt should be last argument
	require.Equal(t, "hello", args[len(args)-1])
}

// TestBuildExecArgs_WithModel tests command building with model option.
func TestBuildExecArgs_WithModel(t *testing.T) {
	options := &config.Options{
		Model: "gpt-5.4",
	}

	args := BuildExecArgs("test", options)

	require.Contains(t, args, "-m")
	require.Contains(t, args, "gpt-5.4")
}

// TestBuildExecArgs_WithoutModel tests that no -m flag appears when model is empty.
func TestBuildExecArgs_WithoutModel(t *testing.T) {
	options := &config.Options{}
	args := BuildExecArgs("test", options)

	require.NotContains(t, args, "-m")
}

// TestBuildExecArgs_WithMaxTurns tests command building with max turns option.
func TestBuildExecArgs_WithMaxTurns(t *testing.T) {
	options := &config.Options{
		MaxTurns: 5,
	}

	args := BuildExecArgs("test", options)

	require.Contains(t, args, "--max-turns")
	require.Contains(t, args, "5")
}

// TestBuildExecArgs_WithoutMaxTurns tests that no --max-turns flag appears when max turns is zero.
func TestBuildExecArgs_WithoutMaxTurns(t *testing.T) {
	options := &config.Options{}
	args := BuildExecArgs("test", options)

	require.NotContains(t, args, "--max-turns")
}

// TestBuildExecArgs_WithSandboxDirect tests direct sandbox mode option.
func TestBuildExecArgs_WithSandboxDirect(t *testing.T) {
	options := &config.Options{
		Sandbox: "workspace-write",
	}

	args := BuildExecArgs("test", options)

	require.Contains(t, args, "-s")
	require.Contains(t, args, "workspace-write")
}

// TestBuildExecArgs_WithSandboxReadOnly tests read-only sandbox mode.
func TestBuildExecArgs_WithSandboxReadOnly(t *testing.T) {
	options := &config.Options{
		Sandbox: "read-only",
	}

	args := BuildExecArgs("test", options)

	require.Contains(t, args, "-s")
	require.Contains(t, args, "read-only")
}

// TestBuildExecArgs_WithSandboxDangerFullAccess tests danger-full-access sandbox mode.
func TestBuildExecArgs_WithSandboxDangerFullAccess(t *testing.T) {
	options := &config.Options{
		Sandbox: "danger-full-access",
	}

	args := BuildExecArgs("test", options)

	require.Contains(t, args, "-s")
	require.Contains(t, args, "danger-full-access")
}

// TestBuildExecArgs_WithPermissionModeMapping tests that PermissionMode maps to sandbox.
func TestBuildExecArgs_WithPermissionModeMapping(t *testing.T) {
	tests := []struct {
		name           string
		permissionMode string
		wantSandbox    string
		wantFlag       bool
	}{
		{
			name:           "acceptEdits maps to workspace-write",
			permissionMode: "acceptEdits",
			wantSandbox:    "workspace-write",
			wantFlag:       true,
		},
		{
			name:           "bypassPermissions maps to danger-full-access",
			permissionMode: "bypassPermissions",
			wantSandbox:    "danger-full-access",
			wantFlag:       true,
		},
		{
			name:           "default maps to empty",
			permissionMode: "default",
			wantFlag:       false,
		},
		{
			name:           "empty maps to empty",
			permissionMode: "",
			wantFlag:       false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			options := &config.Options{
				PermissionMode: tt.permissionMode,
			}

			args := BuildExecArgs("test", options)

			if tt.wantFlag {
				require.Contains(t, args, "-s")
				require.Contains(t, args, tt.wantSandbox)
			} else {
				require.NotContains(t, args, "-s")
			}
		})
	}
}

// TestBuildExecArgs_SandboxOverridesPermissionMode tests that Sandbox field takes precedence.
func TestBuildExecArgs_SandboxOverridesPermissionMode(t *testing.T) {
	options := &config.Options{
		Sandbox:        "read-only",
		PermissionMode: "bypassPermissions",
	}

	args := BuildExecArgs("test", options)

	require.Contains(t, args, "-s")
	require.Contains(t, args, "read-only")
	require.NotContains(t, args, "danger-full-access")
}

// TestBuildExecArgs_WithCwd tests working directory option.
func TestBuildExecArgs_WithCwd(t *testing.T) {
	options := &config.Options{
		Cwd: "/home/user/project",
	}

	args := BuildExecArgs("test", options)

	require.Contains(t, args, "-C")
	require.Contains(t, args, "/home/user/project")
}

// TestBuildExecArgs_DefaultCwd tests that CWD defaults to os.Getwd when empty.
func TestBuildExecArgs_DefaultCwd(t *testing.T) {
	options := &config.Options{}
	args := BuildExecArgs("test", options)

	// When Cwd is empty, it defaults to os.Getwd()
	cwd, err := os.Getwd()
	require.NoError(t, err)

	require.Contains(t, args, "-C")
	require.Contains(t, args, cwd)
}

// TestBuildExecArgs_WithAddDirs tests additional writable directories.
func TestBuildExecArgs_WithAddDirs(t *testing.T) {
	options := &config.Options{
		AddDirs: []string{"/tmp/one", "/tmp/two"},
	}

	args := BuildExecArgs("test", options)

	addDirCount := 0

	for _, arg := range args {
		if arg == "--add-dir" {
			addDirCount++
		}
	}

	require.Equal(t, 2, addDirCount)
	require.Contains(t, args, "/tmp/one")
	require.Contains(t, args, "/tmp/two")
}

// TestBuildExecArgs_WithImages tests image inputs.
func TestBuildExecArgs_WithImages(t *testing.T) {
	options := &config.Options{
		Images: []string{"/path/to/img1.png", "/path/to/img2.jpg"},
	}

	args := BuildExecArgs("test", options)

	imageCount := 0

	for _, arg := range args {
		if arg == flagImage {
			imageCount++
		}
	}

	require.Equal(t, 2, imageCount)
	require.Contains(t, args, "/path/to/img1.png")
	require.Contains(t, args, "/path/to/img2.jpg")
}

// TestBuildExecArgs_WithSingleImage tests a single image input.
func TestBuildExecArgs_WithSingleImage(t *testing.T) {
	options := &config.Options{
		Images: []string{"/path/to/screenshot.png"},
	}

	args := BuildExecArgs("test", options)

	imageCount := 0

	for _, arg := range args {
		if arg == flagImage {
			imageCount++
		}
	}

	require.Equal(t, 1, imageCount)
	require.Contains(t, args, "/path/to/screenshot.png")
}

// TestBuildExecArgs_WithNoImages tests that no -i flag appears when images is empty.
func TestBuildExecArgs_WithNoImages(t *testing.T) {
	options := &config.Options{}
	args := BuildExecArgs("test", options)

	require.NotContains(t, args, flagImage)
}

// TestBuildExecArgs_WithConfig tests key-value config options.
func TestBuildExecArgs_WithConfig(t *testing.T) {
	options := &config.Options{
		Config: map[string]string{
			"key1": "value1",
			"key2": "value2",
		},
	}

	args := BuildExecArgs("test", options)

	configCount := 0

	for _, arg := range args {
		if arg == "-c" {
			configCount++
		}
	}

	require.Equal(t, 2, configCount)
	// Config keys are sorted for deterministic ordering
	require.Contains(t, args, "key1=value1")
	require.Contains(t, args, "key2=value2")
}

// TestBuildExecArgs_WithConfigSorted tests that config keys are sorted alphabetically.
func TestBuildExecArgs_WithConfigSorted(t *testing.T) {
	options := &config.Options{
		Config: map[string]string{
			"zebra":  "z",
			"alpha":  "a",
			"middle": "m",
		},
	}

	args := BuildExecArgs("test", options)

	// Find positions of config values
	alphaIdx := slices.Index(args, "alpha=a")
	middleIdx := slices.Index(args, "middle=m")
	zebraIdx := slices.Index(args, "zebra=z")

	require.NotEqual(t, -1, alphaIdx)
	require.NotEqual(t, -1, middleIdx)
	require.NotEqual(t, -1, zebraIdx)

	// Keys should be in alphabetical order
	require.Less(t, alphaIdx, middleIdx)
	require.Less(t, middleIdx, zebraIdx)
}

// TestBuildExecArgs_WithSingleConfig tests a single config entry.
func TestBuildExecArgs_WithSingleConfig(t *testing.T) {
	options := &config.Options{
		Config: map[string]string{
			"only-key": "only-value",
		},
	}

	args := BuildExecArgs("test", options)

	configCount := 0

	for _, arg := range args {
		if arg == "-c" {
			configCount++
		}
	}

	require.Equal(t, 1, configCount)
	require.Contains(t, args, "only-key=only-value")
}

// TestBuildExecArgs_WithNoConfig tests that no -c flag appears when config is empty.
func TestBuildExecArgs_WithNoConfig(t *testing.T) {
	options := &config.Options{}
	args := BuildExecArgs("test", options)

	require.NotContains(t, args, "-c")
}

// TestBuildExecArgs_WithOutputSchema tests output schema option.
func TestBuildExecArgs_WithOutputSchema(t *testing.T) {
	options := &config.Options{
		OutputSchema: `{"type":"object","properties":{"name":{"type":"string"}}}`,
	}

	args := BuildExecArgs("test", options)

	require.Contains(t, args, "--output-schema")

	idx := slices.Index(args, "--output-schema")
	require.NotEqual(t, -1, idx)
	require.Less(t, idx+1, len(args))
	require.Equal(t, `{"type":"object","properties":{"name":{"type":"string"}}}`, args[idx+1])
}

// TestBuildExecArgs_WithoutOutputSchema tests that no --output-schema flag appears when empty.
func TestBuildExecArgs_WithoutOutputSchema(t *testing.T) {
	options := &config.Options{}
	args := BuildExecArgs("test", options)

	require.NotContains(t, args, "--output-schema")
}

// TestBuildExecArgs_WithExtraArgs tests arbitrary CLI flag passing.
func TestBuildExecArgs_WithExtraArgs(t *testing.T) {
	t.Run("boolean flag without value", func(t *testing.T) {
		options := &config.Options{
			ExtraArgs: map[string]*string{
				"debug-to-stderr": nil,
			},
		}

		args := BuildExecArgs("test", options)

		require.Contains(t, args, "--debug-to-stderr")
	})

	t.Run("flag with value", func(t *testing.T) {
		value := "custom-value"
		options := &config.Options{
			ExtraArgs: map[string]*string{
				"custom-flag": &value,
			},
		}

		args := BuildExecArgs("test", options)

		require.Contains(t, args, "--custom-flag")
		require.Contains(t, args, "custom-value")
	})

	t.Run("multiple extra args", func(t *testing.T) {
		valueA := "value-a"
		valueB := "value-b"
		options := &config.Options{
			ExtraArgs: map[string]*string{
				"flag-a":       &valueA,
				"flag-b":       &valueB,
				"boolean-flag": nil,
			},
		}

		args := BuildExecArgs("test", options)

		require.Contains(t, args, "--flag-a")
		require.Contains(t, args, "value-a")
		require.Contains(t, args, "--flag-b")
		require.Contains(t, args, "value-b")
		require.Contains(t, args, "--boolean-flag")
	})

	t.Run("no extra args produces no extra flags", func(t *testing.T) {
		options := &config.Options{}
		args := BuildExecArgs("test", options)

		// Should only contain the standard flags, not any --extra ones
		for _, arg := range args {
			if arg == "--debug-to-stderr" || arg == "--custom-flag" {
				t.Fatalf("Unexpected extra arg: %s", arg)
			}
		}
	})
}

// TestBuildExecArgs_PromptIsLastArg tests that the prompt is always the last argument.
func TestBuildExecArgs_PromptIsLastArg(t *testing.T) {
	options := &config.Options{
		Model:   "o4-mini",
		Sandbox: "workspace-write",
		Cwd:     "/tmp",
	}

	args := BuildExecArgs("my prompt text", options)

	require.Equal(t, "my prompt text", args[len(args)-1])
}

// TestBuildExecArgs_PromptWithSpecialChars tests prompts containing special characters.
func TestBuildExecArgs_PromptWithSpecialChars(t *testing.T) {
	tests := []struct {
		name   string
		prompt string
	}{
		{name: "with quotes", prompt: `Say "hello world"`},
		{name: "with newlines", prompt: "Line 1\nLine 2"},
		{name: "with unicode", prompt: "Calculate \u03c0 to 5 decimal places"},
		{name: "with backticks", prompt: "Run `echo hello`"},
		{name: "empty prompt", prompt: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			options := &config.Options{}
			args := BuildExecArgs(tt.prompt, options)

			require.Equal(t, tt.prompt, args[len(args)-1])
		})
	}
}

// TestBuildExecArgs_AllOptionsCombined tests building args with many options together.
func TestBuildExecArgs_AllOptionsCombined(t *testing.T) {
	options := &config.Options{
		Model:        "o4-mini",
		Sandbox:      "workspace-write",
		Cwd:          "/home/user/project",
		Images:       []string{"/tmp/img1.png", "/tmp/img2.jpg"},
		Config:       map[string]string{"key1": "val1", "key2": "val2"},
		OutputSchema: `{"type":"object"}`,
		ExtraArgs:    map[string]*string{"debug": new("true"), "verbose": nil},
	}

	args := BuildExecArgs("do something", options)

	// Verify all options are present
	require.Contains(t, args, "exec")
	require.Contains(t, args, "-m")
	require.Contains(t, args, "o4-mini")
	require.Contains(t, args, "-s")
	require.Contains(t, args, "workspace-write")
	require.Contains(t, args, "-C")
	require.Contains(t, args, "/home/user/project")
	require.Contains(t, args, "/tmp/img1.png")
	require.Contains(t, args, "/tmp/img2.jpg")
	require.Contains(t, args, "key1=val1")
	require.Contains(t, args, "key2=val2")
	require.Contains(t, args, "--output-schema")
	require.Contains(t, args, `{"type":"object"}`)
	require.Contains(t, args, "--debug")
	require.Contains(t, args, "true")
	require.Contains(t, args, "--verbose")

	// Prompt is always last
	require.Equal(t, "do something", args[len(args)-1])
}

// TestBuildExecArgs_PermissionModeWithoutSandbox tests that permission mode alone
// correctly maps to sandbox without explicit Sandbox field.
func TestBuildExecArgs_PermissionModeWithoutSandbox(t *testing.T) {
	options := &config.Options{
		PermissionMode: "acceptEdits",
	}

	args := BuildExecArgs("test", options)

	require.Contains(t, args, "-s")
	require.Contains(t, args, "workspace-write")
}

// TestBuildExecArgs_NoSandboxNoPermission tests that no -s flag when both are empty.
func TestBuildExecArgs_NoSandboxNoPermission(t *testing.T) {
	options := &config.Options{}
	args := BuildExecArgs("test", options)

	require.NotContains(t, args, "-s")
}

// TestBuildExecArgs_UnknownPermissionMode tests that unknown permission modes produce no sandbox.
func TestBuildExecArgs_UnknownPermissionMode(t *testing.T) {
	options := &config.Options{
		PermissionMode: "unknownMode",
	}

	args := BuildExecArgs("test", options)

	require.NotContains(t, args, "-s")
}

// TestBuildAppServerArgs tests app-server command building.
func TestBuildAppServerArgs(t *testing.T) {
	options := &config.Options{}
	args := BuildAppServerArgs(options)

	require.Equal(t, []string{"app-server"}, args)
}

// TestBuildAppServerArgs_IgnoresNonMCPOptions tests that app-server ignores non-MCP options.
func TestBuildAppServerArgs_IgnoresNonMCPOptions(t *testing.T) {
	options := &config.Options{
		Model:   "o4-mini",
		Sandbox: "workspace-write",
		Cwd:     "/tmp",
	}

	args := BuildAppServerArgs(options)

	// Without MCP servers, app server args should be just ["app-server"]
	require.Equal(t, []string{"app-server"}, args)
}

// TestBuildAppServerArgs_WithMCPServers tests that MCP servers are serialized as -c flags.
func TestBuildAppServerArgs_WithMCPServers(t *testing.T) {
	options := &config.Options{
		MCPServers: map[string]mcp.ServerConfig{
			"my-api": &mcp.HTTPServerConfig{
				Type:    mcp.ServerTypeHTTP,
				URL:     "https://api.example.com/mcp",
				Headers: map[string]string{"Authorization": "Bearer tok123"},
			},
		},
	}

	args := BuildAppServerArgs(options)

	require.Equal(t, "app-server", args[0])
	require.Contains(t, args, "mcp_servers.my-api.type=http")
	require.Contains(t, args, "mcp_servers.my-api.url=https://api.example.com/mcp")
	require.Contains(t, args, "mcp_servers.my-api.http_headers.Authorization=Bearer tok123")
}

// TestBuildAppServerArgs_WithMCPServersSDKSkipped tests that SDK servers produce no extra flags.
func TestBuildAppServerArgs_WithMCPServersSDKSkipped(t *testing.T) {
	options := &config.Options{
		MCPServers: map[string]mcp.ServerConfig{
			"sdk-server": &mcp.SdkServerConfig{
				Type: mcp.ServerTypeSDK,
				Name: "in-process",
			},
		},
	}

	args := BuildAppServerArgs(options)

	require.Equal(t, []string{"app-server"}, args)
}

// TestBuildEnvironment_Basic tests basic environment variable setup.
func TestBuildEnvironment_Basic(t *testing.T) {
	options := &config.Options{}
	env := BuildEnvironment(options)

	require.NotNil(t, env)
	require.True(t, slices.Contains(env, "CODEX_CLI_SDK_VERSION=0.1.0"),
		"Expected CODEX_CLI_SDK_VERSION=0.1.0 in environment")
}

// TestBuildEnvironment_EnvVarsPassedToSubprocess tests environment variable handling.
func TestBuildEnvironment_EnvVarsPassedToSubprocess(t *testing.T) {
	options := &config.Options{
		Env: map[string]string{
			"CUSTOM_VAR": "custom_value",
		},
	}

	env := BuildEnvironment(options)
	require.NotNil(t, env)

	require.True(t, slices.Contains(env, "CUSTOM_VAR=custom_value"),
		"Expected CUSTOM_VAR=custom_value in environment")
}

// TestBuildEnvironment_MultipleVars tests multiple custom environment variables.
func TestBuildEnvironment_MultipleVars(t *testing.T) {
	options := &config.Options{
		Env: map[string]string{
			"API_KEY":   "secret-key",
			"DEBUG":     "true",
			"LOG_LEVEL": "verbose",
		},
	}

	env := BuildEnvironment(options)
	require.NotNil(t, env)

	require.True(t, slices.Contains(env, "API_KEY=secret-key"))
	require.True(t, slices.Contains(env, "DEBUG=true"))
	require.True(t, slices.Contains(env, "LOG_LEVEL=verbose"))
}

// TestBuildEnvironment_SDKVersionAlwaysPresent tests that SDK version is always in env.
func TestBuildEnvironment_SDKVersionAlwaysPresent(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
	}{
		{name: "no custom env", env: nil},
		{name: "empty custom env", env: map[string]string{}},
		{name: "with custom env", env: map[string]string{"FOO": "bar"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			options := &config.Options{Env: tt.env}
			env := BuildEnvironment(options)

			require.True(t, slices.Contains(env, "CODEX_CLI_SDK_VERSION=0.1.0"),
				"Expected CODEX_CLI_SDK_VERSION=0.1.0 in environment")
		})
	}
}

// TestBuildEnvironment_InheritsOSEnv tests that the OS environment is inherited.
func TestBuildEnvironment_InheritsOSEnv(t *testing.T) {
	options := &config.Options{}
	env := BuildEnvironment(options)

	// Should contain at least the OS environment plus the SDK version
	require.Greater(t, len(env), 1,
		"Expected environment to contain OS env vars plus SDK version")
}

// TestMapPermissionToSandbox tests the permission mode to sandbox mapping.
func TestMapPermissionToSandbox(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{name: "default", input: "default", expected: ""},
		{name: "empty", input: "", expected: ""},
		{name: "acceptEdits", input: "acceptEdits", expected: "workspace-write"},
		{name: "bypassPermissions", input: "bypassPermissions", expected: "danger-full-access"},
		{name: "unknown", input: "unknown", expected: ""},
		{name: "plan mode", input: "plan", expected: ""},
		{name: "random string", input: "randomString", expected: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := mapPermissionToSandbox(tt.input)
			require.Equal(t, tt.expected, result)
		})
	}
}

// TestCompareVersions tests semantic version comparison.
func TestCompareVersions(t *testing.T) {
	tests := []struct {
		name     string
		a        string
		b        string
		expected int
	}{
		// Equal versions
		{name: "equal versions", a: "1.0.0", b: "1.0.0", expected: 0},
		{name: "equal versions 2", a: "2.5.10", b: "2.5.10", expected: 0},

		// A < B (should return -1)
		{name: "major version less", a: "1.0.0", b: "2.0.0", expected: -1},
		{name: "minor version less", a: "1.0.0", b: "1.1.0", expected: -1},
		{name: "patch version less", a: "1.0.0", b: "1.0.1", expected: -1},
		{name: "complex less", a: "1.9.9", b: "2.0.0", expected: -1},
		{name: "minor rollover", a: "1.99.0", b: "2.0.0", expected: -1},

		// A > B (should return 1)
		{name: "major version greater", a: "2.0.0", b: "1.0.0", expected: 1},
		{name: "minor version greater", a: "1.1.0", b: "1.0.0", expected: 1},
		{name: "patch version greater", a: "1.0.1", b: "1.0.0", expected: 1},
		{name: "complex greater", a: "2.0.0", b: "1.9.9", expected: 1},

		// Minimum version check (0.100.0 is minimum for codex)
		{name: "below minimum", a: "0.99.0", b: "0.100.0", expected: -1},
		{name: "at minimum", a: "0.100.0", b: "0.100.0", expected: 0},
		{name: "above minimum", a: "0.101.0", b: "0.100.0", expected: 1},
		{name: "much above minimum", a: "1.0.0", b: "0.100.0", expected: 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := compareVersions(tt.a, tt.b)
			require.Equal(t, tt.expected, result, "compareVersions(%q, %q)", tt.a, tt.b)
		})
	}
}

// TestBuildExecArgs_ModelAndSandboxCombined tests model and sandbox together.
func TestBuildExecArgs_ModelAndSandboxCombined(t *testing.T) {
	options := &config.Options{
		Model:   "o4-mini",
		Sandbox: "workspace-write",
	}

	args := BuildExecArgs("test prompt", options)

	require.Contains(t, args, "-m")
	require.Contains(t, args, "o4-mini")
	require.Contains(t, args, "-s")
	require.Contains(t, args, "workspace-write")
	require.Equal(t, "test prompt", args[len(args)-1])
}

// TestBuildExecArgs_ImagesAndConfigCombined tests images and config together.
func TestBuildExecArgs_ImagesAndConfigCombined(t *testing.T) {
	options := &config.Options{
		Images: []string{"/path/to/image.png"},
		Config: map[string]string{"model": "o4-mini"},
	}

	args := BuildExecArgs("test", options)

	require.Contains(t, args, flagImage)
	require.Contains(t, args, "/path/to/image.png")
	require.Contains(t, args, "-c")
	require.Contains(t, args, "model=o4-mini")
}

// TestBuildExecArgs_ManyImages tests a larger number of image inputs.
func TestBuildExecArgs_ManyImages(t *testing.T) {
	options := &config.Options{
		Images: []string{
			"/path/img1.png",
			"/path/img2.jpg",
			"/path/img3.gif",
			"/path/img4.webp",
		},
	}

	args := BuildExecArgs("test", options)

	imageCount := 0

	for _, arg := range args {
		if arg == flagImage {
			imageCount++
		}
	}

	require.Equal(t, 4, imageCount)
}

// TestBuildExecArgs_OutputSchemaComplex tests a complex output schema.
func TestBuildExecArgs_OutputSchemaComplex(t *testing.T) {
	schema := `{"type":"object","properties":{"name":{"type":"string"},"items":{"type":"array","items":{"type":"object","properties":{"id":{"type":"integer"}}}}},"required":["name","items"]}`
	options := &config.Options{
		OutputSchema: schema,
	}

	args := BuildExecArgs("test", options)

	idx := slices.Index(args, "--output-schema")
	require.NotEqual(t, -1, idx)
	require.Equal(t, schema, args[idx+1])
}

// TestBuildExecArgs_WithMCPServers_HTTP tests HTTP MCP server serialization.
func TestBuildExecArgs_WithMCPServers_HTTP(t *testing.T) {
	options := &config.Options{
		MCPServers: map[string]mcp.ServerConfig{
			"my-api": &mcp.HTTPServerConfig{
				Type:    mcp.ServerTypeHTTP,
				URL:     "https://api.example.com/mcp",
				Headers: map[string]string{"Authorization": "Bearer tok123"},
			},
		},
	}

	args := BuildExecArgs("test", options)

	require.Contains(t, args, "mcp_servers.my-api.type=http")
	require.Contains(t, args, "mcp_servers.my-api.url=https://api.example.com/mcp")
	require.Contains(t, args, "mcp_servers.my-api.http_headers.Authorization=Bearer tok123")
	require.Equal(t, "test", args[len(args)-1])
}

// TestBuildExecArgs_WithMCPServers_SSE tests SSE MCP server serialization.
func TestBuildExecArgs_WithMCPServers_SSE(t *testing.T) {
	options := &config.Options{
		MCPServers: map[string]mcp.ServerConfig{
			"events": &mcp.SSEServerConfig{
				Type:    mcp.ServerTypeSSE,
				URL:     "https://sse.example.com/events",
				Headers: map[string]string{"X-Api-Key": "secret"},
			},
		},
	}

	args := BuildExecArgs("test", options)

	require.Contains(t, args, "mcp_servers.events.type=sse")
	require.Contains(t, args, "mcp_servers.events.url=https://sse.example.com/events")
	require.Contains(t, args, "mcp_servers.events.http_headers.X-Api-Key=secret")
}

// TestBuildExecArgs_WithMCPServers_Stdio tests stdio MCP server serialization.
//
// Args must be emitted as a single TOML inline array under one -c override.
// Emitting one -c mcp_servers.NAME.args=VAL per element does NOT accumulate
// into an array — each -c override replaces the previous value at the same
// dotted path. Codex's config schema requires Vec<String>, so repeated scalar
// overrides cause the subprocess to exit before handshake with
// `invalid type: string ..., expected a sequence`.
func TestBuildExecArgs_WithMCPServers_Stdio(t *testing.T) {
	options := &config.Options{
		MCPServers: map[string]mcp.ServerConfig{
			"local-tool": &mcp.StdioServerConfig{
				Command: "/usr/bin/my-tool",
				Args:    []string{"--port", "8080"},
				Env:     map[string]string{"DEBUG": "true", "HOME": "/tmp"},
			},
		},
	}

	args := BuildExecArgs("test", options)

	require.Contains(t, args, "mcp_servers.local-tool.type=stdio")
	require.Contains(t, args, "mcp_servers.local-tool.command=/usr/bin/my-tool")
	require.Contains(t, args, `mcp_servers.local-tool.args=["--port","8080"]`,
		"Args must be serialized as a single TOML inline array override")
	require.NotContains(t, args, "mcp_servers.local-tool.args=--port",
		"Args must NOT be emitted as repeated scalar -c overrides")
	require.NotContains(t, args, "mcp_servers.local-tool.args=8080",
		"Args must NOT be emitted as repeated scalar -c overrides")
	require.Contains(t, args, "mcp_servers.local-tool.env.DEBUG=true")
	require.Contains(t, args, "mcp_servers.local-tool.env.HOME=/tmp")
}

// TestBuildExecArgs_WithMCPServers_StdioArgsNpx covers the real-world npx case
// from the issue report: `npx -y @playwright/mcp@latest`.
func TestBuildExecArgs_WithMCPServers_StdioArgsNpx(t *testing.T) {
	options := &config.Options{
		MCPServers: map[string]mcp.ServerConfig{
			"playwright": &mcp.StdioServerConfig{
				Command: "npx",
				Args:    []string{"-y", "@playwright/mcp@latest"},
			},
		},
	}

	args := BuildExecArgs("test", options)

	require.Contains(t, args, `mcp_servers.playwright.args=["-y","@playwright/mcp@latest"]`)
}

// TestBuildExecArgs_WithMCPServers_StdioArgsSingleton ensures even a single arg
// is wrapped in array form — codex config declares args as Vec<String>, so a
// single scalar value fails type-checking even with one element.
func TestBuildExecArgs_WithMCPServers_StdioArgsSingleton(t *testing.T) {
	options := &config.Options{
		MCPServers: map[string]mcp.ServerConfig{
			"solo": &mcp.StdioServerConfig{
				Command: "tool",
				Args:    []string{"only-arg"},
			},
		},
	}

	args := BuildExecArgs("test", options)

	require.Contains(t, args, `mcp_servers.solo.args=["only-arg"]`)
	require.NotContains(t, args, "mcp_servers.solo.args=only-arg")
}

// TestBuildExecArgs_WithMCPServers_StdioArgsEscaping verifies that args with
// quotes, backslashes, or other special characters are safely escaped so the
// resulting TOML is valid.
func TestBuildExecArgs_WithMCPServers_StdioArgsEscaping(t *testing.T) {
	options := &config.Options{
		MCPServers: map[string]mcp.ServerConfig{
			"edge": &mcp.StdioServerConfig{
				Command: "echo",
				Args:    []string{`has "quote"`, `c:\path\to`},
			},
		},
	}

	args := BuildExecArgs("test", options)

	require.Contains(t, args, `mcp_servers.edge.args=["has \"quote\"","c:\\path\\to"]`)
}

// TestBuildExecArgs_WithMCPServers_StdioArgsEmpty ensures no args= override is
// emitted at all when Args is empty — emitting args="" would make codex see a
// scalar empty string instead of an empty Vec<String>.
func TestBuildExecArgs_WithMCPServers_StdioArgsEmpty(t *testing.T) {
	options := &config.Options{
		MCPServers: map[string]mcp.ServerConfig{
			"no-args": &mcp.StdioServerConfig{
				Command: "my-cmd",
			},
		},
	}

	args := BuildExecArgs("test", options)

	for _, a := range args {
		require.NotContains(t, a, "mcp_servers.no-args.args",
			"Empty Args must not produce any args= override")
	}
}

// TestBuildExecArgs_WithMCPServers_SDKSkipped tests that SDK servers are skipped.
func TestBuildExecArgs_WithMCPServers_SDKSkipped(t *testing.T) {
	options := &config.Options{
		MCPServers: map[string]mcp.ServerConfig{
			"sdk-server": &mcp.SdkServerConfig{
				Type: mcp.ServerTypeSDK,
				Name: "in-process",
			},
		},
	}

	args := BuildExecArgs("test", options)

	for _, arg := range args {
		require.NotContains(t, arg, "mcp_servers.sdk-server",
			"SDK server should not produce any -c flags")
	}
}

// TestBuildExecArgs_WithMCPServers_Mixed tests mixed server types together.
func TestBuildExecArgs_WithMCPServers_Mixed(t *testing.T) {
	options := &config.Options{
		MCPServers: map[string]mcp.ServerConfig{
			"http-svc": &mcp.HTTPServerConfig{
				Type: mcp.ServerTypeHTTP,
				URL:  "https://http.example.com",
			},
			"stdio-svc": &mcp.StdioServerConfig{
				Command: "my-cmd",
			},
			"sdk-svc": &mcp.SdkServerConfig{
				Type: mcp.ServerTypeSDK,
				Name: "skip-me",
			},
		},
	}

	args := BuildExecArgs("test", options)

	require.Contains(t, args, "mcp_servers.http-svc.type=http")
	require.Contains(t, args, "mcp_servers.http-svc.url=https://http.example.com")
	require.Contains(t, args, "mcp_servers.stdio-svc.type=stdio")
	require.Contains(t, args, "mcp_servers.stdio-svc.command=my-cmd")

	for _, arg := range args {
		require.NotContains(t, arg, "mcp_servers.sdk-svc")
	}
}

// TestBuildExecArgs_WithMCPServers_Empty tests that empty MCP servers add no flags.
func TestBuildExecArgs_WithMCPServers_Empty(t *testing.T) {
	options := &config.Options{
		MCPServers: map[string]mcp.ServerConfig{},
	}

	args := BuildExecArgs("test", options)

	for _, arg := range args {
		require.NotContains(t, arg, "mcp_servers.")
	}
}

// TestBuildExecArgs_WithMCPServers_DeterministicOrder tests that server names are sorted.
func TestBuildExecArgs_WithMCPServers_DeterministicOrder(t *testing.T) {
	options := &config.Options{
		MCPServers: map[string]mcp.ServerConfig{
			"zebra": &mcp.HTTPServerConfig{Type: mcp.ServerTypeHTTP, URL: "https://z.example.com"},
			"alpha": &mcp.HTTPServerConfig{Type: mcp.ServerTypeHTTP, URL: "https://a.example.com"},
			"mid":   &mcp.HTTPServerConfig{Type: mcp.ServerTypeHTTP, URL: "https://m.example.com"},
		},
	}

	args := BuildExecArgs("test", options)

	alphaIdx := slices.Index(args, "mcp_servers.alpha.type=http")
	midIdx := slices.Index(args, "mcp_servers.mid.type=http")
	zebraIdx := slices.Index(args, "mcp_servers.zebra.type=http")

	require.NotEqual(t, -1, alphaIdx)
	require.NotEqual(t, -1, midIdx)
	require.NotEqual(t, -1, zebraIdx)

	require.Less(t, alphaIdx, midIdx)
	require.Less(t, midIdx, zebraIdx)
}

// TestBuildExecArgs_AlwaysContainsBaseFlags tests that base flags are always present.
func TestBuildExecArgs_AlwaysContainsBaseFlags(t *testing.T) {
	tests := []struct {
		name    string
		options *config.Options
	}{
		{name: "empty options", options: &config.Options{}},
		{name: "with model", options: &config.Options{Model: "o4-mini"}},
		{name: "with sandbox", options: &config.Options{Sandbox: "read-only"}},
		{name: "with everything", options: &config.Options{
			Model:        "o4-mini",
			Sandbox:      "workspace-write",
			Cwd:          "/tmp",
			Images:       []string{"/img.png"},
			Config:       map[string]string{"k": "v"},
			OutputSchema: "{}",
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			args := BuildExecArgs("test", tt.options)

			require.Equal(t, "exec", args[0], "First arg should be 'exec'")
			require.Contains(t, args, "--json")
			require.Contains(t, args, "--full-auto")
			require.Contains(t, args, "--ephemeral")
			require.Contains(t, args, "--skip-git-repo-check")
		})
	}
}

func TestBuildEnvironment_VersionFromConstant(t *testing.T) {
	env := BuildEnvironment(&config.Options{})

	expected := "CODEX_CLI_SDK_VERSION=" + version.Version

	found := slices.Contains(env, expected)
	require.True(t, found, "expected %q in env", expected)
}
