// Package projectenv builds immutable per-project process environments for a
// unified local Studio. Dotenv evaluation happens in a child shell so shell
// expansion stays compatible with the legacy instance manager without ever
// mutating the long-lived Studio process.
//
// The studio lane is a dotenv LOADER like the CLI's: the values it takes from
// the project `.env` are applied to every host-spawned command of a run
// (server.Config.RunEnv → executor.SetRunExtraEnv), so Snapshot asks the
// same questions the CLI's applyDotEnv asks at its own plant — the routing
// class and the operator grade (envtrust.DotenvPlantRefused), and command
// substitution in a value (envtrust.HasCommandSubstitution) — with the
// refusal going to stderr directly. What the file DOES plant carries the
// provenance mark in the returned environment, so a run's own envtrust reads
// it planted instead of taking it for the operator's. The refusal class and
// the message are envtrust's (DotenvPlantRefused, RoutingRefusalLine): one
// class, one wording, every lane that plants.
//
// The plant set is the PARSE now (internal/dotenv, the same primitive the
// CLI and the desktop loaders read): the file is never handed to bash as
// bytes, so a command substitution is refused at the plant instead of run
// by the sourcing shell, and an unquoted value with spaces is one value,
// like the CLI's parser reads it — the H-10 review measured the sourcing
// shell executing `FOO=$(touch …)` on the host before the routing question
// was even asked, and silently dropping `VAR=ssh -i key`. bash keeps one
// job: expanding `$VAR`/`${VAR}` in the surviving values on the child's
// base environment — what the legacy frame wanted from sourcing. Everything
// else about a value is literal.
package projectenv

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"

	"github.com/SocialGouv/iterion/internal/dotenv"
	"github.com/SocialGouv/iterion/internal/envtrust"
)

var reserved = map[string]struct{}{
	"HOME": {}, "ITERION_HOME": {}, "XDG_CONFIG_HOME": {},
	"ITERION_PROJECTS_CONFIG": {}, "ITERION_INSTANCES_CONFIG": {},
	"ITERION_INSTANCES_STATE": {}, "ITERION_STUDIO_INSECURE_NONLOOPBACK": {},
	"BIND": {}, "PORT": {},
}

// shellDoubleQuoted renders value inside one bash double-quoted string with
// the sourcing shell's own semantics for everything bash treats specially
// in one: a `"` (bare or escaped in the file) cannot close the rendering, a
// doubled `\\` yields one backslash, a `\\$` yields the literal dollar, a
// lone backslash escapes the character it preceded (and goes), and a bare
// `$` survives raw to expand — the `$VAR`/`${VAR}` the legacy frame wanted.
// Command substitution can never be in the value: the loader refused `$(`,
// backticks and their quoted spellings before the script was built, and a
// value the file spelled single-quoted takes the literal path instead.
func shellDoubleQuoted(value string) string {
	var b strings.Builder
	b.WriteByte('"')
	i := 0
	for i < len(value) {
		c := value[i]
		if c != '\\' {
			i++
			switch c {
			case '"':
				b.WriteString(`\"`)
			case '$':
				b.WriteByte('$') // raw: the expansion the legacy frame wanted
			default:
				b.WriteByte(c)
			}
			continue
		}
		n := 0
		for i < len(value) && value[i] == '\\' {
			n++
			i++
		}
		for j := 0; j < n/2; j++ {
			b.WriteString(`\\`)
		}
		if n%2 == 1 && i < len(value) {
			// The file's last backslash escapes what follows; the sourcing
			// shell's result is what lands in the value.
			switch value[i] {
			case '$':
				b.WriteString(`\$`) // the literal dollar the file escaped
			case '"':
				b.WriteString(`\"`) // the literal quote the file escaped
			default:
				b.WriteByte(value[i]) // the escaped character, backslash gone
			}
			i++
		}
	}
	b.WriteByte('"')
	return b.String()
}

// shellSingleQuoted renders a value the file spelled inside '...' — bytes
// the sourcing shell would have kept LITERAL — inside a bash single-quoted
// string, where nothing expands.
func shellSingleQuoted(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}

// Snapshot returns the effective environment for envFile layered over base.
// An empty or missing envFile returns a clone of base. The returned KEY=value
// slice is sorted, immutable by convention, and safe to append to os/exec.Cmd.
//
// The file is PARSED, not sourced: internal/dotenv reads it (the same
// primitive the CLI's applyDotEnv and the desktop env-file loader use), the
// refusals are decided on the parsed entries — routing/operator-grade names
// by envtrust.DotenvPlantRefused, command substitution by
// envtrust.HasCommandSubstitution — and only then does a child bash see the
// surviving values, to expand `$VAR`/`${VAR}` (the legacy frame) on its base
// environment. The child receives only base and the constructed script — no
// file bytes are interpreted by it, so nothing in the file runs. A refused
// entry restores the operator's own value where they exported one and does
// not exist where they did not; every refusal names the file, the key and
// (routing class alone) the opt-in, on stderr — the log level a logger
// would honour is itself something this file may have planted. Whatever the
// file DOES plant travels with the envtrust provenance marker (merged onto
// the marker the studio itself inherited), so a run that inherits this
// environment reads those values as planted, never as the operator's own.
// The opt-in itself never crosses into the returned environment: a line in
// the file grants nothing, and the operator's export answers the studio
// process, not the runs it launches.
func Snapshot(base []string, envFile string) ([]string, error) {
	baseMap := toMap(base)
	if envFile == "" {
		return fromMap(baseMap), nil
	}
	if info, err := os.Stat(envFile); err != nil {
		if os.IsNotExist(err) {
			return fromMap(baseMap), nil
		}
		return nil, fmt.Errorf("project env %s: %w", envFile, err)
	} else if info.IsDir() {
		return nil, fmt.Errorf("project env %s is a directory", envFile)
	}

	// One parse, one plant set. Later lines win, like the sourcing shell's
	// assignments did. The order is the file's, so a value's `$VAR` finds
	// the variable the file defined earlier and the diagnostics read the
	// same on every start.
	entries, err := dotenv.Parse(envFile)
	if err != nil {
		return nil, fmt.Errorf("project env %s: %w", envFile, err)
	}
	folded := make(map[string]dotenv.Entry, len(entries))
	var order []string
	for _, entry := range entries {
		if _, seen := folded[entry.Key]; !seen {
			order = append(order, entry.Key)
		}
		folded[entry.Key] = entry
	}

	// The refusals are decided HERE, on the parsed entries, before any shell
	// exists: a refused entry restores the operator's own value (base)
	// where they exported one and does not exist where they did not — a
	// refused name is never planted either. The marker line is not a value
	// a repository gets to declare (the mark is what a loader RECORDS, and
	// the studio's inherited marker travels on).
	effective := make(map[string]string, len(baseMap)+len(order))
	for key, value := range baseMap {
		effective[key] = value
	}
	var script []string
	var plantedNames []string
	for _, key := range order {
		entry := folded[key]
		if envtrust.CanonicalName(key) == envtrust.EnvPlantedNames {
			continue // the inherited mark travels; the file's spelling never does
		}
		baseValue, inherited := baseMap[key]
		if envtrust.DotenvPlantRefused(key) {
			// Not applied: the operator's own value survives the file's
			// attempt to take it, and a name only the file carried does not
			// exist — a refused name is not planted either.
			if inherited {
				effective[key] = baseValue
			} else {
				delete(effective, key)
			}
			fmt.Fprintln(os.Stderr, "iterion: "+envtrust.RoutingRefusalLine(key, "the project `.env`", envFile))
			continue
		}
		if envtrust.HasCommandSubstitution(entry.Value) {
			// Nothing of ours evaluates a value — but it reaches a run one
			// fork away, where a shell might — so the trigger is refused,
			// not planted: the same line, the same reasoning, every lane.
			if inherited {
				effective[key] = baseValue
			} else {
				delete(effective, key)
			}
			fmt.Fprintln(os.Stderr, "iterion: "+envtrust.CommandSubstitutionRefusalLine(key, "the project `.env`", envFile))
			continue
		}
		// A plant. Values the file spelled single-quoted stay literal;
		// everything else is rendered double-quoted for the child bash,
		// where `$VAR`/`${VAR}` expand on the base environment and nothing
		// else can: `$( ` and backticks were refused above, and the
		// rendering pins the value's other bytes.
		line := "export " + key + "=" + shellDoubleQuoted(entry.Value)
		if entry.SingleQuoted {
			line = "export " + key + "=" + shellSingleQuoted(entry.Value)
		}
		script = append(script, line)
		plantedNames = append(plantedNames, key)
	}

	// bash expands the surviving values, in file order, on the child's base
	// environment; the child receives the constructed script — never the
	// file's bytes — so nothing in the file runs. Only the PLANT names are
	// read back: the shell's own exports (PWD, SHLVL, OLDPWD, _) never
	// enter the result, and a line naming one is a plant like any other.
	if len(script) > 0 {
		cmd := exec.Command("bash", "--noprofile", "--norc", "-s")
		cmd.Env = fromMap(baseMap)
		cmd.Stdin = strings.NewReader(strings.Join(script, "\n") + "\nenv -0\n")
		raw, err := cmd.Output()
		if err != nil {
			if exitErr, isExit := err.(*exec.ExitError); isExit {
				return nil, fmt.Errorf("project env %s: shell evaluation failed: %s", envFile, strings.TrimSpace(string(exitErr.Stderr)))
			}
			return nil, fmt.Errorf("project env %s: %w", envFile, err)
		}
		for _, field := range bytes.Split(raw, []byte{0}) {
			if len(field) == 0 {
				continue
			}
			k, v, ok := strings.Cut(string(field), "=")
			if ok && k != "" {
				if _, isPlant := folded[k]; isPlant {
					effective[k] = v
				}
			}
		}
	}
	for key := range reserved {
		if effective[key] != baseMap[key] {
			return nil, fmt.Errorf("project env %s may not override reserved variable %s", envFile, key)
		}
	}
	if _, carried := effective[envtrust.DotenvEndpointsOptIn]; carried {
		delete(effective, envtrust.DotenvEndpointsOptIn)
		fmt.Fprintf(os.Stderr, "iterion: %s does not cross into a run's environment: a `.env` line naming it grants nothing, and the operator's export answers the studio process, not the runs it launches\n", envtrust.DotenvEndpointsOptIn)
	}
	if len(plantedNames) > 0 {
		effective[envtrust.EnvPlantedNames] = envtrust.MergePlantedMarker(baseMap[envtrust.EnvPlantedNames], plantedNames...)
	}
	return fromMap(effective), nil
}
func toMap(env []string) map[string]string {
	out := make(map[string]string, len(env))
	for _, item := range env {
		key, value, ok := strings.Cut(item, "=")
		if ok && key != "" {
			out[key] = value
		}
	}
	return out
}

func fromMap(env map[string]string) []string {
	keys := make([]string, 0, len(env))
	for key := range env {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, key := range keys {
		out = append(out, key+"="+env[key])
	}
	return out
}
