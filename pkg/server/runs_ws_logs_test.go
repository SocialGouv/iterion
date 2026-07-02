package server

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/SocialGouv/iterion/pkg/store"
)

// These tests pin the WS log-subscription and cross-store streaming wire
// contract BEFORE the ADR-053 refactor routes both through
// runstream.Source: same envelopes (log_chunk{offset,text,total},
// log_terminated, event/event_batch, terminated) must keep flowing for
// every production mode. They are the review gate for the WS rewrite.

// dialRunWSStore is dialRunWS with a ?store= query (cross-store mode).
func dialRunWSStore(t *testing.T, hs *httptest.Server, runID, storePath string) *websocket.Conn {
	t.Helper()
	wsURL := "ws" + strings.TrimPrefix(hs.URL, "http") + "/api/ws/runs/" + runID +
		"?store=" + url.QueryEscape(storePath)
	c, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial cross-store: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// readEnvelopeWithin is readEnvelope with a caller-chosen deadline, for
// paths whose latency is dominated by a coarse ticker (the 5s cross-store
// terminal poll).
func readEnvelopeWithin(t *testing.T, c *websocket.Conn, timeout time.Duration, allowedTypes ...string) runWSEnvelope {
	t.Helper()
	allowed := map[string]bool{}
	for _, a := range allowedTypes {
		allowed[a] = true
	}
	deadline := time.Now().Add(timeout)
	for {
		_ = c.SetReadDeadline(deadline)
		_, raw, err := c.ReadMessage()
		if err != nil {
			t.Fatalf("read (within %s): %v", timeout, err)
		}
		var env runWSEnvelope
		if err := json.Unmarshal(raw, &env); err != nil {
			t.Fatalf("unmarshal envelope: %v", err)
		}
		if len(allowed) == 0 || allowed[env.Type] {
			return env
		}
	}
}

func decodeLogChunk(t *testing.T, env runWSEnvelope) wsLogChunkPayload {
	t.Helper()
	var p wsLogChunkPayload
	if err := json.Unmarshal(env.Payload, &p); err != nil {
		t.Fatalf("decode log_chunk: %v", err)
	}
	return p
}

// collectLogText drains log_chunk envelopes until the reassembled text
// (by offset) covers want, or the deadline passes. have is the prefix
// already received by earlier calls on the same connection (chunk
// offsets are absolute). Overlapping chunks are deduped by offset so an
// at-least-once source passes unchanged.
func collectLogText(t *testing.T, c *websocket.Conn, have, want string, timeout time.Duration) string {
	t.Helper()
	buf := append(make([]byte, 0, len(want)), have...)
	if string(buf) == want {
		return want
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		env := readEnvelopeWithin(t, c, time.Until(deadline), wsTypeLogChunk, wsTypeLogTerminated)
		if env.Type == wsTypeLogTerminated {
			break
		}
		p := decodeLogChunk(t, env)
		end := p.Offset + int64(len(p.Text))
		if end > int64(len(buf)) {
			if p.Offset > int64(len(buf)) {
				t.Fatalf("log chunk gap: have %d bytes, chunk starts at %d", len(buf), p.Offset)
			}
			buf = append(buf[:p.Offset], []byte(p.Text)...)
		}
		if string(buf) == want {
			return string(buf)
		}
	}
	return string(buf)
}

func writeRunLog(t *testing.T, storeDir, runID, content string) string {
	t.Helper()
	runDir := filepath.Join(storeDir, "runs", runID)
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		t.Fatalf("mkdir run dir: %v", err)
	}
	logPath := filepath.Join(runDir, "run.log")
	if err := os.WriteFile(logPath, []byte(content), 0o644); err != nil {
		t.Fatalf("write run.log: %v", err)
	}
	return logPath
}

func appendRunLog(t *testing.T, logPath, content string) {
	t.Helper()
	f, err := os.OpenFile(logPath, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatalf("open run.log for append: %v", err)
	}
	if _, err := f.WriteString(content); err != nil {
		t.Fatalf("append run.log: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close run.log: %v", err)
	}
}

// TestRunsWSLogs_TerminatedRunOneShotReplay: a finished run with a
// persisted run.log replays the whole file as log_chunk(s) covering
// [0, len) then closes the stream with log_terminated.
func TestRunsWSLogs_TerminatedRunOneShotReplay(t *testing.T) {
	srv, hs := newTestServer(t)
	seedRun(t, srv, "run-log-done", "wf", store.RunStatusFinished)
	const content = "line one\nline two\n"
	writeRunLog(t, srv.cfg.StoreDir, "run-log-done", content)

	c := dialRunWS(t, hs, "run-log-done")
	writeJSONMessage(t, c, runWSEnvelope{Type: wsTypeSubscribeLogs, AckID: "l1"})

	if env := readEnvelope(t, c, wsTypeAck); env.AckID != "l1" {
		t.Errorf("AckID = %q, want l1", env.AckID)
	}
	env := readEnvelope(t, c, wsTypeLogChunk)
	p := decodeLogChunk(t, env)
	if p.Offset != 0 || p.Text != content {
		t.Errorf("chunk = offset %d text %q, want offset 0 text %q", p.Offset, p.Text, content)
	}
	if p.Total != int64(len(content)) {
		t.Errorf("Total = %d, want %d", p.Total, len(content))
	}
	if env := readEnvelope(t, c, wsTypeLogTerminated); env.Type != wsTypeLogTerminated {
		t.Fatalf("Type = %q, want log_terminated", env.Type)
	}
}

// TestRunsWSLogs_FromOffsetSkipsPrefix: replay honours from_offset —
// the client already holds the prefix and must not receive it again.
func TestRunsWSLogs_FromOffsetSkipsPrefix(t *testing.T) {
	srv, hs := newTestServer(t)
	seedRun(t, srv, "run-log-off", "wf", store.RunStatusFinished)
	const content = "0123456789"
	writeRunLog(t, srv.cfg.StoreDir, "run-log-off", content)

	c := dialRunWS(t, hs, "run-log-off")
	writeJSONMessage(t, c, runWSEnvelope{
		Type:    wsTypeSubscribeLogs,
		Payload: json.RawMessage(`{"from_offset":4}`),
	})

	env := readEnvelope(t, c, wsTypeLogChunk, wsTypeLogTerminated)
	if env.Type == wsTypeLogTerminated {
		t.Fatalf("got log_terminated before any chunk; want the [4,10) suffix")
	}
	p := decodeLogChunk(t, env)
	if p.Offset != 4 || p.Text != content[4:] {
		t.Errorf("chunk = offset %d text %q, want offset 4 text %q", p.Offset, p.Text, content[4:])
	}
	_ = readEnvelope(t, c, wsTypeLogTerminated)
}

// TestRunsWSLogs_MissingLogTerminates: a terminated run without a
// run.log (very early failure) still closes the stream cleanly — the
// client gets log_terminated and no chunk.
func TestRunsWSLogs_MissingLogTerminates(t *testing.T) {
	srv, hs := newTestServer(t)
	seedRun(t, srv, "run-log-none", "wf", store.RunStatusFailed)

	c := dialRunWS(t, hs, "run-log-none")
	writeJSONMessage(t, c, runWSEnvelope{Type: wsTypeSubscribeLogs})

	env := readEnvelope(t, c, wsTypeLogChunk, wsTypeLogTerminated)
	if env.Type != wsTypeLogTerminated {
		t.Fatalf("Type = %q, want log_terminated (no chunk for a missing run.log)", env.Type)
	}
}

// TestRunsWSLogs_ActiveExternalRunStreamsLive: an ACTIVE run this
// process did not launch (external `iterion run`, dispatcher) has no
// in-process buffer — the server must stand up an on-demand run.log
// tailer and live-stream appended bytes (the EnsureLogSource path; the
// pre-d4f12e39d bug was a frozen one-shot replay here).
func TestRunsWSLogs_ActiveExternalRunStreamsLive(t *testing.T) {
	srv, hs := newTestServer(t)
	st, err := store.New(srv.cfg.StoreDir)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	const runID = "run-log-live"
	if _, err := st.CreateRun(context.Background(), runID, "wf", nil); err != nil {
		t.Fatalf("CreateRun: %v", err) // CreateRun → status running (non-terminal)
	}
	logPath := writeRunLog(t, srv.cfg.StoreDir, runID, "hello ")

	c := dialRunWS(t, hs, runID)
	writeJSONMessage(t, c, runWSEnvelope{Type: wsTypeSubscribeLogs})

	if got := collectLogText(t, c, "", "hello ", 3*time.Second); got != "hello " {
		t.Fatalf("initial drain = %q, want %q", got, "hello ")
	}
	appendRunLog(t, logPath, "world")
	if got := collectLogText(t, c, "hello ", "hello world", 3*time.Second); got != "hello world" {
		t.Fatalf("after live append = %q, want %q", got, "hello world")
	}
}

// newCrossStore provisions a foreign store under a fake $HOME/.iterion
// (the only root resolveCrossStore accepts) and returns it with its path.
func newCrossStore(t *testing.T) (*store.FilesystemRunStore, string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".iterion")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir foreign store: %v", err)
	}
	st, err := store.New(dir)
	if err != nil {
		t.Fatalf("open foreign store: %v", err)
	}
	return st, dir
}

// TestRunsWSLogs_CrossStore: logs of a run living in a foreign store
// (another daemon's ~/.iterion) stream through ?store= — initial drain,
// live appends, and a final log_terminated when run.json flips terminal.
func TestRunsWSLogs_CrossStore(t *testing.T) {
	srv, hs := newTestServer(t)
	_ = srv
	xs, xdir := newCrossStore(t)
	const runID = "run-x-logs"
	if _, err := xs.CreateRun(context.Background(), runID, "wf", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	logPath := writeRunLog(t, xdir, runID, "alpha ")

	c := dialRunWSStore(t, hs, runID, xdir)
	writeJSONMessage(t, c, runWSEnvelope{Type: wsTypeSubscribeLogs})

	if got := collectLogText(t, c, "", "alpha ", 4*time.Second); got != "alpha " {
		t.Fatalf("initial drain = %q, want %q", got, "alpha ")
	}
	appendRunLog(t, logPath, "beta")
	if got := collectLogText(t, c, "alpha ", "alpha beta", 4*time.Second); got != "alpha beta" {
		t.Fatalf("after live append = %q, want %q", got, "alpha beta")
	}

	// Terminal flip in the foreign run.json must close the stream (the
	// terminal poll is coarse — allow generously more than one tick).
	if err := xs.UpdateRunStatus(context.Background(), runID, store.RunStatusFinished, ""); err != nil {
		t.Fatalf("UpdateRunStatus: %v", err)
	}
	env := readEnvelopeWithin(t, c, 12*time.Second, wsTypeLogTerminated)
	if env.Type != wsTypeLogTerminated {
		t.Fatalf("Type = %q, want log_terminated after terminal flip", env.Type)
	}
}

// TestRunsWSEvents_CrossStore: events of a foreign-store run stream
// through ?store= — snapshot, historical replay, live tail of appended
// events, and terminated when run.json flips terminal.
func TestRunsWSEvents_CrossStore(t *testing.T) {
	srv, hs := newTestServer(t)
	_ = srv
	xs, xdir := newCrossStore(t)
	const runID = "run-x-events"
	if _, err := xs.CreateRun(context.Background(), runID, "wf", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	for i, evt := range []store.Event{
		{Type: store.EventRunStarted, RunID: runID},
		{Type: store.EventNodeStarted, RunID: runID, NodeID: "analyze"},
		{Type: store.EventNodeFinished, RunID: runID, NodeID: "analyze"},
	} {
		if _, err := xs.AppendEvent(context.Background(), runID, evt); err != nil {
			t.Fatalf("seed event %d: %v", i, err)
		}
	}

	c := dialRunWSStore(t, hs, runID, xdir)
	writeJSONMessage(t, c, runWSEnvelope{
		Type:    wsTypeSubscribe,
		Payload: json.RawMessage(`{"replay_history":true}`),
	})
	_ = readEnvelope(t, c, wsTypeSnapshot)

	// Historical replay: seqs 0..2, batch or single envelopes both valid.
	got := []int64{}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && len(got) < 3 {
		env := readEnvelope(t, c, wsTypeEvent, wsTypeEventBatch)
		for _, ev := range decodeEventEnvelope(t, env) {
			got = append(got, ev.Seq)
		}
	}
	if len(got) != 3 || got[0] != 0 || got[2] != 2 {
		t.Fatalf("replayed seqs = %v, want [0 1 2]", got)
	}

	// Live tail: an event appended by the foreign daemon reaches the WS.
	if _, err := xs.AppendEvent(context.Background(), runID,
		store.Event{Type: store.EventNodeStarted, RunID: runID, NodeID: "late"}); err != nil {
		t.Fatalf("append live event: %v", err)
	}
	liveEnv := readEnvelopeWithin(t, c, 4*time.Second, wsTypeEvent, wsTypeEventBatch)
	live := decodeEventEnvelope(t, liveEnv)
	if len(live) == 0 || live[len(live)-1].NodeID != "late" {
		t.Fatalf("live tail = %+v, want the appended 'late' event", live)
	}

	if err := xs.UpdateRunStatus(context.Background(), runID, store.RunStatusFinished, ""); err != nil {
		t.Fatalf("UpdateRunStatus: %v", err)
	}
	env := readEnvelopeWithin(t, c, 12*time.Second, wsTypeTerminated)
	if env.Type != wsTypeTerminated {
		t.Fatalf("Type = %q, want terminated after terminal flip", env.Type)
	}
}
