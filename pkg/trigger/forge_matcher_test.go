package trigger

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/webhooks"
)

// The added-label dimension reads its patterns with the grammar of
// webhooks.MatchLabel — "*", a leading-"*" suffix, a case-insensitive
// literal, and no prefix wildcard — over the labels the event ADDED, never
// the ones the subject carries, and an excluded pattern takes a label away
// from the matcher.
func TestMatcher_AddedLabels_MatchLabelGrammar(t *testing.T) {
	for _, tc := range []struct {
		name    string
		labels  []string
		exclude []string
		added   []string
		want    bool
	}{
		{name: "a literal matches whatever its case", labels: []string{"nestor"}, added: []string{"Nestor"}, want: true},
		{name: "a literal is trimmed", labels: []string{" nestor "}, added: []string{"nestor"}, want: true},
		{name: "another label is not claimed", labels: []string{"nestor"}, added: []string{"bug"}},
		{name: "one pattern of several is enough", labels: []string{"nestor", "saboteur"}, added: []string{"saboteur"}, want: true},
		{name: "one added label of several is enough", labels: []string{"nestor"}, added: []string{"bug", "nestor"}, want: true},
		{name: "a star claims any label", labels: []string{"*"}, added: []string{"bug"}, want: true},
		{name: "a star claims nothing when no label was added", labels: []string{"*"}},
		{name: "a leading star is a suffix wildcard", labels: []string{"*-chaos"}, added: []string{"run-chaos"}, want: true},
		{name: "the suffix is compared whatever its case", labels: []string{"*-CHAOS"}, added: []string{"run-chaos"}, want: true},
		{name: "the suffix wildcard does not match a prefix", labels: []string{"*-chaos"}, added: []string{"chaos-run"}},
		{name: "a trailing star is no prefix wildcard", labels: []string{"chaos-*"}, added: []string{"chaos-run"}},
		{name: "a trailing star is the literal label", labels: []string{"chaos-*"}, added: []string{"chaos-*"}, want: true},
		{name: "a star in the middle is no wildcard", labels: []string{"a*b"}, added: []string{"axb"}},

		{name: "an excluded label is not claimed", exclude: []string{"nestor"}, added: []string{"nestor"}},
		{name: "the exclusion is compared whatever its case", exclude: []string{"nestor"}, added: []string{"NESTOR"}},
		{name: "a label nobody excludes is claimed", exclude: []string{"nestor"}, added: []string{"bug"}, want: true},
		{name: "one label outside the exclusion is enough", exclude: []string{"nestor"}, added: []string{"nestor", "bug"}, want: true},
		{name: "an exclusion claims nothing when no label was added", exclude: []string{"nestor"}},
		{name: "an exclusion reads the suffix wildcard", exclude: []string{"*-chaos"}, added: []string{"run-chaos"}},
		{name: "an exclusion has no prefix wildcard either", exclude: []string{"chaos-*"}, added: []string{"chaos-run"}, want: true},
		{name: "an exclusion narrows a star", labels: []string{"*"}, exclude: []string{"nestor"}, added: []string{"Nestor"}},
		{name: "an exclusion narrows a star to the rest", labels: []string{"*"}, exclude: []string{"nestor"}, added: []string{"bug"}, want: true},
		{name: "a label both wanted and excluded is not claimed", labels: []string{"nestor"}, exclude: []string{"nestor"}, added: []string{"nestor"}},
		{name: "an excluded star claims nothing", exclude: []string{"*"}, added: []string{"bug"}},

		{name: "no pattern at all passes an event that added no label", want: true},
		{name: "no pattern at all passes any added label", added: []string{"bug"}, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := Matcher{AddedLabels: tc.labels, ExcludeAddedLabels: tc.exclude}
			ev := Event{AddedLabels: tc.added}
			if got := m.Match(ev); got != tc.want {
				t.Fatalf("Match(added_labels=%v exclude=%v, added %v) = %t, want %t", tc.labels, tc.exclude, tc.added, got, tc.want)
			}
		})
	}

	// The dimension reads what the event added: a label the subject already
	// carries starts nothing.
	t.Run("a label the subject carries is not a label the event added", func(t *testing.T) {
		m := Matcher{AddedLabels: []string{"nestor"}}
		if m.Match(Event{Labels: []string{"nestor"}}) {
			t.Fatal("a label on the subject, added by no event, was claimed")
		}
		if !m.Match(Event{Labels: []string{"bug"}, AddedLabels: []string{"nestor"}}) {
			t.Fatal("the added label was not claimed")
		}
	})

	// One pattern against one label is exactly webhooks.MatchLabel: the
	// matcher owns no grammar of its own.
	t.Run("one pattern against one label is MatchLabel", func(t *testing.T) {
		patterns := []string{"*", "nestor", " Nestor ", "*-chaos", "*CHAOS", "chaos-*", "a*b", "**", ""}
		labels := []string{"nestor", "NESTOR", "run-chaos", "chaos", "chaos-run", "chaos-*", "axb", "a*b", "x*", " nestor ", ""}
		for _, pat := range patterns {
			for _, label := range labels {
				want := webhooks.MatchLabel([]string{pat}, label)
				if got := (Matcher{AddedLabels: []string{pat}}).Match(Event{AddedLabels: []string{label}}); got != want {
					t.Errorf("pattern %q, label %q: Match = %t, MatchLabel = %t", pat, label, got, want)
				}
				if got := (Matcher{ExcludeAddedLabels: []string{pat}}).Match(Event{AddedLabels: []string{label}}); got == want {
					t.Errorf("excluded pattern %q, label %q: Match = %t, MatchLabel = %t — an exclusion claims what the pattern does not match", pat, label, got, want)
				}
			}
		}
	})
}

// Explain answers what Match answers, and names the first dimension that
// refused the event with what the matcher wants and what the event carries.
func TestMatcher_Explain(t *testing.T) {
	ev := Event{
		Source: SourceForge, Kind: "issue_labeled", Action: "labeled", Repo: "acme/api", Actor: "alice",
		Subject: Subject{State: "open"}, Labels: []string{"bug"}, AddedLabels: []string{"bug"},
	}
	for _, tc := range []struct {
		name string
		m    Matcher
		want string
	}{
		{name: "everything matches", m: Matcher{
			Sources: []Source{SourceForge}, Kinds: []string{"issue_labeled"}, Actions: []string{"LABELED"}, Repos: []string{"Acme/API"},
			Authors: []string{"Alice"}, SubjectStates: []string{"open"}, Labels: []string{"BUG"}, AddedLabels: []string{"bug"},
		}},
		{name: "an empty matcher matches", m: Matcher{}},
		{name: "sources", m: Matcher{Sources: []Source{SourceBoard}}, want: "sources: wants [board] got forge"},
		{name: "kinds", m: Matcher{Kinds: []string{"pull_request"}}, want: "kinds: wants [pull_request] got issue_labeled"},
		{name: "kinds are compared exactly", m: Matcher{Kinds: []string{"Issue_Labeled"}}, want: "kinds: wants [Issue_Labeled] got issue_labeled"},
		{name: "actions", m: Matcher{Actions: []string{"opened"}}, want: "actions: wants [opened] got labeled"},
		{name: "repos", m: Matcher{Repos: []string{"acme/web"}}, want: "repos: wants [acme/web] got acme/api"},
		{name: "authors", m: Matcher{Authors: []string{"bob"}}, want: "authors: wants [bob] got alice"},
		{name: "subject states", m: Matcher{SubjectStates: []string{"closed"}}, want: "subject_states: wants [closed] got open"},
		{name: "labels", m: Matcher{Labels: []string{"bug", "feature"}}, want: "labels: wants all of [bug feature] got [bug]"},
		{name: "added labels", m: Matcher{AddedLabels: []string{"nestor"}}, want: "added_labels: wants [nestor] got [bug]"},
		{name: "excluded added labels", m: Matcher{ExcludeAddedLabels: []string{"bug"}}, want: "added_labels: wants any except [bug] got [bug]"},
		{name: "added labels with an exclusion", m: Matcher{AddedLabels: []string{"*"}, ExcludeAddedLabels: []string{"bug"}},
			want: "added_labels: wants [*] except [bug] got [bug]"},
		{name: "the first refusing dimension is the one named", m: Matcher{Kinds: []string{"pull_request"}, AddedLabels: []string{"nestor"}},
			want: "kinds: wants [pull_request] got issue_labeled"},
		{name: "a dimension that matches is not named", m: Matcher{Kinds: []string{"issue_labeled"}, AddedLabels: []string{"nestor"}},
			want: "added_labels: wants [nestor] got [bug]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ok, failed := tc.m.Explain(ev)
			if failed != tc.want {
				t.Fatalf("Explain failed = %q, want %q", failed, tc.want)
			}
			if ok != (tc.want == "") {
				t.Fatalf("Explain ok = %t with failed = %q: ok means no dimension refused", ok, failed)
			}
			if got := tc.m.Match(ev); got != ok {
				t.Fatalf("Match = %t, Explain ok = %t: one predicate, two readings", got, ok)
			}
			if failed != "" && !strings.Contains(failed, ": wants ") {
				t.Fatalf("failed = %q, want the dimension, what it wants and what the event carries", failed)
			}
		})
	}
}

// The label that started a subscription is the smallest claimed one, read
// lower-cased and returned in the event's spelling. It is the same whatever
// order the event lists its added labels in.
func TestMatchedAddedLabel_StableInBothOrders(t *testing.T) {
	for _, tc := range []struct {
		name  string
		m     Matcher
		added []string
		want  string
	}{
		{name: "the smallest of two claimed labels", m: Matcher{AddedLabels: []string{"zeta", "alpha"}}, added: []string{"zeta", "alpha"}, want: "alpha"},
		{name: "the event's own spelling", m: Matcher{AddedLabels: []string{"nestor"}}, added: []string{"Nestor"}, want: "Nestor"},
		{name: "smallest lower-cased, not smallest in bytes", m: Matcher{AddedLabels: []string{"*"}}, added: []string{"Zeta", "alpha"}, want: "alpha"},
		{name: "a label the matcher does not claim is never named", m: Matcher{AddedLabels: []string{"nestor"}},
			added: []string{"alpha", "nestor", "bug"}, want: "nestor"},
		{name: "an excluded label is never named", m: Matcher{ExcludeAddedLabels: []string{"alpha"}}, added: []string{"alpha", "beta", "gamma"}, want: "beta"},
		{name: "no claimed label", m: Matcher{AddedLabels: []string{"nestor"}}, added: []string{"bug", "alpha"}},
		{name: "no added label", m: Matcher{AddedLabels: []string{"nestor"}}},
		{name: "two spellings of one label", m: Matcher{AddedLabels: []string{"nestor"}}, added: []string{"nestor", "Nestor"}, want: "Nestor"},
		{name: "a matcher with no pattern claims every label", m: Matcher{}, added: []string{"beta", "alpha"}, want: "alpha"},
		{name: "a suffix wildcard claims several", m: Matcher{AddedLabels: []string{"*-chaos"}}, added: []string{"run-chaos", "bug", "Big-Chaos"}, want: "Big-Chaos"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			forward := append([]string(nil), tc.added...)
			backward := append([]string(nil), tc.added...)
			for i, j := 0, len(backward)-1; i < j; i, j = i+1, j-1 {
				backward[i], backward[j] = backward[j], backward[i]
			}
			for _, order := range [][]string{forward, backward} {
				if got := MatchedAddedLabel(tc.m, Event{AddedLabels: order}); got != tc.want {
					t.Errorf("MatchedAddedLabel(added %v) = %q, want %q", order, got, tc.want)
				}
			}
		})
	}
}
