package model

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/secretguard"
	"github.com/SocialGouv/iterion/pkg/llmroute"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

// The routing handoff (ADR-121 § Delivery 2): the recorder's vocabulary,
// the redaction boundary, the seal-on-fallback, the attach gate, the
// render. Each test names its mutation in a comment.

func newTestRecorder(t *testing.T) *HandoffRecorder {
	t.Helper()
	return NewHandoffRecorder(t.TempDir())
}

func sealForTest(t *testing.T, rec *HandoffRecorder, nodeID string, data map[string]any) string {
	t.Helper()
	p := rec.Seal(nodeID, data)
	if p == "" {
		t.Fatal("seal produced no file")
	}
	return p
}

// TestHandoffRecorderVocabulary: the four neutral types render; every
// other type is ignored; claw's double capture (the step text riding both
// llm_step_finished and a derived assistant_text) lands ONCE.
// Mutants: add EventLLMPrompt to the set → the ignored case reds; drop
// the dedup → the double-capture case reds.
func TestHandoffRecorderVocabulary(t *testing.T) {
	rec := newTestRecorder(t)
	rec.Observe(store.EventLLMPrompt, "n", map[string]any{"user_message": "hello"})
	rec.Observe(store.EventLLMStepFinished, "n", map[string]any{"response_text": "mid-turn narration", "number": 1})
	rec.Observe(store.EventAssistantText, "n", map[string]any{"text": "mid-turn narration"})
	rec.Observe(store.EventAssistantText, "n", map[string]any{"text": "a DISTINCT final answer"})
	rec.Observe(store.EventToolCalled, "n", map[string]any{"tool": "Bash", "input_size": 12})
	rec.Observe(store.EventToolError, "n", map[string]any{"tool": "Bash", "error": "exit 1"})

	body := renderHandoffFileForTest(t, rec, "n")
	if strings.Contains(body, "hello") {
		t.Fatal("llm_prompt recorded — the vocabulary must stay the four neutral types")
	}
	if got := strings.Count(body, "mid-turn narration"); got != 1 {
		t.Fatalf("the step's own prose landed %d times, want once (the derived assistant_text dedups against response_text)", got)
	}
	for _, want := range []string{"a DISTINCT final answer", "Bash", "exit 1", "llm_step_finished", "assistant_text", "tool_called", "tool_error"} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q:\n%s", want, body)
		}
	}
}

// TestHandoffRecorderTruncation: a buffer past the budget renders the
// head, the omission marker with the count, and the tail.
// Mutant: drop the middle-drops → the oversize case reds.
func TestHandoffRecorderTruncation(t *testing.T) {
	rec := newTestRecorder(t)
	first := "HEAD-MARKER"
	last := "TAIL-MARKER"
	rec.Observe(store.EventAssistantText, "n", map[string]any{"text": first})
	filler := strings.Repeat("filler ", 40)
	for i := 0; i < 1500; i++ {
		rec.Observe(store.EventToolCalled, "n", map[string]any{"tool": fmt.Sprintf("tool-%04d %s", i, filler)})
	}
	rec.Observe(store.EventAssistantText, "n", map[string]any{"text": last})

	body := renderHandoffFileForTest(t, rec, "n")
	if !strings.Contains(body, first) || !strings.Contains(body, last) {
		t.Fatal("head or tail lost to truncation — both ends must survive")
	}
	if !strings.Contains(body, "events omitted") {
		t.Fatal("the omission marker is missing — the middle collapsed silently")
	}
	if len(body) > handoffHeadBytes+handoffTailBytes+4096 {
		t.Fatalf("body = %d bytes, want bounded by the head+tail budget", len(body))
	}
}

// TestHandoffRecorderPerNodeIsolationConcurrent: concurrent branch
// goroutines append to per-node buffers; nothing crosses.
// Mutant: one shared buffer → the cross-node leak reds.
func TestHandoffRecorderPerNodeIsolationConcurrent(t *testing.T) {
	rec := newTestRecorder(t)
	var wg sync.WaitGroup
	for _, node := range []string{"a", "b"} {
		wg.Add(1)
		go func(node string) {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				rec.Observe(store.EventAssistantText, node, map[string]any{"text": node + "-work"})
			}
		}(node)
	}
	wg.Wait()
	for node, other := range map[string]string{"a": "b", "b": "a"} {
		body := renderHandoffFileForTest(t, rec, node)
		if !strings.Contains(body, node+"-work") {
			t.Errorf("node %s lost its own events", node)
		}
		if strings.Contains(body, other+"-work") {
			t.Errorf("node %s leaked %s's events — buffers must be per node", node, other)
		}
	}
}

// TestHandoffSealAndReseal: the sealed file is read-only; a SECOND
// crossing re-renders from the fuller buffer over the same path.
// Mutant: one-shot seal → the re-seal case reds.
func TestHandoffSealAndReseal(t *testing.T) {
	rec := newTestRecorder(t)
	rec.Observe(store.EventAssistantText, "n", map[string]any{"text": "before the crossing"})
	p := sealForTest(t, rec, "n", map[string]any{"cross_harness": "restart"})
	info, err := os.Stat(p)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != 0o444 {
		t.Fatalf("sealed mode = %v, want 0444 (read-only is the design's word)", info.Mode().Perm())
	}
	rec.Observe(store.EventAssistantText, "n", map[string]any{"text": "the first rung's own work"})
	p2 := rec.Seal("n", map[string]any{"cross_harness": "restart"})
	if p2 != p {
		t.Fatalf("re-seal path = %q, want the same %q", p2, p)
	}
	body, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read sealed file: %v", err)
	}
	if !strings.Contains(string(body), "the first rung's own work") {
		t.Fatal("re-seal did not re-render — a second crossing must hand over the first rung's work too")
	}
}

// TestHandoffRedactionBoundary: the recorder observes what PERSISTS —
// redacting(recorder(store)) — so a secret in an event lands redacted in
// the file. Mutant: recorder outside redactingEmitter → this test reds.
func TestHandoffRedactionBoundary(t *testing.T) {
	const secret = "supersecret-handler-value"
	g := secretguard.New([]secretguard.Secret{{Name: "MAPS_KEY", Value: secret}}, secretguard.DefaultConfig())
	rec := newTestRecorder(t)
	storeEmitter := &capturingEmitter{}
	chain := redactingEmitter{inner: HandoffEmitter(storeEmitter, rec), guard: g}
	_, _ = chain.AppendEvent(context.Background(), "run", store.Event{
		Type:   store.EventAssistantText,
		NodeID: "n",
		Data:   map[string]any{"text": "called with " + secret},
	})
	body := renderHandoffFileForTest(t, rec, "n")
	if strings.Contains(body, secret) {
		t.Fatal("the handoff file carries the RAW secret — the recorder must observe post-redaction data only")
	}
	if !strings.Contains(body, "called with") {
		t.Fatal("the redacted text did not arrive at all")
	}
}

// TestHandoffEmitterSealsOnFallback: the model_fallback event carrying an
// active posture seals and stamps the path on the SAME event; no posture
// stamps nothing; an empty buffer stamps "unavailable".
// Mutants: seal on every fallback → the no-posture case reds; drop the
// stamp → the path case reds.
func TestHandoffEmitterSealsOnFallback(t *testing.T) {
	rec := newTestRecorder(t)
	storeEmitter := &capturingEmitter{}
	em := HandoffEmitter(storeEmitter, rec)

	rec.Observe(store.EventAssistantText, "n", map[string]any{"text": "work"})
	_, err := em.AppendEvent(context.Background(), "run", store.Event{
		Type: store.EventModelFallback, NodeID: "n",
		Data: map[string]any{"cross_harness": "restart", "reason": "auth"},
	})
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	stamp, _ := storeEmitter.events[len(storeEmitter.events)-1].Data["handoff"].(string)
	if stamp == "" || stamp == "unavailable" {
		t.Fatalf("handoff stamp = %q, want the sealed path", stamp)
	}
	if _, err := os.Stat(stamp); err != nil {
		t.Fatalf("stamped path does not exist: %v", err)
	}

	// No posture: no stamp, no key — the event shape is unchanged.
	_, err = em.AppendEvent(context.Background(), "run", store.Event{
		Type: store.EventModelFallback, NodeID: "n2",
		Data: map[string]any{"reason": "boom"},
	})
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	if _, ok := storeEmitter.events[len(storeEmitter.events)-1].Data["handoff"]; ok {
		t.Fatal("a same-backend fall-through stamped a handoff — the gate is the marker value, not the event type")
	}

	// Active posture, empty buffer: said, not invented.
	_, err = em.AppendEvent(context.Background(), "run", store.Event{
		Type: store.EventModelFallback, NodeID: "n3",
		Data: map[string]any{"cross_harness": "reuse"},
	})
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	if stamp, _ := storeEmitter.events[len(storeEmitter.events)-1].Data["handoff"].(string); stamp != "unavailable" {
		t.Fatalf("empty-buffer stamp = %q, want \"unavailable\"", stamp)
	}
}

// TestHandoffEmitterNilByDefault: no recorder, no wrapper — the emitter
// is the store's, byte-identical. Mutant: construct on emptiness → reds.
func TestHandoffEmitterNilByDefault(t *testing.T) {
	storeEmitter := &capturingEmitter{}
	if got := HandoffEmitter(storeEmitter, nil); got != EventEmitter(storeEmitter) {
		t.Fatal("a nil recorder must return the inner emitter unchanged")
	}
}

// TestHandoffFreshnessDelayedRelay: the NAMED freshness witness — events
// delivered late through a relay-style channel, but before the seal, all
// land in the sealed file. The dispatch is synchronous in production
// (mux.Run drains inline), so a partial handoff can only come from a
// buffer-then-flush break, which is what this pins.
func TestHandoffFreshnessDelayedRelay(t *testing.T) {
	rec := newTestRecorder(t)
	type relayed struct {
		typ  store.EventType
		data map[string]any
	}
	ch := make(chan relayed, 4)
	go func() {
		for i := 0; i < 4; i++ {
			ch <- relayed{store.EventAssistantText, map[string]any{"text": fmt.Sprintf("relayed-%d", i)}}
			time.Sleep(time.Millisecond)
		}
	}()
	for i := 0; i < 4; i++ {
		r := <-ch
		rec.Observe(r.typ, "n", r.data)
	}
	p := sealForTest(t, rec, "n", map[string]any{"cross_harness": "restart"})
	body, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	for i := 0; i < 4; i++ {
		if !strings.Contains(string(body), fmt.Sprintf("relayed-%d", i)) {
			t.Fatalf("relayed-%d missing from the sealed file — a late-delivered event was dropped", i)
		}
	}
}

// TestHandoffRecorderNoRootSealsNothing: no state dir, no artifact dir —
// the recorder is inert and removes nothing at Cleanup.
func TestHandoffRecorderNoRootSealsNothing(t *testing.T) {
	rec := &HandoffRecorder{nodes: map[string]*handoffNode{}, now: time.Now}
	rec.Observe(store.EventAssistantText, "n", map[string]any{"text": "work"})
	if p := rec.Seal("n", map[string]any{"cross_harness": "restart"}); p != "" {
		t.Fatalf("seal without a state dir produced %q", p)
	}
	rec.Cleanup() // must not panic, must not remove anything
}

// TestHandoffCleanup: Close removes the directory the recorder created —
// and nothing else (deletion safety).
// Mutant: drop the created guard → the not-created case reds.
func TestHandoffCleanup(t *testing.T) {
	root := t.TempDir()
	rec := NewHandoffRecorder(root)
	rec.Observe(store.EventAssistantText, "n", map[string]any{"text": "work"})
	p := sealForTest(t, rec, "n", map[string]any{"cross_harness": "restart"})
	if filepath.Dir(p) != filepath.Join(root, "routing-handoff") {
		t.Fatalf("sealed under %q, want the routing-handoff dir", filepath.Dir(p))
	}
	keep := filepath.Join(root, "unrelated")
	if err := os.WriteFile(keep, []byte("keep me"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	// A recorder that never created the directory removes nothing — even
	// when the directory exists (another's creation is not its to delete).
	bystander := NewHandoffRecorder(root)
	bystander.Cleanup()
	if _, err := os.Stat(p); err != nil {
		t.Fatal("a recorder that created nothing removed the directory")
	}
	rec.Cleanup()
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatal("the routing-handoff dir survived Cleanup")
	}
	if _, err := os.Stat(keep); err != nil {
		t.Fatal("Cleanup removed a path it did not create")
	}
	rec.Cleanup() // idempotent
}

// capturingEmitter records what reached the store.
type capturingEmitter struct {
	events []store.Event
}

func (c *capturingEmitter) AppendEvent(_ context.Context, _ string, evt store.Event) (*store.Event, error) {
	c.events = append(c.events, evt)
	return &evt, nil
}

func renderHandoffFileForTest(t *testing.T, rec *HandoffRecorder, nodeID string) string {
	t.Helper()
	p := sealForTest(t, rec, nodeID, map[string]any{"cross_harness": "restart"})
	body, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read sealed file: %v", err)
	}
	return string(body)
}

// TestElementBuilderHandoffAttach: the clearing branch clears the handoff
// fields UNCONDITIONALLY on the cached task and re-attaches only under
// the FULL marker (backend changed AND posture active). Chain
// [claude_code, claw(restart), claw] under an active posture: rung 2
// carries the sealed path; rung 3 — the same cached claw task, same
// backend, marker empty — carries NONE (F1's both halves).
// Mutants: a posture-only gate → this test reds on rung 3; dropping the
// unconditional clears → rung 3 keeps rung 2's stale fields, same red.
func TestElementBuilderHandoffAttach(t *testing.T) {
	tail := &backendScriptedBackend{name: delegate.BackendClaw}
	head := &backendScriptedBackend{name: delegate.BackendClaudeCode}
	reg := delegate.NewRegistry()
	reg.Register(delegate.BackendClaw, tail)
	reg.Register(delegate.BackendClaudeCode, head)
	e := newFallbackExecutor(reg, EventHooks{})
	rec := newTestRecorder(t)
	rec.Observe(store.EventAssistantText, "review", map[string]any{"text": "the failed rung's work"})
	if p := rec.Seal("review", map[string]any{"cross_harness": "restart"}); p == "" {
		t.Fatal("setup: seal produced nothing")
	}
	e.handoff = rec

	clawEl := chainElement{Label: "api", Backend: delegate.BackendClaw, Model: "anthropic/claude-opus-5", CrossHarness: llmroute.CrossHarnessRestart}
	build := e.newElementBuilder("review", delegate.BackendClaudeCode, nil,
		func(_ context.Context, bn string) (*delegate.Task, error) {
			return &delegate.Task{NodeID: "review", Model: "claude-opus-5", SystemPromptMode: delegate.SystemPromptModeForBackend(bn)}, nil
		})

	// Rung 2: a cross-backend crossing with the marker — attaches.
	_, _, task2, err := build(context.Background(), 1, clawEl, llmroute.CrossHarnessRestart)
	if err != nil {
		t.Fatalf("build rung 2: %v", err)
	}
	if task2.Handoff == "" || task2.HandoffMode != llmroute.CrossHarnessRestart {
		t.Fatalf("rung 2 handoff = %q/%q, want the sealed path + restart", task2.Handoff, task2.HandoffMode)
	}

	// Rung 3: the SAME cached claw task, same backend, marker empty —
	// cleared, no stale path.
	_, _, task3, err := build(context.Background(), 2, chainElement{Label: "api2", Backend: delegate.BackendClaw, Model: "anthropic/claude-opus-5"}, "")
	if err != nil {
		t.Fatalf("build rung 3: %v", err)
	}
	if task3 != task2 {
		t.Fatal("setup: the builder must hand back the same cached task for the same backend")
	}
	if task3.Handoff != "" || task3.HandoffMode != "" {
		t.Fatalf("rung 3 kept handoff %q/%q — a same-backend later rung must neither re-attach nor keep the previous rung's fields", task3.Handoff, task3.HandoffMode)
	}

	// The carry paths never see a handoff: a same-backend FIRST rung
	// (index 1, session carried) reaches the builder with an EMPTY marker
	// — crossHarnessMarker returns "" for a same-backend crossing (the
	// S3 marker test pins that), so the walk cannot hand the builder a
	// posture here. Zero fields, session rides: the pre-S4 contract.
	sameEl := chainElement{Label: "same", Model: "claude-opus-5"}
	_, _, taskSame, err := build(context.Background(), 1, sameEl, "")
	if err != nil {
		t.Fatalf("build carry: %v", err)
	}
	if taskSame.Handoff != "" {
		t.Fatalf("carry rung attached a handoff: %q", taskSame.Handoff)
	}
}

// TestHandoffPromptRender: reuse = preamble + transcript + the original
// prompt as the task statement; restart = the original prompt bytes FIRST
// (byte-preserved) + the reference section; the composer NEVER writes
// back into the task; an unreadable file is said, not silently dropped.
// Mutant: restart rewriting the prompt → the byte-preservation case reds.
func TestHandoffPromptRender(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "handoff.md")
	if err := os.WriteFile(path, []byte("## assistant_text\n\nthe work\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	const prompt = "do the thing, carefully"

	reuse := &delegate.Task{UserPrompt: prompt, Handoff: path, HandoffMode: llmroute.CrossHarnessReuse}
	got := reuse.HandoffPrompt()
	if !strings.HasPrefix(got, "CONTINUATION:") {
		t.Fatalf("reuse prompt missing the continuation preamble:\n%s", got)
	}
	if !strings.Contains(got, "the work") || !strings.HasSuffix(got, prompt) {
		t.Fatalf("reuse prompt must carry the transcript then the original prompt as the task statement:\n%s", got)
	}

	restart := &delegate.Task{UserPrompt: prompt, Handoff: path, HandoffMode: llmroute.CrossHarnessRestart}
	got = restart.HandoffPrompt()
	if !strings.HasPrefix(got, prompt) {
		t.Fatalf("restart prompt must open with the ORIGINAL bytes, got:\n%s", got)
	}
	if !strings.Contains(got, "the work") || !strings.Contains(got, path) {
		t.Fatalf("restart prompt must carry the transcript section naming the path:\n%s", got)
	}

	// Purity: composing leaves the task's own fields untouched.
	if restart.UserPrompt != prompt || restart.Handoff != path {
		t.Fatal("the composer wrote back into the task — it must be pure (a cached task re-renders)")
	}

	// No handoff: byte-identity.
	if got := (&delegate.Task{UserPrompt: prompt}).HandoffPrompt(); got != prompt {
		t.Fatal("a task without a handoff must return the prompt unchanged")
	}

	// Unreadable: said in the section.
	missing := &delegate.Task{UserPrompt: prompt, Handoff: filepath.Join(root, "gone.md"), HandoffMode: llmroute.CrossHarnessRestart}
	if got := missing.HandoffPrompt(); !strings.Contains(got, "unreadable") {
		t.Fatalf("an unreadable transcript must be said:\n%s", got)
	}
}

// TestNewStoreEventHooks_capabilityDetectionSurvivesTheHandoffWrap: the
// recorder wraps INSIDE the constructor, AFTER the capability detection —
// a store's optional-sink interfaces stay visible under an active
// posture. This is revi's R-finding on #2237: an emitter wrapper around
// the argument hid them and every sink went nil (turns, plans, tool
// blobs, served records, attachments) for exactly the runs the posture
// arms.
// Mutant: the handoff wrap moved OUTSIDE NewStoreEventHooks (the caller
// wraps the emitter) → this test reds on the turn never landing.
func TestNewStoreEventHooks_capabilityDetectionSurvivesTheHandoffWrap(t *testing.T) {
	fs := &turnRecordingEmitter{store: &capturingEmitter{}}
	rec := NewHandoffRecorder(t.TempDir())
	hooks := NewStoreEventHooks(context.Background(), fs, "run-cap", iterlog.New(iterlog.LevelError, nil), nil, rec)
	if hooks.OnLLMTurnCapture == nil {
		t.Fatal("OnLLMTurnCapture not registered — the turn capability was not detected")
	}
	hooks.OnLLMTurnCapture("n", LLMTurnCaptureInfo{Iteration: 1})
	if len(fs.turns) != 1 {
		t.Fatalf("turns captured = %d, want 1 — the handoff wrap hid the store's TurnWriter from the capability detection", len(fs.turns))
	}
	// And the handoff still observed through the same chain: the recorder
	// buffered the neutral events that rode the same AppendEvent path
	// (a seal produces a file only because the buffer is non-empty).
	hooks.OnAssistantText("n", AssistantTextInfo{Text: "work"})
	if p := rec.Seal("n", map[string]any{"cross_harness": "restart"}); p == "" {
		t.Fatal("the recorder did not observe through the internal wrap — its buffer is empty")
	}
}

// turnRecordingEmitter is a store that also implements the optional
// TurnWriter capability.
type turnRecordingEmitter struct {
	store EventEmitter
	turns []store.TurnCheckpoint
}

func (e *turnRecordingEmitter) AppendEvent(ctx context.Context, runID string, evt store.Event) (*store.Event, error) {
	return e.store.AppendEvent(ctx, runID, evt)
}

func (e *turnRecordingEmitter) WriteTurn(_ context.Context, tc *store.TurnCheckpoint) error {
	e.turns = append(e.turns, *tc)
	return nil
}

// TestHandoffRecorder_tailSurvivesAnOversizedHead: a first event bigger
// than the head budget cannot eat the tail's share — the newest work
// keeps its ~160 KiB — and a single event bigger than the WHOLE budget is
// clamped, never evaporated (revi R49ea1b).
// Mutants: the old head-extension (sum-before ≤ head) → the starve case
// reds on the last event; the append clamp dropped → the oversize case
// reds on the truncated marker.
func TestHandoffRecorder_tailSurvivesAnOversizedHead(t *testing.T) {
	rec := newTestRecorder(t)
	big := strings.Repeat("H", handoffHeadBytes+40*1024) // 72 KiB first event: alone exceeds the 32 KiB head
	rec.Observe(store.EventAssistantText, "n", map[string]any{"text": big})
	for i := 0; i < 200; i++ {
		rec.Observe(store.EventToolCalled, "n", map[string]any{"tool": strings.Repeat("t", 1024)})
	}
	last := "TAIL-MARKER-LAST"
	rec.Observe(store.EventAssistantText, "n", map[string]any{"text": last})

	body := renderHandoffFileForTest(t, rec, "n")
	if !strings.Contains(body, last) {
		t.Fatal("the newest event starved — an oversized head ate the tail's budget")
	}
	if len(body) > handoffHeadBytes+handoffTailBytes+8192 {
		t.Fatalf("body = %d bytes, want bounded by the budget", len(body))
	}
	// The oversized FIRST event is tail-region (it does not fit the head
	// budget): when the budget overflows it drops FIRST and is COUNTED —
	// it must not be retained as a protected head that starves the newest
	// work down to the leftover.
	if strings.Contains(body, big[:512]) {
		t.Fatal("the oversized first event stayed as the head — the tail kept only the budget's leftover")
	}
	if !strings.Contains(body, "events omitted") {
		t.Fatal("the dropped entries were not counted in the omission marker")
	}

	// A single event bigger than the WHOLE budget: clamped and present.
	huge := strings.Repeat("W", handoffHeadBytes+handoffTailBytes+64*1024)
	rec.Observe(store.EventAssistantText, "n2", map[string]any{"text": huge})
	body2 := renderHandoffFileForTest(t, rec, "n2")
	if !strings.Contains(body2, "event truncated") {
		t.Fatalf("the oversized event vanished or went unmarked:\n%s", body2[:300])
	}
	if len(body2) > handoffHeadBytes+handoffTailBytes+8192 {
		t.Fatalf("body2 = %d bytes, want clamped to the budget", len(body2))
	}
}
