package model

import (
	"context"
	"os"
	"sort"
)

// The env seam (#2808): every environment read of this package goes
// through envGetenv / envLookupEnv / envEnviron, which answer from the
// environment the CONTEXT carries (withCarriedEnv — a run's envelope, a
// test's isolation) or, absent one, from the process. The #1804 doctrine:
// a bound rides the task envelope rather than the process env — and a
// test that runs in parallel CANNOT t.Setenv (the testing package
// panics), so per-test isolation comes from the carried environment.
//
// The seam is three package FUNCTIONS, not an interface: the sandbox
// env-read guard (claw_sandbox_env_class_test.go) scans calls statically
// and a method reached through an interface would go unseen — the
// function form keeps every read greppable at its call site, constant
// name included.
//
// The three shapes cover what the package reads: a named value (Getenv),
// the planted-vs-unset distinction (LookupEnv — a name set to empty is
// not the name absent), and the whole environment (Environ) for the
// whole-env readers, secretguard's ambient-secret enumeration among them
// — which is why the seam is a security surface, not only a refactor: a
// carried environment that answers empty where redaction expects a value
// is a secret printed in a log.

// carriedEnv is the environment a context carries: a fixed map, HERMETIC
// — a name the map does not hold reads as unset, never as the process
// value. Falling back to the process would let a "parallel" test read
// whatever the host (or a sibling test's leak) holds, which is exactly
// the interference the seam exists to close. Treat the map as read-only
// once carried.
type carriedEnv map[string]string

// carriedEnvKey keys the carried environment; unexported, so the only way
// in is withCarriedEnv.
type carriedEnvKey struct{}

// withCarriedEnv carries env on the context. Callers today: the seam's
// tests; the run-envelope wiring that makes a run read its own envelope
// is the next slices' work (#2808). A nil env is a no-op (the process
// stays the source); an empty non-nil map is a deliberately empty
// environment.
func withCarriedEnv(ctx context.Context, env carriedEnv) context.Context {
	if env == nil {
		return ctx
	}
	return context.WithValue(ctx, carriedEnvKey{}, env)
}

// carriedEnvFromContext returns the carried environment, or nil for the
// process default.
func carriedEnvFromContext(ctx context.Context) carriedEnv {
	if env, ok := ctx.Value(carriedEnvKey{}).(carriedEnv); ok && env != nil {
		return env
	}
	return nil
}

// envGetenv reads name from the carried environment, else the process.
func envGetenv(ctx context.Context, name string) string {
	if env := carriedEnvFromContext(ctx); env != nil {
		return env[name]
	}
	return os.Getenv(name)
}

// envLookupEnv is envGetenv with the planted-vs-unset distinction.
func envLookupEnv(ctx context.Context, name string) (string, bool) {
	if env := carriedEnvFromContext(ctx); env != nil {
		v, ok := env[name]
		return v, ok
	}
	return os.LookupEnv(name)
}

// envEnviron enumerates the carried environment in os.Environ's shape —
// sorted, so a consumer's output is deterministic across runs (map
// iteration order is not) — else the process's.
func envEnviron(ctx context.Context) []string {
	env := carriedEnvFromContext(ctx)
	if env == nil {
		return os.Environ()
	}
	out := make([]string, 0, len(env))
	for name, val := range env {
		out = append(out, name+"="+val)
	}
	sort.Strings(out)
	return out
}
