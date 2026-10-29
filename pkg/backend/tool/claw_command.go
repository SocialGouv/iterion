package tool

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/SocialGouv/claw-code-go/pkg/api"
	clawcmds "github.com/SocialGouv/claw-code-go/pkg/api/commands"
)

// DefaultWorkspaceCommandMaxBytes is the built-in ceiling on a workspace
// command's body — and on its expansion — for the `command` tool. It is the
// same bound the prompt-invocation path applies (pkg/backend/model's
// ITERION_CLAW_SLASH_COMMAND_MAX_BYTES, whose host-side resolution the
// host's registration passes in): on a review run the workspace is a
// checkout the run does not control, so the file is untrusted input that
// turns straight into a billed request.
const DefaultWorkspaceCommandMaxBytes = 256 << 10

// commandArgsPrefix introduces the arguments appended to a command body
// that consumes none — what Claude Code 2.1.x does rather than drop them,
// and what the prompt-invocation path does (slashCommandArgsPrefix): one
// spelling, so an operator meets the same shape on both paths.
const commandArgsPrefix = "\n\nARGUMENTS: "

// RegisterClawCommand registers the `command` tool: the MODEL-invoked half
// of the workspace slash-command capability (#1716), which claude_code
// carries natively (its SlashCommand tool — withheld headless, since a
// command there can be Bash-backed) and claw had no surface for. The tool
// resolves `<workDir>/.claude/commands/<name>.md` through the same
// contained pkg/api/commands lookup the prompt path uses and returns the
// expanded body for the model to follow; executing anything the body asks
// for still takes the node's own declared tools, so a `command` grant hands
// over no capability the node did not already hold.
//
// maxBytes is the body/expansion ceiling: 0 falls back to
// DefaultWorkspaceCommandMaxBytes, a negative value removes the ceiling
// (the operator's dial, resolved host-side). An empty workDir registers the
// tool against no workspace: every call answers "unknown command".
//
// Nothing here fails a call with a Go error but a missing name: claude_code
// answers an unknown command locally (is_error: false), so every domain
// outcome — unknown name, unreadable file, oversized body, empty expansion
// — is the tool's TEXT, for the model to report. The frontmatter divergence
// is the prompt path's own (#1717): keys other than `description:` are read
// and discarded by the lookup, `disable-model-invocation:` included.
func RegisterClawCommand(reg *Registry, workDir string, maxBytes int) error {
	if maxBytes == 0 {
		maxBytes = DefaultWorkspaceCommandMaxBytes
	}
	t := api.Tool{
		Name: "command",
		Description: "Invoke a workspace slash command from .claude/commands/ by name (without the leading slash), " +
			"with optional arguments. The command's body — a curated prompt the workspace defines — is returned to you to follow. " +
			"Invoke it when the task matches a command the workspace carries, instead of improvising the procedure it encodes.",
		InputSchema: api.InputSchema{
			Type: "object",
			Properties: map[string]api.Property{
				"name": {Type: "string", Description: "The command name, without the leading slash (a namespaced command uses ':', e.g. 'db:migrate')."},
				"args": {Type: "string", Description: "Optional arguments the command body substitutes."},
			},
			Required: []string{"name"},
		},
	}
	return RegisterClawTool(reg, t, func(_ context.Context, input map[string]any) (string, error) {
		return executeWorkspaceCommand(workDir, maxBytes, input)
	})
}

func executeWorkspaceCommand(workDir string, maxBytes int, input map[string]any) (string, error) {
	name, _ := input["name"].(string)
	// The model thinks in invocations: accept the slash it would type.
	name = strings.TrimSpace(strings.TrimPrefix(name, "/"))
	if name == "" {
		return "", fmt.Errorf("command: 'name' is required")
	}
	args, _ := input["args"].(string)
	cmd, found, err := clawcmds.LookupWorkspace(workDir, name, maxBytes)
	switch {
	case errors.Is(err, clawcmds.ErrBodyTooLarge):
		return fmt.Sprintf("/%s is larger than the %d-byte ceiling — not invoked", name, maxBytes), nil
	case err != nil:
		return fmt.Sprintf("/%s could not be read (%s) — not invoked", name, err), nil
	case !found:
		return fmt.Sprintf("Unknown command: /%s — %s does not define it", name, clawcmds.CommandsDir(workDir)), nil
	}
	expanded, consumed, err := clawcmds.Expand(cmd, args, maxBytes)
	if err != nil {
		return fmt.Sprintf("/%s (%s) expands past the %d-byte ceiling — not invoked", name, cmd.Path, maxBytes), nil
	}
	if strings.TrimSpace(expanded) == "" {
		return fmt.Sprintf("/%s (%s) expands to nothing — not invoked", name, cmd.Path), nil
	}
	if a := strings.TrimSpace(args); a != "" && !consumed {
		expanded += commandArgsPrefix + a
	}
	if maxBytes > 0 && len(expanded) > maxBytes {
		return fmt.Sprintf("/%s (%s) reaches %d bytes with its arguments, over the %d-byte ceiling — not invoked", name, cmd.Path, len(expanded), maxBytes), nil
	}
	return expanded, nil
}
