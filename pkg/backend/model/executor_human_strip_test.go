package model

import (
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/cost"
)

// The metadata strip on the interaction path must remove everything the
// generation layer stamps (`_tokens`, `_model`, `_cost_usd`) and nothing the
// delegate actually asked — including questions whose sanitized key starts
// with `_`, which sanitizeSchemaKey produces from any leading punctuation.
func TestStripInteractionMetadata(t *testing.T) {
	t.Run("drops the metadata namespace", func(t *testing.T) {
		answers := map[string]any{"which_db": "postgres"}
		// Exactly what the real path stamps, via the real annotator.
		cost.Annotate(answers, "anthropic/claude-opus-5", 1000, 200)
		if _, ok := answers["_cost_usd"]; !ok {
			t.Fatal("precondition: cost.Annotate did not stamp _cost_usd")
		}

		stripInteractionMetadata(answers, map[string]any{"which_db": "which database?"})

		if got, want := answers["which_db"], "postgres"; got != want {
			t.Errorf("answer = %v, want %v", got, want)
		}
		for _, k := range []string{"_cost_usd", "_tokens", "_model"} {
			if _, ok := answers[k]; ok {
				t.Errorf("%s survived the strip — it would reach the re-invoked node's input", k)
			}
		}
	})

	t.Run("keeps an asked key that sanitizes to a _ prefix", func(t *testing.T) {
		// A delegate question keyed with leading punctuation. This is the
		// regression: a blanket prefix strip deletes the answer, the agent
		// re-asks, and the run burns interaction depth to no purpose.
		const question = "(a) which db?"
		key := sanitizeSchemaKey(question)
		if key[0] != '_' {
			t.Fatalf("precondition: sanitizeSchemaKey(%q) = %q, expected a _ prefix", question, key)
		}

		asked := map[string]any{key: question}
		answers := map[string]any{key: "postgres", "_tokens": 1200}

		stripInteractionMetadata(answers, asked)

		if got, ok := answers[key]; !ok || got != "postgres" {
			t.Errorf("answer for %q was dropped (%v, present=%v)", key, got, ok)
		}
		if _, ok := answers["_tokens"]; ok {
			t.Error("_tokens was not asked — it must still be stripped")
		}
	})

	t.Run("a literal underscore key is an asked key like any other", func(t *testing.T) {
		answers := map[string]any{"_notes": "keep me", "_model": "some/model"}
		stripInteractionMetadata(answers, map[string]any{"_notes": "notes?"})

		if answers["_notes"] != "keep me" {
			t.Errorf("_notes = %v, want it kept: it was asked", answers["_notes"])
		}
		if _, ok := answers["_model"]; ok {
			t.Error("_model was not asked — it must be stripped")
		}
	})
}
