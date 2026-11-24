package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
)

// The runner side of the approval channel (#3513): the ask-then-block
// round trip behind [model.WithSandboxApprovalForwarder]. The launcher
// consumes the approval and executes the call host-side; the decision
// envelope carries the real tool result, so the runner appends it to the
// transcript and never executes the call itself.

// runApprovalRoundTrip emits the permission_ask envelope and blocks
// pending the correlated permission_decision. A mid-stream envelope that
// names no pending call while this blocks is refused by route(); a dying
// channel is refused by failAllPending — both inject a non-decision
// envelope, which the type check below turns into the ask failing.
func runApprovalRoundTrip(ctx context.Context, d *proxyDispatcher, ask delegate.PermissionAskData) (delegate.PermissionDecisionData, error) {
	id := "approval-" + strconv.FormatUint(d.nextID.Add(1), 10)
	env, err := delegate.NewPermissionAskEnvelope(id, ask)
	if err != nil {
		return delegate.PermissionDecisionData{}, fmt.Errorf("build the approval ask: %w", err)
	}
	ch := d.beginApprovalAsk(id)
	if ch == nil {
		return delegate.PermissionDecisionData{}, errors.New("runner: a permission ask is already pending — protocol bug on the runner side")
	}
	if werr := d.write(env); werr != nil {
		d.endApprovalAsk(id)
		return delegate.PermissionDecisionData{}, fmt.Errorf("send the approval ask: %w", werr)
	}
	defer d.endApprovalAsk(id)
	select {
	case reply := <-ch:
		if reply.Type != delegate.EnvelopePermissionDecision {
			var res delegate.IOResult
			msg := fmt.Sprintf("runner: received %q while awaiting the permission decision", reply.Type)
			if json.Unmarshal(reply.Data, &res) == nil && res.Error != "" {
				msg = res.Error
			}
			return delegate.PermissionDecisionData{}, errors.New(msg)
		}
		var data delegate.PermissionDecisionData
		if err := json.Unmarshal(reply.Data, &data); err != nil {
			return delegate.PermissionDecisionData{}, fmt.Errorf("decode permission_decision: %w", err)
		}
		return data, nil
	case <-ctx.Done():
		return delegate.PermissionDecisionData{}, ctx.Err()
	case <-d.done:
		select {
		case reply := <-ch:
			if reply.Type != delegate.EnvelopePermissionDecision {
				return delegate.PermissionDecisionData{}, errors.New("runner: envelope channel closed mid-approval")
			}
			var data delegate.PermissionDecisionData
			if err := json.Unmarshal(reply.Data, &data); err != nil {
				return delegate.PermissionDecisionData{}, fmt.Errorf("decode permission_decision: %w", err)
			}
			return data, nil
		default:
			return delegate.PermissionDecisionData{}, errors.New("runner: envelope channel closed before the permission decision")
		}
	}
}

// beginApprovalAsk registers the ask currently blocking. Returns nil when
// another ask is already in flight — the gate is serial per tool call, so
// that is a runner-side protocol bug; the ask is refused rather than
// interleaved with another pending approval.
func (d *proxyDispatcher) beginApprovalAsk(id string) chan delegate.Envelope {
	ch := make(chan delegate.Envelope, 1)
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.approvalCh != nil {
		return nil
	}
	d.approvalInFlight = id
	d.approvalCh = ch
	return ch
}

// endApprovalAsk clears the in-flight ask when it still holds id, so a
// late clear never drops a NEWER ask registration.
func (d *proxyDispatcher) endApprovalAsk(id string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.approvalInFlight == id {
		d.approvalInFlight = ""
		d.approvalCh = nil
	}
}
