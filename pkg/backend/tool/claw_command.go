package tool

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/SocialGouv/claw-code-go/pkg/api"
	clawcmds "github.com/SocialGouv/claw-code-go/pkg/api/commands"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
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
// workDir is CAPTURED once; hosts whose workspace can move after
// registration — the engine replaces a run's workDir when `worktree: auto`
// produces a per-run worktree (ClawExecutor.SetWorkDir, #2870) — register
// through RegisterClawCommandWithProvider instead, so the lookup follows.
//
// Nothing here fails a call with a Go error but a missing name: claude_code
// answers an unknown command locally (is_error: false), so every domain
// outcome — unknown name, unreadable file, oversized body, empty expansion
// — is the tool's TEXT, for the model to report. So are the two divergences
// the lookup reports (commandNotices): the enforcement of
// `disable-model-invocation:` and the rest of the frontmatter stays #1717's,
// the VISIBILITY of what was discarded is this surface's own.
func RegisterClawCommand(reg *Registry, workDir string, maxBytes int) error {
	return RegisterClawCommandWithProvider(reg, staticWorkspaceProvider(workDir), maxBytes)
}

// RegisterClawCommandWithProvider is RegisterClawCommand over a workspace
// PROVIDER, consulted on every call, rather than a string captured at
// registration. The prompt half of the capability reads the executor's
// workDir live — the engine replaces it at run start when `worktree: auto`
// produces a per-run worktree — so a captured string would resolve
// `.claude/commands/` in a different checkout than the prompt path:
// possibly a different commit, for the capability whose byte ceiling exists
// precisely for that review case (#2870). A nil provider behaves as the
// empty workspace: every call answers "unknown command".
func RegisterClawCommandWithProvider(reg *Registry, workspace func() string, maxBytes int) error {
	if maxBytes == 0 {
		maxBytes = DefaultWorkspaceCommandMaxBytes
	}
	if workspace == nil {
		workspace = staticWorkspaceProvider("")
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
		return executeWorkspaceCommand(workspace(), maxBytes, input)
	})
}

// staticWorkspaceProvider wraps a registration-time workspace string into
// the provider shape the workspace-scoped lookup tools resolve through, for
// hosts whose workspace never moves after registration (the in-container
// runner: its cwd IS the run's bind-mounted worktree).
func staticWorkspaceProvider(workDir string) func() string {
	return func() string { return workDir }
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
	return commandNotices(name, cmd, args) + expanded, nil
}

// commandNotices prefixes the body with the divergences the lookup reported
// — the frontmatter it discarded, and the body forms this backend expands
// differently — each the sentence the prompt path logs
// (model.expandWorkspaceSlashCommand), addressed to the reader this surface
// has.
//
// The frontmatter one is the divergence that carries a SECURITY
// consequence: a command narrowing itself with `allowed-tools:` is bounded
// on claude_code and keeps the node's whole tool set here, so an operator
// reads a restriction nothing enforces. Saying nothing while #1717's
// enforcement is deferred is the "one file means two things silently"
// outcome the prompt path's warning exists to prevent, and the model-driven
// surface needs it MORE: nobody typed the invocation, so nobody is watching
// for the file's claims.
//
// The tool's own answer is the channel because it is the only one BOTH
// placements have: `command` is PlacementSandbox, so in a sandboxed run the
// registration that executes is the in-container one — no run logger, no
// build session, no event bus. The answer crosses instead, and the host
// persists it on the node's `tool_called` event (persistToolPayload), so a
// gate reading events.jsonl finds the notice where the model found it. At
// the HEAD for the same reason: an oversized answer is persisted as a
// preview of its first bytes.
func commandNotices(name string, cmd clawcmds.WorkspaceCommand, args string) string {
	var b strings.Builder
	if keys := cmd.DiscardedFrontmatter; len(keys) > 0 {
		fmt.Fprintf(&b, "NOTE (iterion): /%s (%s) declares %s in its frontmatter — claude_code honours those, this backend ignores them, so the command does not narrow the tools you hold (#1717). Report this with your answer.\n",
			noticeValue(name), cmd.Path, noticeValue(strings.Join(keys, ", ")))
	}
	if forms := clawcmds.DynamicBodyForms(cmd.Body, args); len(forms) > 0 {
		fmt.Fprintf(&b, "NOTE (iterion): /%s uses %s — those mean something else here than in claude_code; see docs/backends.md#workspace-slash-commands.\n",
			noticeValue(name), strings.Join(forms, ", "))
	}
	if b.Len() == 0 {
		return ""
	}
	return b.String() + "\n"
}

// noticeValue bounds a value a notice interpolates from untrusted input, to
// the same 200 bytes the prompt path's safeDiag keeps. The notice is added
// AFTER the ceiling check, so without this a frontmatter of megabyte-long
// key names would reach the model past the bound the ceiling exists to
// hold: the keys come from a file in a checkout the run does not control,
// and the command name is whatever the model asked for. The forms need no
// bound — they are this package's own words, from a closed vocabulary.
func noticeValue(s string) string { return iterlog.Truncate(s, 200) }
