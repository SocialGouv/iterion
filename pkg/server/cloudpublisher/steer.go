package cloudpublisher

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	natsq "github.com/SocialGouv/iterion/pkg/queue/nats"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
)

// SteerRun routes a live-steering command to whichever runner pod
// holds the run's lease, and returns its typed reply — implementing
// the runview runSteerer seam so BumpLoopCtx / RaiseBudgetCtx fall
// through here when the run is not held in-process.
//
// The command id is minted HERE (one per API call): a same-command
// republish by an impatient client is a new id and a new application —
// deliberate, so the runner's dedup only guards transport-level
// duplicates, never collapses two operator intents.
func (p *Publisher) SteerRun(ctx context.Context, runID string, cmd runview.SteerCommand) (runview.SteerReply, error) {
	if p.nats == nil {
		return runview.SteerReply{}, fmt.Errorf("cloudpublisher: NATS publisher is not configured")
	}
	// Terminal fast-path with the same tenant-bypass rationale as
	// CancelRun: the HTTP layer already gated access via LoadRunCtx.
	sctx := store.WithoutTenantFilter(ctx)
	r, err := p.store.LoadRun(sctx, runID)
	if err != nil {
		return runview.SteerReply{}, fmt.Errorf("cloudpublisher: load run %s: %w", runID, err)
	}
	if r.Status.IsTerminal() {
		return runview.SteerReply{}, &runview.RunTerminalError{Status: r.Status}
	}

	if cmd.CommandID == "" {
		cmd.CommandID = uuid.NewString()
	}
	if cmd.IssuedAt.IsZero() {
		cmd.IssuedAt = time.Now().UTC()
	}
	body, err := json.Marshal(cmd)
	if err != nil {
		return runview.SteerReply{}, fmt.Errorf("cloudpublisher: marshal steer command: %w", err)
	}

	replyBody, err := p.nats.SteerRun(ctx, runID, body, cmd.CommandID)
	if err != nil {
		switch {
		case errors.Is(err, natsq.ErrSteerNoRunner):
			return runview.SteerReply{}, runview.ErrRunNotHeld
		case errors.Is(err, natsq.ErrSteerTimeout):
			return runview.SteerReply{}, &runview.SteerError{
				Code:    "engine_stalled",
				Message: "runner did not reply in time — the command may still apply at the run's next boundary",
			}
		default:
			return runview.SteerReply{}, fmt.Errorf("cloudpublisher: steer %s: %w", runID, err)
		}
	}

	var reply runview.SteerReply
	if err := json.Unmarshal(replyBody, &reply); err != nil {
		return runview.SteerReply{}, fmt.Errorf("cloudpublisher: malformed steer reply for %s: %w", runID, err)
	}
	if reply.Err != nil {
		return runview.SteerReply{}, reply.Err
	}
	return reply, nil
}
