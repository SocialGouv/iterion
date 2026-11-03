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
