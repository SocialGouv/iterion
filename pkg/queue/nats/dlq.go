package nats

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// DLQ headers stamped on parked messages so the admin view can
// explain WHY a run landed there without decoding the full payload.
const (
	dlqHeaderReason    = "Iterion-DLQ-Reason"
	dlqHeaderRunID     = "Iterion-Run-Id"
	dlqHeaderTenant    = "Iterion-Tenant-Id"
	dlqHeaderDelivered = "Iterion-Num-Delivered"
)

// NumDelivered reports how many times JetStream has delivered this
// message (1 = first attempt). The runner compares it against
// MaxDeliver to decide Nak-for-retry vs park-in-DLQ.
func (d *Delivery) NumDelivered() int {
	md, err := d.raw.Metadata()
	if err != nil {
		return 1
	}
	return int(md.NumDelivered)
}

// MaxDeliver exposes the configured redelivery budget so consumers
// (the runner) can implement the "exhausted → DLQ" bridge without
// duplicating the default.
func (c *Conn) MaxDeliver() int { return c.cfg.MaxDeliver }

// PublishDLQ parks a copy of the delivery on the DLQ stream. The
// payload is copied verbatim (so a replay re-publishes the exact
// RunMessage); reason/run/tenant ride headers for cheap listing.
//
// The header identity comes from the version-agnostic Envelope, NOT from a
// full validating Decode: a schema-mismatched message (the rolling-upgrade
// case) is exactly the one that must be parkable, and it can never pass
// Decode on this build (issue #481). The verbatim payload means a replay
// re-publishes the original message untouched, ready for a runner that
// speaks its version.
func (c *Conn) PublishDLQ(ctx context.Context, d *Delivery, reason string) error {
	env, err := d.Envelope()
	if err != nil {
		return fmt.Errorf("queue/nats: dlq decode: %w", err)
	}
	h := nats.Header{}
	h.Set(dlqHeaderReason, reason)
	h.Set(dlqHeaderRunID, env.RunID)
	h.Set(dlqHeaderTenant, env.TenantID)
	h.Set(dlqHeaderDelivered, fmt.Sprintf("%d", d.NumDelivered()))
	_, err = c.js.PublishMsg(ctx, &nats.Msg{
		Subject: SubjectRunsDLQ,
		Header:  h,
		Data:    d.raw.Data(),
	})
	if err != nil {
		return fmt.Errorf("queue/nats: dlq publish: %w", err)
	}
	return nil
}

// DLQMessage is the admin view of one parked message.
type DLQMessage struct {
	Seq          uint64    `json:"seq"`
	RunID        string    `json:"run_id"`
	TenantID     string    `json:"tenant_id"`
	Reason       string    `json:"reason"`
	NumDelivered string    `json:"num_delivered,omitempty"`
	ParkedAt     time.Time `json:"parked_at"`
	Size         int       `json:"size_bytes"`
}

func dlqView(seq uint64, m *jetstream.RawStreamMsg) DLQMessage {
	v := DLQMessage{Seq: seq, ParkedAt: m.Time, Size: len(m.Data)}
	if m.Header != nil {
		v.RunID = m.Header.Get(dlqHeaderRunID)
		v.TenantID = m.Header.Get(dlqHeaderTenant)
		v.Reason = m.Header.Get(dlqHeaderReason)
		v.NumDelivered = m.Header.Get(dlqHeaderDelivered)
	}
	return v
}

// ListDLQ pages through the DLQ stream from cursorSeq (0 = oldest),
// returning up to limit messages and the cursor to continue from
// (0 = exhausted). Deleted sequences are skipped — DiscardDLQ leaves
// holes by design.
func (c *Conn) ListDLQ(ctx context.Context, cursorSeq uint64, limit int) ([]DLQMessage, uint64, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	stream, err := c.js.Stream(ctx, c.cfg.DLQStream)
	if err != nil {
		return nil, 0, fmt.Errorf("queue/nats: dlq stream: %w", err)
	}
	info, err := stream.Info(ctx)
	if err != nil {
		return nil, 0, fmt.Errorf("queue/nats: dlq info: %w", err)
	}
	first, last := info.State.FirstSeq, info.State.LastSeq
	if cursorSeq < first {
		cursorSeq = first
	}
	out := make([]DLQMessage, 0, limit)
	seq := cursorSeq
	for ; seq <= last && len(out) < limit; seq++ {
		m, err := stream.GetMsg(ctx, seq)
		if err != nil {
			if errors.Is(err, jetstream.ErrMsgNotFound) {
				continue // discarded — hole in the sequence
			}
			return nil, 0, fmt.Errorf("queue/nats: dlq get %d: %w", seq, err)
		}
		out = append(out, dlqView(seq, m))
	}
	next := uint64(0)
	if seq <= last {
		next = seq
	}
	return out, next, nil
}

// PeekDLQ returns one parked message including its raw payload (the
// serialized RunMessage) for inspection.
func (c *Conn) PeekDLQ(ctx context.Context, seq uint64) (DLQMessage, json.RawMessage, error) {
	stream, err := c.js.Stream(ctx, c.cfg.DLQStream)
	if err != nil {
		return DLQMessage{}, nil, fmt.Errorf("queue/nats: dlq stream: %w", err)
	}
	m, err := stream.GetMsg(ctx, seq)
	if err != nil {
		return DLQMessage{}, nil, fmt.Errorf("queue/nats: dlq get %d: %w", seq, err)
	}
	return dlqView(seq, m), json.RawMessage(m.Data), nil
}

// RepublishDLQ re-enqueues a parked message onto the live runs
// subject and removes it from the DLQ. The Nats-Msg-Id is salted
// with the DLQ sequence so JetStream's dedup window can't silently
// swallow the replay (the original publish used the bare run id).
func (c *Conn) RepublishDLQ(ctx context.Context, seq uint64) (string, error) {
	if err := c.requireRunnerEpochClaim(); err != nil {
		return "", err
	}
	if c.js == nil {
		return "", fmt.Errorf("queue/nats: connection not initialised")
	}
	view, payload, err := c.PeekDLQ(ctx, seq)
	if err != nil {
		return "", err
	}
	h := nats.Header{}
	h.Set("Nats-Msg-Id", fmt.Sprintf("%s|dlq-replay-%d", view.RunID, seq))
	if _, err := c.js.PublishMsg(ctx, &nats.Msg{
		Subject: SubjectRuns,
		Header:  h,
		Data:    payload,
	}); err != nil {
		return "", fmt.Errorf("queue/nats: dlq replay publish: %w", err)
	}
	if err := c.DiscardDLQ(ctx, seq); err != nil {
		// The replay IS enqueued; a lingering DLQ copy is only
		// confusing, not harmful — surface but don't fail.
		return view.RunID, fmt.Errorf("queue/nats: dlq replay cleanup: %w", err)
	}
	return view.RunID, nil
}

// DLQDepth reports how many messages are currently parked. Feeds the
// iterion_dlq_depth gauge (polled by the server's sweeper loop).
func (c *Conn) DLQDepth(ctx context.Context) (uint64, error) {
	stream, err := c.js.Stream(ctx, c.cfg.DLQStream)
	if err != nil {
		return 0, fmt.Errorf("queue/nats: dlq stream: %w", err)
	}
	info, err := stream.Info(ctx)
	if err != nil {
		return 0, fmt.Errorf("queue/nats: dlq info: %w", err)
	}
	return info.State.Msgs, nil
}

// DiscardDLQ permanently deletes one parked message.
func (c *Conn) DiscardDLQ(ctx context.Context, seq uint64) error {
	stream, err := c.js.Stream(ctx, c.cfg.DLQStream)
	if err != nil {
		return fmt.Errorf("queue/nats: dlq stream: %w", err)
	}
	if err := stream.DeleteMsg(ctx, seq); err != nil {
		return fmt.Errorf("queue/nats: dlq delete %d: %w", seq, err)
	}
	return nil
}

// IsRunLocked reports whether a runner currently holds the KV lease
// for runID. Consumed by the orphan sweeper to distinguish "stuck"
// from "in flight with a lagging status write".
func (c *Conn) IsRunLocked(ctx context.Context, runID string) (bool, error) {
	if c.kv == nil {
		return false, fmt.Errorf("queue/nats: KV bucket not initialised")
	}
	_, err := c.kv.Get(ctx, runID)
	if err != nil {
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}
