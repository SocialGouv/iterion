package kubernetes

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/SocialGouv/iterion/pkg/internal/shellquote"
)

// validateEnvVar mirrors the discipline applied on the docker driver
// (pkg/sandbox/docker/driver.go:validateEnvVar): reject env names
// containing '=' / newline / NUL and values containing newline / NUL.
// Kubernetes is more forgiving than docker's argv parser — the manifest
// is built as JSON, so embedded newlines survive — but operators reading
// `kubectl describe` see corrupted output, and downstream consumers that
// split on \n misbehave. Same posture across both drivers.
func validateEnvVar(k, v string) error {
	if strings.ContainsAny(k, "=\n\r\x00") {
		return fmt.Errorf("kubernetes: invalid env var name (contains '=', newline, or NUL): %q", k)
	}
	if strings.ContainsAny(v, "\n\r\x00") {
		return fmt.Errorf("kubernetes: env var %q value contains newline, CR, or NUL — refusing to inject", k)
	}
	return nil
}

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

// replaceUnderscores swaps "_" → "-" so legacy run IDs that predate
// the UUIDv7 switch (e.g. "run_1777989944581") fit DNS-1123 subdomain
// rules. UUIDv7 strings have no underscores so this is a no-op for
// new runs.
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
	keys := slices.Sorted(maps.Keys(env))
	args = append(args, "env")
	for _, k := range keys {
		// Defense-in-depth: validateEnvVar rejects newlines and NULs
		// that would split the inner `env` tool's argv parsing or
		// corrupt `kubectl describe` output. Silently skip rather
		// than return — the parent callers don't return an error
		// from this helper. validateEnvVar in strings.go.
		if validateEnvVar(k, env[k]) != nil {
			continue
		}
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
	b.WriteString(shellquote.Quote(dir))
	b.WriteString(" && exec ")
	if len(env) > 0 {
		b.WriteString("env ")
		for _, k := range slices.Sorted(maps.Keys(env)) {
			b.WriteString(shellquote.Quote(k + "=" + env[k]))
			b.WriteByte(' ')
		}
	}
	for i, a := range argv {
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(shellquote.Quote(a))
	}
	return b.String()
}

// buildShellChdirScript renders a chdir + env + INLINE script payload for
// a shell reading it on stdin (`<shell> -s`). Unlike
// [buildShellChdirExec] it never puts the script back on an argv
// element, so nothing in the payload is subject to the kernel's
// per-argument cap — the point of streaming it in the first place.
//
// Env arrives as `export` lines rather than an `env K=V ...` prefix
// because there is no command to prefix: the script runs in the shell
// that read it. `cd` failing aborts instead of running the script in the
// wrong directory.
//
// Quoting, stated because it differs from [buildShellChdirExec]: `dir`
// and each `K=V` pair are shell-quoted, but `script` is written RAW and
// unquoted — it IS shell source, and quoting it would stop it being
// executable. So this builder gives the script no isolation whatsoever
// from the surrounding payload, by design; the caller's script is trusted
// exactly as much as it was when it arrived as `<shell> -c <script>`,
// which is where every caller of this path gets it from.
func buildShellChdirScript(dir string, env map[string]string, script string) string {
	var b strings.Builder
	b.WriteString("cd ")
	b.WriteString(shellquote.Quote(dir))
	b.WriteString(" || exit 1\n")
	for _, k := range slices.Sorted(maps.Keys(env)) {
		b.WriteString("export ")
		b.WriteString(shellquote.Quote(k + "=" + env[k]))
		b.WriteByte('\n')
	}
	b.WriteString(script)
	return b.String()
}
