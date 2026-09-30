package delegate

import (
	"strings"
	"sync"
)

// forfaitFirst records, per backend, the providers for which the backend
// spends the run's subscription (an OAuth forfait) BEFORE a key a shared tier
// sealed only for the route (secrets.RunBundle.PinnedAPIKeys). Each backend
// declares it beside the precedence that makes it true, so a new backend
// states its own behaviour instead of joining a switch in the core.
var (
	forfaitFirstMu sync.RWMutex
	forfaitFirst   = map[string]map[string]bool{}
)

// RegisterForfaitFirst declares that `backend`, on a route to `provider`,
// spends the run's forfait before a key pinned for that route. The credential
// accounting relies on it: a pinned key whose every route is served by such a
// consumer, beside a sealed forfait of its provider, is never spent.
func RegisterForfaitFirst(backend, provider string) {
	forfaitFirstMu.Lock()
	defer forfaitFirstMu.Unlock()
	b := strings.ToLower(strings.TrimSpace(backend))
	if forfaitFirst[b] == nil {
		forfaitFirst[b] = map[string]bool{}
	}
	forfaitFirst[b][strings.ToLower(strings.TrimSpace(provider))] = true
}

// ForfaitFirst reports whether `backend` declared RegisterForfaitFirst for
// `provider`. An unknown backend ("" — auto-detected at dispatch) never has.
func ForfaitFirst(backend, provider string) bool {
	forfaitFirstMu.RLock()
	defer forfaitFirstMu.RUnlock()
	return forfaitFirst[strings.ToLower(strings.TrimSpace(backend))][strings.ToLower(strings.TrimSpace(provider))]
}
