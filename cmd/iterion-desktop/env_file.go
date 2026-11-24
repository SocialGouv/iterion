package main

import (
	"bufio"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/SocialGouv/iterion/internal/dotenv"
	"github.com/SocialGouv/iterion/internal/envtrust"
)

// dotenvAppliedKeys tracks which keys were set into the process env by a
// previous applyDotenvFile call. Used by reloadIterionEnvFile so that
// commenting out a key in ~/.iterion/env and clicking Refresh actually
// clears the value (default os.Setenv-then-skip-if-present semantics
// would otherwise pin the original value for the life of the process).
//
// Keys passed in by the launching shell are NOT tracked here, so they
// survive a reload — shell wins remains the precedence rule.
var (
	dotenvMu          sync.Mutex
	dotenvAppliedKeys []string
)

// loadIterionEnvFile sources ~/.iterion/env (or ~/.iterion/.env as a
// fallback) into the current process's env BEFORE the studio server
// starts. Operators stash provider credentials there (OPENAI_API_KEY,
// AZURE_OPENAI_API_KEY, etc.) so iterion-desktop launched from a
// .desktop file (no shell to source ~/.bashrc) still has the keys
// available — and ClawBackend.executeViaSandboxRunner can then forward
// them into the sandbox runner.
//
// Existing env vars are NOT overwritten: a value already set on the
// process by the launching shell wins over the file. This matches
// dotenv-style precedence (.env is a default, not an override) and
// keeps `OPENAI_API_KEY=sk-... iterion-desktop` working as a one-shot
// override without editing the file.
//
// Failures are silent — the file is optional. Lookup order:
//
//  1. $ITERION_ENV_FILE (explicit override path)
//  2. $ITERION_HOME/env (when $ITERION_HOME is set)
//  3. ~/.iterion/env
//  4. ~/.iterion/.env (defensive: dotenv-style filename)
func loadIterionEnvFile() {
	for _, path := range candidateEnvFiles() {
		if path == "" {
			continue
		}
		if applyDotenvFile(path) {
			return
		}
	}
}

// ReloadIterionEnvFile is the refresh hook. Treats the dotenv file as
// the source of truth at refresh time: every key that appears in the
// file (active `KEY=value` OR commented `# KEY=...`) is unset, then
// the file is re-applied. This matters when the launching shell
// pre-loaded the same .env (e.g. direnv following an `~/.iterion/env`
// symlink into the project root) — in that case applyDotenvFile saw
// the key already in env at boot, "shell wins" skipped it, and the
// tracked-keys list alone wouldn't be enough to clear it.
//
// Keys not present in the file at all (e.g. a pure shell-passed
// override like `OPENAI_API_KEY=sk-... iterion-desktop`) are not
// touched — those remain owned by the shell.
func ReloadIterionEnvFile() {
	fileKeys := scanDotenvKeys()
	dotenvMu.Lock()
	previous := dotenvAppliedKeys
	dotenvAppliedKeys = nil
	dotenvMu.Unlock()
	toUnset := dedupeKeys(previous, fileKeys)
	for _, key := range toUnset {
		_ = os.Unsetenv(key)
	}
	loadIterionEnvFile()
	// One-shot visibility so the user can confirm in the iterion-desktop
	// log whether the refresh hook actually fired and what it touched.
	log.Printf("dotenv-reload: candidates=%v unset=%v applied=%v",
		candidateEnvFiles(), toUnset, dotenvAppliedKeys)
}

// scanDotenvKeys returns the set of keys mentioned in the first
// existing candidate env file — both active and commented-out lines.
// Used by ReloadIterionEnvFile to know which keys the file "claims"
// even when a previous applyDotenvFile call skipped them.
func scanDotenvKeys() []string {
	for _, path := range candidateEnvFiles() {
		if path == "" {
			continue
		}
		if keys, ok := readDotenvKeys(path); ok {
			return keys
		}
	}
	return nil
}

func readDotenvKeys(path string) ([]string, bool) {
	f, err := os.Open(path)
	if err != nil {
		return nil, false
	}
	defer f.Close()
	var out []string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		// Allow commented forms: "# KEY=...", "#KEY=...", "## KEY=...".
		// Strip leading #'s then re-trim.
		for strings.HasPrefix(line, "#") {
			line = strings.TrimSpace(strings.TrimPrefix(line, "#"))
		}
		if line == "" {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		eq := strings.IndexByte(line, '=')
		if eq <= 0 {
			continue
		}
		key := strings.TrimSpace(line[:eq])
		if key == "" {
			continue
		}
		out = append(out, key)
	}
	return out, true
}

func dedupeKeys(a, b []string) []string {
	seen := make(map[string]struct{}, len(a)+len(b))
	out := make([]string, 0, len(a)+len(b))
	for _, src := range [][]string{a, b} {
		for _, k := range src {
			if _, ok := seen[k]; ok {
				continue
			}
			seen[k] = struct{}{}
			out = append(out, k)
		}
	}
	return out
}

func candidateEnvFiles() []string {
	var paths []string
	if explicit := strings.TrimSpace(os.Getenv("ITERION_ENV_FILE")); explicit != "" {
		paths = append(paths, explicit)
	}
	if home := strings.TrimRight(os.Getenv("ITERION_HOME"), string(filepath.Separator)); home != "" {
		paths = append(paths, filepath.Join(home, "env"))
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		paths = append(paths,
			filepath.Join(home, ".iterion", "env"),
			filepath.Join(home, ".iterion", ".env"),
		)
	}
	return paths
}

// applyDotenvFile reads a KEY=value file and sets each key on the
// process env unless it's already set. Returns true when the file
// existed and was processed (even if empty or all-comments) so callers
// know to stop probing fallbacks. Comments and blank lines are
// ignored; values may be optionally double- or single-quoted, in which
// case the matching outer quotes are stripped. The parse is the ONE
// reader (internal/dotenv) the CLI and the studio snapshot share — the
// same bytes in, the same entries out, no lane's own dialect.
//
// The refusals are the same three every lane that plants asks, with
// envtrust's one predicate and message:
//
//   - the routing, execution and operator grades, one predicate
//     (envtrust.DotenvPlantRefused): the file may be operator-owned,
//     but it is also the file a direnv symlink can point at a PROJECT's
//     `.env` (ReloadIterionEnvFile's comment), and the class is the one
//     guard that holds whatever the path resolves to — and the
//     operator's home file does not outrank a bot's stored record, the
//     H-10 finding being exactly that the file did;
//
//   - command substitution in a value (envtrust.HasCommandSubstitution):
//     nothing here evaluates values, but the trigger does not travel
//     either — a refused value does not exist.
//
// What IS applied is not marked planted, on purpose: the file sits in the
// operator's home — the values it carries ARE the operator's own, and
// marking them would strip the documented desktop credential flow (the
// provider keys stashed here) of its authority at the belt's reads. The
// refusal goes to stderr directly, not through the
// level-configured logger: the level it would ride on is itself something
// this file may have planted a line above.
func applyDotenvFile(path string) bool {
	entries, err := dotenv.Parse(path)
	if err != nil {
		return false
	}
	for _, entry := range entries {
		key, val := entry.Key, entry.Value
		// The provenance marker is never taken from a file — the mark is
		// what a loader records, not what a file declares.
		if envtrust.CanonicalName(key) == envtrust.EnvPlantedNames {
			continue
		}
		// Honour an existing process env value (shell-passed vars
		// override the dotenv default).
		if _, present := os.LookupEnv(key); present {
			continue
		}
		// The routing, execution and operator grades, one predicate; then
		// a value spelling shell execution, refused like on every lane
		// that plants. What IS applied is not marked planted, on purpose —
		// see the function comment.
		if envtrust.DotenvPlantRefused(key) {
			fmt.Fprintln(os.Stderr, "iterion: "+envtrust.RoutingRefusalLine(key, "the dotenv file", path))
			continue
		}
		if envtrust.HasCommandSubstitution(val) {
			fmt.Fprintln(os.Stderr, "iterion: "+envtrust.CommandSubstitutionRefusalLine(key, "the dotenv file", path))
			continue
		}
		// This lane plants NO expansion: a $-reference stays the bytes the
		// file spelled, where the studio lane expands the same file (and a
		// child shell will not re-expand them either — a planted `$HOME/...`
		// execs a path literally named `$HOME/...`). The bytes are the
		// operator's to plant, so this warns instead of refusing;
		// single-quoting the value keeps it literal on every lane and
		// silences the line.
		if !entry.SingleQuoted && envtrust.CarriesExpansionReference(val) {
			fmt.Fprintln(os.Stderr, "iterion: "+envtrust.UnexpandedReferenceWarningLine(key, "the dotenv file", path))
		}
		if err := os.Setenv(key, val); err != nil {
			continue
		}
		dotenvMu.Lock()
		dotenvAppliedKeys = append(dotenvAppliedKeys, key)
		dotenvMu.Unlock()
	}
	return true
}
