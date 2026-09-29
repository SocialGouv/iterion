// Package envtrust tells apart the environment iterion INHERITED from the
// part a project `.env` filled in.
//
// The CLI auto-loads the nearest `.env` walking up from the working directory
// and sets every key that is not already set (cmd/iterion: applyDotEnv). That
// is a convenience for API keys next to a project, and it is also a file
// inside a repository — including one under review — answering questions whose
// answer is supposed to be the operator's: where plugins are installed, which
// ones are enabled, what an enabled plugin is configured with, whether a
// workflow-controlled MCP config may read this process's environment.
//
// So everything keeps reading the live environment, exactly as before; what
// changes is that a decision about AUTHORITY asks whether the value was
// planted. A planted value still works — it simply does not speak for the
// operator.
//
// # Across processes
//
// applyDotEnv plants with os.Setenv, so the value is in os.Environ() for
// every child iterion spawns (`iterion run --background`, the operator MCP
// server). A child re-deriving "inherited" from its own environment would
// find the planted value indistinguishable from an operator's — the
// repository's answer laundered through one fork. The provenance therefore
// travels with the value: the planted NAMES are exported in
// EnvPlantedNames, and a child adds them to its own planted set before
// answering anything.
//
// The marker is not a secret and does not need to be unforgeable: listing a
// name can only REMOVE trust from it. A process that arrives with a forged
// marker distrusts more than it must, which is the safe direction.
package envtrust

import (
	"os"
	"strings"
	"sync"
)

// EnvPlantedNames carries the names a project `.env` filled in, so a child
// process does not mistake them for the operator's own environment.
const EnvPlantedNames = "ITERION_DOTENV_PLANTED"

var (
	mu      sync.RWMutex
	planted map[string]bool
)

// MarkPlanted records that these variables were set from a project `.env`,
// and re-exports the marker so children of this process know too.
func MarkPlanted(names ...string) {
	if len(names) == 0 {
		return
	}
	mu.Lock()
	ensurePlantedLocked()
	for _, name := range names {
		planted[name] = true
	}
	marker := markerLocked()
	mu.Unlock()
	// Best effort: a child that does not receive the marker re-derives the
	// answer from its own environment, which is what happened before this
	// existed. Failing the run over it would be worse than the narrowing.
	_ = os.Setenv(EnvPlantedNames, marker)
}

// Planted reports whether this variable's value came from a project `.env`
// (in this process or in an ancestor).
func Planted(name string) bool {
	mu.RLock()
	if planted != nil {
		defer mu.RUnlock()
		return planted[name]
	}
	mu.RUnlock()

	mu.Lock()
	defer mu.Unlock()
	ensurePlantedLocked()
	return planted[name]
}

// Inherited returns the variable's value as the process INHERITED it: the
// live value, or the empty string when a project `.env` is what set it.
//
// The empty string means "the operator said nothing about this", which every
// caller must read as the closed answer rather than as a default.
func Inherited(name string) string {
	if Planted(name) {
		return ""
	}
	return os.Getenv(name)
}

// ensurePlantedLocked seeds the set from the marker an ancestor exported.
// Caller holds mu for writing.
func ensurePlantedLocked() {
	if planted != nil {
		return
	}
	planted = map[string]bool{}
	for _, name := range strings.Split(os.Getenv(EnvPlantedNames), ",") {
		if name = strings.TrimSpace(name); name != "" {
			planted[name] = true
		}
	}
}

// markerLocked renders the set for the child environment. Caller holds mu.
func markerLocked() string {
	names := make([]string, 0, len(planted))
	for name := range planted {
		names = append(names, name)
	}
	// Sorted so the variable a child inherits does not change from run to run
	// over one map's iteration order.
	sortStrings(names)
	return strings.Join(names, ",")
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// ResetForTest clears the planted set (and re-reads it from the marker on the
// next question). TESTS ONLY.
func ResetForTest() {
	mu.Lock()
	defer mu.Unlock()
	planted = nil
}
