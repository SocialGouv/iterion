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

// ErrBodyTooLarge is returned by LookupWorkspace when the command file is
// larger than the caller's ceiling. The read stops one byte past the bound,
// so the refusal never holds what it refuses.
var ErrBodyTooLarge = intl.ErrBodyTooLarge

// LookupWorkspace resolves a command name against workDir's
// `.claude/commands/`, mapping a `:` namespace onto directories. It is
// scoped to workDir — it never walks up into the ancestors.
//
// maxBytes bounds the FILE: the read stops one byte past the ceiling and
// returns ErrBodyTooLarge, so a large command file in a checkout the caller
// does not control costs the bound rather than its own size. Non-positive
// means unbounded.
func LookupWorkspace(workDir, name string, maxBytes int) (WorkspaceCommand, bool, error) {
	return intl.LookupWorkspace(workDir, name, maxBytes)
}

// ErrExpansionTooLarge is returned by Expand when the substitution would
// produce more than the caller's ceiling.
var ErrExpansionTooLarge = intl.ErrExpansionTooLarge

// Expand substitutes `$ARGUMENTS` and the one-based `$1` … `$9`
// placeholders of a command body. Text that came from an argument is never
// re-scanned.
//
// The second return says whether a placeholder actually TOOK the arguments.
// A body that consumes none must have them appended — that is what Claude
// Code does, and dropping them deletes the operator's message.
//
// maxBytes bounds the OUTPUT as it is produced: Expand stops and returns
// ErrExpansionTooLarge the moment the next write would cross it, so an
// embedder handed an untrusted command body pays the bound rather than the
// expansion. A body under a file-size check can still amplify — 24 000
// `$ARGUMENTS` times a large argument tail reaches gigabytes — and
// measuring the finished string would bound only the bill. Non-positive
// means unbounded.
func Expand(cmd WorkspaceCommand, args string, maxBytes int) (expanded string, consumed bool, err error) {
	return intl.Expand(cmd, args, maxBytes)
}

// DynamicBodyForms names the Claude Code command features present in a body
// that Expand handles differently given these arguments, so an embedder can
// report them instead of letting one file mean two things on two backends.
func DynamicBodyForms(body, args string) []string { return intl.DynamicBodyForms(body, args) }
