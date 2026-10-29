package mcp

import (
	"fmt"
	"maps"
	"os"
	"sort"
	"strings"

	"github.com/SocialGouv/iterion/internal/envtrust"
)

// EnvServerEnvPassthrough is the operator's explicit, greppable way to hand a
// launcher-started stdio MCP server more of the launcher's environment than
// its own `env:` block and the process-essential allowlist: a comma-separated
// list of variable names, or `all` for full inheritance.
//
// It exists because the default is the strong one — a server receives what it
// DECLARES — and that is the reading most likely to break a working setup: an
// operator may legitimately keep a plugin server's token in their shell or in
// a project `.env` rather than in the server's `env:` map. Naming it here is
// how they say so.
const EnvServerEnvPassthrough = "ITERION_MCP_SERVER_ENV_PASSTHROUGH"

// envPassthroughAll restores the inheritance that existed before the
// allowlist: the launcher's whole environment, planted names included.
const envPassthroughAll = "all"

// stdioAmbientAllowlist is the only ambient environment a launcher-started
// stdio server keeps: what a child process needs to EXIST, never what it
// fetches, loads or trusts.
//
// The operator plugin servers that start beside the launcher are npx-based
// (firecrawl, codeindex) and a node process without PATH or HOME does not
// start at all — while NODE_OPTIONS decides what it loads, npm_config_* where
// it fetches from, and *_PROXY through whom. Those are the answers a `.env` in
// the repository under review could supply, so they are not here: an operator
// who needs one names it in EnvServerEnvPassthrough.
var stdioAmbientAllowlist = []string{
	"PATH", "HOME", "TMPDIR",
	// A corporate TLS root reaches node this way and nowhere else; without
	// it every fetch of an allowlisted registry fails on a MITM'd link.
	"NODE_EXTRA_CA_CERTS",
	// Windows: a child without these does not run at all (the shell, the
	// per-user directories, the extension list). Absent elsewhere, so the
	// list stays one list.
	"SYSTEMROOT", "SYSTEMDRIVE", "COMSPEC", "PATHEXT",
	"APPDATA", "LOCALAPPDATA", "USERPROFILE", "TEMP", "TMP",
}

// stdioEnvDecision is the environment one stdio child receives, and what the
// operator is owed about it.
type stdioEnvDecision struct {
	// Env is the complete child environment, sorted: a child's variable
	// order must not change from one start to the next over one map's
	// iteration order.
	Env []string
	// Info states an operator's own explicit widening, once per start — the
	// one place they read why a name does, or does not, reach the server.
	Info string
	// Warning states what weakens the default: full inheritance restored, or
	// tokens that name nothing.
	Warning string
}

// stdioChildEnv composes the environment of a launcher-started stdio MCP
// server: the names the server DECLARES (`env:`, operator-controlled — it
// comes from the operator's home), the process-essential allowlist, and
// whatever EnvServerEnvPassthrough names. The ambient environment is not
// inherited otherwise.
//
// The reason is provenance, not hygiene: the CLI fills every unset variable
// from the nearest `.env` walking up from the working directory
// (cmd/iterion applyDotEnv), a file that sits in the repository under review,
// and #1945 already stopped that file from choosing which executable the
// launcher starts. The child's own environment was the way left in.
//
// Declared names win over everything: the server's `env:` is the operator's
// own statement about this server.
func stdioChildEnv(declared map[string]string) stdioEnvDecision {
	named, malformed, all := parsePassthroughNames(strings.TrimSpace(envtrust.Inherited(EnvServerEnvPassthrough)))
	if all {
		merged := environMap(os.Environ())
		// The server's own `env:` on top, so a declared override wins
		// instead of being a duplicate appended after the inherited value —
		// execve does not dedup and Go's docs leave duplicate resolution
		// implementation-defined.
		maps.Copy(merged, declared)
		return stdioEnvDecision{
			Env: renderEnv(merged),
			Warning: EnvServerEnvPassthrough + " names " + envPassthroughAll +
				": the server inherits the launcher's whole environment, including the names a project `.env` planted",
		}
	}

	merged := make(map[string]string, len(stdioAmbientAllowlist)+len(named)+len(declared))
	for _, name := range stdioAmbientAllowlist {
		// Read as INHERITED, not live: a `.env` in the repository under
		// review does not get to answer what the launcher's own process
		// answers here. applyDotEnv only fills names that are UNSET, so a
		// real process's PATH and HOME are the operator's anyway — reading
		// them this way is what makes that a rule rather than a
		// coincidence.
		if v := envtrust.Inherited(name); v != "" {
			merged[name] = v
		}
	}
	for _, name := range named {
		// LIVE, not inherited: the operator named this one, and `.env` is
		// where iterion tells people to put run variables. Refusing the
		// planted value here would refuse the documented workflow the
		// escape hatch exists for; the hatch itself is read as inherited,
		// so the repository cannot name its own.
		if v := os.Getenv(name); v != "" {
			merged[name] = v
		}
	}
	maps.Copy(merged, declared)

	dec := stdioEnvDecision{Env: renderEnv(merged)}
	if len(named) > 0 {
		dec.Info = fmt.Sprintf("%s names %d ambient variable(s) the server also receives: %s",
			EnvServerEnvPassthrough, len(named), strings.Join(named, ","))
	}
	if len(malformed) > 0 {
		dec.Warning = fmt.Sprintf("%s: %d token(s) are not variable names and pass nothing — one name per comma: %q",
			EnvServerEnvPassthrough, len(malformed), malformed)
	}
	return dec
}

// parsePassthroughNames splits the knob's value into names, the tokens that
// are not names, and whether full inheritance was asked for.
//
// A token that could not be a variable name passes nothing, and saying so
// beats dropping it silently: the operator who wrote `NODE_OPTIONS=--x` there
// would otherwise read the strict default as a bug. `all` is honoured
// wherever it appears in the list, not only alone: `all,NODE_OPTIONS` asks
// for everything, and reading it as the name of a variable called `all` would
// silently give less than the operator asked for.
func parsePassthroughNames(spec string) (names, malformed []string, all bool) {
	seen := map[string]bool{}
	for _, raw := range strings.Split(spec, ",") {
		token := strings.TrimSpace(raw)
		if token == "" {
			continue
		}
		if strings.EqualFold(token, envPassthroughAll) {
			all = true
			continue
		}
		if !envtrust.Exportable(token) {
			malformed = append(malformed, token)
			continue
		}
		if seen[envtrust.CanonicalName(token)] {
			continue
		}
		seen[envtrust.CanonicalName(token)] = true
		names = append(names, token)
	}
	return names, malformed, all
}

// environMap reads a `NAME=value` list into a map. A line with no `=`, or one
// starting with `=`, is not a variable and is dropped.
func environMap(environ []string) map[string]string {
	out := make(map[string]string, len(environ))
	for _, kv := range environ {
		if i := strings.IndexByte(kv, '='); i > 0 {
			out[kv[:i]] = kv[i+1:]
		}
	}
	return out
}

// renderEnv is the `NAME=value` list, sorted.
func renderEnv(merged map[string]string) []string {
	out := make([]string, 0, len(merged))
	for k, v := range merged {
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return out
}
