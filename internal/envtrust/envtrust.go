// Package envtrust keeps the value of the variables that select iterion's
// control plane as they were INHERITED — before a project `.env` could fill
// in the ones the operator left unset.
//
// The CLI auto-loads the nearest `.env` walking up from the working directory
// and sets every key that is not already in the environment (cmd/iterion:
// applyDotEnv). That is a convenience for API keys next to a project, but it
// also means a file inside a repository can answer questions whose answer is
// supposed to be the operator's: where plugins are installed, which ones are
// enabled. A repository under review is not the operator.
//
// So the answer to "did the OPERATOR say this?" reads the snapshot, never the
// live environment. Everything else keeps reading the live environment: a
// dotenv-selected iterion home still works exactly as before; it simply does
// not confer the operator's authority.
package envtrust

import (
	"os"
	"sync"
)

// ControlPlaneNames are the variables whose inherited value decides whether
// something carries operator authority. ITERION_HOME selects the plugin root
// (store.GlobalIterionDataDir); HOME is its fallback.
var ControlPlaneNames = []string{"ITERION_HOME", "HOME"}

var (
	mu    sync.RWMutex
	taken bool
	snap  map[string]string
)

// SnapshotControlPlane records ControlPlaneNames as they stand now. It must
// run before any project `.env` is applied — applyDotEnv's caller does it, so
// a dotenv cannot be loaded without a snapshot existing first.
//
// The first call wins: a later one cannot overwrite the inherited truth with
// values a dotenv has since planted.
func SnapshotControlPlane() {
	mu.Lock()
	defer mu.Unlock()
	if taken {
		return
	}
	snap = make(map[string]string, len(ControlPlaneNames))
	for _, name := range ControlPlaneNames {
		if v, ok := os.LookupEnv(name); ok {
			snap[name] = v
		}
	}
	taken = true
}

// Inherited returns name's value as inherited by the process.
//
// With no snapshot taken, no dotenv has been applied either — nothing can have
// altered the environment behind the operator's back — so the live value IS
// the inherited one. That is the case for every host that embeds the engine as
// a library, and for tests.
func Inherited(name string) string {
	mu.RLock()
	defer mu.RUnlock()
	if !taken {
		return os.Getenv(name)
	}
	return snap[name]
}

// ResetForTest clears the snapshot so a test can take its own. TESTS ONLY:
// production takes exactly one snapshot, before the first dotenv line is read.
// A test that wants to stage "the operator inherited X, a dotenv then planted
// Y" resets, sets X, snapshots, then sets Y.
func ResetForTest() {
	mu.Lock()
	defer mu.Unlock()
	taken = false
	snap = nil
}
