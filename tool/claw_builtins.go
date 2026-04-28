package tool

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/SocialGouv/claw-code-go/pkg/api"
	clawtools "github.com/SocialGouv/claw-code-go/pkg/api/tools"
)

// RegisterClawBuiltins registers the standard claw-code-go built-in tools
// against the given Registry, making them callable by claw-backend agents
// that declare e.g. `tools: [read_file, write_file, bash]` in their .iter
// fixture.
//
// Workspace is forwarded to the bash tool for command validation; pass an
// empty string to skip workspace-based validation. Pass an empty string
// when registering on a registry that may be reused across workspaces.
//
// The set is intentionally curated — these are the seven workflow-grade
// tools that map cleanly onto file IO, shell, search, and HTTP fetch.
// Specialised tools (todo_write, plan_mode, agent, mcp_*, ...) live in
// claw-code-go's internal/tools/ and are not auto-registered here; callers
// that need them should import claw-code-go/pkg/api/tools and register
// individual entries via RegisterClawTool.
func RegisterClawBuiltins(reg *Registry, workspace string) error {
	bashExec := func(ctx context.Context, input map[string]any) (string, error) {
		return clawtools.ExecuteBash(ctx, input, workspace)
	}

	specs := []clawBuiltinSpec{
		{tool: clawtools.ReadFileTool(), exec: clawtools.ExecuteReadFile},
		{tool: clawtools.WriteFileTool(), exec: clawtools.ExecuteWriteFile},
		{tool: clawtools.GlobTool(), exec: clawtools.ExecuteGlob},
		{tool: clawtools.GrepTool(), exec: clawtools.ExecuteGrep},
		{tool: clawtools.FileEditTool(), exec: clawtools.ExecuteFileEdit},
		{tool: clawtools.WebFetchTool(), exec: clawtools.ExecuteWebFetch},
		{tool: clawtools.BashTool(), exec: bashExec},
	}

	for _, s := range specs {
		if err := RegisterClawTool(reg, s.tool, s.exec); err != nil {
			return fmt.Errorf("register %q: %w", s.tool.Name, err)
		}
	}
	return nil
}

// RegisterClawComputerUse registers the vision-grade computer-use tools
// (read_image, screenshot stub) against reg. These are kept out of the
// default RegisterClawBuiltins set because most workflows don't need
// vision content blocks; opt in explicitly when you have an agent that
// processes images.
//
// read_image returns a JSON payload describing the image plus a base64
// content block envelope; downstream agents can either inline it as
// commentary or splice the block into their next-turn message
// (multimodal models accept it directly).
//
// screenshot is a stub today — it returns an *api.APIError{StatusCode:
// 501}. Registering it surfaces the tool definition to the model so
// that prompt-engineered agents can be authored against the eventual
// implementation.
func RegisterClawComputerUse(reg *Registry) error {
	specs := []clawBuiltinSpec{
		{tool: clawtools.ReadImageTool(), exec: clawComputerUseAdapter(clawtools.ExecuteReadImage)},
		{tool: clawtools.ScreenshotTool(), exec: clawComputerUseAdapter(clawtools.ExecuteScreenshot)},
	}
	for _, s := range specs {
		if err := RegisterClawTool(reg, s.tool, s.exec); err != nil {
			return fmt.Errorf("register %q: %w", s.tool.Name, err)
		}
	}
	return nil
}

// clawComputerUseAdapter wraps a (ctx, input) → (ReadImageResult, error)
// function into the (ctx, input) → (string, error) signature the
// iterion tool registry expects. The result is JSON-encoded so
// downstream agents see the description + blocks structure verbatim.
func clawComputerUseAdapter(fn func(context.Context, map[string]any) (clawtools.ReadImageResult, error)) func(context.Context, map[string]any) (string, error) {
	return func(ctx context.Context, input map[string]any) (string, error) {
		res, err := fn(ctx, input)
		if err != nil {
			return "", err
		}
		buf, err := json.Marshal(res)
		if err != nil {
			return "", fmt.Errorf("encode read_image result: %w", err)
		}
		return string(buf), nil
	}
}

// RegisterClawTool registers a single claw-code-go tool against the
// registry. Use this to add specialised tools that RegisterClawBuiltins
// does not include by default.
func RegisterClawTool(reg *Registry, t api.Tool, exec func(ctx context.Context, input map[string]any) (string, error)) error {
	schemaJSON, err := json.Marshal(t.InputSchema)
	if err != nil {
		return fmt.Errorf("marshal schema: %w", err)
	}
	wrapped := func(ctx context.Context, input json.RawMessage) (string, error) {
		var args map[string]any
		if len(input) > 0 {
			if jerr := json.Unmarshal(input, &args); jerr != nil {
				return "", fmt.Errorf("decode tool input: %w", jerr)
			}
		}
		if args == nil {
			args = map[string]any{}
		}
		return exec(ctx, args)
	}
	return reg.RegisterBuiltin(t.Name, t.Description, schemaJSON, wrapped)
}

type clawBuiltinSpec struct {
	tool api.Tool
	exec func(ctx context.Context, input map[string]any) (string, error)
}
