package model

import (
	"errors"
	"os"
	"strconv"
	"strings"
	"sync"

	clawcmds "github.com/SocialGouv/claw-code-go/pkg/api/commands"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// SlashCommandsEnv turns the workspace slash-command expansion off for
// `claw` nodes, restoring the raw `/name` text the backend sent before the
// capability existed. "off", "0" and "false" disable it; anything else
// (including unset) leaves it on.
const SlashCommandsEnv = "ITERION_CLAW_SLASH_COMMANDS"

// SlashCommandMaxBytesEnv overrides the ceiling on a workspace command's
// body, in bytes. "0" removes the ceiling.
const SlashCommandMaxBytesEnv = "ITERION_CLAW_SLASH_COMMAND_MAX_BYTES"

// defaultSlashCommandMaxBytes bounds the body a workspace command may
// substitute into a prompt. On a review run the workspace is a checkout the
// run does not control, so this file is untrusted input that turns straight
// into a BILLED request: a repository could otherwise make one node send a
// multi-megabyte prompt by committing one markdown file. 256 KiB is two
// orders of magnitude above any real command body (the largest in this
// repo's own catalog is a few KB), so it bounds the abuse without bounding
// the use — and it is an abstention, never a truncation, because half a
// command body is an instruction nobody wrote.
const defaultSlashCommandMaxBytes = 256 << 10

// slashCommandArgsPrefix introduces the arguments appended to a command
// body that consumes none. Claude Code 2.1.220 does this rather than drop
// them, and dropping them would delete the operator's actual question —
// `/review the auth module, focus on session fixation` would reach the
// model as `Review the code and report.`
const slashCommandArgsPrefix = "\n\nARGUMENTS: "

// expandWorkspaceSlashCommand resolves a user prompt that OPENS with a
// `/name` invocation against the workspace's `.claude/commands/`, giving a
// `claw` node the command body that `claude_code` reads from the workspace
// itself (measured: the CLI resolves a project command with
// `--setting-sources` omitted entirely; the flag decides the OTHER scopes). It returns the prompt to send and whether a command was
// substituted.
//
// It is claw-only on purpose. `claude_code` resolves these itself, and
// expanding first would hand its CLI a body where it expects an
// invocation; the CLI agents (`codex`, `pi`, `kimi`, `grok`, `opencode`)
// have no workspace-command convention at all — that gap is named in
// docs/backends.md rather than papered over here.
//
// Nothing here fails a node. Measured on claude_code 2.1.220, an unknown
// command answers locally with `Unknown command: /x` and reports
// `is_error: false`, `subtype: success`, exit 0 — the node succeeds. A hard
// failure would therefore be a divergence, not parity, and it would break
// an ordinary prompt that merely opens with a slash-shaped word ("/tmp is
// full, clean it up"). Every abstention is logged with its reason and the
// text travels unchanged, which is also what the backend did before this
// existed: degraded, never silent.
func expandWorkspaceSlashCommand(userText, workDir, backendName, nodeID string, iteration int, logger *iterlog.Logger, once *slashWarnOnce) (string, bool) {
	if backendName != delegate.BackendClaw || workDir == "" || userText == "" {
		return userText, false
	}
	switch strings.ToLower(strings.TrimSpace(os.Getenv(SlashCommandsEnv))) {
	case "off", "0", "false":
		return userText, false
	}
	// The overwhelming majority of workspaces contribute no command at all;
	// skip the whole path — and its diagnostics — for them.
	if !clawcmds.HasWorkspaceCommands(workDir) {
		return userText, false
	}
	name, args, ok := clawcmds.ParseInvocation(userText)
	if !ok {
		return userText, false
	}
	// The ceiling reaches the READ, not just the write: a large command file
	// needs no amplification at all, so measuring it after loading it would
	// be the same defect one call earlier.
	max := slashCommandMaxBytes()
	cmd, found, err := clawcmds.LookupWorkspace(workDir, name, max)
	switch {
	case errors.Is(err, clawcmds.ErrBodyTooLarge):
		warnSlashCommand(logger, nodeID, iteration,
			"/%s is larger than the %d-byte ceiling (%s) — sending the prompt unchanged rather than loading it",
			safeDiag(name), max, SlashCommandMaxBytesEnv)
		return userText, false
	case err != nil:
		warnSlashCommand(logger, nodeID, iteration,
			"/%s could not be read (%s) — sending the prompt unchanged", safeDiag(name), readFailureReason(err))
		return userText, false
	}
	if !found {
		warnSlashCommand(logger, nodeID, iteration, "prompt opens with /%s, which %s does not define — sending the text unchanged",
			safeDiag(name), clawcmds.CommandsDir(workDir))
		return userText, false
	}
	// The bound travels INTO the expander, which aborts as it produces. A
	// length check on the finished string would bound the bill and not the
	// memory: a body under the pre-check still amplifies (`$ARGUMENTS`
	// repeated N times × the arguments), and the allocation lands before
	// anything is billed — on a multi-replica server, on the co-tenants.
	expanded, consumed, err := clawcmds.Expand(cmd, args, max)
	if err != nil {
		warnSlashCommand(logger, nodeID, iteration,
			"/%s (%s) expands past the %d-byte ceiling (%s) — sending the prompt unchanged rather than materialising it",
			safeDiag(name), cmd.Path, max, SlashCommandMaxBytesEnv)
		return userText, false
	}
	// Judged on the EXPANDED text and before the arguments are appended:
	// otherwise `/empty some question` reaches the model as a bare
	// "ARGUMENTS: some question" with no instruction at all — a command that
	// only looks non-empty because the operator typed something after it.
	// Expanded rather than raw, because a body of just `$ARGUMENTS` is a
	// non-empty file that expands to nothing when invoked bare, and the
	// message has to say which of the two happened or the trail goes cold.
	if strings.TrimSpace(expanded) == "" {
		warnSlashCommand(logger, nodeID, iteration, "/%s (%s) expands to nothing (body %q) — sending the prompt unchanged rather than an empty one",
			safeDiag(name), cmd.Path, safeDiag(cmd.Body))
		return userText, false
	}
	// A body that took none of the arguments still has to carry them, or the
	// operator's message is destroyed rather than merely unexpanded. The
	// question is answered by what Expand DID, never by a predicate over the
	// body: the two disagreed on `$0` and on an out-of-range `$N`, and on
	// that disagreement the arguments were neither substituted nor appended.
	if a := strings.TrimSpace(args); a != "" && !consumed {
		expanded += slashCommandArgsPrefix + a
	}
	// The third end: the appended arguments. They cannot amplify (they are
	// the prompt's own bytes, appended once), so a length check is the right
	// instrument here — unlike the expansion, where it would have been a
	// measurement after the damage.
	if max > 0 && len(expanded) > max {
		warnSlashCommand(logger, nodeID, iteration,
			"/%s (%s) reaches %d bytes with its arguments, over the %d-byte ceiling (%s) — sending the prompt unchanged rather than billing it",
			safeDiag(name), cmd.Path, len(expanded), max, SlashCommandMaxBytesEnv)
		return userText, false
	}
	if logger != nil {
		logger.Info("[%s#%d/claw] 📎 workspace command /%s from %s", nodeID, iteration, name, cmd.Path)
	}
	// Once per command file per run, not once per invocation. The
	// divergence is REAL every time — claw's `$1` is the first argument and
	// the CLI's is the second, so they differ on every substitution — but a
	// node in a loop would otherwise repeat the same line every iteration,
	// and a warning that repeats on correct usage costs the others their
	// audience. That is the standard `@path` was dropped under; the answer
	// there was to stop warning on something that was never a divergence,
	// and the answer here is to say a real one once.
	if forms := clawcmds.DynamicBodyForms(cmd.Body, args); len(forms) > 0 && once.first("forms\x00"+cmd.Path) {
		warnSlashCommand(logger, nodeID, iteration, "/%s uses %s — those mean something else here than in claude_code; see docs/backends.md#workspace-slash-commands",
			safeDiag(name), strings.Join(forms, ", "))
	}
	// The frontmatter is the divergence that carries a SECURITY consequence
	// and it was the only one that expanded in silence: a command narrowing
	// itself with `allowed-tools:` is bounded on claude_code and keeps the
	// node's whole tool set here, so the operator reads a restriction that
	// nothing enforces. Same channel as the body divergences — documenting
	// it in bold and saying nothing at runtime is the "one file means two
	// things silently" outcome this warning exists to prevent. Enforcement
	// is #1717; this is the diagnostic that stops it being invisible.
	if keys := cmd.DiscardedFrontmatter; len(keys) > 0 && once.first("fm\x00"+cmd.Path) {
		warnSlashCommand(logger, nodeID, iteration,
			"/%s (%s) declares %s in its frontmatter — claude_code honours those, this backend ignores them, so a command that narrows itself keeps this node's full tool set (#1717)",
			safeDiag(name), cmd.Path, safeDiag(strings.Join(keys, ", ")))
	}
	return expanded, true
}

// slashCommandUserContent rebuilds the multimodal blocks of a prompt whose
// invocation was substituted: the command body replaces every TEXT block —
// they carried the invocation, not the instruction — and every image block
// travels untouched, in order, so the model still receives the bytes.
//
// Returns nil when the prompt carried no block at all, which is what
// buildUserContent returns for a prompt with no inlined image: the plain
// path then keeps using UserPrompt alone, as before.
func slashCommandUserContent(blocks []delegate.ContentBlock, expanded string) []delegate.ContentBlock {
	if len(blocks) == 0 {
		return nil
	}
	out := make([]delegate.ContentBlock, 0, len(blocks)+1)
	out = append(out, delegate.ContentBlock{Type: "text", Text: expanded})
	for _, b := range blocks {
		if b.Type == "text" {
			continue
		}
		out = append(out, b)
	}
	return out
}

// slashCommandMaxBytes resolves the body ceiling: the operator's override
// when it parses, the built-in default otherwise. A non-numeric value is
// ignored rather than obeyed — a typo must not silently remove a bound.
func slashCommandMaxBytes() int {
	raw := strings.TrimSpace(os.Getenv(SlashCommandMaxBytesEnv))
	if raw == "" {
		return defaultSlashCommandMaxBytes
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return defaultSlashCommandMaxBytes
	}
	return n
}

// slashWarnOnce keeps a divergence notice to one line per command file per
// run. It is shared across a run's nodes and branches run in parallel, so it
// locks.
type slashWarnOnce struct {
	mu   sync.Mutex
	seen map[string]bool
}

// first reports whether this is the first time key is seen. A nil receiver
// never suppresses, so a caller that does not care keeps every line.
func (o *slashWarnOnce) first(key string) bool {
	if o == nil {
		return true
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.seen[key] {
		return false
	}
	if o.seen == nil {
		o.seen = map[string]bool{}
	}
	o.seen[key] = true
	return true
}

// readFailureReason renders why a command file could not be read, in terms
// an operator can act on. os.Root reports its refusal as "path escapes from
// parent", which reads as an accusation the repository tried to escape —
// true for a symlink pointing out of the workspace, and MISLEADING for an
// absolute symlink whose target is inside it, which os.Root refuses just as
// flatly. Naming the boundary is the honest form; the raw error is kept
// alongside so nothing is hidden. Matching the wording is a diagnostic
// refinement, never the guarantee: the guarantee is that the read failed and
// the prompt travelled unchanged.
func readFailureReason(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	if strings.Contains(msg, "escapes from parent") {
		return "refused by the workspace containment boundary — a command file that is a symlink out of the workspace, or ANY absolute symlink, is refused whatever it points at: " + msg
	}
	return msg
}

// safeDiag bounds a value a diagnostic interpolates from untrusted input.
// A command name is the whole whitespace-delimited token of a prompt that is
// routinely model- or repo-derived, and the frontmatter keys come from a
// file in a checkout the run does not control — either can be megabytes, and
// these lines fire per node per iteration into the process log AND the run
// log stream. The body was already truncated; this is the rest of that class,
// in one place so the next value cannot be forgotten.
func safeDiag(s string) string { return iterlog.Truncate(s, 200) }

// warnSlashCommand emits one abstention or divergence notice, tagged the way
// every other per-node line in this package is (`[<node>#<iter>/claw]`).
// The studio scopes its per-node Logs tab on `[<node>#<iteration>/` and only
// falls back to `[<node>#` when that set is EMPTY, so a line tagged with a
// fixed 0 disappears from the panel the moment the node runs inside a loop —
// which is exactly when a silent prompt substitution is hardest to explain.
func warnSlashCommand(logger *iterlog.Logger, nodeID string, iteration int, format string, args ...any) {
	if logger == nil {
		return
	}
	logger.Warn("[%s#%d/claw] "+format, append([]any{nodeID, iteration}, args...)...)
}
