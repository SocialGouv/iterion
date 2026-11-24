package model

import (
	"context"

	"github.com/SocialGouv/iterion/pkg/backend/credroute"
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
//
// A reader with no context in scope calls the seam with
// context.Background(): the read stays process-scoped, and greppable,
// until an envelope reaches it. The complete list of those sites:
// the registry's per-process factories (a resolved client is cached per
// process, so no envelope could reach them anyway) and their helpers
// (moonshotBaseURL, xaiBaseURL, withClientIdentity, openAIStreamUsage,
// codexCLIVersion), OpenAIForfaitServes, NewClawExecutor's env defaults,
// RetryPolicyFromEnv's envRetryCount, resolveBackendName, the
// slash-command knobs (SlashCommandsDisabled, slashCommandMaxBytes),
// treeNoiseEnvAppend and lookupToolEnv. The risk this carries, declared:
// the day the run-envelope wiring lands, a name read both through a
// carried ctx and at one of these sites has TWO values in one process —
// the wiring names each site it takes over when it lands.

// carriedEnv is the environment a context carries: a fixed map, HERMETIC
// — a name the map does not hold reads as unset, never as the process
// value. Falling back to the process would let a "parallel" test read
// whatever the host (or a sibling test's leak) holds, which is exactly
// the interference the seam exists to close. Treat the map as read-only
// once carried.
//
// The carried environment lives in pkg/backend/credroute (ADR-161): it is
// the posture a call's credential is resolved against, captured once per
// attempt (credroute.Capture), so the arm and this package's every other
// read answer from one environment.
type carriedEnv map[string]string

// withCarriedEnv carries env on the context. Callers today: the seam's
// tests; a run's own envelope arrives through credroute.WithPosture at the
// head of an attempt. A nil env is a no-op (the process stays the source);
// an empty non-nil map is a deliberately empty environment.
func withCarriedEnv(ctx context.Context, env carriedEnv) context.Context {
	if env == nil {
		return ctx
	}
	return credroute.WithEnv(ctx, env)
}

// envGetenv reads name from the carried environment, else the process.
func envGetenv(ctx context.Context, name string) string {
	return credroute.Getenv(ctx, name)
}

// envLookupEnv is envGetenv with the planted-vs-unset distinction.
func envLookupEnv(ctx context.Context, name string) (string, bool) {
	return credroute.LookupEnv(ctx, name)
}

// envEnviron enumerates the carried environment in os.Environ's shape —
// sorted, so a consumer's output is deterministic across runs (map
// iteration order is not) — else the process's.
func envEnviron(ctx context.Context) []string {
	return credroute.Environ(ctx)
}
