package kubernetes

import (
	"sort"
	"strings"
)

// toLowerASCII returns a copy of s with ASCII upper-case letters
// replaced by their lower-case equivalents. We avoid the stdlib
// strings.ToLower here because it does Unicode case folding that
// can produce non-DNS-1123 characters for some inputs. Pod names
// must satisfy DNS-1123 (a-z0-9-).
func toLowerASCII(s string) string {
	out := make([]byte, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			out[i] = c + ('a' - 'A')
		} else {
			out[i] = c
		}
	}
	return string(out)
}

// replaceUnderscores swaps "_" → "-" so iterion-derived run IDs
// (e.g. "run_1777989944581") fit DNS-1123 subdomain rules.
func replaceUnderscores(s string) string {
	return strings.ReplaceAll(s, "_", "-")
}

// appendEnvPrefix prepends a `env KEY1=v1 KEY2=v2 …` segment to the
// kubectl-exec argv when the caller wants per-call env vars beyond
// what the pod was started with. `kubectl exec` doesn't expose a
// --env flag, so we use the standard `env` tool which is in
// PATH on every container we ship for.
//
// Sorted insertion keeps the final argv stable across runs (useful
// for `kubectl describe` debugging).
func appendEnvPrefix(args []string, env map[string]string) []string {
	if len(env) == 0 {
		return args
	}
	keys := sortedKeys(env)
	args = append(args, "env")
	for _, k := range keys {
		args = append(args, k+"="+env[k])
	}
	return args
}

// buildShellChdirExec returns a `cd <dir> && exec <argv...>` snippet
// suitable for `sh -c`. Used when a per-call WorkDir overrides the
// pod's container.workingDir. `exec` replaces the shell with the
// inner program so signal semantics and exit codes round-trip
// correctly.
//
// Each argv element is single-quoted with embedded ' escaped per
// POSIX rules. We don't use Go's strconv because shell quoting
// follows different rules.
func buildShellChdirExec(dir string, argv []string, env map[string]string) string {
	var b strings.Builder
	b.WriteString("cd ")
	b.WriteString(shellSingleQuote(dir))
	b.WriteString(" && exec ")
	if len(env) > 0 {
		b.WriteString("env ")
		for _, k := range sortedKeys(env) {
			b.WriteString(shellSingleQuote(k + "=" + env[k]))
			b.WriteByte(' ')
		}
	}
	for i, a := range argv {
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(shellSingleQuote(a))
	}
	return b.String()
}

// shellSingleQuote wraps s in single quotes and escapes any
// embedded single quotes via the canonical '\” sequence.
func shellSingleQuote(s string) string {
	if s == "" {
		return "''"
	}
	if !strings.ContainsAny(s, " \t\n'\"\\$`*?(){}[]<>|&;") {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// sortedKeys returns keys of m in ascending order — used for stable
// argv ordering in `kubectl describe` output across runs.
func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
