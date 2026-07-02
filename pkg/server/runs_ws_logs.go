package server

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"

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
	// Cross-store: the in-process broker (GetLogBuffer) and the local
	// run.log file (replayPersistedLog → c.server.runs.StoreDir()) both
	// belong to the daemon driving the run. For a foreign-store run we
	// tail <c.xStorePath>/runs/<id>/run.log directly; see
	// streamLogsCrossStore for the full contract.
	if c.xStore != nil {
		c.logSubscribed = true
		c.mu.Unlock()
		c.sendAck(env.AckID)
		go c.streamLogsCrossStore(req.FromOffset)
		return
	}
	buf := c.server.runs.GetLogBuffer(c.runID)
	if buf == nil && !c.server.runs.Active(c.runID) {
		// No live buffer AND not produced in this process. buf==nil used to be
		// treated as "terminated", but an ACTIVE run launched elsewhere (an
		// external `iterion run`/`resume`, the dispatcher) also has no buffer
		// here — for those the one-shot replay was the whole bug: new lines
		// never streamed and the studio needed a full page refresh. Mirror the
		// events path (EnsureEventSource): if the run is still active, start an
		// on-demand run.log tailer and live-stream it. A terminal run falls
		// through to the one-shot replay below.
		if run, err := c.server.runs.LoadRun(c.runID); err == nil && !run.Status.IsTerminal() {
			if rel, live := c.server.runs.EnsureLogSource(c.runID); live != nil {
				c.logSrcRelease = rel
				buf = live
			}
		}
	}
	if buf == nil {
		// Terminated run (or no source could start) — no live buffer. Replay
		// the persisted run.log via the same wsTypeLogChunk envelope so the
		// client renders it identically to a live tail, then terminate. Skip
		// silently if the file is missing (very early failure before any log
		// was written) — the terminator still tells the client it's over.
		c.mu.Unlock()
		c.sendAck(env.AckID)
		c.replayPersistedLog(req.FromOffset)
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

	// The ring is a 1 MiB tail (pkg/runview.runLogRingCap). On long
	// runs the early bytes are evicted; reopening the studio after a
	// disconnect resubscribes from offset 0 and would otherwise miss
	// everything before the ring's lower bound — the per-node Logs
	// tab then shows "No log lines tagged with this node yet" for
	// any node whose output was emitted in the evicted prefix.
	// Fill the gap from run.log, which is the authoritative source
	// (file_log_source pumps disk → ring, so disk_size >= ring's
	// start offset). Best-effort: a missing file just degrades to
	// the previous behaviour.
	if startOffset > fromOffset {
		if !c.replayPersistedLogRange(fromOffset, startOffset) {
			return
		}
	}

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

// replayPersistedLog reads <store>/runs/<id>/run.log from `fromOffset`
// and emits it as a single log_chunk envelope. Used when a client
// subscribes to a terminated run that no longer has an in-memory tail.
// Failures are best-effort: a missing file is treated as empty (the
// caller still sends log_terminated afterwards).
func (c *runConn) replayPersistedLog(fromOffset int64) {
	storeDir := c.server.runs.StoreDir()
	if storeDir == "" {
		return
	}
	logPath := filepath.Join(storeDir, "runs", c.runID, "run.log")
	data, err := os.ReadFile(logPath)
	if err != nil {
		return
	}
	if fromOffset < 0 {
		fromOffset = 0
	}
	if fromOffset >= int64(len(data)) {
		return
	}
	tail := data[fromOffset:]
	c.sendEnvelope(wsTypeLogChunk, wsLogChunkPayload{
		Offset: fromOffset,
		Text:   string(tail),
		Total:  int64(len(data)),
	}, "")
}

// replayPersistedLogRange reads bytes [from, until) from run.log and
// emits them as a single log_chunk envelope. Used by streamLogs when
// the in-memory ring has evicted bytes older than the client's
// requested offset. Returns false on a send failure (caller stops the
// stream); a missing file or short read is best-effort and returns
// true so the live stream can still take over.
func (c *runConn) replayPersistedLogRange(from, until int64) bool {
	if from < 0 {
		// Defensive: a negative client-supplied offset must never reach the
		// Seek / make([]byte, until-from) below. Clamp to 0 (replay from the
		// start of the persisted log), matching replayPersistedLog. Today a
		// negative Seek would fail and silently skip the gap-fill; clamping
		// removes that reliance and gives the client the persisted prefix.
		from = 0
	}
	if from >= until {
		return true
	}
	storeDir := c.server.runs.StoreDir()
	if storeDir == "" {
		return true
	}
	logPath := filepath.Join(storeDir, "runs", c.runID, "run.log")
	f, err := os.Open(logPath)
	if err != nil {
		return true
	}
	defer f.Close()
	if _, err := f.Seek(from, 0); err != nil {
		return true
	}
	buf := make([]byte, until-from)
	n, _ := io.ReadFull(f, buf)
	if n == 0 {
		return true
	}
	return c.sendEnvelope(wsTypeLogChunk, wsLogChunkPayload{
		Offset: from,
		Text:   string(buf[:n]),
		Total:  from + int64(n),
	}, "")
}

func (c *runConn) handleUnsubscribeLogs(env runWSEnvelope) {
	c.mu.Lock()
	if c.logSub != nil {
		c.logSub.Cancel()
		c.logSub = nil
	}
	if c.logSrcRelease != nil {
		c.logSrcRelease()
		c.logSrcRelease = nil
	}
	c.logSubscribed = false
	c.mu.Unlock()
	c.sendAck(env.AckID)
}
