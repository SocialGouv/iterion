// Package projectenv builds immutable per-project process environments for a
// unified local Studio. Dotenv expansion happens in Go: the values the file
// survives with are expanded by expandReferences against the environment
// built so far, and no shell ever runs — the legacy instance manager's
// sourcing frame was the one lane that EXECUTED the file (the H-10 review
// measured `FOO=$(touch …)` running on the host), and the bash the
// parse-first tour kept for the `$VAR` expansion alone still executed a
// value the file never spelled (its prompt expansion ran the substitutions
// a base variable carried, `${BAR@P}`). Nothing of ours evaluates a value
// in a shell now; the belt refusal (envtrust.HasCommandSubstitution) keeps
// the trigger from travelling to a run's own shell tooling one fork away.
//
// The studio lane is a dotenv LOADER like the CLI's: the values it takes from
// the project `.env` are applied to every host-spawned command of a run
// (server.Config.RunEnv → executor.SetRunExtraEnv), so Snapshot asks the
// same questions the CLI's applyDotEnv asks at its own plant — the routing
// and execution classes and the operator grade (envtrust.DotenvPlantRefused),
// and shell execution spelled in a value (envtrust.HasCommandSubstitution) —
// with the refusal going to stderr directly. What the file DOES plant carries
// the provenance mark in the returned environment, so a run's own envtrust
// reads it planted instead of taking it for the operator's. The refusal
// class and the message are envtrust's (DotenvPlantRefused,
// RoutingRefusalLine): one class, one wording, every lane that plants.
//
// The plant set is the PARSE (internal/dotenv, the same primitive the CLI
// and the desktop loaders read): the file is never handed to a shell as
// bytes, and an unquoted value with spaces is one value, like the CLI's
// parser reads it. Expansion is the one thing this lane adds over the
// CLI's raw planting, and it is Go's now: `$VAR`/`${VAR}` — what the
// legacy frame wanted from sourcing, and the documented contract since the
// parse-first tour ("everything else about a value is literal", a contract
// the bash frame exceeded). Everything else about a value — an operator
// form, a default form, an escaped dollar's neighbour — is literal bytes,
// the same bytes the CLI lane plants.
package projectenv

import (
	"fmt"
	"os"
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

// isNameByte reports whether b may appear in a variable name (after the
// first byte, which only expandValue's caller has checked).
func isNameByte(b byte) bool {
	return b == '_' || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
}

// expandReferences expands the simple `$VAR` / `${VAR}` references the
// legacy frame wanted, in value, against lookup — the environment built so
// far, base first and the file's own earlier plants on top. Nothing else
// is touched, and the rules are stated here because this IS the lane's
// expansion contract now, not a shell's:
//
//   - `\$` yields one literal `$` and suppresses the expansion that would
//     have followed it;
//   - a backslash before any other byte is KEPT, both bytes literal — the
//     bytes the CLI lane plants, and what a shell keeps before a
//     non-special character too;
//   - `$$` is two literal dollars (a dotenv value is not a shell prompt;
//     the PID bash would put there is corruption, not a feature);
//   - `$NAME` and `${NAME}` — NAME spelled `[A-Za-z_][A-Za-z0-9_]*`, the
//     braced form carrying NOTHING but the name — expand to lookup's
//     value, empty when unset, like the sourcing shell's;
//   - every other `$` is one literal dollar: `${VAR:-default}`,
//     `${VAR@P}`, `$'…'` stay the bytes the file spelled — the operator
//     forms a shell would act on are exactly the ones this lane keeps
//     dead.
func expandReferences(value string, lookup func(name string) (string, bool)) string {
	var b strings.Builder
	for i := 0; i < len(value); i++ {
		c := value[i]
		switch {
		case c == '\\' && i+1 < len(value) && value[i+1] == '$':
			b.WriteByte('$')
			i++
		case c == '\\' && i+1 < len(value):
			b.WriteByte(c)
			b.WriteByte(value[i+1])
			i++
		case c == '$' && i+1 < len(value) && value[i+1] == '{':
			end := i + 2
			for end < len(value) && isNameByte(value[end]) {
				end++
			}
			if end > i+2 && end < len(value) && value[end] == '}' {
				name := value[i+2 : end]
				if v, ok := lookup(name); ok {
					b.WriteString(v)
				}
				i = end
				continue
			}
			b.WriteByte(c)
		case c == '$' && i+1 < len(value) && isNameStart(value[i+1]):
			end := i + 2
			for end < len(value) && isNameByte(value[end]) {
				end++
			}
			name := value[i+1 : end]
			if v, ok := lookup(name); ok {
				b.WriteString(v)
			}
			i = end - 1
		case c == '$' && i+1 < len(value) && value[i+1] == '$':
			b.WriteString("$$")
			i++
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// isNameStart reports whether b may begin a variable name.
func isNameStart(b byte) bool {
	return b == '_' || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

// Snapshot returns the effective environment for envFile layered over base.
// An empty or missing envFile returns a clone of base. The returned KEY=value
// slice is sorted, immutable by convention, and safe to append to os/exec.Cmd.
//
// The file is PARSED, never sourced: internal/dotenv reads it (the same
// primitive the CLI's applyDotEnv and the desktop env-file loader use), the
// refusals are decided on the parsed entries — routing/execution/operator-
// grade names by envtrust.DotenvPlantRefused, shell execution spelled in a
// value by envtrust.HasCommandSubstitution — and the surviving values are
// expanded in Go (expandReferences, `$VAR`/`${VAR}` on the environment built
// so far). No shell exists anywhere on this lane: a value the file spelled,
// a value an expansion produced and a base value the file references are
// bytes and Go strings end to end. A refused entry restores the operator's
// own value where they exported one and does not exist where they did not;
// every refusal names the file, the key and (the opt-in-able classes) the
// opt-in, on stderr — the log level a logger would honour is itself
// something this file may have planted. Whatever the file DOES plant travels
// with the envtrust provenance marker (merged onto the marker the studio
// itself inherited), so a run that inherits this environment reads those
// values as planted, never as the operator's own. The opt-in itself never
// crosses into the returned environment: a line in the file grants nothing,
// and the operator's export answers the studio process, not the runs it
// launches.
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

	// The refusals are decided HERE, on the parsed entries, before any
	// expansion: a refused entry restores the operator's own value (base)
	// where they exported one and does not exist where they did not — a
	// refused name is never planted either. The marker line is not a value
	// a repository gets to declare (the mark is what a loader RECORDS, and
	// the studio's inherited marker travels on).
	effective := make(map[string]string, len(baseMap)+len(order))
	for key, value := range baseMap {
		effective[key] = value
	}
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
		// A plant. Values the file spelled single-quoted stay the file's
		// bytes; everything else has its simple references expanded against
		// the environment built so far — base first, the file's earlier
		// plants on top, the current key's own base value for a
		// self-reference, like the sourcing shell's order. An expansion can
		// INTRODUCE a trigger the file never spelled (the base value a
		// `$VAR` pulled in may carry `$(…)` bytes a shell would act on one
		// fork away), so the belt asks the EXPANDED value too: a value
		// spelling execution is refused whatever road its bytes arrived by.
		expanded := entry.Value
		if !entry.SingleQuoted {
			expanded = expandReferences(entry.Value, func(name string) (string, bool) {
				v, ok := effective[name]
				return v, ok
			})
			if envtrust.HasCommandSubstitution(expanded) {
				if inherited {
					effective[key] = baseValue
				} else {
					delete(effective, key)
				}
				fmt.Fprintln(os.Stderr, "iterion: "+envtrust.CommandSubstitutionRefusalLine(key, "the project `.env`", envFile))
				continue
			}
		}
		effective[key] = expanded
		plantedNames = append(plantedNames, key)
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
