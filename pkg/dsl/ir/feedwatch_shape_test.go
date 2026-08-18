package ir

import (
	"os"
	"path/filepath"
	"testing"
)

// TestAlwaysReachesLLM_TheRealTwoModeBot is the regression lock for the
// defect the FIRST attempt at this guard shipped with.
//
// feed-watch carries both halves of a two-mode bot in one `.bot`: `collect`
// polls feeds with tool nodes and never calls a model, `digest` synthesises
// with an agent. A predicate that asks "does this graph CONTAIN an LLM
// node?" answers yes for both, so the collect half kept being refused by a
// cap it could not have spent against — which is what silenced the
// production veille for five days.
//
// Reading the real bot rather than a hand-built fixture is the point: the
// first fix passed every synthetic test and still failed here.
func TestAlwaysReachesLLM_TheRealTwoModeBot(t *testing.T) {
	path := filepath.Join("..", "..", "..", "bots", "feed-watch", "main.bot")
	src, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("catalog bot not readable from here: %v", err)
	}
	w := mustCompile(t, string(src))

	if !w.UsesLLM() {
		t.Fatal("precondition: the bot does contain an agent node (synthesize)")
	}
	if w.AlwaysReachesLLM() {
		t.Error("feed-watch has a model-free path (mode=collect); a pre-flight cap must not refuse it")
	}
}
