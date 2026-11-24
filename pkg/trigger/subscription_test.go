package trigger

import (
	"errors"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/bundle"
)

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

// A subscription's mode reads one way everywhere (bundle.ResolveMode): "" and
// direct launch, board promotes a card; deliver, an unknown mode, and board on
// a connector subscription are a typed refusal — none defaults to a launch.
func TestSubscriptionEffectiveMode(t *testing.T) {
	for _, c := range []struct {
		sub  Subscription
		want bundle.ExecutionMode
	}{
		{Subscription{}, bundle.ExecutionDirect},
		{Subscription{Mode: bundle.ExecutionDirect}, bundle.ExecutionDirect},
		{Subscription{Mode: bundle.ExecutionBoard, Invocation: bundle.InvocationKindBoard}, bundle.ExecutionBoard},
	} {
		if got, err := c.sub.EffectiveMode(); err != nil || got != c.want {
			t.Errorf("%+v = (%q, %v), want %q", c.sub.Mode, got, err, c.want)
		}
	}
	for name, sub := range map[string]Subscription{
		"deliver":              {Mode: bundle.ExecutionDeliver, Invocation: bundle.InvocationKindBoard},
		"an unknown mode":      {Mode: "launch-twice"},
		"board on a connector": {Mode: bundle.ExecutionBoard, Invocation: bundle.InvocationKindConnector},
	} {
		if got, err := sub.EffectiveMode(); !errors.Is(err, bundle.ErrUnsupported) || got != "" {
			t.Errorf("%s = (%q, %v), want a typed unsupported refusal", name, got, err)
		}
	}
}

// A deliver invocation derives no subscription from any builder: it is never
// silently read as board (the board builder's default) or direct (the
// schedule and keepalive builders' forced mode).
func TestNoBuilderDerivesADeliverInvocation(t *testing.T) {
	now := time.Now()
	deliver := bundle.ExecutionDeliver
	board := bundle.Invocation{Kind: bundle.InvocationKindBoard, Mode: deliver, Board: &bundle.InvocationBoard{On: []string{KindCardMoved}}}
	schedule := bundle.Invocation{Kind: bundle.InvocationKindSchedule, Mode: deliver, Schedule: &bundle.InvocationSchedule{SuggestedCron: "0 * * * *"}}
	keepalive := bundle.Invocation{Kind: bundle.InvocationKindKeepalive, Mode: deliver, Keepalive: &bundle.InvocationKeepalive{Interval: "1m"}}
	if _, ok := FromBoardInvocation("id", "", "", "b", "o", board, now); ok {
		t.Error("the board builder derived a deliver invocation")
	}
	if _, ok := FromScheduleInvocation("id", "", "", "b", "o", schedule, now); ok {
		t.Error("the schedule builder derived a deliver invocation")
	}
	if _, ok := FromKeepaliveInvocation("id", "", "", "b", "o", keepalive, now); ok {
		t.Error("the keepalive builder derived a deliver invocation")
	}
	board.Mode, schedule.Mode, keepalive.Mode = "", "", ""
	if _, ok := FromBoardInvocation("id", "", "", "b", "o", board, now); !ok {
		t.Error("the control board invocation derived nothing")
	}
	if _, ok := FromScheduleInvocation("id", "", "", "b", "o", schedule, now); !ok {
		t.Error("the control schedule invocation derived nothing")
	}
	if _, ok := FromKeepaliveInvocation("id", "", "", "b", "o", keepalive, now); !ok {
		t.Error("the control keepalive invocation derived nothing")
	}
}
