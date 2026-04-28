// Package tools exposes a stable public-API surface over the built-in tool
// implementations that ship with claw-code-go. Internal tool packages live
// under internal/ and cannot be imported from outside the module — this
// wrapper makes them callable by external consumers (e.g. iterion) without
// leaking internal types beyond what is strictly necessary.
//
// Each tool comes as a pair: a `XxxTool() api.Tool` returning the schema and
// description the model needs to call the tool, and an `ExecuteXxx` function
// that runs the tool with the parsed input arguments. Consumers register
// these against their own LLM client / workflow engine.
//
// The bash tool is special: the underlying executor needs a permission mode
// and a workspace root for command validation. The public wrapper pins
// permissions.ModeAllow (no policy gating) and forwards the workspace
// argument; consumers that want a stricter policy should reach for the
// internal API directly via the same module.
package tools

import (
	"context"

	"github.com/SocialGouv/claw-code-go/internal/permissions"
	intl "github.com/SocialGouv/claw-code-go/internal/tools"
	"github.com/SocialGouv/claw-code-go/pkg/api"
)

// ----- read_file -----

// ReadFileTool returns the tool definition for reading files.
func ReadFileTool() api.Tool { return intl.ReadFileTool() }

// ExecuteReadFile reads the file at input["path"] and returns its contents.
func ExecuteReadFile(ctx context.Context, input map[string]any) (string, error) {
	return intl.ExecuteReadFile(input)
}

// ----- write_file -----

// WriteFileTool returns the tool definition for writing files.
func WriteFileTool() api.Tool { return intl.WriteFileTool() }

// ExecuteWriteFile writes input["content"] to input["path"], creating
// parent directories as needed.
func ExecuteWriteFile(ctx context.Context, input map[string]any) (string, error) {
	return intl.ExecuteWriteFile(input)
}

// ----- bash -----

// BashTool returns the tool definition for executing shell commands.
func BashTool() api.Tool { return intl.BashTool() }

// ExecuteBash runs the bash command at input["command"]. Workspace is the
// directory used for command validation; pass an empty string to skip
// workspace-based validation entirely. Permission mode is fixed to
// ModeAllow — the wrapper assumes the caller has already gated invocations
// upstream (e.g. via an iterion workflow's allowed-tools list).
func ExecuteBash(ctx context.Context, input map[string]any, workspace string) (string, error) {
	return intl.ExecuteBash(input, permissions.ModeAllow, workspace)
}

// ----- glob -----

// GlobTool returns the tool definition for filesystem glob matching.
func GlobTool() api.Tool { return intl.GlobTool() }

// ExecuteGlob expands input["pattern"] into matching paths.
func ExecuteGlob(ctx context.Context, input map[string]any) (string, error) {
	return intl.ExecuteGlob(input)
}

// ----- grep -----

// GrepTool returns the tool definition for content search.
func GrepTool() api.Tool { return intl.GrepTool() }

// ExecuteGrep runs a ripgrep-style search using the input arguments.
func ExecuteGrep(ctx context.Context, input map[string]any) (string, error) {
	return intl.ExecuteGrep(input)
}

// ----- file_edit -----

// FileEditTool returns the tool definition for in-place file editing
// (string replacement with optional replace_all semantics).
func FileEditTool() api.Tool { return intl.FileEditTool() }

// ExecuteFileEdit applies the requested edit at input["path"].
func ExecuteFileEdit(ctx context.Context, input map[string]any) (string, error) {
	return intl.ExecuteFileEdit(input)
}

// ----- web_fetch -----

// WebFetchTool returns the tool definition for fetching URLs.
func WebFetchTool() api.Tool { return intl.WebFetchTool() }

// ExecuteWebFetch performs an HTTP GET for input["url"] and returns the body.
func ExecuteWebFetch(ctx context.Context, input map[string]any) (string, error) {
	return intl.ExecuteWebFetch(input)
}
