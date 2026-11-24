package credroute

import (
	"context"
	"fmt"

	"github.com/SocialGouv/iterion/pkg/store"
)

// runTrustKey keys the workspace trust a context carries; unexported, so
// WithRunTrust is the only way in.
type runTrustKey struct{}

// WithRunTrust pins the run's workspace trust on ctx — who wrote the code
// the run is about to execute (store.RunTrust). The credential arms read it
// where a floor depends on trust rather than on the ambient stamp: an
// untrusted run's keys cross the sandbox as Layer 2 placeholders and every
// channel the proxy cannot hold is refused (ADR-162 decision 7), while a
// TRUSTED run stamped to its own credentials (ADR-131 §7) keeps today's
// crossing — the stamp refuses the pod's money, and the operator's own
// forfait still crosses for their own run. The two signals are distinct on
// purpose; reading the stamp for the floor would refuse a trusted tenant's
// forfait for no untrusted workspace in sight.
//
// The zero value (never pinned) is TRUSTED, RunTrust's own zero value: a
// ctx that carries no trust is one the launch did not judge, and the floors
// that read it lose nothing they had.
func WithRunTrust(ctx context.Context, trust store.RunTrust) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if trust.Trusted() {
		// The trusted value is never written, matching RunTrustEnvEntry's
		// rule: a trusted run's contexts look exactly as they did before
		// the carrier existed.
		return ctx
	}
	return context.WithValue(ctx, runTrustKey{}, trust)
}

// CarriedRunTrust is the workspace trust ctx carries, TRUSTED when it
// carries none (the zero value is the trusted default — a missing signal
// can only lower nothing).
func CarriedRunTrust(ctx context.Context) store.RunTrust {
	if ctx == nil {
		return store.RunTrustDefault
	}
	if trust, ok := ctx.Value(runTrustKey{}).(store.RunTrust); ok {
		return trust
	}
	return store.RunTrustDefault
}

// ErrCrossingRefused is the typed refusal of a channel the sandbox proxy
// cannot hold for an untrusted run (ADR-162 decision 7): Layer 2 substitutes
// placeholder tokens in headers toward the credential's own host, and every
// channel that is not a substitutable token toward a known host — a forfait
// (an OAuth credential and its refresh flow), a provider whose credential
// rides an SDK signing chain (bedrock, vertex, azure), the launcher's
// ambient env — is refused rather than crossed raw or crossed dead. It lives
// here because both arms that decide a crossing (claw's, claude_code's) and
// the launch's admission read it as one vocabulary.
type ErrCrossingRefused struct {
	// Channel names what was refused, in the operator's words: "the run's
	// Claude forfait", "the run's ChatGPT forfait", "a bedrock key", "the
	// launcher's ambient env".
	Channel string
	// Spec is the model spec the leg names, "" when the refusal is not
	// about one leg.
	Spec string
	// Because, when set, is the second clause of the message — what the
	// operator can do about it.
	Because string
}

func (e *ErrCrossingRefused) Error() string {
	msg := fmt.Sprintf("untrusted workspace: %s cannot cross the sandbox (the egress proxy substitutes placeholder tokens toward the credential's own host, and it cannot hold this channel)", e.Channel)
	if e.Spec != "" {
		msg += " — leg " + e.Spec
	}
	if e.Because != "" {
		msg += "; " + e.Because
	}
	return msg
}
