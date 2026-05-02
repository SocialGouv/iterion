package server

import (
	"encoding/json"

	"github.com/SocialGouv/iterion/pkg/runview"
)

// handleSubscribeLogs registers a per-run log subscription. Mirrors
// handleSubscribe: snapshot from `from_offset`, dispatch log_chunk
// envelopes, then drain the live channel. Opt-in so clients that
// don't render logs don't pay the bandwidth.
func (c *runConn) handleSubscribeLogs(env runWSEnvelope) {
	var req wsSubscribeLogsRequest
	if len(env.Payload) > 0 {
		if err := json.Unmarshal(env.Payload, &req); err != nil {
			c.sendError("bad_payload", err.Error(), env.AckID)
			return
		}
	}

	c.mu.Lock()
	if c.logSubscribed {
		c.mu.Unlock()
		c.sendAck(env.AckID)
		return
	}
	buf := c.server.runs.GetLogBuffer(c.runID)
	if buf == nil {
		// Terminated run — historical bytes are reachable via
		// GET /api/runs/{id}/log. Signal end-of-stream so the client
		// doesn't dangle on the live channel.
		c.mu.Unlock()
		c.sendAck(env.AckID)
		c.sendEnvelope(wsTypeLogTerminated, map[string]string{"run_id": c.runID}, "")
		return
	}
	// Subscribe before Snapshot so chunks landing during the read are
	// dedup'd by offset on the consumer side rather than lost.
	c.logSub = buf.Subscribe()
	c.logSubscribed = true
	c.mu.Unlock()

	c.sendAck(env.AckID)

	go c.streamLogs(buf, req.FromOffset)
}

// streamLogs replays the in-memory tail from fromOffset, then drains
// the live channel. Live chunks overlapping the snapshot are sliced
// at the cutoff so bytes never go out twice.
func (c *runConn) streamLogs(buf *runview.RunLogBuffer, fromOffset int64) {
	startOffset, snapshot, _ := buf.Snapshot(fromOffset)
	cutoff := startOffset + int64(len(snapshot))

	if len(snapshot) > 0 {
		if !c.sendEnvelope(wsTypeLogChunk, wsLogChunkPayload{
			Offset: startOffset,
			Text:   string(snapshot),
			Total:  cutoff,
		}, "") {
			return
		}
	}

	c.mu.Lock()
	sub := c.logSub
	c.mu.Unlock()
	if sub == nil {
		return
	}

	for {
		select {
		case <-c.closed:
			return
		case chunk, ok := <-sub.C:
			if !ok {
				c.sendEnvelope(wsTypeLogTerminated, map[string]string{"run_id": c.runID}, "")
				return
			}
			text := chunk.Bytes
			offset := chunk.Offset
			if offset < cutoff {
				skip := int(cutoff - offset)
				if skip >= len(text) {
					continue
				}
				text = text[skip:]
				offset = cutoff
			}
			if !c.sendEnvelope(wsTypeLogChunk, wsLogChunkPayload{
				Offset: offset,
				Text:   string(text),
				Total:  offset + int64(len(text)),
			}, "") {
				return
			}
			cutoff = offset + int64(len(text))
		}
	}
}

func (c *runConn) handleUnsubscribeLogs(env runWSEnvelope) {
	c.mu.Lock()
	if c.logSub != nil {
		c.logSub.Cancel()
		c.logSub = nil
	}
	c.logSubscribed = false
	c.mu.Unlock()
	c.sendAck(env.AckID)
}
