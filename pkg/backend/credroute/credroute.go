// Package credroute is ADR-161's one resolver: which credential a call
// spends. Every reader of that question — the dispatch that builds the client
// or crosses the env, the launch's admission, the usage-cap meter, the spend
// ledger and the credential plan's fine table — asks Resolve and reads its
// answer verbatim, rather than re-deriving it from the raw fields.
//
// The leaf holds the vocabulary (Call, Leg, Spend, Refusal and its reasons),
// the posture a call is judged against (Capture), the sandbox's home state and
// the arm registry. An arm is one backend's precedence moved out of its
// dispatch verbatim; the backend's own package registers it (Register), and
// the readers above call Resolve. The leaf imports pkg/secrets and envtrust,
// nothing above them.
//
// A backend whose arm has not landed answers ErrNoArm: not a refusal, so an
// unstamped reader keeps today's path for that leg (ADR-161 decision 9).
package credroute

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
)

// Role is who calls the model.
type Role string

const (
	// RoleNode is a workflow node's own call.
	RoleNode Role = "node"
	// RoleSupervisor is a declared supervisor's evaluation call.
	RoleSupervisor Role = "supervisor"
	// RoleAuxiliary is a server-side surface's call (the merge-conflict
	// resolver, the session board).
	RoleAuxiliary Role = "auxiliary"
)

// Leg is one element of a node's chain: its credential-routing hint (the
// `provider:` element, folded to its route form; "" defers to the backend's
// default precedence) and the spec the element pins ("" inherits the node's
// model).
type Leg struct {
	Provider string
	Model    string
}

// Sandbox is where a call runs.
type Sandbox struct {
	// Contained reports that the call runs in the run's sandbox, the
	// crossing in between; false is the host — `sandbox: none`, a degraded
	// `auto` included.
	Contained bool
	// Home is what the sandbox binds of the operator's home, as the runtime
	// resolved it (the driver's host-state mount); zero on the host.
	Home SandboxHomeState
}

// Call is one leg's question: which credential does this call spend.
type Call struct {
	Backend string
	// Spec is the model reference the leg sends: the leg's own model, else
	// the node's.
	Spec    string
	Leg     Leg
	Role    Role
	Sandbox Sandbox
}

// Channel is where the spent credential comes from (ADR-161 decision 3).
type Channel string

const (
	// ChannelRunKey is a key of the run's own default set.
	ChannelRunKey Channel = "run key"
	// ChannelPinnedKey is a key a shared tier sealed for the route alone.
	ChannelPinnedKey Channel = "pinned key"
	// ChannelRunForfait is a subscription the run sealed (its own, or one
	// lent through the credential pool).
	ChannelRunForfait Channel = "run forfait"
	// ChannelGatewayPair is the run's sealed gateway key and base URL.
	ChannelGatewayPair Channel = "gateway pair"
	// ChannelPodEnv is a credential of the pod's environment.
	ChannelPodEnv Channel = "pod env"
	// ChannelHostLogin is a login on the host's disk.
	ChannelHostLogin Channel = "host login"
	// ChannelOwnConfig is a backend that reads its own configuration, never
	// a run credential.
	ChannelOwnConfig Channel = "own config"
)

// Spend is what a call spends. It names a credential, never carries one: it
// crosses the sandbox channel as JSON (the crossing's record, the container's
// result).
type Spend struct {
	Channel Channel `json:"channel,omitempty"`
	// Slot is the run slot the spend books on — a provider name or an OAuth
	// kind; "" for none of the run's.
	Slot string `json:"slot,omitempty"`
	// Fingerprint is the slot's audit fingerprint (Credentials.Fingerprint),
	// the value the credential plan predicts; "" when the slot has none. A
	// spend of the pod's environment names no slot: its Fingerprint is the
	// account the request carries, in the digest every lane's ambient label
	// keys on (the usage meter's key), "" where no such label names the
	// account — the z.ai synthesis is z.ai's.
	Fingerprint string `json:"fingerprint,omitempty"`
	// BaseURL is the destination the credential is sent to; "" is the
	// provider's published endpoint.
	BaseURL string `json:"base_url,omitempty"`
	// Source is claude_code's session label; "" for the other arms.
	Source string `json:"source,omitempty"`
}

// Reason is why a leg spends nothing (ADR-161 decision 7).
type Reason string

const (
	// ReasonAmbientStamp: the run is stamped (ADR-131 §7) and only the pod's
	// money would fund the leg.
	ReasonAmbientStamp Reason = "ambient-stamp"
	// ReasonSubscriptionSwitch: ITERION_FORBID_SUBSCRIPTION_OAUTH refuses
	// the subscription the leg would spend.
	ReasonSubscriptionSwitch Reason = "subscription-switch"
	// ReasonHintUnfunded: nothing funds the route the leg's hint mandates.
	ReasonHintUnfunded Reason = "hint-unfunded"
	// ReasonNoCredential: no credential reaches the call.
	ReasonNoCredential Reason = "no-credential"
	// ReasonOutsideAllowlist: the leg's provider is outside the run's
	// allowlist (ADR-129 §5).
	ReasonOutsideAllowlist Reason = "outside-allowlist"
	// ReasonContentBoundary: the run's content boundary refuses the spec.
	ReasonContentBoundary Reason = "content-boundary"
	// ReasonNoArm: no arm decides the backend under the stamp.
	ReasonNoArm Reason = "no-arm"
	// ReasonArmPending: the interim guard refuses a stamped leg of a backend
	// whose arm has not landed.
	ReasonArmPending Reason = "arm-pending"
	// ReasonUntrustedChannel: an untrusted run's leg would spend a channel
	// the sandbox proxy cannot hold (ADR-162 decision 7) — a forfait's
	// OAuth credential, a provider whose credential rides an SDK signing
	// chain, or the launcher's ambient env — or the start's Layer 2 is
	// unusable (TLS inspection off, a driver that cannot inspect).
	ReasonUntrustedChannel Reason = "untrusted-channel"
)

var (
	// ErrAmbientRefused is the stamp's refusal: the run's own credentials
	// are the only funds for its LLM routes, and the pod's environment is
	// refused (ADR-131 §7). An arm whose site already returns a typed stamp
	// refusal keeps it as the cause, and that type answers errors.Is for
	// this sentinel.
	ErrAmbientRefused = errors.New("credroute: the run's own credentials are the only funds for its LLM routes — the pod's environment is refused")
	// ErrPoolContentRefused is the sovereign-pool refusal (D12): a
	// server-side surface declines to send content derived from a run
	// stamped to a sovereign pool through a vendor-default model. It lives
	// here, the leaf every arm reaches; runview re-exports it.
	ErrPoolContentRefused = errors.New("sovereign pool: refusing to send this run's content through a server-side LLM")
	// ErrNoArm reports that no arm decides the backend: the reader keeps its
	// own path for that leg. It is not a Refusal.
	ErrNoArm = errors.New("credroute: no arm decides this backend's credential")
)

// Refusal is a leg no credential funds. It unwraps to the cause its arm set —
// the error that site returns, so every errors.As keeps matching — or to a
// leaf sentinel where the site has none.
type Refusal struct {
	Backend  string
	Provider string
	Spec     string
	Leg      Leg
	Reason   Reason
	Cause    error
}

func (r *Refusal) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "credroute: %s leg", r.Backend)
	if r.Leg.Provider != "" {
		fmt.Fprintf(&b, " %q", r.Leg.Provider)
	}
	if r.Spec != "" {
		fmt.Fprintf(&b, " on %s", r.Spec)
	}
	fmt.Fprintf(&b, " refused (%s)", r.Reason)
	if r.Cause != nil {
		fmt.Fprintf(&b, ": %v", r.Cause)
	}
	return b.String()
}

func (r *Refusal) Unwrap() error { return r.Cause }

// Arm is one backend's precedence: the Spend its call lands on, or a
// *Refusal. The posture is pinned on ctx (Capture answers it).
type Arm func(ctx context.Context, call Call) (Spend, error)

var (
	armsMu sync.RWMutex
	arms   = map[string]Arm{}
)

// Register installs backend's arm. A second arm for one backend panics: two
// arms are two answers to one question.
func Register(backend string, arm Arm) {
	if backend == "" || arm == nil {
		panic("credroute: Register needs a backend name and an arm")
	}
	armsMu.Lock()
	defer armsMu.Unlock()
	if _, taken := arms[backend]; taken {
		panic(fmt.Sprintf("credroute: backend %q already has an arm", backend))
	}
	arms[backend] = arm
}

// Registered lists the backends that hold an arm, sorted.
func Registered() []string {
	armsMu.RLock()
	defer armsMu.RUnlock()
	out := make([]string, 0, len(arms))
	for name := range arms {
		out = append(out, name)
	}
	slices.Sort(out)
	return out
}

// Resolve answers which credential call spends: its backend's arm over the
// posture ctx carries — captured once per attempt and pinned there, so every
// read of the call's environment answers from one capture. A backend with no
// arm answers ErrNoArm.
func Resolve(ctx context.Context, call Call) (Spend, error) {
	armsMu.RLock()
	arm := arms[call.Backend]
	armsMu.RUnlock()
	if arm == nil {
		return Spend{}, fmt.Errorf("credroute: backend %q: %w", call.Backend, ErrNoArm)
	}
	return arm(WithPosture(ctx, Capture(ctx)), call)
}
