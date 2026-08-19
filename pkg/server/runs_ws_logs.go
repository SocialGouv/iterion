package server

import (
	"encoding/json"

	"github.com/SocialGouv/iterion/pkg/errtrack"
	"github.com/SocialGouv/iterion/pkg/runview/runstream"
)

// handleSubscribeLogs registers a per-run log subscription. Mirrors
// handleSubscribe: the connection's store-agnostic source delivers the
// persisted backlog from from_offset then the live tail, whatever mode
// produced the run (ADR-053). Opt-in so clients that don't render logs
// don't pay the bandwidth.
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
	c.logSubscribed = true
	c.mu.Unlock()

	sub, err := c.src.SubscribeLogs(c.authCtx(), c.runID, req.FromOffset)
	if err != nil {
		c.mu.Lock()
		c.logSubscribed = false
		c.mu.Unlock()
		c.sendError("log_stream_failed", err.Error(), env.AckID)
		return
	}
	c.mu.Lock()
	c.logSub = sub
	c.mu.Unlock()

	c.sendAck(env.AckID)
	errtrack.Go("server.runWS.pumpLogs", func() { c.pumpLogs(sub) })
}

// pumpLogs forwards log chunks to the WS as log_chunk envelopes. The
// Chunks channel closing means the log stream is over (run terminal, or
// no log will ever exist) — translated into log_terminated so the
// client renders its final state. Source errors are logged; the stream
// stays open.
func (c *runConn) pumpLogs(sub runstream.LogSubscription) {
	defer func() { _ = sub.Close() }()
	chunks := sub.Chunks()
	errs := sub.Errors()
	for {
		select {
		case <-c.closed:
			return
		case chunk, ok := <-chunks:
			if !ok {
				c.sendEnvelope(wsTypeLogTerminated, map[string]string{"run_id": c.runID}, "")
				return
			}
			if len(chunk.Data) == 0 {
				continue
			}
			if !c.sendEnvelope(wsTypeLogChunk, wsLogChunkPayload{
				Offset: chunk.Offset,
				Text:   string(chunk.Data),
				Total:  chunk.Offset + int64(len(chunk.Data)),
			}, "") {
				return
			}
		case err, ok := <-errs:
			if !ok {
				errs = nil // see pumpEvents: closed channel would spin
				continue
			}
			if c.server.logger != nil {
				c.server.logger.Warn("server: ws log stream %s: %v", c.runID, err)
			}
		}
	}
}

func (c *runConn) handleUnsubscribeLogs(env runWSEnvelope) {
	c.mu.Lock()
	if c.logSub != nil {
		_ = c.logSub.Close()
		c.logSub = nil
	}
	c.logSubscribed = false
	c.mu.Unlock()
	c.sendAck(env.AckID)
}
