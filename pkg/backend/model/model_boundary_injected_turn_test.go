package model

import (
	"context"
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
// Content slice, same length, new text) hands the history's own slice back
// carrying a text it never held — the wrap's snapshot of texts, not the
// slice identity alone, screens it; and a hook that returns the input's
// backing array would let a screened copy overwrite the caller's live
// history. The caller's turn keeps what the HOOK wrote: the wrap screens
// what it hands the summariser, it never writes through.

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
	if got := history[1].Content[0].Text; got != "rewritten with "+sentinel {
		t.Errorf("the wrap wrote the screened text through to the caller's history: the turn the hook rewrote now holds %q", got)
	}
}

func TestAHookReturningTheInputsBackingLeavesTheCallersHistoryWhole(t *testing.T) {
	const sentinel = "ghp-backing-9c2e7a1d4f83"
	placeholder := secretguard.PlaceholderForName("api_token")
	screen := func(s string) string { return strings.ReplaceAll(s, sentinel, placeholder) }

	// Spare capacity: with len < cap the hook's append writes the added turn
	// into the input's own backing array, so "returns the input's backing
	// array" is the shape this witness actually builds.
	history := make([]api.Message, 1, 2)
	history[0] = api.Message{Role: "user", Content: []api.ContentBlock{
		{Type: "tool_use", Name: "tool_" + sentinel, Text: "carries " + sentinel},
	}}
	inject := func(msgs []api.Message) []api.Message {
		// Rewrite the history turn's tool Name in place (its Text keeps) AND
		// add an unmarked turn carrying a tool_use block, returning the
		// input's own backing array.
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
	// The in-place Name rewrite is a changed turn (the snapshot reads Names),
	// so the whole block takes the pass; the hook-added turn takes it marked
	// or not, its tool Name included.
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
	if got := history[0].Content[0].Name; got != sentinel {
		t.Errorf("the wrap wrote the screened tool Name through the returned backing array: the caller's history now holds %q", got)
	}
}
