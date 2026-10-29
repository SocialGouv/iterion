package model

import (
	"errors"
	"testing"
)

// The one refusal body every enforcing lane reads (#2665 item 7): the
// unreadable route refuses before the forbidden primary — a lock that
// cannot be checked must not pass, whatever else the walk read — and the
// forbidden primary names node, provider and level. Mutant: the two checks
// swapped → the both-set case names the provider instead of the span.
func TestAllowlistRefusal_UnreadableFirstThenForbiddenPrimary(t *testing.T) {
	t.Parallel()
	if err := AllowlistRefusal(ProviderResolution{}, "team"); err != nil {
		t.Fatalf("a clean walk refused: %v", err)
	}
	forbidden := ProviderResolution{ForbiddenPrimary: map[string][]string{"openai": {"work"}}}
	var outside *ProviderOutsideError
	if err := AllowlistRefusal(forbidden, "team"); !errors.As(err, &outside) || outside.Site != "work" || outside.Provider != "openai" || outside.Level != "team" {
		t.Fatalf("forbidden primary: got %v, want the outside refusal naming work/openai/team", err)
	}
	both := ProviderResolution{
		Unreadable:       []UnreadableRoute{{Site: "draft", Field: "model", Hint: "${X}"}},
		ForbiddenPrimary: map[string][]string{"openai": {"work"}},
	}
	var unread *RouteUnreadableError
	if err := AllowlistRefusal(both, "team"); !errors.As(err, &unread) || unread.Site != "draft" || unread.Field != "model" || unread.Hint != "${X}" {
		t.Fatalf("unreadable beside a forbidden primary: got %v, want the unreadable refusal first, naming draft/model/${X}", err)
	}
}

// The vocabulary is one list, lower-cased as the walk reads it.
func TestKnownProviderSet_IsTheLowerCasedVocabulary(t *testing.T) {
	t.Parallel()
	set := KnownProviderSet()
	if len(set) != len(KnownRoutingProviders) {
		t.Fatalf("set has %d entries, want %d", len(set), len(KnownRoutingProviders))
	}
	for _, want := range []string{"anthropic", "openai", "zai", "moonshot"} {
		if !set[want] {
			t.Errorf("vocabulary lacks %q: %v", want, set)
		}
	}
}
