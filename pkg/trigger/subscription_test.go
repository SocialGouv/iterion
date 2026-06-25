package trigger

import "testing"

func TestMatcherMatch(t *testing.T) {
	base := Event{
		Source: SourceBoard,
		Kind:   KindCardMoved,
		Action: "",
		Repo:   "acme/widgets",
		Actor:  "alice",
		Labels: []string{"feature", "p1"},
		Subject: Subject{
			Type:  "card",
			State: "ready",
		},
	}

	cases := []struct {
		name string
		m    Matcher
		ev   Event
		want bool
	}{
		{"empty matcher matches anything", Matcher{}, base, true},
		{"source match", Matcher{Sources: []Source{SourceBoard}}, base, true},
		{"source mismatch", Matcher{Sources: []Source{SourceForge}}, base, false},
		{"kind match", Matcher{Kinds: []string{KindCardMoved}}, base, true},
		{"kind mismatch", Matcher{Kinds: []string{KindCardCreated}}, base, false},
		{"kind OR within dimension", Matcher{Kinds: []string{KindCardCreated, KindCardMoved}}, base, true},
		{"repo fold match", Matcher{Repos: []string{"ACME/Widgets"}}, base, true},
		{"repo mismatch", Matcher{Repos: []string{"other/repo"}}, base, false},
		{"author fold match", Matcher{Authors: []string{"ALICE"}}, base, true},
		{"author mismatch", Matcher{Authors: []string{"bob"}}, base, false},
		{"state match", Matcher{SubjectStates: []string{"ready"}}, base, true},
		{"state mismatch", Matcher{SubjectStates: []string{"in_progress"}}, base, false},
		{"single label present", Matcher{Labels: []string{"feature"}}, base, true},
		{"label fold present", Matcher{Labels: []string{"FEATURE"}}, base, true},
		{"all labels present (AND)", Matcher{Labels: []string{"feature", "p1"}}, base, true},
		{"missing one label fails (AND)", Matcher{Labels: []string{"feature", "p2"}}, base, false},
		{"cross-dimension AND: state+label both match", Matcher{SubjectStates: []string{"ready"}, Labels: []string{"feature"}}, base, true},
		{"cross-dimension AND: state ok but label missing", Matcher{SubjectStates: []string{"ready"}, Labels: []string{"bug"}}, base, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.m.Match(tc.ev); got != tc.want {
				t.Fatalf("Match()=%v want %v", got, tc.want)
			}
		})
	}
}

func TestSubscriptionEffectiveMode(t *testing.T) {
	if got := (Subscription{}).EffectiveMode(); got != "direct" {
		t.Fatalf("empty mode = %q, want direct", got)
	}
	if got := (Subscription{Mode: "board"}).EffectiveMode(); got != "board" {
		t.Fatalf("board mode = %q, want board", got)
	}
}
