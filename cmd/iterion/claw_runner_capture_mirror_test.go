package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/SocialGouv/claw-code-go/pkg/api"
	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/model"
)

// mirrorCapture drives the real sink over an in-memory wire and returns what
// crossed and what the runner said about it.
func mirrorCapture(t *testing.T, snapshot []byte) (wire, stderr string) {
	t.Helper()
	var wireBuf, stderrBuf bytes.Buffer
	sink := &dispatcherCaptureSink{
		dispatcher: newProxyDispatcher(strings.NewReader(""), &wireBuf),
		stderr:     &stderrBuf,
	}
	sink.Capture(sandboxRunnerSessionID, "assistant_conversation", snapshot)
	return wireBuf.String(), stderrBuf.String()
}

func mirrorConversation(t *testing.T, n int) []byte {
	t.Helper()
	big := strings.Repeat("x", 1<<20)
	msgs := make([]api.Message, 0, n)
	for i := range n {
		msgs = append(msgs, api.Message{
			Role:    "user",
			Content: []api.ContentBlock{{Type: "text", Text: fmt.Sprintf("turn-%d:", i) + big}},
		})
	}
	raw, err := json.Marshal(msgs)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// The mirror is what keeps the host's copy of the conversation current, and
// the host's copy is what CompactAndRetry shrinks and the next crossing
// replays. Nothing caps the in-container store it comes from, while the IPC
// refuses a line over delegate.MaxEnvelopeLineBytes — and a conversation
// that only grows is refused on EVERY later save too, so an unbounded mirror
// does not fall one snapshot behind, it stops mirroring for the rest of the
// run and leaves the host compacting a conversation the model never had.
func TestCaptureSink_AnOversizeConversationStillReachesTheHost(t *testing.T) {
	snapshot := mirrorConversation(t, 5)
	if len(snapshot) <= delegate.MaxEnvelopeLineBytes {
		t.Fatalf("the fixture is %d bytes, under the %d-byte line cap — it proves nothing", len(snapshot), delegate.MaxEnvelopeLineBytes)
	}

	wire, stderr := mirrorCapture(t, snapshot)
	if wire == "" {
		t.Fatal("nothing crossed: the host's mirror of this conversation is frozen at its last in-budget snapshot for the rest of the run")
	}
	lines := strings.Split(strings.TrimSuffix(wire, "\n"), "\n")
	if len(lines) != 1 {
		t.Fatalf("the sink wrote %d lines, want exactly one session_capture", len(lines))
	}
	if len(lines[0])+1 > delegate.MaxEnvelopeLineBytes {
		t.Fatalf("the mirror crossed as a %d-byte line, over the %d-byte cap — the launcher's reader fails the whole channel on it", len(lines[0])+1, delegate.MaxEnvelopeLineBytes)
	}
	var env delegate.Envelope
	if err := json.Unmarshal([]byte(lines[0]), &env); err != nil {
		t.Fatalf("decode what crossed: %v", err)
	}
	if env.Type != delegate.EnvelopeSessionCapture {
		t.Fatalf("the sink wrote a %q envelope, want %q", env.Type, delegate.EnvelopeSessionCapture)
	}
	if !strings.Contains(string(env.Data), "turn-4:") {
		t.Error("the mirrored conversation dropped its most recent turn")
	}
	if !strings.Contains(stderr, "compacted the session snapshot") {
		t.Errorf("the runner compacted the mirror and said nothing: stderr = %q", stderr)
	}
}

// A write the channel still refuses — a closed pipe, a peer gone — leaves the
// host's copy behind for the rest of the run. It is reported on the runner's
// stderr, not swallowed: a mirror that stopped mirroring is what makes
// CompactAndRetry shrink a conversation the model never had, and the write's
// own reason is the only account of why it stopped.
func TestCaptureSink_ARefusedWriteIsReportedNotSwallowed(t *testing.T) {
	snapshot, err := json.Marshal([]api.Message{{
		Role:    "assistant",
		Content: []api.ContentBlock{{Type: "text", Text: "the-in-container-turn"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	var stderrBuf bytes.Buffer
	sink := &dispatcherCaptureSink{
		dispatcher: newProxyDispatcher(strings.NewReader(""), brokenWire{}),
		stderr:     &stderrBuf,
	}
	sink.Capture(sandboxRunnerSessionID, "assistant_conversation", snapshot)
	if !strings.Contains(stderrBuf.String(), "the host's mirror of this conversation is now behind") {
		t.Errorf("a refused mirror write said nothing: %q", stderrBuf.String())
	}
	if !strings.Contains(stderrBuf.String(), "the peer is gone") {
		t.Errorf("the report does not carry the write's own reason: %q", stderrBuf.String())
	}
	// This is the gap the relay cannot carry — it travels on the channel
	// that just refused a write — so the stderr line is the whole account,
	// and the attempt to relay it is reported rather than swallowed.
	if !strings.Contains(stderrBuf.String(), "relaying that gap to the launcher failed too") {
		t.Errorf("the gap's own relay failed in silence: %q", stderrBuf.String())
	}
}

// brokenWire is a channel that refuses every write.
type brokenWire struct{}

func (brokenWire) Write([]byte) (int, error) { return 0, errors.New("the peer is gone") }

// The common case pays nothing: an in-budget snapshot crosses byte for byte,
// and a silent sink means a mirror that is exactly what the loop holds.
func TestCaptureSink_AnInBudgetConversationCrossesVerbatim(t *testing.T) {
	snapshot, err := json.Marshal([]api.Message{{
		Role:    "assistant",
		Content: []api.ContentBlock{{Type: "text", Text: "the-in-container-turn"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	wire, stderr := mirrorCapture(t, snapshot)
	var env delegate.Envelope
	if err := json.Unmarshal([]byte(strings.TrimSuffix(wire, "\n")), &env); err != nil {
		t.Fatalf("decode what crossed: %v", err)
	}
	if string(env.Data) != string(snapshot) {
		t.Errorf("the mirror rewrote an in-budget snapshot: %s, want %s", env.Data, snapshot)
	}
	if stderr != "" {
		t.Errorf("an in-budget mirror warned about nothing: %q", stderr)
	}
}

// shortMirrorConversation is a session no compaction brings under the
// channel's budget: tens of thousands of short turns, so the one-line-per
// message summary of all of them is over it too and no recent window helps.
func shortMirrorConversation(t *testing.T, n int) []byte {
	t.Helper()
	msgs := make([]api.Message, 0, n)
	for i := range n {
		msgs = append(msgs, api.Message{
			Role:    "user",
			Content: []api.ContentBlock{{Type: "text", Text: fmt.Sprintf("turn-%d:", i) + strings.Repeat("z", 200)}},
		})
	}
	raw, err := json.Marshal(msgs)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// A snapshot the bound cannot deliver leaves the host's copy behind for the
// rest of the run — it only grows — on the path where the node goes on to
// ANSWER. The runner's stderr says so for the container's own log, and the
// gap also crosses as a session_mirror_gap event: the channel the launcher
// reports from whatever the outcome. It carries the reason and the size,
// never the far side's own words.
func TestCaptureSink_ARefusedSnapshotRelaysTheGapToTheLauncher(t *testing.T) {
	snapshot := shortMirrorConversation(t, 40_000)
	if _, _, err := model.ConstrainSessionSnapshotForWire(snapshot); err == nil {
		t.Fatalf("the %d-byte fixture is deliverable once compacted — it drives no gap", len(snapshot))
	}

	wire, stderr := mirrorCapture(t, snapshot)
	if !strings.Contains(stderr, "the host's mirror of this conversation is now behind") {
		t.Errorf("the container's own log does not name the gap: %q", stderr)
	}
	if wire == "" {
		t.Fatal("nothing crossed: the gap stays in the container's own log, which is not what the launcher reports from")
	}
	var env delegate.Envelope
	if err := json.Unmarshal([]byte(strings.TrimSuffix(wire, "\n")), &env); err != nil {
		t.Fatalf("decode what crossed (%q): %v", wire, err)
	}
	if env.Type != delegate.EnvelopeEvent {
		t.Fatalf("the sink wrote a %q envelope, want the %q that relays the gap", env.Type, delegate.EnvelopeEvent)
	}
	var ev delegate.EventData
	if err := json.Unmarshal(env.Data, &ev); err != nil {
		t.Fatalf("decode the relayed event: %v", err)
	}
	if ev.Type != model.SessionMirrorGapEvent {
		t.Fatalf("the relayed event is %q, want %q", ev.Type, model.SessionMirrorGapEvent)
	}
	if ev.Payload["reason"] != model.MirrorGapSnapshotRefused {
		t.Errorf("the gap's reason is %v, want %q", ev.Payload["reason"], model.MirrorGapSnapshotRefused)
	}
	if got, want := ev.Payload["bytes"], float64(len(snapshot)); got != want {
		t.Errorf("the gap is reported over %v bytes, want the %v it could not deliver", got, want)
	}
	for _, s := range []string{"turn-0:", "zzz"} {
		if strings.Contains(string(env.Data), s) {
			t.Errorf("the relayed gap carries the conversation's own text (%q): the host composes its sentence from numbers", s)
		}
	}
}
