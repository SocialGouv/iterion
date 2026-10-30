package runner

import (
	"sync/atomic"

	"github.com/SocialGouv/iterion/pkg/backend/secretguard"
)

// runSecretGuard is the late-bound handle between the runner's mid-run
// secret rotation loops and the run's live secret guard (#2756). The
// loops start BEFORE runview.BuildExecutor builds the guard (the clone's
// credential refresher is armed next to the clone, the executor after the
// sandbox), so they cannot receive it as a value: the runner creates the
// holder first, BuildExecutor delivers the guard through the
// ExecutorSpec.SecretGuardSink when it lands, and every rotation tick
// Loads the current guard — nil until then. A tick that fires before the
// executor builds registers nothing AND IS NOT REPLAYED: the refresher
// loops advance their `last` watermark once a rotation is delivered, so
// the same unchanged value never re-registers — the first tick of a
// rotation lands on the live guard only when it fires after the build
// (the build precedes the first node dispatch; the first tick is minutes
// away).
type runSecretGuard struct {
	g atomic.Pointer[secretguard.Guard]
}

// set is the ExecutorSpec.SecretGuardSink shape: a nil guard (redaction
// built to nothing — the kill switch, or no known value with the
// heuristic pass off) is not stored: there is nothing to register into.
func (h *runSecretGuard) set(g *secretguard.Guard) {
	if g != nil {
		h.g.Store(g)
	}
}

// Load returns the live guard, or nil before it lands (or when the run
// has none). Nil-receiver safe so a rotation site needs no guard clause.
func (h *runSecretGuard) Load() *secretguard.Guard {
	if h == nil {
		return nil
	}
	return h.g.Load()
}

// registerRotated registers a mid-run rotated credential on the live
// guard so the fresh value is scrubbed from every sink from this tick on
// — the launch-time value is otherwise the only one the known-literal
// layer ever matched. RedactOnly: for a name nothing declared, the
// placeholder resolves nowhere; for a DECLARED secret of the same name,
// Register carries the replaced registration's attributes over and the
// fresh value takes the materialisable slot (the declared placeholder
// keeps resolving — to the live value from now on). No-op when the guard
// has not landed yet or the run has none; the value is re-registered on a
// later tick only if it changed again (Register itself dedupes an
// already-known value).
func (h *runSecretGuard) registerRotated(name, value string) {
	if g := h.Load(); g != nil {
		g.Register(secretguard.Secret{Name: name, Value: value, RedactOnly: true})
	}
}

// secretGuardSinkOf is the ExecutorSpec.SecretGuardSink for a holder —
// nil when the caller carries none (tests that never arm a rotation loop).
func secretGuardSinkOf(h *runSecretGuard) func(*secretguard.Guard) {
	if h == nil {
		return nil
	}
	return h.set
}
