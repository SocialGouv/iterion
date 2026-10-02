package commands

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
)

// ErrExpansionTooLarge is returned by Expand when the substitution would
// produce more than the caller's ceiling. It is raised DURING production,
// so the caller pays the bound rather than the expansion.
var ErrExpansionTooLarge = errors.New("commands: expansion exceeds the byte ceiling")

// ErrBodyTooLarge is returned by LookupWorkspace when the command file is
// larger than the caller's ceiling. Like ErrExpansionTooLarge it is raised
// WITHOUT materialising what it refuses: the read stops one byte past the
// bound. A command file is the cheapest half of this class — it needs no
// amplification at all, just a large file in a checkout under review.
var ErrBodyTooLarge = errors.New("commands: command file exceeds the byte ceiling")

// WorkspaceCommand is one markdown slash command discovered under a
// workspace's `.claude/commands/` directory — the project-commands
// convention Claude Code reads through `--setting-sources project`.
//
// It is the prompt-side view of a command file: the body a caller
// substitutes into the conversation. The TUI's own registry view lives in
// LoadDirCommands, which registers printing handlers instead.
type WorkspaceCommand struct {
	// Name is the invocation name without the leading slash. A command in
	// a subdirectory carries its namespace, colon-separated: `sub:nested`.
	Name string
	// Description is the frontmatter `description:` when present, else the
	// first non-empty body line, capped for help display.
	Description string
	// Body is the command's prompt text, frontmatter stripped.
	Body string
	// Path is the file the body was read from.
	Path string
	// DiscardedFrontmatter names the frontmatter keys this parser read and
	// then THREW AWAY — everything but `description:`. Claude Code honours
	// several of them (`allowed-tools:`, `model:`, `argument-hint:`,
	// `disable-model-invocation:`), so a command that narrows itself means
	// one thing there and another here. Surfacing the keys is what lets an
	// embedder say so instead of expanding in silence; it is the divergence
	// with a security consequence, and it was the only one a caller could
	// not see.
	DiscardedFrontmatter []string
}

// CommandsDir returns the `.claude/commands` directory of a workspace.
func CommandsDir(workDir string) string {
	return filepath.Join(workDir, ".claude", "commands")
}

// HasWorkspaceCommands reports whether workDir carries a
// `.claude/commands` directory. A caller uses it to skip the whole
// resolution path — and any diagnostic about it — for the overwhelmingly
// common workspace that contributes no command at all.
func HasWorkspaceCommands(workDir string) bool {
	if workDir == "" {
		return false
	}
	info, err := os.Stat(CommandsDir(workDir))
	return err == nil && info.IsDir()
}

// ParseInvocation reports whether prompt OPENS with a slash-command
// invocation, and splits it into the command name (without the slash) and
// the raw argument string that follows it.
//
// A name accepts letters, digits, `_`, `-`, `.` and `:` namespace
// separators, and nothing else; a component that is nothing but dots (`.`,
// `..`) is refused. That charset is CLAW'S OWN conservative rule, not the
// reference's: Claude Code applies none — its name is the whole
// whitespace-delimited token, and `/usr/bin/foo is broken` stays prose
// there only because no command named `usr/bin/foo` is registered. Keeping
// prose out by shape rather than by lookup is the safer default for an
// embedder, and it costs two divergences worth knowing: a command file
// named with a dot (`db.migrate.md`) is unreachable, and a non-breaking
// space does not separate the name from its arguments.
//
// It is NOT the containment guard. That is os.Root in LookupWorkspace; a
// charset can only refuse spellings, and a symlink has none.
func ParseInvocation(prompt string) (name, args string, ok bool) {
	// Trim first, as the reference does (`let t=e.trim()` before the "/"
	// test): a prompt built from a template or a prior node's output
	// routinely arrives with a leading newline, and it is an invocation
	// there, so it has to be one here.
	rest, found := strings.CutPrefix(strings.TrimLeft(prompt, " \t\r\n\v\f"), "/")
	if !found {
		return "", "", false
	}
	token := rest
	if i := strings.IndexAny(rest, " \t\r\n"); i >= 0 {
		token = rest[:i]
		args = strings.TrimLeft(rest[i:], " \t")
	}
	if token == "" || !validCommandName(token) {
		return "", "", false
	}
	return token, args, true
}

// discarded returns the frontmatter keys this package does not act on,
// sorted and deduplicated. `description:` is the one it consumes.
func discarded(keys []string) []string {
	var out []string
	for _, k := range keys {
		if k != "description" {
			out = append(out, k)
		}
	}
	if len(out) == 0 {
		return nil
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// notACommandFile reports whether err says the name cannot designate a
// command file at all — it does not exist, it is too long for the
// filesystem, it is a directory, or a path segment is not one. Those are
// "no such command", not "the disk refused the read": a prompt that merely
// opens with a slash-shaped word must not fail a node.
func notACommandFile(err error) bool {
	return errors.Is(err, fs.ErrNotExist) ||
		errors.Is(err, syscall.ENAMETOOLONG) ||
		errors.Is(err, syscall.EISDIR) ||
		errors.Is(err, syscall.ENOTDIR)
}

// validCommandName reports whether token is a well-formed command name:
// one or more `[A-Za-z0-9_-]` segments joined by `:`, with no empty
// segment.
func validCommandName(token string) bool {
	segment := false
	for i := 0; i < len(token); i++ {
		c := token[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '_', c == '-', c == '.':
			segment = true
		case c == ':':
			if !segment {
				return false
			}
			segment = false
		default:
			return false
		}
	}
	if !segment {
		return false
	}
	// A dot INSIDE a component is an ordinary file name (`db.migrate.md`
	// resolves on Claude Code, which applies no charset at all). A component
	// that IS a dot run is the traversal spelling, and it is refused here so
	// the name cannot express it — os.Root is what makes that a guarantee
	// rather than a convention, but a name has no business saying it.
	for _, seg := range strings.Split(token, ":") {
		if strings.Trim(seg, ".") == "" {
			return false
		}
	}
	return true
}

// LookupWorkspace resolves a command name against workDir's
// `.claude/commands/`. A namespaced name maps its `:` separators to
// directories: `sub:nested` reads `.claude/commands/sub/nested.md`.
//
// Resolution is scoped to workDir alone — no walk up the ancestors. A
// workspace is a checkout of a repository the caller does not control, and
// an ancestor walk would read command files from directories outside it.
// LoadDirCommands keeps the ancestor walk for the interactive session,
// where the ancestors are the operator's own.
//
// The read goes through os.Root, so the commands directory is a real
// containment boundary and not merely a naming convention: a repository
// that ships `.claude/commands/leak.md` as a SYMLINK to ~/.ssh/id_rsa or
// /proc/self/environ gets a refusal, not the file. The name charset alone
// cannot do this — it stops `..` from being spelled, and a symlink does
// not need to spell it.
//
// A body may legitimately be empty (an empty file, or one that is only
// frontmatter). The caller decides what that means; substituting it blindly
// would send an empty prompt.
func LookupWorkspace(workDir, name string, maxBytes int) (WorkspaceCommand, bool, error) {
	if workDir == "" {
		return WorkspaceCommand{}, false, nil
	}
	if !validCommandName(name) {
		return WorkspaceCommand{}, false, fmt.Errorf("commands: %q is not a valid command name", name)
	}
	// A fast path, not the guarantee: most workspaces contribute no command
	// at all and should not pay a root open per lookup. What KEEPS a
	// repository that ships `.claude/commands` as a regular file from
	// failing a node is notACommandFile below — removing this branch alone
	// leaves the refusal intact; removing both is what reddens
	// TestLookupWorkspaceTreatsAFileShapedCommandsDirAsNotFound.
	if !HasWorkspaceCommands(workDir) {
		return WorkspaceCommand{}, false, nil
	}
	rel := filepath.Join(strings.Split(name, ":")...) + ".md"
	path := filepath.Join(CommandsDir(workDir), rel)
	// Root at the WORKSPACE, not at the commands directory: rooting at
	// `.claude/commands` would resolve that path first, so a repository
	// shipping `.claude/commands` (or `.claude`) as a SYMLINK would move the
	// root itself — out to the operator's `~/.claude/commands`, or up out of
	// the checkout — and every read would be "contained" inside the wrong
	// tree. Opening the subdirectory THROUGH the root puts the whole chain
	// under the same boundary.
	root, err := os.OpenRoot(workDir)
	if err != nil {
		if notACommandFile(err) {
			return WorkspaceCommand{}, false, nil
		}
		return WorkspaceCommand{}, false, fmt.Errorf("commands: open %s: %w", workDir, err)
	}
	defer root.Close()
	f, err := root.Open(filepath.Join(".claude", "commands", rel))
	if err != nil {
		if notACommandFile(err) {
			return WorkspaceCommand{}, false, nil
		}
		return WorkspaceCommand{}, false, fmt.Errorf("commands: read %s: %w", path, err)
	}
	defer f.Close()
	// Read at most one byte past the ceiling: enough to KNOW the file is
	// over it, never enough to hold it. Reading the file whole and checking
	// its length afterwards is the defect this bound exists to remove, and
	// it is the same one measured inside Expand — a 256 MiB command file in
	// a checkout under review costs the server 512 MB before any ceiling is
	// consulted, with no amplification required.
	var data []byte
	if maxBytes > 0 {
		data, err = io.ReadAll(io.LimitReader(f, int64(maxBytes)+1))
	} else {
		data, err = io.ReadAll(f)
	}
	if err != nil {
		if notACommandFile(err) {
			return WorkspaceCommand{}, false, nil
		}
		return WorkspaceCommand{}, false, fmt.Errorf("commands: read %s: %w", path, err)
	}
	if maxBytes > 0 && len(data) > maxBytes {
		return WorkspaceCommand{}, false, ErrBodyTooLarge
	}
	body, desc, keys := stripFrontmatter(string(data))
	return WorkspaceCommand{
		Name:                 name,
		Description:          desc,
		Body:                 strings.TrimSpace(body),
		Path:                 path,
		DiscardedFrontmatter: discarded(keys),
	}, true, nil
}

// Expand substitutes the argument placeholders of a command body.
//
//   - `$ARGUMENTS` takes the whole raw argument string. It matches as a bare
//     prefix, so `$ARGUMENTS_FILE` becomes `<args>_FILE` — measured on
//     Claude Code 2.1.220, which does the same (its substitution is a plain
//     replaceAll), so this is parity rather than a divergence and carries no
//     warning;
//   - `$1` … `$9` take the n-th whitespace-separated argument, ONE-BASED
//     (`$1` is the first). An index past the end is left as written, which
//     is what Claude Code does for an out-of-range position.
//
// The body is scanned once, left to right: text that came FROM an argument
// is never re-scanned, so an argument that itself contains `$1` cannot
// inject a second substitution.
//
// The second return reports whether any placeholder actually TOOK the
// arguments. A caller needs that to decide whether to append them (Claude
// Code appends to a body that consumes none), and it has to be the
// expander's own answer: a separate predicate over the body disagreed with
// this loop at both ends — `$0` and an out-of-range `$N` are placeholders a
// reader sees and this loop leaves literal, so a body carrying one had its
// arguments neither substituted NOR appended, which deleted the operator's
// message.
//
// maxBytes bounds the OUTPUT, and the bound is enforced AS THE OUTPUT IS
// PRODUCED: the loop stops and returns ErrExpansionTooLarge the moment the
// next write would cross it, so the refusal costs the bound and not the
// expansion. Measuring a finished string instead would bound the bill and
// not the memory, and both inputs here are attacker-influenced on a review
// run — a body of 24 000 `$ARGUMENTS` sits under any sane file ceiling and
// still reaches gigabytes once the arguments carry a pasted diff, which in
// a multi-replica server takes co-tenant runs down before anyone is billed.
// A non-positive maxBytes means unbounded. A body that is ITSELF over the
// ceiling is refused whatever it would expand to, including one that would
// shrink: holding it was already the cost this bound exists to avoid.
func Expand(cmd WorkspaceCommand, args string, maxBytes int) (expanded string, consumed bool, err error) {
	body := cmd.Body
	if maxBytes > 0 && len(body) > maxBytes {
		// Nothing to produce: the literal text alone already crosses.
		return "", false, ErrExpansionTooLarge
	}
	if !strings.ContainsRune(body, '$') {
		return body, false, nil
	}
	// Split lazily. strings.Fields allocates a 16-byte header per field —
	// a fixed 8x amplification of the ARGUMENT bytes that maxBytes does not
	// see, paid even on the refusal path and even by the overwhelmingly
	// common `$ARGUMENTS`-only body that never looks at a positional.
	// Measured before this: a 10-byte body refused against a 12-byte ceiling
	// still cost 268 MB on a 32 MiB argument.
	var fields []string
	fieldsSplit := false
	positional := func(n int) (string, bool) {
		if !fieldsSplit {
			fields, fieldsSplit = strings.Fields(args), true
		}
		if n >= 1 && n <= len(fields) {
			return fields[n-1], true
		}
		return "", false
	}
	var b strings.Builder
	b.Grow(growHint(len(body)+len(args), maxBytes))
	// write appends s unless it would cross the bound.
	write := func(s string) bool {
		if maxBytes > 0 && b.Len()+len(s) > maxBytes {
			return false
		}
		b.WriteString(s)
		return true
	}
	for i := 0; i < len(body); {
		if body[i] != '$' {
			if !write(body[i : i+1]) {
				return "", false, ErrExpansionTooLarge
			}
			i++
			continue
		}
		if rest, found := strings.CutPrefix(body[i:], "$ARGUMENTS"); found {
			if !write(args) {
				return "", false, ErrExpansionTooLarge
			}
			consumed = true
			i = len(body) - len(rest)
			continue
		}
		if n, width, ok := positionalAt(body, i); ok {
			if field, inRange := positional(n); inRange {
				if !write(field) {
					return "", false, ErrExpansionTooLarge
				}
				consumed = true
				i += width
				continue
			}
		}
		if !write(body[i : i+1]) {
			return "", false, ErrExpansionTooLarge
		}
		i++
	}
	return b.String(), consumed, nil
}

// growHint caps the builder's pre-allocation at the bound: the unbounded
// hint (body + args) is the right guess for a body with one placeholder and
// a gross over-allocation for the refusal path this bound exists to make
// cheap.
func growHint(want, maxBytes int) int {
	if maxBytes > 0 && want > maxBytes {
		return maxBytes
	}
	return want
}

// maxPositional bounds the index a `$N` placeholder may name. It is a
// property of the PLACEHOLDER, never of the body: a ceiling derived from
// len(body) made "$3" a placeholder in a long body and literal text in a
// short one, so padding a body with prose changed what it meant.
const maxPositional = 1 << 20

// positionalAt reads a `$<digits>` placeholder starting at body[i] and
// returns its value, the byte width of the whole placeholder, and whether
// one is there at all.
//
// The digit run is consumed WHOLE and the placeholder must not be the
// prefix of a word — the reference matches `/\$(\d+)(?!\w)/`. Reading a
// single digit instead would rewrite "$100" into "<arg1>00" and "$1_id"
// into "<arg1>_id": prices, shell snippets and identifiers in a command
// body would be silently corrupted.
func positionalAt(body string, i int) (n, width int, ok bool) {
	if i >= len(body) || body[i] != '$' {
		return 0, 0, false
	}
	j := i + 1
	for j < len(body) && body[j] >= '0' && body[j] <= '9' {
		j++
	}
	if j == i+1 {
		return 0, 0, false
	}
	if j < len(body) && isWordByte(body[j]) {
		return 0, 0, false
	}
	for _, c := range []byte(body[i+1 : j]) {
		n = n*10 + int(c-'0')
		if n > maxPositional {
			// Far past any possible argument count, and past the point where
			// the accumulator would overflow; leave it literal.
			return 0, 0, false
		}
	}
	return n, j - i, true
}

// isWordByte reports whether c is a `\w` byte (letter, digit, underscore).
func isWordByte(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}

// DynamicBodyForms names the Claude Code command features present in a
// body that Expand handles DIFFERENTLY — either not at all, or with other
// semantics — so a caller can report them instead of letting one file mean
// two things on two backends, given the arguments it will be expanded with.
// The result is sorted and deduplicated; empty means the body is fully
// equivalent on both.
//
// `@path` file references are NOT reported, deliberately: distinguishing one
// from an npm scope (`@anthropic-ai/sdk`) or a decorator (`@mcp.tool(`) by
// text alone measured 0% precision over the real command bodies on a
// developer machine, and a warning that is always wrong teaches an author to
// ignore the ones that are not. The difference stays documented.
//
//   - "!`…`" and "```!" — shell substitution, executed by Claude Code,
//     passed through as text here.
//   - "$ARGUMENTS[n]" — indexed argument, substituted by Claude Code; here
//     the "$ARGUMENTS" prefix expands and the "[n]" is left dangling.
//   - "$0" — substituted by Claude Code (first argument), literal here.
//   - "$N" — `$1`, `$2`, … substituted by BOTH, but Claude Code 2.1.x
//     resolves them off by one against its own documented contract; Expand
//     is one-based. Reported for exactly the indices Expand would substitute
//     for THESE arguments — every one of them, not just the single digits,
//     and none of the out-of-range ones, so a price reads as a price and
//     `$10` with twelve arguments still gets its warning.
//   - "\$" — an escape Claude Code honours (before a digit or ARGUMENTS)
//     and Expand does not.
//   - "${CLAUDE_*}" — `${CLAUDE_PROJECT_DIR}`, `${CLAUDE_SESSION_ID}` and
//     `${CLAUDE_EFFORT}` are substituted by Claude Code in a command body
//     and left literal here.
func DynamicBodyForms(body, args string) []string {
	var forms []string
	if containsShellSubstitution(body) {
		forms = append(forms, "!`…`")
	}
	if strings.Contains(body, "$ARGUMENTS[") {
		forms = append(forms, "$ARGUMENTS[n]")
	}
	if _, _, ok := findPositional(body, 0, 0); ok {
		forms = append(forms, "$0")
	}
	// Only the indices Expand would ACTUALLY substitute for these arguments:
	// the diagnostic shares the scanner AND the range with the expander, so
	// "$500" in a price is silent while "$10" with twelve arguments — which
	// Expand does substitute — is named.
	if _, _, ok := findPositional(body, 1, len(strings.Fields(args))); ok {
		forms = append(forms, "$N")
	}
	// `\$` is an escape the reference honours only before a digit or
	// ARGUMENTS; `\$HOME` in a shell snippet is identical on both backends
	// and warning about it would be the always-wrong warning `@path` was
	// dropped for.
	if containsEscapedPlaceholder(body) {
		forms = append(forms, `\$`)
	}
	if strings.Contains(body, "${CLAUDE_") {
		forms = append(forms, "${CLAUDE_*}")
	}
	slices.Sort(forms)
	return slices.Compact(forms)
}

// findPositional reports whether body carries a `$N` placeholder whose
// index falls within [lo, hi]. It reuses the scanner Expand uses, so the
// diagnostic and the substitution can never disagree about what counts as
// a placeholder.
func findPositional(body string, lo, hi int) (n, at int, ok bool) {
	for i := 0; i < len(body); i++ {
		if body[i] != '$' {
			continue
		}
		if v, _, found := positionalAt(body, i); found && v >= lo && v <= hi {
			return v, i, true
		}
	}
	return 0, 0, false
}

// containsShellSubstitution reports whether body carries a Claude Code
// shell substitution: an inline "!`cmd`" opening a token, or a fenced
// "```!" block. A bare "!" followed by a backtick mid-word ("shout!`") is
// not one, and warning about it would train an author to ignore the
// warning.
func containsShellSubstitution(body string) bool {
	if strings.Contains(body, "```!") {
		return true
	}
	for i := 0; i+1 < len(body); i++ {
		if body[i] != '!' || body[i+1] != '`' {
			continue
		}
		if i > 0 && !isSpaceByte(body[i-1]) {
			continue
		}
		if strings.IndexByte(body[i+2:], '`') > 0 {
			return true
		}
	}
	return false
}

// containsEscapedPlaceholder reports whether body carries a `\$` escape in
// a position where the reference honours it — immediately before a digit or
// before ARGUMENTS. Anywhere else the backslash is ordinary text on both
// backends.
func containsEscapedPlaceholder(body string) bool {
	for i := 0; i+1 < len(body); i++ {
		if body[i] != '\\' || body[i+1] != '$' {
			continue
		}
		rest := body[i+2:]
		if strings.HasPrefix(rest, "ARGUMENTS") {
			return true
		}
		if rest != "" && rest[0] >= '0' && rest[0] <= '9' {
			return true
		}
	}
	return false
}

// isSpaceByte reports whether c is ASCII whitespace.
func isSpaceByte(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f' || c == '\v'
}
