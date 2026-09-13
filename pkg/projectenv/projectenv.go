// Package projectenv builds immutable per-project process environments for a
// unified local Studio. Dotenv evaluation happens in a child shell so shell
// expansion stays compatible with the legacy instance manager without ever
// mutating the long-lived Studio process.
package projectenv

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
)

var reserved = map[string]struct{}{
	"HOME": {}, "ITERION_HOME": {}, "XDG_CONFIG_HOME": {},
	"ITERION_PROJECTS_CONFIG": {}, "ITERION_INSTANCES_CONFIG": {},
	"ITERION_INSTANCES_STATE": {}, "ITERION_STUDIO_INSECURE_NONLOOPBACK": {},
	"BIND": {}, "PORT": {},
}

// Snapshot returns the effective environment for envFile layered over base.
// An empty or missing envFile returns a clone of base. The returned KEY=value
// slice is sorted, immutable by convention, and safe to append to os/exec.Cmd.
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
