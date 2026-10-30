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
//
// # Who loads a `.env` at all
//
// The convenience is the CLI's, so it is the human invocation's: a process
// the engine spawned against a workspace already holds the environment the
// engine handed it, and reading the repository's `.env` on top would let the
// tree under review fill in the names nobody set on purpose. EnvEngineChild
// is the invocation-class marker the engine writes on such a child, and
// EnvDotEnvForce the operator's way back in.
package envtrust

import (
	"os"
	"runtime"
	"strings"
	"sync"
	"testing"
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
		planted[CanonicalName(name)] = true
	}
	marker := markerLocked()
	mu.Unlock()
	// Best effort: a child that does not receive the marker re-derives the
	// answer from its own environment, which is what happened before this
	// existed. Failing the run over it would be worse than the narrowing.
	_ = os.Setenv(EnvPlantedNames, marker)
}

// Exportable reports whether a name is one a shell could export, which is
// the only kind the marker can carry — and the only kind a `.env` may plant
// (cmd/iterion applyDotEnv refuses any other key): a name only ASCII spells is
// also the only kind CanonicalName folds exactly as Windows does.
//
// The comma was the first character found to cross badly — the marker joins
// on it and the reader splits on it — but it is not the class. The reader
// also TrimSpaces each name while the writer does not, so " ITERION_HOME"
// arrives in a child as a different name than it left as, denying the
// operator's own home its authority there. One predicate, stated the way the
// comma guard justified itself: nothing iterion asks about provenance is
// spelled any other way.
func Exportable(name string) bool {
	if name == "" {
		return false
	}
	for i, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r == '_':
		case i > 0 && r >= '0' && r <= '9':
		default:
			return false
		}
	}
	return true
}

// Planted reports whether this variable's value came from a project `.env`
// (in this process or in an ancestor).
func Planted(name string) bool {
	mu.RLock()
	if planted != nil {
		defer mu.RUnlock()
		return planted[CanonicalName(name)]
	}
	mu.RUnlock()

	mu.Lock()
	defer mu.Unlock()
	ensurePlantedLocked()
	return planted[CanonicalName(name)]
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

// MergePlantedMarker renders the EnvPlantedNames value for a process whose
// inherited marker read baseMarker, with names added. The studio's project
// snapshot plants a `.env`'s values straight into a CHILD's environment —
// the returned slice, not this process's own — so the mark has to travel in
// the bytes rather than through MarkPlanted, which would record names this
// process does not even carry. The rules are the marker's own: names the
// marker cannot carry (not Exportable) are dropped, names already in the
// inherited marker are kept once, the result is sorted so a child inherits
// the same string twice in a row. Empty when nothing survives to carry.
func MergePlantedMarker(baseMarker string, names ...string) string {
	set := map[string]bool{}
	for _, name := range strings.Split(baseMarker, ",") {
		if name = strings.TrimSpace(name); name != "" {
			set[CanonicalName(name)] = true
		}
	}
	for _, name := range names {
		set[CanonicalName(name)] = true
	}
	merged := make([]string, 0, len(set))
	for name := range set {
		if Exportable(name) {
			merged = append(merged, name)
		}
	}
	if len(merged) == 0 {
		return ""
	}
	sortStrings(merged)
	return strings.Join(merged, ",")
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
			planted[CanonicalName(name)] = true
		}
	}
}

// markerLocked renders the set for the child environment. Caller holds mu.
//
// A name carrying the separator is left OUT rather than written: the marker
// is a comma-joined list with no escaping, so such a name reaches the child
// as two, and the second — `ITERION_HOME,HARMLESS` splitting into
// `ITERION_HOME` — strips the OPERATOR's own home of its authority in every
// descendant. That is not an escalation, it is an injectable denial of the
// operator's own capability, with no diagnostic anywhere. Nothing iterion
// asks about provenance has a comma in its name, so dropping it costs
// nothing; applyDotEnv refuses such a key outright, and this is the backstop
// for every other caller.
func markerLocked() string {
	names := make([]string, 0, len(planted))
	for name := range planted {
		if !Exportable(name) {
			continue
		}
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

// goos is the platform whose variable naming the planted set follows.
var goos = runtime.GOOS

// CanonicalName is the spelling the planted set records a variable under. On
// Windows environment variable names are case-insensitive — a `.env` line
// `iterion_home=…` sets ITERION_HOME — so the set records the upper-case name
// and a question in any case finds it.
func CanonicalName(name string) string {
	if goos == "windows" {
		return strings.ToUpper(name)
	}
	return name
}

// UseGOOSForTest makes the planted set follow platform's variable naming for
// the rest of tb, and clears the set before and after. TESTS ONLY.
func UseGOOSForTest(tb testing.TB, platform string) {
	tb.Helper()
	prev := goos
	goos = platform
	ResetForTest()
	tb.Cleanup(func() {
		goos = prev
		ResetForTest()
	})
}

// ResetForTest clears the planted set (and re-reads it from the marker on the
// next question). TESTS ONLY.
func ResetForTest() {
	mu.Lock()
	defer mu.Unlock()
	planted = nil
}

// EnvEngineChild marks a process the engine spawned against a workspace: a
// tool node's command or script, the in-container claw runner, a CLI agent —
// and, by inheritance, everything they spawn in turn, including an iterion
// that re-enters the CLI (sandbox.EngineBinaryEnvVar names the binary for
// it).
//
// Such a process is handed its environment by the engine that spawned it. It
// must not ALSO read the `.env` of the workspace it was pointed at: that file
// belongs to the repository under review, and the names it would fill in are
// the ones nobody set on purpose — the provider endpoints a credential is
// spent against first among them. The CLI convenience it gives up is the
// operator's, and the operator's own `.env` already reached this process the
// way every other variable did, through the environment its parent passed on.
//
// Like EnvPlantedNames the marker is not a secret and does not need to be
// unforgeable: carrying it can only COST the process the convenience, which
// is the safe direction.
const EnvEngineChild = "ITERION_ENGINE_CHILD"

// EnvDotEnvForce is the named escape hatch on that skip: an operator who
// wants engine children to honour the workspace `.env` too sets it to 1 or
// true in the environment they launch from.
//
// Only an INHERITED value counts (Inherited, not os.Getenv): a `.env` that
// voted itself back in would be the hole with an extra step, and a stored bot
// var cannot spell the name at all (pkg/platformcfg botVarsInfraExact).
const EnvDotEnvForce = "ITERION_DOTENV_FORCE"

// EngineChildEnvEntry is the KEY=value entry an engine hands every child it
// spawns against a workspace, in the shape exec.Cmd.Env and the sandbox
// ExecOpts both take.
func EngineChildEnvEntry() string { return EnvEngineChild + "=1" }

// SpawnedByEngine reports whether this process arrived with the engine's
// invocation-class marker — set by the engine on the child, so a nested
// iterion knows it is not a human's CLI call.
func SpawnedByEngine() bool { return os.Getenv(EnvEngineChild) != "" }

// DotEnvForced reports whether the operator asked for the workspace `.env`
// to be loaded even where this package says to skip it.
func DotEnvForced() bool {
	v := Inherited(EnvDotEnvForce)
	return v == "1" || strings.EqualFold(v, "true")
}
