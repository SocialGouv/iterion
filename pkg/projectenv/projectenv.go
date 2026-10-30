// Package projectenv builds immutable per-project process environments for a
// unified local Studio. Dotenv evaluation happens in a child shell so shell
// expansion stays compatible with the legacy instance manager without ever
// mutating the long-lived Studio process.
//
// The studio lane is a dotenv LOADER like the CLI's: the values it takes from
// the project `.env` are applied to every host-spawned command of a run
// (server.Config.RunEnv → executor.SetRunExtraEnv), so Snapshot asks the same
// question the CLI's applyDotEnv asks at its own plant — a routing-shaped
// name is not applied unless the operator's inherited environment opted in —
// and what the file DOES plant carries the provenance mark in the returned
// environment, so a run's own envtrust reads it planted instead of taking it
// for the operator's. The refusal class and the message are envtrust's
// (DotenvPlantRefused, RoutingRefusalLine): one class, one wording, every
// lane that plants.
package projectenv

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"

	"github.com/SocialGouv/iterion/internal/envtrust"
)

var reserved = map[string]struct{}{
	"HOME": {}, "ITERION_HOME": {}, "XDG_CONFIG_HOME": {},
	"ITERION_PROJECTS_CONFIG": {}, "ITERION_INSTANCES_CONFIG": {},
	"ITERION_INSTANCES_STATE": {}, "ITERION_STUDIO_INSECURE_NONLOOPBACK": {},
	"BIND": {}, "PORT": {},
}

// shellArtefacts are the variables the sourcing shell sets BY ITSELF: the
// bash child that evaluates the file exports its own PWD, SHLVL, OLDPWD
// and _ alongside the file's assignments (set -a exports everything). They
// are not the file's plants — no line named them — so no provenance
// question travels on them; they flow into the run environment as they
// always have.
var shellArtefacts = map[string]struct{}{
	"PWD": {}, "SHLVL": {}, "OLDPWD": {}, "_": {},
}

// Snapshot returns the effective environment for envFile layered over base.
// An empty or missing envFile returns a clone of base. The returned KEY=value
// slice is sorted, immutable by convention, and safe to append to os/exec.Cmd.
//
// A routing-shaped name the file plants (envtrust.RoutingShapedName) is not
// applied unless the environment the studio process INHERITED granted
// ITERION_DOTENV_ENDPOINTS=allow: the operator's own value, if they exported
// one, is restored in place of the file's line, and the refusal goes to
// stderr directly — the log level a logger would honour is itself something
// this file may have planted. Whatever the file DOES plant travels with the
// envtrust provenance marker (merged onto the marker the studio itself
// inherited), so a run that inherits this environment reads those values as
// planted, never as the operator's own. The opt-in itself never crosses into
// the returned environment: a line in the file grants nothing, and the
// operator's export answers the studio process, not the runs it launches.
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

	// Match the existing instance manager exactly: export all assignments,
	// source the project file in noninteractive bash, then capture NUL-delimited
	// output. The child receives only base and cannot contaminate this process.
	cmd := exec.Command("bash", "--noprofile", "--norc", "-c", `set -a; . "$1"; set +a; env -0`, "iterion-project-env", envFile)
	cmd.Env = fromMap(baseMap)
	raw, err := cmd.Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return nil, fmt.Errorf("project env %s: shell evaluation failed: %s", envFile, strings.TrimSpace(string(exitErr.Stderr)))
		}
		return nil, fmt.Errorf("project env %s: %w", envFile, err)
	}
	effective := make(map[string]string)
	for _, field := range bytes.Split(raw, []byte{0}) {
		if len(field) == 0 {
			continue
		}
		key, value, ok := strings.Cut(string(field), "=")
		if ok && key != "" {
			effective[key] = value
		}
	}
	for key := range reserved {
		if effective[key] != baseMap[key] {
			return nil, fmt.Errorf("project env %s may not override reserved variable %s", envFile, key)
		}
	}

	// What the file plants is what differs from base — the names changed or
	// introduced by its lines. Deterministic order, so the diagnostics read
	// the same on every start.
	keys := make([]string, 0, len(effective))
	for key := range effective {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	var plantedNames []string
	for _, key := range keys {
		if _, artefact := shellArtefacts[envtrust.CanonicalName(key)]; artefact {
			continue // the shell's own exports, not the file's plants
		}
		baseValue, inherited := baseMap[key]
		if inherited && baseValue == effective[key] {
			continue // the operator's own value, untouched by the file
		}
		switch {
		case envtrust.CanonicalName(key) == envtrust.EnvPlantedNames:
			// The mark is what a loader RECORDS, not what a file declares:
			// one line seeding it would strip the operator's own environment
			// of its authority in the run. The studio's inherited marker
			// travels on; the file's spelling of it never does.
			if inherited {
				effective[key] = baseValue
			} else {
				delete(effective, key)
			}
		case envtrust.DotenvPlantRefused(key):
			// Not applied: the operator's own route survives the file's
			// attempt to take it, and a name only the file carried is
			// dropped outright — a refused name does not exist.
			if inherited {
				effective[key] = baseValue
			} else {
				delete(effective, key)
			}
			fmt.Fprintln(os.Stderr, "iterion: "+envtrust.RoutingRefusalLine(key, "the project `.env`", envFile))
		default:
			plantedNames = append(plantedNames, key)
		}
	}
	// The opt-in never crosses to the run environment, whoever said it — the
	// file's line grants nothing, and the operator's export answered the
	// studio process (it decided the refusals above), not the runs launched
	// with what this returns.
	if _, carried := effective[envtrust.DotenvEndpointsOptIn]; carried {
		delete(effective, envtrust.DotenvEndpointsOptIn)
		fmt.Fprintf(os.Stderr, "iterion: %s does not cross into a run's environment: a `.env` line naming it grants nothing, and the operator's own export answers the studio process, not the runs it launches\n", envtrust.DotenvEndpointsOptIn)
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
