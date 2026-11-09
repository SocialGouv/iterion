package tool

import (
	"context"
	"errors"
	"fmt"
	"slices"
)

// hostGrantKey carries, on the context of ONE node's execution, the host
// capabilities that node holds and those the workspace-trust floor withdrew.
type hostGrantKey struct{}

type hostGrant struct{ held, withdrawn []string }

// WithHostCapabilities returns ctx carrying the host capabilities the calling
// node HOLDS — its declared `capabilities:` after the workspace-trust floor
// (store.RunTrust.HostCapabilities) — and the ones that floor WITHDREW, so a
// refusal says which of the two decided it. The claw executor stamps it at the
// one entry every node execution crosses, so a host tool reached by any route
// — an agent's tool loop, a tool node's registry command, a subagent, the
// sandboxed runner's proxy back to the launcher — reads the same answer.
func WithHostCapabilities(ctx context.Context, held, withdrawn []string) context.Context {
	return context.WithValue(ctx, hostGrantKey{}, hostGrant{held: slices.Clone(held), withdrawn: slices.Clone(withdrawn)})
}

// HostCapabilitiesFrom returns the capabilities the grant ctx carries holds. A
// call that did not come through a node execution carries none, and holds
// nothing.
func HostCapabilitiesFrom(ctx context.Context) []string {
	g, _ := ctx.Value(hostGrantKey{}).(hostGrant)
	return g.held
}

// ErrHostCapabilityNotHeld is the refusal of one of iterion's own host tools —
// the board, the run store, the watch fan-out — called from a node that does
// not hold the capability the tool needs.
var ErrHostCapabilityNotHeld = errors.New("tool: host capability not held")

// ErrHostCapabilityWithdrawn is the refusal of a capability the calling node
// DECLARES and the workspace-trust floor withdrew: the run's trust decided it,
// not the program. It is an ErrHostCapabilityNotHeld too.
var ErrHostCapabilityWithdrawn = fmt.Errorf("%w, withdrawn on a workspace its tenant did not write", ErrHostCapabilityNotHeld)

// requireHostCapability is the check every host tool makes when it is CALLED,
// whatever offered it. What a node is offered is a comfort for the model; this
// is the guarantee: the registry holds the whole family for the run, and
// naming a tool — in `tools:`, in a tool node's `command:`, from a subagent —
// is not holding its capability.
func requireHostCapability(ctx context.Context, tool, capability string) error {
	g, _ := ctx.Value(hostGrantKey{}).(hostGrant)
	if capability != "" && slices.Contains(g.held, capability) {
		return nil
	}
	if capability != "" && slices.Contains(g.withdrawn, capability) {
		return fmt.Errorf("%w: %s needs capability %q, which the calling node declares — this run's workspace was not written by its tenant, and such a run holds no host capability",
			ErrHostCapabilityWithdrawn, tool, capability)
	}
	return fmt.Errorf("%w: %s needs capability %q, which the calling node does not declare — an agent or judge holds its own `capabilities:` (else the workflow's), any other node the workflow's",
		ErrHostCapabilityNotHeld, tool, capability)
}
