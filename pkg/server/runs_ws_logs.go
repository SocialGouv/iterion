package server

import (
	"context"
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
	if err := c.requireBoundSource(); err != nil {
		c.sendError("log_stream_failed", err.Error(), env.AckID)
		return
	}
	// The same origin-bound parent as the event reservation (streamCtx): a
	// log subscription reads the run the upgrade authorized, or is refused.
	parent, err := c.streamCtx()
	if err != nil {
		c.sendError("log_stream_failed", err.Error(), env.AckID)
		return
	}
	owner, created := c.reserveSubscription(parent, true)
	if !created {
		if owner != nil {
			c.sendAck(env.AckID)
		}
		return
	}
	var sub runstream.LogSubscription
	handedOff := false
	defer func() {
		if !handedOff {
			if sub != nil {
				_ = sub.Close()
			}
			c.clearFailedSubscription(owner, true)
			c.finishSubscription(owner)
		}
	}()
	sub, err = c.src.SubscribeLogs(owner.ctx, c.runID, req.FromOffset)
	if err != nil {
		if owner.ctx.Err() == nil {
			c.sendError("log_stream_failed", err.Error(), env.AckID)
			c.closeOnOriginLost(err)
		}
		return
	}
	if sub == nil {
		c.sendError("log_stream_failed", "source returned no subscription", env.AckID)
		return
	}
	if !c.ownsSubscription(owner, true) {
		return
	}
	// ACK belongs to admission; a slow consumer can close the connection here.
	c.sendAck(env.AckID)
	if !c.ownsSubscription(owner, true) {
		return
	}
	handedOff = true
	errtrack.Go("server.runWS.pumpLogs", func() { defer c.finishSubscription(owner); c.pumpLogsCtx(owner.ctx, sub) })
}

// pumpLogs forwards log chunks to the WS as log_chunk envelopes. The
// Chunks channel closing means the log stream is over (run terminal, or
// no log will ever exist) — translated into log_terminated so the
// client renders its final state. Already-published permanent errors instead
// become log_stream_failed; transient warnings do not change clean completion.
func (c *runConn) pumpLogs(sub runstream.LogSubscription) {
	c.pumpLogsCtx(c.authCtx(), sub)
}
func (c *runConn) pumpLogsCtx(ctx context.Context, sub runstream.LogSubscription) {
	defer func() { _ = sub.Close() }()
	chunks := sub.Chunks()
	errs := sub.Errors()
	var fatal error
	for {
		if ctx.Err() != nil {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-c.closed:
			return
		case chunk, ok := <-chunks:
			if !ok {
				fatal = drainRunStreamFatal(fatal, errs)
				if fatal != nil {
					c.sendError("log_stream_failed", fatal.Error(), "")
					c.closeOnOriginLost(fatal)
				} else {
					c.sendEnvelope(wsTypeLogTerminated, map[string]string{"run_id": c.runID}, "")
				}
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
			fatal = retainRunStreamFatal(fatal, err)
			if c.server.logger != nil {
				c.server.logger.Warn("server: ws log stream %s: %v", c.runID, err)
			}
		}
	}
}

func (c *runConn) handleUnsubscribeLogs(env runWSEnvelope) {
	c.unsubscribe(true)
	c.sendAck(env.AckID)
}
