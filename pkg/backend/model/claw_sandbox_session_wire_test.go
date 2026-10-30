package model

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SocialGouv/claw-code-go/pkg/api"
	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/sandbox"
)

// oversizeSlotConversation is a session a sandboxed crossing can legitimately
// find under its key: n messages of a megabyte each, the size an in-process
// sibling on the same slot leaves behind (nothing caps the store, and a
// single tool result may carry delegate.MaxToolResultBytes).
func oversizeSlotConversation(n int) []api.Message {
	big := strings.Repeat("x", 1<<20)
	msgs := make([]api.Message, 0, n)
	for i := range n {
		msgs = append(msgs, api.Message{
			Role:    "user",
			Content: []api.ContentBlock{{Type: "text", Text: fmt.Sprintf("turn-%d:", i) + big}},
		})
	}
	return msgs
}

// unshrinkableSlotConversation is a session no compaction brings under the
// budget: tens of thousands of short turns, so even the one-line-per-message
// summary of ALL of them is over it and no recent window helps. The
// deliverable shape is oversizeSlotConversation — few messages, each huge.
func unshrinkableSlotConversation() []api.Message {
	msgs := make([]api.Message, 0, 40_000)
	for i := range 40_000 {
		msgs = append(msgs, api.Message{
			Role:    "user",
			Content: []api.ContentBlock{{Type: "text", Text: fmt.Sprintf("turn-%d:", i) + strings.Repeat("z", 200)}},
		})
	}
	return msgs
}

// The pre-spawn seed reads the SESSION KEY, so for a shared session_slot it
// reads a conversation no per-crossing budget bounds — while the IPC refuses
// a line over delegate.MaxEnvelopeLineBytes at the source. Written whole, the
// refusal takes the dispatch into its Kill+Wait abort: the node neither
// answers nor fails. The seed is therefore compacted to fit, keeping the
// recent turns, and the crossing is spawned WITH the history it can carry.
func TestSandboxSeed_AnOversizeSessionCrossesCompactedNotRefused(t *testing.T) {
	t.Parallel()
	const runID = "run-1"
	store := newNodeSessionStore()
	store.save(runID, "assistant_conversation", oversizeSlotConversation(5))
	raw := store.LoadSnapshot(runID, "assistant_conversation")
	if len(raw) <= delegate.MaxEnvelopeLineBytes {
		t.Fatalf("the fixture is %d bytes, under the %d-byte line cap — it proves nothing", len(raw), delegate.MaxEnvelopeLineBytes)
	}

	ctx := withRuntimeContext(context.Background(), runID, store)
	var logs strings.Builder
	b := NewClawBackend(NewRegistry(), EventHooks{}, RetryPolicy{},
		WithClawLogger(iterlog.New(iterlog.LevelInfo, &logs)))
	captured := filepath.Join(t.TempDir(), "first.ndjson")
	task := delegate.Task{
		NodeID: "revise", SessionSlot: "assistant_conversation",
		Model: "openai/gpt-5", WorkDir: t.TempDir(),
		Sandbox: &taskCaptureSandbox{path: captured},
	}

	done := make(chan error, 1)
	go func() {
		_, err := b.executeViaSandboxRunner(ctx, task)
		done <- err
	}()
	select {
	case <-done:
	case <-time.After(dispatchBound):
		t.Fatalf("the dispatch did not return in %s — an over-cap seed strands executeViaSandboxRunner in its abort, so the node never answers and never fails", dispatchBound)
	}

	line, err := os.ReadFile(captured)
	if err != nil {
		t.Fatalf("read what the runner received: %v", err)
	}
	if len(line) == 0 {
		t.Fatal("nothing crossed: the slot's conversation was refused at the channel instead of being compacted onto it, and the crossing is replayed none of it")
	}
	if len(line)+1 > delegate.MaxEnvelopeLineBytes {
		t.Fatalf("the seed crossed as a %d-byte line, over the %d-byte cap", len(line)+1, delegate.MaxEnvelopeLineBytes)
	}
	var env delegate.Envelope
	if err := json.Unmarshal(line, &env); err != nil {
		t.Fatalf("decode the runner's first envelope: %v", err)
	}
	if env.Type != delegate.EnvelopeSessionReplay {
		t.Fatalf("the runner's first envelope is %q, want %q", env.Type, delegate.EnvelopeSessionReplay)
	}
	if !strings.Contains(string(env.Data), "turn-4:") {
		t.Error("the compacted seed dropped the most recent turn — the retry is replayed a history that stops before what it is retrying")
	}
	if !strings.Contains(string(env.Data), "Conversation summary:") {
		t.Error("the seed carries no compaction summary, so it was not the compacted form that crossed")
	}
	// The crossing got less than the slot holds; the operator is told so,
	// with both sizes — a replay quietly shrunk is a node answering from a
	// history nobody can account for.
	if !strings.Contains(logs.String(), "compacted the replayed conversation") ||
		!strings.Contains(logs.String(), fmt.Sprint(len(raw))) ||
		!strings.Contains(logs.String(), fmt.Sprint(len(env.Data))) {
		t.Errorf("the compaction of the replay is unreported, or does not name both sizes (%d -> %d): %q", len(raw), len(env.Data), logs.String())
	}
}

// The budget is a bound, not a promise: a conversation that stays over it
// once compacted, or one the store cannot even decode, is REFUSED with its
// size named. Never written (that kills the channel), never silently emptied.
func TestConstrainSessionSnapshotForWire_RefusesWhatItCannotBound(t *testing.T) {
	t.Parallel()
	t.Run("a snapshot that is not a conversation", func(t *testing.T) {
		_, reduced, err := constrainSessionSnapshotForWire([]byte(strings.Repeat("x", clawSessionWireBudget+1)))
		if err == nil {
			t.Fatal("an undecodable over-budget snapshot was accepted")
		}
		if reduced {
			t.Error("reduced must be false on a refusal")
		}
		if !strings.Contains(err.Error(), fmt.Sprint(clawSessionWireBudget+1)) {
			t.Errorf("the refusal does not name the size it refused: %v", err)
		}
	})

	t.Run("a conversation whose own summary is over the budget", func(t *testing.T) {
		msgs := unshrinkableSlotConversation()
		raw, err := json.Marshal(msgs)
		if err != nil {
			t.Fatal(err)
		}
		if len(raw) <= clawSessionWireBudget {
			t.Fatalf("the fixture is %d bytes, inside the %d-byte budget", len(raw), clawSessionWireBudget)
		}
		got, reduced, err := constrainSessionSnapshotForWire(raw)
		if err == nil {
			t.Fatalf("a %d-byte snapshot that cannot be compacted under %d bytes was accepted as %d bytes", len(raw), clawSessionWireBudget, len(got))
		}
		if reduced || got != nil {
			t.Errorf("a refusal returned a payload (%d bytes, reduced=%v)", len(got), reduced)
		}
		if !strings.Contains(err.Error(), fmt.Sprint(len(msgs))) {
			t.Errorf("the refusal does not name the conversation it refused: %v", err)
		}
		if !strings.Contains(err.Error(), "once compacted") {
			t.Errorf("a conversation the compactor DID squeeze is refused as one it could not: %v", err)
		}
	})

	// A conversation already reduced to its own continuation summary is
	// re-emitted whole by every window — the compactor excludes a leading
	// summary from what it summarizes — so nothing was compacted and saying
	// "once compacted" would describe a pass that never ran.
	t.Run("a conversation already reduced to its own summary", func(t *testing.T) {
		const preamble = "This session is being continued from a previous conversation that ran out of context. The summary below covers the earlier portion of the conversation.\n\n"
		raw, err := json.Marshal([]api.Message{{
			Role:       "user",
			IsInjected: true,
			Content:    []api.ContentBlock{{Type: "text", Text: preamble + "Conversation summary:\n" + strings.Repeat("z", clawSessionWireBudget)}},
		}})
		if err != nil {
			t.Fatal(err)
		}
		got, reduced, err := constrainSessionSnapshotForWire(raw)
		if err == nil {
			t.Fatalf("a %d-byte snapshot was accepted as %d bytes", len(raw), len(got))
		}
		if reduced || got != nil {
			t.Errorf("a refusal returned a payload (%d bytes, reduced=%v)", len(got), reduced)
		}
		if !strings.Contains(err.Error(), "holds nothing the compactor can summarize") {
			t.Errorf("the refusal names a compaction that never ran: %v", err)
		}
		if !strings.Contains(err.Error(), fmt.Sprint(len(raw))) {
			t.Errorf("the refusal does not name the size it refused: %v", err)
		}
	})
}

// A slot can hold a conversation no compaction brings under the channel's
// line cap. The replay is context the model is better off with, never the
// node's own work, so a refusal does not fail the crossing: the runner is
// spawned with its task and without that history. The size left behind is
// named — a node answering from a history nobody can account for is what
// this reports.
func TestSandboxSeed_AnUnboundableSessionIsLeftBehindNotFailed(t *testing.T) {
	t.Parallel()
	const runID = "run-1"
	store := newNodeSessionStore()
	store.save(runID, "assistant_conversation", unshrinkableSlotConversation())
	raw := store.LoadSnapshot(runID, "assistant_conversation")
	if _, _, err := constrainSessionSnapshotForWire(raw); err == nil {
		t.Fatalf("the %d-byte fixture is deliverable once compacted — it drives no refusal", len(raw))
	}

	ctx := withRuntimeContext(context.Background(), runID, store)
	var logs strings.Builder
	b := NewClawBackend(NewRegistry(), EventHooks{}, RetryPolicy{},
		WithClawLogger(iterlog.New(iterlog.LevelInfo, &logs)))
	env := firstDispatchedEnvelope(t, b, ctx, delegate.Task{
		NodeID: "revise", SessionSlot: "assistant_conversation",
		Model: "openai/gpt-5", WorkDir: t.TempDir(),
	})
	if env.Type != delegate.EnvelopeTask {
		t.Fatalf("the runner's first envelope is %q, want %q — a snapshot the channel cannot carry went onto it anyway", env.Type, delegate.EnvelopeTask)
	}
	if !strings.Contains(logs.String(), "spawned without its replayed conversation") ||
		!strings.Contains(logs.String(), fmt.Sprint(len(raw))) {
		t.Errorf("the refused replay is unreported, or does not name the %d bytes left behind: %q", len(raw), logs.String())
	}
}

// scriptedSandbox is a runner the test writes itself: sh runs script with
// args, so a crossing can be made to say something on its stderr, relay an
// envelope, and answer — the three outcomes a diagnostic has to survive.
type scriptedSandbox struct {
	script string
	args   []string
	// spawned counts the runners the launcher asked this sandbox for, so a
	// test can assert that a refusal happened BEFORE any process existed.
	spawned atomic.Int32
}

func (s *scriptedSandbox) Driver() string { return "scripted" }
func (s *scriptedSandbox) Command(ctx context.Context, argv []string, _ sandbox.ExecOpts) *exec.Cmd {
	return scriptedSpawn(ctx, argv, func() *exec.Cmd {
		s.spawned.Add(1)
		return exec.CommandContext(ctx, "sh", append([]string{"-c", s.script, "sh"}, s.args...)...)
	})
}

// scriptedSpawn gives the runner's spawn to a test's script and runs every
// other exec as written, as it would in the container. The launcher spawns
// the runner as `… iterion __claw-runner` behind the node session's wrapper;
// the exec that ends the node session is another argv. A double answering
// that exec with the runner's script would run the script a second time, and
// a pipe-holding script would then hold the end of the node. The script never
// ran the wrapper, so the sweep finds nothing of the runner's to stop.
func scriptedSpawn(ctx context.Context, argv []string, runner func() *exec.Cmd) *exec.Cmd {
	if n := len(argv); n >= 2 && argv[n-2] == "iterion" && argv[n-1] == "__claw-runner" {
		return runner()
	}
	if len(argv) == 0 {
		return exec.CommandContext(ctx, "false")
	}
	return exec.CommandContext(ctx, argv[0], argv[1:]...)
}
func (s *scriptedSandbox) Exec(context.Context, []string, sandbox.ExecOpts) (sandbox.ExecResult, error) {
	return sandbox.ExecResult{}, nil
}
func (s *scriptedSandbox) Cleanup(context.Context) error { return nil }

// envelopeScript writes envs as NDJSON and returns a script that answers the
// launcher with them, plus the file it reads from. preTaskLines is what the
// launcher writes before it waits for an answer — the task envelope, plus a
// session_replay ahead of it wherever the seed delivers one: the script
// drains them, so its own exit cannot race a launcher write into an EPIPE.
func envelopeScript(t *testing.T, preTaskLines int, envs ...delegate.Envelope) (string, string) {
	t.Helper()
	var buf strings.Builder
	for _, env := range envs {
		raw, err := json.Marshal(env)
		if err != nil {
			t.Fatalf("marshal %s envelope: %v", env.Type, err)
		}
		buf.Write(raw)
		buf.WriteByte('\n')
	}
	path := filepath.Join(t.TempDir(), "runner-says.ndjson")
	if err := os.WriteFile(path, []byte(buf.String()), 0o600); err != nil {
		t.Fatalf("write the runner's script: %v", err)
	}
	return fmt.Sprintf(`printf '%%s\n' "$1" >&2; head -n %d >/dev/null; cat "$2"; exit 0`, preTaskLines), path
}

func resultEnvelope(t *testing.T, output map[string]any) delegate.Envelope {
	t.Helper()
	env, err := delegate.NewResultEnvelope(delegate.IOResult{Output: output})
	if err != nil {
		t.Fatalf("build the runner's result envelope: %v", err)
	}
	return env
}

// A gap in the host's copy of the conversation fires on the path where the
// crossing goes on to ANSWER — the in-container loop saves, the bound or the
// channel refuses the snapshot — and the host's copy is what CompactAndRetry
// shrinks and what the next crossing on that session key replays. The gap
// therefore crosses as an event the launcher reports whatever the outcome,
// composed into the operator's line on THIS side from the reason and the size
// the runner sent. It is the launcher's own diagnostic — never handed to the
// node's event hooks.
func TestSandboxDispatch_AMirrorGapIsHeardOnACrossingThatAnswers(t *testing.T) {
	t.Parallel()
	gap, err := NewSessionMirrorGapEvent(MirrorGapSnapshotRefused, 10_588_891)
	if err != nil {
		t.Fatalf("build the gap envelope: %v", err)
	}
	script, says := envelopeScript(t, 1, gap, resultEnvelope(t, map[string]any{"verdict": "ok"}))

	var logs strings.Builder
	b := NewClawBackend(NewRegistry(), EventHooks{}, RetryPolicy{},
		WithClawLogger(iterlog.New(iterlog.LevelDebug, &logs)))
	res, err := b.executeViaSandboxRunner(context.Background(), delegate.Task{
		NodeID: "revise", SessionSlot: "assistant_conversation", Model: "openai/gpt-5", WorkDir: t.TempDir(),
		Sandbox: &scriptedSandbox{script: script, args: []string{
			"iterion-claw-runner: warn: the host's mirror of this conversation is now behind: the peer is gone", says}},
	})
	if err != nil {
		t.Fatalf("the crossing was failed by a diagnostic it answered through: %v", err)
	}
	if res.Output["verdict"] != "ok" {
		t.Fatalf("output = %v, want the runner's answer", res.Output)
	}
	if !strings.Contains(logs.String(), "10588891") || !strings.Contains(logs.String(), "assistant_conversation") {
		t.Errorf("the mirror gap reached nobody: the operator's log is %q", logs.String())
	}
	if !strings.Contains(logs.String(), "over the channel's line budget") {
		t.Errorf("the gap is reported without its reason: %q", logs.String())
	}
	// What the operator reads is composed on THIS side: the sentence the
	// container wrote is not it, so the account cannot be the stderr line
	// the launcher has no error to carry here.
	if !strings.Contains(logs.String(), "this copy is now behind what the container holds") {
		t.Errorf("the operator's line is not the host's own account of the gap: %q", logs.String())
	}
	// The launcher's own diagnostic, not one of the node's events: handed to
	// ApplyRelayedEvent it would be reported as a type this host does not
	// consume, and metered as nothing.
	if strings.Contains(logs.String(), "does not consume") {
		t.Errorf("the gap was passed to the node's event relay: %q", logs.String())
	}
}

// The seed refuses a conversation no compaction brings under the channel's
// line cap, and the crossing is spawned without it. What that crossing then
// captures is NOT that session's conversation — it holds none of it — so
// mirroring it under the session key would replace what the session's other
// nodes built, and what a persist node packs into its durable blob, with the
// turns of one crossing that never saw it. The retained conversation is also
// the only thing CompactAndRetry can shrink to make the next seed fit.
func TestSandboxMirror_ARefusedSeedKeepsTheSessionsConversation(t *testing.T) {
	t.Parallel()
	const runID = "run-1"
	store := newNodeSessionStore()
	store.save(runID, "assistant_conversation", unshrinkableSlotConversation())
	before := len(store.load(runID, "assistant_conversation"))
	raw := store.LoadSnapshot(runID, "assistant_conversation")
	if _, _, err := constrainSessionSnapshotForWire(raw); err == nil {
		t.Fatalf("the %d-byte fixture is deliverable once compacted — it drives no refusal", len(raw))
	}

	capture, err := json.Marshal([]api.Message{{
		Role:    "user",
		Content: []api.ContentBlock{{Type: "text", Text: "the-crossings-own-single-turn"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	script, says := envelopeScript(t, 1,
		delegate.NewSessionCaptureEnvelope(capture),
		resultEnvelope(t, map[string]any{"verdict": "ok"}))

	ctx := withRuntimeContext(context.Background(), runID, store)
	var logs strings.Builder
	b := NewClawBackend(NewRegistry(), EventHooks{}, RetryPolicy{},
		WithClawLogger(iterlog.New(iterlog.LevelDebug, &logs)))
	if _, err := b.executeViaSandboxRunner(ctx, delegate.Task{
		NodeID: "revise", SessionSlot: "assistant_conversation", Model: "openai/gpt-5", WorkDir: t.TempDir(),
		Sandbox: &scriptedSandbox{script: script, args: []string{"", says}},
	}); err != nil {
		t.Fatalf("a refused replay must not fail the crossing: %v", err)
	}

	after := store.load(runID, "assistant_conversation")
	if len(after) != before {
		t.Fatalf("the session holds %d messages, want the %d it accumulated — a crossing spawned with none of them replaced them with its own",
			len(after), before)
	}
	if strings.Contains(string(mustMarshalMessages(t, after)), "the-crossings-own-single-turn") {
		t.Error("the crossing's own capture landed in the session it was not given")
	}
	if !strings.Contains(logs.String(), "stays out of session") || !strings.Contains(logs.String(), "assistant_conversation") {
		t.Errorf("the crossing's conversation was dropped without a word: %q", logs.String())
	}
}

// The host learns that a crossing holds none of the session's conversation
// from two sides: its own seed, which could not deliver it, and the runner,
// which relays that the replay did not decode over there. Both keep that
// crossing's capture OUT of the session — its turns are not a continuation
// of a conversation it never saw, and the host's copy is what CompactAndRetry
// shrinks for the next attempt. A gap this host cannot place in the replay
// direction is read the same way: holding the mirror for one crossing costs
// that crossing's turns, taking a blind crossing's capture costs the
// session. A gap in the OTHER direction says the opposite — the container
// holds MORE than this copy — so the capture that follows it is a
// continuation the mirror must still take.
func TestSandboxMirror_ARelayedReplayGapKeepsTheSessionsConversation(t *testing.T) {
	t.Parallel()
	const runID = "run-1"
	sibling := []api.Message{
		{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: "sibling-turn-1"}}},
		{Role: "assistant", Content: []api.ContentBlock{{Type: "text", Text: "sibling-turn-2"}}},
		{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: "sibling-turn-3"}}},
	}
	for _, tc := range []struct {
		name string
		// reason is the gap's own, built by the runner's constructor;
		// payload, when set, is a wire form that constructor cannot make —
		// what a far side the host cannot read at all sends.
		reason  string
		payload map[string]any
		keeps   bool
		failed  bool
	}{
		{
			name:   "the replay did not decode in the container",
			reason: MirrorGapReplayUndecodable,
			keeps:  true,
		},
		{
			name:   "a reason this host has no sentence for",
			reason: "SOMETHING-NEWER",
			keeps:  true,
		},
		{
			name:    "a payload that does not decode into (reason, bytes)",
			payload: map[string]any{"reason": 7, "bytes": "lots"},
			keeps:   true,
		},
		{
			name:   "the replay did not decode, and the crossing then FAILS",
			reason: MirrorGapReplayUndecodable,
			keeps:  true,
			failed: true,
		},
		{
			name:   "the container's own snapshot was over the budget: it holds more, not less",
			reason: MirrorGapSnapshotRefused,
			keeps:  false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newNodeSessionStore()
			store.save(runID, "assistant_conversation", sibling)
			seeded := store.LoadSnapshot(runID, "assistant_conversation")
			if _, _, err := constrainSessionSnapshotForWire(seeded); err != nil {
				t.Fatalf("the fixture is not deliverable (%v): the seed would refuse it and the gap would prove nothing", err)
			}

			gap, err := NewSessionMirrorGapEvent(tc.reason, len(seeded))
			if tc.payload != nil {
				gap, err = delegate.NewEventEnvelope(SessionMirrorGapEvent, tc.payload)
			}
			if err != nil {
				t.Fatalf("build the gap envelope: %v", err)
			}
			last := resultEnvelope(t, map[string]any{"verdict": "ok"})
			if tc.failed {
				env, rerr := delegate.NewResultEnvelope(delegate.IOResult{Error: "the model refused"})
				if rerr != nil {
					t.Fatalf("build the failing result envelope: %v", rerr)
				}
				last = env
			}
			// The launcher writes the seed's replay AND the task envelope
			// before it waits, so the runner's script drains both.
			script, says := envelopeScript(t, 2, gap,
				delegate.NewSessionCaptureEnvelope(mustMarshalMessages(t, []api.Message{{
					Role:    "user",
					Content: []api.ContentBlock{{Type: "text", Text: "the-crossings-own-single-turn"}},
				}})),
				last)

			ctx := withRuntimeContext(context.Background(), runID, store)
			var logs strings.Builder
			b := NewClawBackend(NewRegistry(), EventHooks{}, RetryPolicy{},
				WithClawLogger(iterlog.New(iterlog.LevelDebug, &logs)))
			_, err = b.executeViaSandboxRunner(ctx, delegate.Task{
				NodeID: "revise", SessionSlot: "assistant_conversation", Model: "openai/gpt-5", WorkDir: t.TempDir(),
				Sandbox: &scriptedSandbox{script: script, args: []string{"", says}},
			})
			switch {
			case tc.failed && err == nil:
				t.Fatalf("the crossing answered: this case proves nothing about a gap on a FAILING one (log %q)", logs.String())
			case !tc.failed && err != nil:
				t.Fatalf("a relayed gap must not fail the crossing: %v", err)
			}

			after := mustMarshalMessages(t, store.load(runID, "assistant_conversation"))
			held := strings.Contains(string(after), "the-crossings-own-single-turn")
			if tc.keeps && held {
				t.Errorf("the capture of a crossing the runner says never read the replay REPLACED the session: %d messages left of %d",
					len(store.load(runID, "assistant_conversation")), len(sibling))
			}
			if tc.keeps && len(store.load(runID, "assistant_conversation")) != len(sibling) {
				t.Errorf("the session holds %d messages, want the %d it accumulated", len(store.load(runID, "assistant_conversation")), len(sibling))
			}
			if !tc.keeps && !held {
				t.Errorf("the mirror refused the capture of a crossing that DID hold the conversation: the host's copy is frozen at %d messages",
					len(store.load(runID, "assistant_conversation")))
			}
			// The gap is reported whatever the outcome — that is what the
			// event buys over the container's own stderr.
			if !strings.Contains(logs.String(), "assistant_conversation") ||
				(tc.payload == nil && !strings.Contains(logs.String(), fmt.Sprint(len(seeded)))) {
				t.Errorf("the gap did not reach the operator, or without the size the runner named: %q", logs.String())
			}
			if tc.keeps != strings.Contains(logs.String(), "stays out of session") {
				t.Errorf("the dropped capture is reported = %v, want %v: %q",
					strings.Contains(logs.String(), "stays out of session"), tc.keeps, logs.String())
			}
		})
	}
}

// A task that carries its own history — a paused conversation resumed, a
// completed one continued — is seeded NOTHING: the in-container path ignores
// the store for such a task exactly as an in-process node does, so a replay
// would cross for a reader that never reads it. And that crossing's capture
// IS the session's: it continued the conversation the task delivered, so the
// mirror takes it even where the host's own copy is one no channel could
// have carried.
func TestSandboxSeed_ATaskCarryingItsHistoryIsSeededNothingAndStillMirrors(t *testing.T) {
	t.Parallel()
	const runID = "run-1"
	resumed := func(t *testing.T) json.RawMessage {
		t.Helper()
		return mustMarshalMessages(t, []api.Message{{
			Role:    "user",
			Content: []api.ContentBlock{{Type: "text", Text: "the-task-carries-its-own-history"}},
		}})
	}

	t.Run("no replay crosses, because nothing over there would read it", func(t *testing.T) {
		store := newNodeSessionStore()
		store.save(runID, "assistant_conversation", []api.Message{{
			Role:    "user",
			Content: []api.ContentBlock{{Type: "text", Text: "a-stored-turn"}},
		}})
		ctx := withRuntimeContext(context.Background(), runID, store)
		task := delegate.Task{
			NodeID: "copi", SessionSlot: "assistant_conversation", Model: "openai/gpt-5", WorkDir: t.TempDir(),
			ResumeConversation: resumed(t),
		}
		// The in-container reader, on this very task: the store is not read.
		if got := applySessionMessagesForTask(ctx, task, GenerationOptions{}); len(got.Messages) != 0 {
			t.Fatalf("the in-container path prepends %d stored messages for a resumed task — the seed would not be redundant", len(got.Messages))
		}
		env := firstDispatchedEnvelope(t, newTestClawBackend(), ctx, task)
		if env.Type != delegate.EnvelopeSessionReplay {
			return
		}
		t.Errorf("the seed shipped a %d-byte session_replay the in-container path then ignores", len(env.Data))
	})

	t.Run("its capture is the session's where the host's copy could have crossed", func(t *testing.T) {
		store := newNodeSessionStore()
		// Deliverable, and only once compacted: the crossing that produced
		// the history this task carries was given this conversation, so the
		// capture coming back continues it.
		store.save(runID, "assistant_conversation", oversizeSlotConversation(5))
		before := len(store.load(runID, "assistant_conversation"))
		if _, reduced, err := constrainSessionSnapshotForWire(store.LoadSnapshot(runID, "assistant_conversation")); err != nil || !reduced {
			t.Fatalf("the fixture must be deliverable and compacted to make the point (reduced=%v): %v", reduced, err)
		}
		script, says := envelopeScript(t, 1,
			delegate.NewSessionCaptureEnvelope(mustMarshalMessages(t, []api.Message{{
				Role:    "user",
				Content: []api.ContentBlock{{Type: "text", Text: "the-resumed-crossings-continuation"}},
			}})),
			resultEnvelope(t, map[string]any{"verdict": "ok"}))

		ctx := withRuntimeContext(context.Background(), runID, store)
		var logs strings.Builder
		b := NewClawBackend(NewRegistry(), EventHooks{}, RetryPolicy{},
			WithClawLogger(iterlog.New(iterlog.LevelDebug, &logs)))
		if _, err := b.executeViaSandboxRunner(ctx, delegate.Task{
			NodeID: "copi", SessionSlot: "assistant_conversation", Model: "openai/gpt-5", WorkDir: t.TempDir(),
			ResumeConversation: resumed(t),
			Sandbox:            &scriptedSandbox{script: script, args: []string{"", says}},
		}); err != nil {
			t.Fatalf("the crossing failed: %v (log %q)", err, logs.String())
		}
		after := store.load(runID, "assistant_conversation")
		if !strings.Contains(string(mustMarshalMessages(t, after)), "the-resumed-crossings-continuation") {
			t.Errorf("the host dropped the capture of a crossing whose history came with the TASK and whose session it could have delivered: the session still holds %d of the %d messages",
				len(after), before)
		}
		if strings.Contains(logs.String(), "spawned without its replayed conversation") ||
			strings.Contains(logs.String(), "compacted the replayed conversation") {
			t.Errorf("the seed announced a replay nobody reads — it must say only what crossed: %q", logs.String())
		}
	})

	// The other verdict of the same bound. A host copy the channel cannot
	// carry reached NO crossing: the history this task carries came from a
	// crossing that ran blind, so its capture is one crossing's own turns,
	// not the session the host still holds — the corruption measured at
	// 40 000 messages down to 2, through the pause a blind crossing
	// produced.
	t.Run("a host copy that reached no crossing is not replaced by what the task carried", func(t *testing.T) {
		store := newNodeSessionStore()
		store.save(runID, "assistant_conversation", unshrinkableSlotConversation())
		before := len(store.load(runID, "assistant_conversation"))
		if _, _, err := constrainSessionSnapshotForWire(store.LoadSnapshot(runID, "assistant_conversation")); err == nil {
			t.Fatal("the fixture is deliverable: the seed would not refuse it and no crossing would be blind")
		}
		script, says := envelopeScript(t, 1,
			delegate.NewSessionCaptureEnvelope(mustMarshalMessages(t, []api.Message{{
				Role:    "user",
				Content: []api.ContentBlock{{Type: "text", Text: "the-blind-crossings-own-turns"}},
			}})),
			resultEnvelope(t, map[string]any{"verdict": "ok"}))

		ctx := withRuntimeContext(context.Background(), runID, store)
		var logs strings.Builder
		b := NewClawBackend(NewRegistry(), EventHooks{}, RetryPolicy{},
			WithClawLogger(iterlog.New(iterlog.LevelDebug, &logs)))
		if _, err := b.executeViaSandboxRunner(ctx, delegate.Task{
			NodeID: "copi", SessionSlot: "assistant_conversation", Model: "openai/gpt-5", WorkDir: t.TempDir(),
			ResumeConversation:     resumed(t),
			ResumePendingToolUseID: "toolu_pending", ResumeAnswer: "go on",
			Sandbox: &scriptedSandbox{script: script, args: []string{"", says}},
		}); err != nil {
			t.Fatalf("the crossing failed: %v (log %q)", err, logs.String())
		}
		after := store.load(runID, "assistant_conversation")
		if len(after) != before || strings.Contains(string(mustMarshalMessages(t, after[:1])), "the-blind-crossings-own-turns") {
			t.Errorf("the %d messages the channel could not deliver were replaced by the turns of a crossing that never saw them: %d left",
				before, len(after))
		}
		if !strings.Contains(logs.String(), "reached no crossing") ||
			!strings.Contains(logs.String(), "assistant_conversation") {
			t.Errorf("the host kept its copy without saying why, or without naming the session: %q", logs.String())
		}
	})
}

// The stranding fact travels WITH the pause (#2461). Crossing A relays a gap
// the host reads as "the container holds LESS than I do", so its capture stays
// out of the session — and the pause it then produces carries the fact on its
// result. The resume that brings the blind turns back is seeded with it
// (Task.SessionStranded, rehydrated from the checkpoint), so the resumed
// crossing's capture stays out too. The control arm resumes WITHOUT the fact
// — a checkpoint written before the field existed — and the mirror takes the
// capture: absent stays the pre-field behaviour, which is exactly the residual
// the carriage closes.
func TestSandboxMirror_AGapStrandsThePauseItProduces(t *testing.T) {
	t.Parallel()
	const runID = "run-1"
	const slot = "assistant_conversation"
	blind := mustMarshalMessages(t, []api.Message{{
		Role:    "user",
		Content: []api.ContentBlock{{Type: "text", Text: "the-blind-crossings-own-turn"}},
	}})

	store := newNodeSessionStore()
	store.save(runID, slot, []api.Message{
		{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: "sibling-turn-1"}}},
		{Role: "assistant", Content: []api.ContentBlock{{Type: "text", Text: "sibling-turn-2"}}},
	})
	before := len(store.load(runID, slot))
	ctx := withRuntimeContext(context.Background(), runID, store)

	gap, err := NewSessionMirrorGapEvent(MirrorGapReplayUndecodable, 210)
	if err != nil {
		t.Fatalf("build the gap envelope: %v", err)
	}
	pauseEnv, err := delegate.NewResultEnvelope(delegate.IOResult{
		Output: map[string]any{
			"_needs_interaction":     true,
			"_interaction_questions": map[string]any{delegate.AskUserQuestionKey: "go on?"},
		},
		PendingConversation: blind,
		PendingToolUseID:    "toolu_pending",
	})
	if err != nil {
		t.Fatalf("build the pause envelope: %v", err)
	}
	scriptA, saysA := envelopeScript(t, 2, gap,
		delegate.NewSessionCaptureEnvelope(blind),
		pauseEnv)
	var logsA strings.Builder
	bA := NewClawBackend(NewRegistry(), EventHooks{}, RetryPolicy{},
		WithClawLogger(iterlog.New(iterlog.LevelDebug, &logsA)))
	resA, err := bA.executeViaSandboxRunner(ctx, delegate.Task{
		NodeID: "copi", SessionSlot: slot, Model: "openai/gpt-5", WorkDir: t.TempDir(),
		Sandbox: &scriptedSandbox{script: scriptA, args: []string{"", saysA}},
	})
	if err != nil {
		t.Fatalf("the stranded crossing failed: %v (log %q)", err, logsA.String())
	}
	if got := len(store.load(runID, slot)); got != before {
		t.Fatalf("the gap-stranded crossing replaced the session: %d messages left of %d", got, before)
	}
	if !resA.SessionStranded {
		t.Fatalf("the pause a stranded crossing produced dropped the fact: SessionStranded = false beside %d bytes of blind conversation", len(blind))
	}

	// The resume of that pause: the fact came back with the conversation, so
	// the resumed crossing's own turns stay out of the session it never saw.
	resumedCapture := delegate.NewSessionCaptureEnvelope(mustMarshalMessages(t, []api.Message{
		{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: "the-blind-crossings-own-turn"}}},
		{Role: "assistant", Content: []api.ContentBlock{{Type: "text", Text: "the-answer-after-the-pause"}}},
	}))
	scriptB, saysB := envelopeScript(t, 1, resumedCapture, resultEnvelope(t, map[string]any{"verdict": "ok"}))
	bB := NewClawBackend(NewRegistry(), EventHooks{}, RetryPolicy{})
	if _, err := bB.executeViaSandboxRunner(ctx, delegate.Task{
		NodeID: "copi", SessionSlot: slot, Model: "openai/gpt-5", WorkDir: t.TempDir(),
		ResumeConversation:     blind,
		ResumePendingToolUseID: "toolu_pending", ResumeAnswer: "go on",
		SessionStranded: true,
		Sandbox:         &scriptedSandbox{script: scriptB, args: []string{"", saysB}},
	}); err != nil {
		t.Fatalf("the resumed crossing failed: %v", err)
	}
	if !strings.Contains(string(mustMarshalMessages(t, store.load(runID, slot))), "sibling-turn-1") {
		t.Fatal("the resumed crossing's capture became the session although the pause it resumes carried the stranding")
	}

	// The control: the same resume WITHOUT the fact — the checkpoint a
	// pre-#2461 runner wrote — still mirrors. The field is what decides,
	// and a legacy pause keeps the behaviour the carriage replaces.
	scriptC, saysC := envelopeScript(t, 1, resumedCapture, resultEnvelope(t, map[string]any{"verdict": "ok"}))
	bC := NewClawBackend(NewRegistry(), EventHooks{}, RetryPolicy{})
	if _, err := bC.executeViaSandboxRunner(ctx, delegate.Task{
		NodeID: "copi", SessionSlot: slot, Model: "openai/gpt-5", WorkDir: t.TempDir(),
		ResumeConversation:     blind,
		ResumePendingToolUseID: "toolu_pending", ResumeAnswer: "go on",
		Sandbox: &scriptedSandbox{script: scriptC, args: []string{"", saysC}},
	}); err != nil {
		t.Fatalf("the legacy resumed crossing failed: %v", err)
	}
	if strings.Contains(string(mustMarshalMessages(t, store.load(runID, slot))), "sibling-turn-1") {
		t.Fatal("a pause that never carried the fact stopped mirroring — absent must stay the pre-field behaviour")
	}
}

// The task envelope is the one launcher->runner envelope whose payload no
// producer bounds: a resumed crossing carries its whole pre-pause
// conversation, which cannot be compacted without dropping the pending
// tool_use block the resume answers. Over the channel's line cap it is
// refused BEFORE the spawn, with both sizes named and no runner started —
// where the same refusal after the spawn lands on the abort path, on a
// runner the launcher then has to kill while the node answers nothing.
func TestSandboxDispatch_ATaskOverTheChannelLineFailsBeforeAnyRunnerExists(t *testing.T) {
	t.Parallel()
	const runID = "run-1"
	carried := mustMarshalMessages(t, oversizeSlotConversation(5))
	for _, tc := range []struct {
		name    string
		task    delegate.Task
		wants   []string
		unwants []string
	}{
		{
			name: "the bytes are the conversation it resumes",
			task: delegate.Task{
				ResumeConversation:     carried,
				ResumePendingToolUseID: "toolu_pending",
				ResumeAnswer:           "go on",
			},
			wants: []string{
				"the task does not cross the sandbox channel",
				"the conversation this crossing resumes",
				fmt.Sprint(len(carried)),
				fmt.Sprint(delegate.MaxEnvelopeLineBytes),
			},
		},
		{
			name:    "the bytes are anywhere else in the task",
			task:    delegate.Task{UserPrompt: strings.Repeat("p", 5<<20)},
			wants:   []string{"the task does not cross the sandbox channel", fmt.Sprint(delegate.MaxEnvelopeLineBytes)},
			unwants: []string{"the conversation this crossing resumes"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			script, says := envelopeScript(t, 1, resultEnvelope(t, map[string]any{"verdict": "ok"}))
			sb := &scriptedSandbox{script: script, args: []string{"", says}}
			task := tc.task
			task.NodeID, task.Model, task.WorkDir = "copi", "openai/gpt-5", t.TempDir()
			task.SessionSlot, task.Sandbox = "assistant_conversation", sb

			var logs strings.Builder
			b := NewClawBackend(NewRegistry(), EventHooks{}, RetryPolicy{},
				WithClawLogger(iterlog.New(iterlog.LevelDebug, &logs)))
			// Bounded, because the failure this guards against is a dispatch
			// that never returns: refused after the spawn, the same line cap
			// lands on Kill + Wait with a runner still holding the channel.
			dispatched := make(chan error, 1)
			go func() {
				_, err := b.executeViaSandboxRunner(
					withRuntimeContext(context.Background(), runID, newNodeSessionStore()), task)
				dispatched <- err
			}()
			var err error
			select {
			case err = <-dispatched:
			case <-time.After(dispatchBound):
				t.Fatalf("the dispatch of a task over the %d-byte line cap did not return in %s: the node neither answers nor fails",
					delegate.MaxEnvelopeLineBytes, dispatchBound)
			}
			if err == nil {
				t.Fatalf("a task over the %d-byte line cap was dispatched", delegate.MaxEnvelopeLineBytes)
			}
			if !errors.Is(err, delegate.ErrEnvelopeLineTooLong) {
				t.Errorf("the refusal is untyped, so no caller can tell it from a dead channel: %v", err)
			}
			for _, want := range tc.wants {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal does not say %q: %v", want, err)
				}
			}
			for _, unwant := range tc.unwants {
				if strings.Contains(err.Error(), unwant) {
					t.Errorf("the refusal blames a resumed conversation this task does not carry: %v", err)
				}
			}
			if n := sb.spawned.Load(); n != 0 {
				t.Errorf("%d runner(s) were started for a task the channel cannot carry", n)
			}
		})
	}
}

// A continued conversation never reaches this arm — Execute refuses it in
// process, before any seed is computed — which is why the seed's rule meets
// a carried history as ResumeConversation and reads the other field only to
// keep the two spellings one rule.
func TestSandboxArm_AContinuedConversationIsRefusedBeforeTheSeed(t *testing.T) {
	t.Parallel()
	script, says := envelopeScript(t, 1, resultEnvelope(t, map[string]any{"verdict": "ok"}))
	sb := &scriptedSandbox{script: script, args: []string{"", says}}
	store := newNodeSessionStore()
	store.save("run-1", "assistant_conversation", []api.Message{{
		Role:    "user",
		Content: []api.ContentBlock{{Type: "text", Text: "a-stored-turn"}},
	}})
	b := NewClawBackend(NewRegistry(), EventHooks{}, RetryPolicy{})
	_, err := b.Execute(withRuntimeContext(context.Background(), "run-1", store), delegate.Task{
		NodeID: "copi", SessionSlot: "assistant_conversation", Model: "openai/gpt-5", WorkDir: t.TempDir(),
		ContinueConversation: mustMarshalMessages(t, []api.Message{{
			Role:    "user",
			Content: []api.ContentBlock{{Type: "text", Text: "a-completed-conversation"}},
		}}),
		Sandbox: sb,
	})
	if err == nil || !strings.Contains(err.Error(), "a continued conversation runs in-process, never through the sandbox runner") {
		t.Fatalf("the sandbox arm accepted a continued conversation: %v", err)
	}
	if n := sb.spawned.Load(); n != 0 {
		t.Errorf("%d runner(s) were started for a shape this arm refuses", n)
	}
}

// The mirror is best-effort, not silent: the host's copy is what
// CompactAndRetry shrinks and what the next crossing replays, so a capture
// that does not decode as a conversation — a version skew on the wire, a
// corrupt in-container store — leaves that copy behind and says so. The size
// is the whole account: the far side's bytes are not quoted into the
// operator's log.
func TestSandboxMirror_ACaptureThatDoesNotDecodeIsReportedNotSwallowed(t *testing.T) {
	t.Parallel()
	const runID = "run-1"
	garbage := json.RawMessage(`{"not":"a conversation"}`)
	script, says := envelopeScript(t, 1,
		delegate.NewSessionCaptureEnvelope(garbage),
		resultEnvelope(t, map[string]any{"verdict": "ok"}))

	store := newNodeSessionStore()
	ctx := withRuntimeContext(context.Background(), runID, store)
	var logs strings.Builder
	b := NewClawBackend(NewRegistry(), EventHooks{}, RetryPolicy{},
		WithClawLogger(iterlog.New(iterlog.LevelDebug, &logs)))
	if _, err := b.executeViaSandboxRunner(ctx, delegate.Task{
		NodeID: "copi", Model: "openai/gpt-5", WorkDir: t.TempDir(),
		Sandbox: &scriptedSandbox{script: script, args: []string{"", says}},
	}); err != nil {
		t.Fatalf("a capture the host cannot decode must not fail the crossing: %v", err)
	}
	if len(store.load(runID, "copi")) != 0 {
		t.Error("a snapshot that is not a conversation was stored as one")
	}
	if !strings.Contains(logs.String(), "is now behind") || !strings.Contains(logs.String(), fmt.Sprint(len(garbage))) {
		t.Errorf("the mirror fell behind in silence, or without naming the size it refused: %q", logs.String())
	}
	if strings.Contains(logs.String(), "not a conversation") {
		t.Errorf("the report quotes the far side's own bytes: %q", logs.String())
	}
}

// A gap this binary has no sentence for is still a gap: a newer runner naming
// a kind this host does not know, or a payload that does not decode into
// (reason, bytes), has left the host's copy behind all the same — and the
// report still composes its own words, so a far side that puts text in the
// reason does not get it into the operator's log.
func TestSandboxDispatch_AnUnknownMirrorGapIsStillReported(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		payload map[string]any
		says    string
	}{
		{
			name:    "a reason this host does not know",
			payload: map[string]any{"reason": "SOMETHING-NEWER", "bytes": 4096},
			says:    "has no sentence for, over 4096 bytes",
		},
		{
			name:    "a payload that does not decode",
			payload: map[string]any{"reason": 7, "bytes": "lots"},
			says:    "does not decode into (reason, bytes)",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env, err := delegate.NewEventEnvelope(SessionMirrorGapEvent, tc.payload)
			if err != nil {
				t.Fatal(err)
			}
			script, says := envelopeScript(t, 1, env, resultEnvelope(t, map[string]any{"verdict": "ok"}))

			var logs strings.Builder
			b := NewClawBackend(NewRegistry(), EventHooks{}, RetryPolicy{},
				WithClawLogger(iterlog.New(iterlog.LevelDebug, &logs)))
			if _, err := b.executeViaSandboxRunner(context.Background(), delegate.Task{
				NodeID: "copi", Model: "openai/gpt-5", WorkDir: t.TempDir(),
				Sandbox: &scriptedSandbox{script: script, args: []string{"", says}},
			}); err != nil {
				t.Fatalf("the crossing was failed by a diagnostic: %v", err)
			}
			if !strings.Contains(logs.String(), tc.says) {
				t.Errorf("the gap went unreported, or not as %q: %q", tc.says, logs.String())
			}
			if strings.Contains(logs.String(), "SOMETHING-NEWER") {
				t.Errorf("the report carries the far side's own words: %q", logs.String())
			}
		})
	}
}
