package delegate

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/SocialGouv/iterion/pkg/backend/credroute"
	"github.com/SocialGouv/iterion/pkg/secrets"
)

// The resolver's arms this package registers (ADR-161, U2):
//
//   - kimi, grok and opencode read their own configuration, never a run
//     credential: no funding arm by design (decision 5). Their arm only
//     refuses — under the run's stamp (ADR-131 §7), whose credentials are the
//     run's only funds — and otherwise answers the own-config channel, the
//     pod's money, admitted with a warning and keyed by the meter as before.
//   - pi and codex meet the interim guard (decision 9): until their arms land
//     (U4), a stamped leg of theirs is refused — its dispatch walks the pod's
//     credentials past the run's seal — and an unstamped one answers ErrNoArm,
//     so every reader keeps its own path for it. Stricter than the end state,
//     never a leak.
//
// Registered as literals calling the arms, as the claw arm is.
func init() {
	for _, backend := range ownConfigBackends {
		credroute.Register(backend, func(ctx context.Context, call credroute.Call) (credroute.Spend, error) {
			return ownConfigArm(ctx, call)
		})
	}
	for _, backend := range armPendingBackends {
		credroute.Register(backend, func(ctx context.Context, call credroute.Call) (credroute.Spend, error) {
			return armPendingGuard(ctx, call)
		})
	}
}

// ownConfigBackends are the backends that read their own configuration.
var ownConfigBackends = []string{BackendKimi, BackendGrok, BackendOpenCode}

// armPendingBackends are the backends the interim guard holds until their
// arms land.
var armPendingBackends = []string{BackendPi, BackendCodex}

// ErrOwnConfigUnderStamp is the cause of an own-config leg refused under the
// stamp: the backend spends what its own configuration holds — the pod's
// money — and the run's credentials are its only funds. It answers
// errors.Is for credroute.ErrAmbientRefused.
type ErrOwnConfigUnderStamp struct {
	Backend string
}

func (e *ErrOwnConfigUnderStamp) Error() string {
	return fmt.Sprintf("delegate: %s reads its own configuration, never a run credential, and this run is stamped to its own credentials (ADR-131 §7): the leg cannot spend", e.Backend)
}

func (e *ErrOwnConfigUnderStamp) Is(target error) bool { return target == credroute.ErrAmbientRefused }

// ErrArmPending is the cause of a pi or codex leg the interim guard refuses on
// a stamped run: the backend's dispatch walks the pod's credentials, its own
// stores and logins, past the run's seal, and its arm — which spends the
// run's sealed money alone — has not landed (ADR-161 U4). It answers
// errors.Is for credroute.ErrAmbientRefused.
type ErrArmPending struct {
	Backend string
}

func (e *ErrArmPending) Error() string {
	return fmt.Sprintf("delegate: %s has no credential arm yet (ADR-161 U4) — its dispatch reads the pod's credentials past the run's seal, so a run stamped to its own credentials (ADR-131 §7) refuses its legs", e.Backend)
}

func (e *ErrArmPending) Is(target error) bool { return target == credroute.ErrAmbientRefused }

// ownConfigArm refuses a stamped leg and answers the own-config channel
// otherwise.
func ownConfigArm(ctx context.Context, call credroute.Call) (credroute.Spend, error) {
	if stampedRun(ctx) {
		return credroute.Spend{}, legRefusal(call, credroute.ReasonAmbientStamp, &ErrOwnConfigUnderStamp{Backend: call.Backend})
	}
	return credroute.Spend{Channel: credroute.ChannelOwnConfig}, nil
}

// armPendingGuard refuses a stamped leg and answers ErrNoArm otherwise.
func armPendingGuard(ctx context.Context, call credroute.Call) (credroute.Spend, error) {
	if stampedRun(ctx) {
		return credroute.Spend{}, legRefusal(call, credroute.ReasonArmPending, &ErrArmPending{Backend: call.Backend})
	}
	return credroute.Spend{}, fmt.Errorf("delegate: %s: %w", call.Backend, credroute.ErrNoArm)
}

// stampedRun reports the run's stamp: its credentials are its LLM routes'
// only funds (secrets.Credentials.AmbientRefused).
func stampedRun(ctx context.Context) bool {
	creds, ok := secrets.CredentialsFromContext(ctx)
	return ok && creds.AmbientRefused
}

func legRefusal(call credroute.Call, reason credroute.Reason, cause error) *credroute.Refusal {
	provider := ""
	if p, _, cut := strings.Cut(call.Spec, "/"); cut {
		provider = p
	}
	return &credroute.Refusal{Backend: call.Backend, Provider: provider, Spec: call.Spec, Leg: call.Leg, Reason: reason, Cause: cause}
}

// dispatchRefusal is the resolver's answer for the leg task dispatches on
// backend, asked before anything of the dispatch runs: the arm's refusal, or
// nil — the arm funds the leg, or no arm decides the backend (ErrNoArm: the
// dispatch keeps its own walk).
func dispatchRefusal(ctx context.Context, backend string, task Task) error {
	_, err := credroute.Resolve(ctx, credroute.Call{
		Backend: backend,
		Spec:    task.Model,
		Leg:     credroute.Leg{Provider: strings.ToLower(strings.TrimSpace(task.ProviderHint))},
		Role:    credroute.RoleNode,
		Sandbox: credroute.Sandbox{Contained: !task.Hostless()},
	})
	if err == nil || errors.Is(err, credroute.ErrNoArm) {
		return nil
	}
	return err
}
