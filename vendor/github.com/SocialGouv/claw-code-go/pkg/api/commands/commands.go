// Package commands exposes the `.claude/commands/` workspace-command
// convention on the public API surface, so an in-process embedder resolves
// a `/name` prompt the way Claude Code resolves it under
// `--setting-sources project`.
//
// The interactive session registers these as printing slash commands
// (internal/commands.LoadDirCommands); an embedder that has no session
// wants the prompt body instead, which is what this package returns.
package commands

import (
	intl "github.com/SocialGouv/claw-code-go/internal/commands"
)

// WorkspaceCommand is one markdown slash command read from a workspace's
// `.claude/commands/` directory.
type WorkspaceCommand = intl.WorkspaceCommand

// CommandsDir returns the `.claude/commands` directory of a workspace.
func CommandsDir(workDir string) string { return intl.CommandsDir(workDir) }

// HasWorkspaceCommands reports whether workDir carries a
// `.claude/commands` directory.
func HasWorkspaceCommands(workDir string) bool { return intl.HasWorkspaceCommands(workDir) }

// ParseInvocation reports whether prompt opens with a slash-command
// invocation and splits it into the command name and its argument string.
// A token carrying `/` or `.` is a path, not a command name, so ordinary
// prose beginning with a filesystem path is left alone.
func ParseInvocation(prompt string) (name, args string, ok bool) {
	return intl.ParseInvocation(prompt)
}

// LookupWorkspace resolves a command name against workDir's
// `.claude/commands/`, mapping a `:` namespace onto directories. It is
// scoped to workDir — it never walks up into the ancestors.
func LookupWorkspace(workDir, name string) (WorkspaceCommand, bool, error) {
	return intl.LookupWorkspace(workDir, name)
}

// Expand substitutes `$ARGUMENTS` and the one-based `$1` … `$9`
// placeholders of a command body. Text that came from an argument is never
// re-scanned.
//
// The second return says whether a placeholder actually TOOK the arguments.
// A body that consumes none must have them appended — that is what Claude
// Code does, and dropping them deletes the operator's message.
func Expand(cmd WorkspaceCommand, args string) (expanded string, consumed bool) {
	return intl.Expand(cmd, args)
}

// DynamicBodyForms names the Claude Code command features present in a body
// that Expand handles differently given these arguments, so an embedder can
// report them instead of letting one file mean two things on two backends.
func DynamicBodyForms(body, args string) []string { return intl.DynamicBodyForms(body, args) }
