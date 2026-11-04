package model

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/SocialGouv/claw-code-go/pkg/api"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/secretguard"
	"github.com/SocialGouv/iterion/pkg/knowledge"
)

// Serial by table (parallel_campaign_test.go): the witness drives the real
// workspace-memory installer over a seeded store, and memTest sets
// ITERION_HOME — a file cannot hold both t.Setenv and t.Parallel.

// shortRootStore is the FS store with a two-character display root. The
// summariser excerpts the first 160 runes of each user turn it folds in, so
// whether the index head reaches its first entry is decided by the root's
// length — a temp dir's path would decide this test instead of the pass.
type shortRootStore struct{ knowledge.MemoryStore }

func (shortRootStore) Root(knowledge.SpaceRef) (string, error) { return "/m", nil }

// The third producer of text claw composes for ITSELF: at compaction the
// workspace-memory installer re-reads the index and autoload and injects
// them as a user turn for the summariser. No request ever carries that turn
// — compaction folds it into the summary first — so the request chokepoint
// cannot see it, and a value a memory entry quoted would reach the model
// through the summary's excerpts. The turn crosses at the summariser's door
// instead, through the hook wrap compactBetweenIterations applies.
//
// The history is a post-first-compaction one: its head is the continuation
// turn an earlier compaction left (IsInjected, as the vendored compactor
// builds it), so the wrap's pass over every injected turn of the slice is
// exercised, not only the hook's addition.
func TestTheInjectedMemoryTurnCrossesTheBoundaryBeforeTheSummariser(t *testing.T) {
	const sentinel = "ghp-precompact-7a1b3d4f9c2e"
	placeholder := secretguard.PlaceholderForName("api_token")
	guard := secretguard.New([]secretguard.Secret{{Name: "api_token", Value: sentinel}}, secretguard.DefaultConfig())

	fs, ref, scope := memTest(t, "session-continuity")
	// The title lands in the index head, the body in the autoload block: the
	// value is in the first 160 runes of the turn either way.
	if err := scope.Write("CONTEXT_BRIEF.md", []byte("---\ntitle: deploy token "+sentinel+"\n---\nthe deploy token is "+sentinel)); err != nil {
		t.Fatalf("memory entry: %v", err)
	}
	opts := &GenerationOptions{
		Model:                 "test-only-unknown-model",
		CompactPreserveRecent: 1,
		ScreenModelText:       guard.RedactForModel,
	}
	// The real installer wires the real injector, as claw's Execute does
	// after it set the pass on the options.
	if err := installWorkspaceMemory(context.Background(), opts, shortRootStore{fs}, ref, &delegate.MemorySpec{
		Scope:            "session-continuity",
		Autoload:         []string{"CONTEXT_BRIEF.md"},
		PreCompactInject: true,
	}); err != nil {
		t.Fatalf("installWorkspaceMemory: %v", err)
	}
	if opts.OnBeforeCompact == nil {
		t.Fatal("the installer wired no compaction hook: this test exercises nothing")
	}
	// Premise: the producer composes the value, so what follows is the pass.
	raw := opts.OnBeforeCompact(nil)
	if len(raw) != 1 || !raw[0].IsInjected || !strings.Contains(raw[0].Content[0].Text, sentinel) {
		t.Fatalf("the injector composed no turn carrying the memory entry's value: %+v", raw)
	}

	// An earlier compaction's continuation turn, then two user turns and two
	// replies, large enough to cross the unknown model's threshold: with one
	// turn preserved, the summariser folds the injected turns and both user
	// turns into its recent-requests excerpts.
	continuation := api.Message{Role: "user", IsInjected: true, Content: []api.ContentBlock{
		{Type: "text", Text: "continuation summary: the deploy token is " + sentinel},
	}}
	history := append([]api.Message{continuation}, bigTranscript()[:4]...)

	// What the summariser is handed: every injected turn of the slice
	// screened — the hook's addition and the continuation alike — and the
	// history's own non-injected messages handed back as they are.
	handed := screenedInjector(opts.OnBeforeCompact, opts.screenModelText())(history)
	if len(handed) != len(history)+1 || !handed[0].IsInjected {
		t.Fatalf("the screened hook changed the injection's shape: %d messages, first injected=%v", len(handed), handed[0].IsInjected)
	}
	if got := handed[0].Content[0].Text; strings.Contains(got, sentinel) || !strings.Contains(got, placeholder) {
		t.Errorf("the summariser is handed the memory turn carrying the value: %q", got)
	}
	if got := handed[1].Content[0].Text; strings.Contains(got, sentinel) || !strings.Contains(got, placeholder) {
		t.Errorf("the summariser is handed the earlier continuation turn carrying the value: %q", got)
	}
	for i := 1; i < len(history); i++ {
		if handed[i+1].Content[0].Text != history[i].Content[0].Text {
			t.Errorf("the screened hook rewrote history message %d, which is not injected and crossed the boundary where it was born", i)
		}
	}
	if !strings.Contains(history[0].Content[0].Text, sentinel) {
		t.Error("the screened hook rewrote the live history's continuation turn in place; the wrap screens a copy the summariser reads")
	}

	// The wiring: the history the NEXT request carries after compaction.
	out := compactBetweenIterations(context.Background(), history, *opts)
	if len(out) >= len(history) {
		t.Fatalf("compaction did not fire (%d → %d messages): the summary was never built, so this test proves nothing", len(history), len(out))
	}
	carried := 0
	for i, msg := range out {
		for _, block := range msg.Content {
			if strings.Contains(block.Text, sentinel) {
				t.Errorf("message %d of the compacted history reaches the model carrying the memory entry's value: %q", i, block.Text)
			}
			carried += strings.Count(block.Text, placeholder)
		}
	}
	// The summary excerpts the injected turns' heads, which hold the title's
	// placeholder and the continuation's: a compaction that dropped them
	// would count 0 and prove nothing about the pass.
	if carried == 0 && !t.Failed() {
		t.Fatal("the compacted history carries no placeholder: the summariser did not fold the injected turns in, so this test proves nothing")
	}
}

// The wrap's contract holds beyond the installer's own injector. Two hook
// shapes would slip past an identity-only rule, and each gets its own
// witness: a hook that rewrites one of the history's turns IN PLACE (same
// Content slice, same length, new text) hands back the slice the wrap gave
// it carrying a text the history never held — the compare against the
// history's own turn, not the slice identity alone, screens it; and a hook
// that returns the backing array it was handed would let a screened copy,
// or its own writes, overwrite the caller's live history. The hook works
// on a copy of the history, so the caller's turn keeps what it held: no
// write of the hook's, nor of the wrap's, goes through.

func TestAHookRewritingAnInputTurnInPlaceCrossesTheBoundary(t *testing.T) {
	const sentinel = "ghp-inplace-4f7a9c1d8e2b"
	placeholder := secretguard.PlaceholderForName("api_token")
	screen := func(s string) string { return strings.ReplaceAll(s, sentinel, placeholder) }

	history := []api.Message{
		{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: "turn one"}}},
		{Role: "assistant", Content: []api.ContentBlock{{Type: "text", Text: "turn two"}}},
	}
	inject := func(msgs []api.Message) []api.Message {
		// In place: same Content slice, same length, new text.
		msgs[1].Content[0].Text = "rewritten with " + sentinel
		return msgs
	}
	handed := screenedInjector(inject, screen)(history)

	if len(handed) != len(history) {
		t.Fatalf("the wrap returned %d turns, want the history's own length", len(handed))
	}
	if got := handed[1].Content[0].Text; strings.Contains(got, sentinel) || !strings.Contains(got, placeholder) {
		t.Errorf("the in-place rewrite reached the summariser as %q; want the sentinel screened to %q", got, placeholder)
	}
	if got := history[1].Content[0].Text; got != "turn two" {
		t.Errorf("a write went through to the caller's history: the turn the hook rewrote now holds %q", got)
	}
}

func TestAHookReturningTheInputsBackingLeavesTheCallersHistoryWhole(t *testing.T) {
	const sentinel = "ghp-backing-9c2e7a1d4f83"
	placeholder := secretguard.PlaceholderForName("api_token")
	screen := func(s string) string { return strings.ReplaceAll(s, sentinel, placeholder) }

	// Spare capacity: with len < cap a hook's append on the caller's own
	// slice would write the added turn into its backing array, so "returns
	// the input's backing array" is a shape this witness can build.
	history := make([]api.Message, 1, 2)
	history[0] = api.Message{Role: "user", Content: []api.ContentBlock{
		{Type: "tool_use", Name: "tool_" + sentinel, Text: "carries " + sentinel},
	}}
	inject := func(msgs []api.Message) []api.Message {
		// Rewrite the history turn's tool Name in place (its Text keeps) AND
		// add an unmarked turn carrying a tool_use block, returning the
		// backing array the hook was handed.
		msgs[0].Content[0].Name = sentinel
		return append(msgs, api.Message{Role: "user", Content: []api.ContentBlock{
			{Type: "text", Text: "added " + sentinel},
			{Type: "tool_use", Name: "tool_" + sentinel},
		}})
	}
	handed := screenedInjector(inject, screen)(history)

	if len(handed) != 2 {
		t.Fatalf("the wrap returned %d turns, want 2", len(handed))
	}
	// The in-place Name rewrite is a changed turn, so the whole block takes
	// the pass; the hook-added turn takes it marked or not, its tool Name
	// included.
	for i, want := range []string{"carries " + placeholder, "added " + placeholder} {
		if got := handed[i].Content[0].Text; got != want {
			t.Errorf("handed turn %d text is %q, want it screened to %q", i, got, want)
		}
	}
	for i := range handed {
		for j := range handed[i].Content {
			if got := handed[i].Content[j].Name; strings.Contains(got, sentinel) {
				t.Errorf("handed turn %d block %d carries the tool Name %q unscreened", i, j, got)
			}
		}
	}
	if got := history[0].Content[0].Name; got != "tool_"+sentinel {
		t.Errorf("a write went through to the caller's history: its tool Name now holds %q", got)
	}
	if got := history[:2][1]; len(got.Content) != 0 {
		t.Errorf("the hook's added turn landed in the caller's backing array: %+v", got)
	}
}

// whereCarried names every place of msgs a model reads that holds value:
// each block's text, its tool name, and its tool input as the summariser
// renders it (fmt's %v, keys included), the blocks nested under a
// tool_result likewise.
func whereCarried(msgs []api.Message, value string) []string {
	var at []string
	var walk func(loc string, blocks []api.ContentBlock)
	walk = func(loc string, blocks []api.ContentBlock) {
		for j, b := range blocks {
			here := fmt.Sprintf("%s block %d (%s)", loc, j, b.Type)
			for _, field := range [][2]string{{"text", b.Text}, {"name", b.Name}, {"input", fmt.Sprintf("%v", b.Input)}} {
				if strings.Contains(field[1], value) {
					at = append(at, here+" "+field[0]+": "+field[1])
				}
			}
			walk(here+" >", b.Content)
		}
	}
	for i, m := range msgs {
		walk(fmt.Sprintf("msg %d", i), m.Content)
	}
	return at
}

// toolPair is an assistant tool_use turn and the user turn carrying its
// result, as the tool loop appends them.
func toolPair(id string, input map[string]any, output string) []api.Message {
	return []api.Message{
		{Role: "assistant", Content: []api.ContentBlock{{Type: "tool_use", ID: id, Name: "bash", Input: input}}},
		{Role: "user", Content: []api.ContentBlock{{Type: "tool_result", ToolUseID: id, Content: []api.ContentBlock{{Type: "text", Text: output}}}}},
	}
}

// historyWithAToolPair is two bigTranscript turns, a tool pair whose
// input is {"cmd": "ls"} and whose output is "listing" (messages 2 and 3),
// then two more bigTranscript turns: past the unknown model's threshold.
func historyWithAToolPair() []api.Message {
	big := bigTranscript()
	history := append([]api.Message{}, big[:2]...)
	history = append(history, toolPair("hist-1", map[string]any{"cmd": "ls"}, "listing")...)
	return append(history, big[2:4]...)
}

// boundaryOptions are the options compactBetweenIterations reads, with a
// guard holding sentinel as api_token: an unknown model whose threshold
// bigTranscript crosses, one turn preserved, the guard's pass.
func boundaryOptions(sentinel string) (GenerationOptions, string) {
	guard := secretguard.New([]secretguard.Secret{{Name: "api_token", Value: sentinel}}, secretguard.DefaultConfig())
	return GenerationOptions{
		Model:                 "test-only-unknown-model",
		CompactPreserveRecent: 1,
		ScreenModelText:       guard.RedactForModel,
	}, secretguard.PlaceholderForName("api_token")
}

// assertCompactedHistoryClean runs opts' hook through
// compactBetweenIterations and refuses a compacted history that carries
// the value anywhere a model reads, or that carries no placeholder at all
// (the summariser folded none of the hook's text in, so the run proves
// nothing).
func assertCompactedHistoryClean(t *testing.T, history []api.Message, opts GenerationOptions, sentinel, placeholder string) {
	t.Helper()
	out := compactBetweenIterations(context.Background(), history, opts)
	if len(out) >= len(history) {
		t.Fatalf("compaction did not fire (%d → %d messages): the summary was never built, so this test proves nothing", len(history), len(out))
	}
	for _, at := range whereCarried(out, sentinel) {
		t.Errorf("the compacted history the next request carries holds the value at %s", at)
	}
	if len(whereCarried(out, placeholder)) == 0 && !t.Failed() {
		t.Fatal("the compacted history carries no placeholder: the summariser folded none of the hook's text in, so this test proves nothing")
	}
}

// The summariser renders a tool_use block's Input whole ("tool_use
// bash(map[...])"), keys included: a tool input a hook adds is text the
// hook composed, and every leaf of it crosses the boundary — a string, a
// key, a nested map or list, and a leaf of a type JSON never decodes to
// (rendered as the summariser renders it).
func TestAToolInputAHookAddsCrossesTheBoundary(t *testing.T) {
	const sentinel = "ghp-toolinput-3e8b1f6a2c9d"
	opts, placeholder := boundaryOptions(sentinel)
	hook := func(msgs []api.Message) []api.Message {
		added := toolPair("added-1", map[string]any{
			"argv":   []string{"echo", sentinel},
			"cmd":    "echo " + sentinel,
			"env":    map[string]any{"list": []any{sentinel}},
			sentinel: "as a key",
		}, "done")
		out := append([]api.Message{msgs[0]}, added...)
		return append(out, msgs[1:]...)
	}
	opts.OnBeforeCompact = hook

	history := bigTranscript()[:4]
	handed := screenedInjector(hook, opts.screenModelText())(history)
	if len(handed) != len(history)+2 {
		t.Fatalf("the wrap changed the hook's shape: %d messages, want %d", len(handed), len(history)+2)
	}
	for _, at := range whereCarried(handed, sentinel) {
		t.Errorf("the summariser is handed the hook's tool input carrying the value at %s", at)
	}
	if got := fmt.Sprintf("%v", handed[1].Content[0].Input); strings.Count(got, placeholder) != 4 {
		t.Errorf("the hook's tool input is handed as %s; want each of its four leaves screened to %q", got, placeholder)
	}
	assertCompactedHistoryClean(t, bigTranscript()[:4], opts, sentinel, placeholder)
}

// A hook that rewrites a history tool_use's Input IN PLACE hands the
// summariser an input the history never held. The hook works on its own
// copy of the history, input maps included, so the rewrite changes that
// copy's turn, which takes the pass, and the caller's turn keeps what it
// held.
func TestAHookRewritingAToolInputInPlaceCrossesTheBoundary(t *testing.T) {
	const sentinel = "ghp-inputinplace-5d2a8c4e1f7b"
	opts, placeholder := boundaryOptions(sentinel)
	hook := func(msgs []api.Message) []api.Message {
		msgs[2].Content[0].Input["cmd"] = "echo " + sentinel
		return msgs
	}
	opts.OnBeforeCompact = hook

	history := historyWithAToolPair()
	handed := screenedInjector(hook, opts.screenModelText())(history)
	if len(handed) != len(history) {
		t.Fatalf("the wrap returned %d turns, want the history's own %d", len(handed), len(history))
	}
	if got := handed[2].Content[0].Input["cmd"]; got != "echo "+placeholder {
		t.Errorf("the rewritten input is handed to the summariser as %v; want it screened to %q", got, "echo "+placeholder)
	}
	if got := history[2].Content[0].Input["cmd"]; got != "ls" {
		t.Errorf("the hook's in-place rewrite reached the caller's live history: its tool input now holds %v", got)
	}
	assertCompactedHistoryClean(t, historyWithAToolPair(), opts, sentinel, placeholder)
}

// The summariser reads the text nested under a tool_result ("tool_result
// <inner text>"): a nested text a hook adds, or rewrites in place on one
// of the history's tool_results, takes the pass like a top-level one, and
// the caller's history keeps what it held.
func TestANestedToolResultTextCrossesTheBoundaryAddedOrRewritten(t *testing.T) {
	const sentinel = "ghp-nested-8b3f1a6d2e9c"
	for _, tc := range []struct {
		name   string
		hook   func(msgs []api.Message) []api.Message
		handed int // the index of the tool_result turn in the hook's result
	}{
		{"added by the hook", func(msgs []api.Message) []api.Message {
			out := append([]api.Message{msgs[0]}, toolPair("added-1", map[string]any{"cmd": "cat"}, "added "+sentinel)...)
			return append(out, msgs[1:]...)
		}, 2},
		{"rewritten in place", func(msgs []api.Message) []api.Message {
			msgs[3].Content[0].Content[0].Text = "rewritten " + sentinel
			return msgs
		}, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts, placeholder := boundaryOptions(sentinel)
			opts.OnBeforeCompact = tc.hook

			history := historyWithAToolPair()
			handed := screenedInjector(tc.hook, opts.screenModelText())(history)
			if len(handed) <= tc.handed || len(handed[tc.handed].Content) == 0 || len(handed[tc.handed].Content[0].Content) == 0 {
				t.Fatalf("the wrap changed the hook's shape: no tool_result at turn %d of %+v", tc.handed, handed)
			}
			if got := handed[tc.handed].Content[0].Content[0].Text; strings.Contains(got, sentinel) || !strings.Contains(got, placeholder) {
				t.Errorf("the summariser is handed the nested tool_result text %q; want the sentinel screened to %q", got, placeholder)
			}
			if got := history[3].Content[0].Content[0].Text; got != "listing" {
				t.Errorf("the hook's nested write reached the caller's live history: it now holds %q", got)
			}
			assertCompactedHistoryClean(t, historyWithAToolPair(), opts, sentinel, placeholder)
		})
	}
}

// compactBetweenIterations compacts the history BEFORE the hook runs, and
// that first result's preserved turns are the history's own. A hook that
// rewrites a preserved turn in place and then returns nil, or a slice that
// does not re-compact, leaves the first result in force: it must still
// carry what the history held, never the hook's text. The hook works on
// its own copy of the history, so none of its writes reach the turns the
// first compaction kept.
func TestAHookRewritingInPlaceNeverReachesTheFirstCompaction(t *testing.T) {
	const sentinel = "ghp-firstcompact-2c7e9a1f4b8d"
	for _, tc := range []struct {
		name    string
		returns func(msgs []api.Message) []api.Message
	}{
		{"nil", func([]api.Message) []api.Message { return nil }},
		{"a slice too short to re-compact", func(msgs []api.Message) []api.Message { return msgs[len(msgs)-1:] }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts, _ := boundaryOptions(sentinel)
			history := bigTranscript()[:4]
			last := history[len(history)-1].Content[0].Text
			opts.OnBeforeCompact = func(msgs []api.Message) []api.Message {
				msgs[len(msgs)-1].Content[0].Text = "rewritten " + sentinel
				return tc.returns(msgs)
			}
			out := compactBetweenIterations(context.Background(), history, opts)
			if len(out) >= len(history) {
				t.Fatalf("compaction did not fire (%d → %d messages): no first result to keep, so this test proves nothing", len(history), len(out))
			}
			for _, at := range whereCarried(out, sentinel) {
				t.Errorf("the hook's in-place rewrite reached the compacted history at %s", at)
			}
			if got := history[len(history)-1].Content[0].Text; got != last {
				t.Errorf("the hook's in-place rewrite reached the caller's live history: its last turn now holds %.40q…", got)
			}
		})
	}
}
