package cli

import (
	"fmt"
	"os"
	"sort"

	"github.com/ethpandaops/codex-agent-sdk-go/internal/config"
	"github.com/ethpandaops/codex-agent-sdk-go/internal/mcp"
	"github.com/ethpandaops/codex-agent-sdk-go/internal/version"
)

// serializeMCPServerConfigArgs converts MCP server configs into `-c` flag pairs
// using the Codex CLI's TOML config mechanism (e.g. `-c mcp_servers.NAME.type=http`).
// Server names are sorted for deterministic output.
func serializeMCPServerConfigArgs(servers map[string]mcp.ServerConfig) []string {
	if len(servers) == 0 {
		return nil
	}

	names := make([]string, 0, len(servers))
	for name := range servers {
		names = append(names, name)
	}

	sort.Strings(names)

	var args []string

	for _, name := range names {
		cfg := servers[name]
		prefix := fmt.Sprintf("mcp_servers.%s", name)

		switch c := cfg.(type) {
		case *mcp.HTTPServerConfig:
			args = append(args, "-c", fmt.Sprintf("%s.type=http", prefix))
			args = append(args, "-c", fmt.Sprintf("%s.url=%s", prefix, c.URL))

			headerKeys := make([]string, 0, len(c.Headers))
			for k := range c.Headers {
				headerKeys = append(headerKeys, k)
			}

			sort.Strings(headerKeys)

			for _, k := range headerKeys {
				args = append(args, "-c", fmt.Sprintf("%s.http_headers.%s=%s", prefix, k, c.Headers[k]))
			}
		case *mcp.SSEServerConfig:
			args = append(args, "-c", fmt.Sprintf("%s.type=sse", prefix))
			args = append(args, "-c", fmt.Sprintf("%s.url=%s", prefix, c.URL))

			headerKeys := make([]string, 0, len(c.Headers))
			for k := range c.Headers {
				headerKeys = append(headerKeys, k)
			}

			sort.Strings(headerKeys)

			for _, k := range headerKeys {
				args = append(args, "-c", fmt.Sprintf("%s.http_headers.%s=%s", prefix, k, c.Headers[k]))
			}
		case *mcp.StdioServerConfig:
			args = append(args, "-c", fmt.Sprintf("%s.type=stdio", prefix))
			args = append(args, "-c", fmt.Sprintf("%s.command=%s", prefix, c.Command))

			for _, arg := range c.Args {
				args = append(args, "-c", fmt.Sprintf("%s.args=%s", prefix, arg))
			}

			envKeys := make([]string, 0, len(c.Env))
			for k := range c.Env {
				envKeys = append(envKeys, k)
			}

			sort.Strings(envKeys)

			for _, k := range envKeys {
				args = append(args, "-c", fmt.Sprintf("%s.env.%s=%s", prefix, k, c.Env[k]))
			}
		case *mcp.SdkServerConfig:
			// SDK servers cannot be serialized to CLI flags; skip.
			continue
		}
	}

	return args
}

// Command represents the CLI command to execute.
type Command struct {
	// Args are the command line arguments.
	Args []string

	// Env are the environment variables.
	Env []string
}

// BuildExecArgs constructs the CLI argument list for a one-shot `codex exec` invocation.
func BuildExecArgs(prompt string, options *config.Options) []string {
	args := []string{
		"exec",
		"--json",
		"--full-auto",
		"--ephemeral",
		"--skip-git-repo-check",
	}

	if options.Model != "" {
		args = append(args, "-m", options.Model)
	}

	// Sandbox mode: explicit Sandbox field takes precedence, then map from PermissionMode
	sandbox := options.Sandbox
	if sandbox == "" {
		sandbox = mapPermissionToSandbox(options.PermissionMode)
	}

	if sandbox != "" {
		args = append(args, "-s", sandbox)
	}

	cwd := options.Cwd
	if cwd == "" {
		cwd, _ = os.Getwd()
	}

	if cwd != "" {
		args = append(args, "-C", cwd)
	}

	for _, dir := range options.AddDirs {
		args = append(args, "--add-dir", dir)
	}

	for _, img := range options.Images {
		args = append(args, "-i", img)
	}

	// Sort config keys for deterministic flag ordering.
	if len(options.Config) > 0 {
		keys := make([]string, 0, len(options.Config))
		for k := range options.Config {
			keys = append(keys, k)
		}

		sort.Strings(keys)

		for _, k := range keys {
			args = append(args, "-c", fmt.Sprintf("%s=%s", k, options.Config[k]))
		}
	}

	// MCP server configs serialized as -c flags.
	args = append(args, serializeMCPServerConfigArgs(options.MCPServers)...)

	if options.OutputSchema != "" {
		args = append(args, "--output-schema", options.OutputSchema)
	}

	// Extra args from the ExtraArgs map
	for key, value := range options.ExtraArgs {
		if value == nil {
			args = append(args, "--"+key)
		} else {
			args = append(args, "--"+key, *value)
		}
	}

	// Prompt is always the last positional argument.
	args = append(args, prompt)

	return args
}

// BuildAppServerArgs constructs the CLI argument list for `codex app-server`.
func BuildAppServerArgs(options *config.Options) []string {
	args := []string{"app-server"}

	if options != nil {
		args = append(args, serializeMCPServerConfigArgs(options.MCPServers)...)
	}

	return args
}

// BuildEnvironment constructs the environment variables for the CLI process.
func BuildEnvironment(options *config.Options) []string {
	env := os.Environ()
	env = append(env, "CODEX_CLI_SDK_VERSION="+version.Version)

	for key, value := range options.Env {
		env = append(env, fmt.Sprintf("%s=%s", key, value))
	}

	return env
}

// mapPermissionToSandbox maps claude-sdk permission modes to Codex sandbox modes.
func mapPermissionToSandbox(permMode string) string {
	switch permMode {
	case "default", "":
		return ""
	case "acceptEdits":
		return "workspace-write"
	case "bypassPermissions":
		return "danger-full-access"
	default:
		return ""
	}
}
