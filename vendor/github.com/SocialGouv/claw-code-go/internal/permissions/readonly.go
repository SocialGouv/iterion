package permissions

import "strings"

// IsReadOnlyBashCommand reports whether a bash command line is verifiably
// read-only, so permission modes that would otherwise prompt (or ModeAuto's
// classifier) can allow it silently. The contract mirrors Claude Code's
// built-in read-only auto-allow: false negatives only cost one prompt,
// false positives are never acceptable — so the parse is deliberately
// conservative:
//
//   - any output redirection, command/process substitution, heredoc or
//     backgrounding rejects the whole line;
//   - the line is split on &&, ||, ;, | and newlines, and EVERY segment's
//     command must independently pass;
//   - env-assignment prefixes (FOO=x cmd) and path-form commands (./x,
//     /bin/x) are rejected — resolution tricks defeat name-based checks.
func IsReadOnlyBashCommand(command string) bool {
	c := strings.TrimSpace(command)
	if c == "" {
		return false
	}
	// Writing, substitution, heredocs and stderr-pipes are disqualifying no
	// matter where they appear (even quoted occurrences only cost a prompt).
	if strings.ContainsAny(c, ">`") {
		return false
	}
	for _, bad := range []string{"$(", "<(", "<<", "|&"} {
		if strings.Contains(c, bad) {
			return false
		}
	}
	// Split on chain operators. && and || first (so a lone & can then be
	// rejected as backgrounding), then ;, | and newlines.
	c = strings.ReplaceAll(c, "&&", "\x00")
	c = strings.ReplaceAll(c, "||", "\x00")
	if strings.Contains(c, "&") {
		return false
	}
	for _, sep := range []string{";", "|", "\n"} {
		c = strings.ReplaceAll(c, sep, "\x00")
	}

	checked := false
	for _, seg := range strings.Split(c, "\x00") {
		fields := strings.Fields(seg)
		if len(fields) == 0 {
			continue
		}
		if !isReadOnlySegment(fields) {
			return false
		}
		checked = true
	}
	return checked
}

// readOnlyAnyArgs are commands allowed with arbitrary arguments (the global
// rejections above already exclude redirection/substitution payloads).
var readOnlyAnyArgs = makeSet(
	// Always read-only core utilities.
	"cal", "uptime", "cat", "head", "tail", "wc", "stat", "strings",
	"hexdump", "od", "nl", "id", "uname", "free", "df", "du", "locale",
	"groups", "nproc", "basename", "dirname", "realpath", "cut", "paste",
	"tr", "column", "tac", "rev", "fold", "expand", "unexpand", "fmt",
	"comm", "cmp", "numfmt", "readlink", "diff", "true", "false", "which",
	"type", "expr", "test", "getconf", "seq", "tsort", "pr", "echo",
	"printf", "ls", "cd", "sleep",
	// Search/inspection utilities, safe once redirection is excluded.
	"grep", "egrep", "fgrep", "rg", "fd", "fdfind", "jq", "uniq",
	"sha256sum", "sha1sum", "md5sum", "tree", "date", "hostname", "man",
	"info", "ps", "pgrep", "lsof", "tput", "ss", "netstat", "file",
	"arch", "base64", "history", "sort", "find",
)

// readOnlyForbiddenArgs lists per-command arguments that flip an otherwise
// read-only command into a writing/executing one.
var readOnlyForbiddenArgs = map[string][]string{
	"sort": {"-o", "--output"},
	"find": {"-delete", "-exec", "-execdir", "-ok", "-okdir", "-fprint", "-fprintf", "-fls"},
	"git":  {"--output", "--output-directory"},
}

// readOnlyZeroArgs are allowed only bare (arguments could change semantics).
var readOnlyZeroArgs = makeSet("pwd", "whoami", "alias", "ifconfig")

// readOnlyExact are allowed only as these exact invocations.
var readOnlyExact = makeSet(
	"node -v", "node --version",
	"python --version", "python3 --version",
	"go version",
	"ip addr",
)

// gitReadOnlySubcommands never mutate regardless of arguments.
var gitReadOnlySubcommands = makeSet(
	"status", "log", "diff", "show", "blame", "ls-files", "ls-remote",
	"ls-tree", "rev-parse", "describe", "shortlog", "cat-file",
	"for-each-ref", "grep", "name-rev", "merge-base", "count-objects",
	"cherry", "whatchanged", "reflog", "help", "version", "var",
	"check-ignore", "check-attr", "show-ref", "symbolic-ref", "show-branch",
)

// ghReadOnly maps a gh command group to its read-only subcommands.
var ghReadOnly = map[string]map[string]struct{}{
	"pr":       makeSet("view", "list", "diff", "checks", "status"),
	"issue":    makeSet("view", "list", "status"),
	"run":      makeSet("view", "list"),
	"workflow": makeSet("list", "view"),
	"repo":     makeSet("view"),
	"release":  makeSet("view", "list"),
	"auth":     makeSet("status"),
	"label":    makeSet("list"),
	"search":   nil, // every gh search subcommand is a query
}

// dockerReadOnlySubcommands inspect state without changing it.
var dockerReadOnlySubcommands = makeSet(
	"ps", "images", "logs", "inspect", "version", "info", "top", "port", "diff",
)

func isReadOnlySegment(fields []string) bool {
	argv0 := fields[0]
	// Env-assignment prefixes and path-form commands defeat name checks.
	if strings.Contains(argv0, "=") || strings.Contains(argv0, "/") {
		return false
	}
	if _, ok := readOnlyExact[strings.Join(fields, " ")]; ok {
		return true
	}
	if _, ok := readOnlyZeroArgs[argv0]; ok {
		return len(fields) == 1
	}
	if _, ok := readOnlyAnyArgs[argv0]; ok {
		return !hasForbiddenArg(argv0, fields[1:])
	}
	switch argv0 {
	case "git":
		return isReadOnlyGit(fields[1:])
	case "gh":
		return isReadOnlyGh(fields[1:])
	case "docker":
		return isReadOnlyDocker(fields[1:])
	}
	return false
}

func hasForbiddenArg(cmd string, args []string) bool {
	forbidden := readOnlyForbiddenArgs[cmd]
	if len(forbidden) == 0 {
		return false
	}
	for _, a := range args {
		for _, f := range forbidden {
			if a == f || strings.HasPrefix(a, f+"=") {
				return true
			}
		}
	}
	return false
}

// isReadOnlyGit validates `git <sub> ...`. Global flags before the
// subcommand (-C, -c, --git-dir…) are rejected outright: they relocate or
// reconfigure the repo and are rare in read-only usage.
func isReadOnlyGit(args []string) bool {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return false
	}
	sub, rest := args[0], args[1:]
	if hasForbiddenArg("git", rest) {
		return false
	}
	if _, ok := gitReadOnlySubcommands[sub]; ok {
		return true
	}
	switch sub {
	case "branch":
		// Bare or list-style flags only; any positional argument (creation)
		// or delete/move/copy flag mutates.
		for _, a := range rest {
			if !strings.HasPrefix(a, "-") {
				return false
			}
			switch a {
			case "-d", "-D", "-m", "-M", "-c", "-C", "-f", "--force",
				"--delete", "--move", "--copy", "--unset-upstream",
				"--edit-description":
				return false
			}
			if strings.HasPrefix(a, "--set-upstream") {
				return false
			}
		}
		return true
	case "tag":
		if len(rest) == 0 {
			return true
		}
		for _, a := range rest {
			if !strings.HasPrefix(a, "-") {
				return false
			}
			switch a {
			case "-d", "--delete", "-f", "--force", "-a", "-s", "-m", "-F", "--edit":
				return false
			}
		}
		return true
	case "remote":
		if len(rest) == 0 {
			return true
		}
		if rest[0] == "-v" || rest[0] == "--verbose" {
			return true
		}
		return rest[0] == "show" || rest[0] == "get-url"
	case "stash":
		return len(rest) > 0 && (rest[0] == "list" || rest[0] == "show")
	case "worktree":
		return len(rest) > 0 && rest[0] == "list"
	case "config":
		if len(rest) == 0 {
			return false
		}
		switch rest[0] {
		case "-l", "--list", "--get", "--get-all", "--get-regexp":
			return true
		}
		return false
	}
	return false
}

// isReadOnlyGh validates `gh <group> <sub> ...`. `gh api` is allowed only
// when nothing forces a non-GET method (explicit -X/--method or body fields,
// which flip gh api to POST).
func isReadOnlyGh(args []string) bool {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return false
	}
	group, rest := args[0], args[1:]
	if group == "status" {
		return true
	}
	if group == "api" {
		for i, a := range rest {
			switch {
			case a == "-X" || a == "--method":
				if i+1 >= len(rest) || !strings.EqualFold(rest[i+1], "GET") {
					return false
				}
			case strings.HasPrefix(a, "--method="):
				if !strings.EqualFold(strings.TrimPrefix(a, "--method="), "GET") {
					return false
				}
			case a == "-f" || a == "-F" || a == "--field" || a == "--raw-field" || a == "--input",
				strings.HasPrefix(a, "--field="), strings.HasPrefix(a, "--raw-field="), strings.HasPrefix(a, "--input="):
				return false
			}
		}
		return true
	}
	subs, ok := ghReadOnly[group]
	if !ok {
		return false
	}
	if subs == nil {
		return true
	}
	if len(rest) == 0 || strings.HasPrefix(rest[0], "-") {
		return false
	}
	_, ok = subs[rest[0]]
	return ok
}

func isReadOnlyDocker(args []string) bool {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return false
	}
	_, ok := dockerReadOnlySubcommands[args[0]]
	return ok
}

func makeSet(items ...string) map[string]struct{} {
	s := make(map[string]struct{}, len(items))
	for _, it := range items {
		s[it] = struct{}{}
	}
	return s
}
