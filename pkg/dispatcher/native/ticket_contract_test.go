package native_test

import (
	"testing"

	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
)

func TestFindByBotInputPath(t *testing.T) {
	s, err := native.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a, _ := s.Create(native.Issue{
		Title: "mesh A", Bot: "mesh-bot", State: native.StateBacklog,
		BotArgs: map[string]string{native.BotArgInputPath: "requests/a.json"},
	})
	_, _ = s.Create(native.Issue{
		Title: "other", Bot: "mesh-bot", State: native.StateBacklog,
		BotArgs: map[string]string{native.BotArgInputPath: "requests/b.json"},
	})

	got, err := native.FindByBotInputPath(s, "mesh-bot", "requests/a.json")
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.ID != a.ID {
		t.Fatalf("got %+v, want %s", got, a.ID)
	}
	miss, _ := native.FindByBotInputPath(s, "mesh-bot", "requests/nope.json")
	if miss != nil {
		t.Fatal("expected nil for missing path")
	}
}

func TestRequireBlockerLabels(t *testing.T) {
	got := native.RequireBlockerLabels(map[string]string{
		native.BotArgRequireBlockerLabels: "accepted, qa",
	})
	if len(got) != 2 || got[0] != "accepted" || got[1] != "qa" {
		t.Fatalf("got %v", got)
	}
}

func TestBlockersSatisfiedRequireLabels(t *testing.T) {
	s, err := native.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// Done but missing accepted label.
	dep, _ := s.Create(native.Issue{
		Title: "mesh", State: native.StateDone, Bot: "mesh",
		Labels: []string{"source:planner"},
	})
	feature, _ := s.Create(native.Issue{
		Title: "feat", State: native.StateReady, Bot: "feature",
		Blockers: []string{dep.ID},
		BotArgs:  map[string]string{native.BotArgRequireBlockerLabels: "accepted"},
	})
	if native.CanLaunch(s, feature) {
		t.Fatal("must not launch without accepted label on blocker")
	}
	if got := native.LaunchBlockedReason(s, feature); got != "blocker_labels" {
		t.Fatalf("reason = %q, want blocker_labels", got)
	}
	// Stamp accepted.
	labels := []string{"source:planner", "accepted"}
	if _, err := s.Update(dep.ID, native.Patch{Labels: &labels}); err != nil {
		t.Fatal(err)
	}
	feature, _ = s.Get(feature.ID)
	if !native.CanLaunch(s, feature) {
		t.Fatal("must launch once blocker has accepted")
	}
}

func TestUpsertKey(t *testing.T) {
	_, _, ok := native.UpsertKey("bot", nil)
	if ok {
		t.Fatal("nil args")
	}
	b, p, ok := native.UpsertKey("bot", map[string]string{native.BotArgInputPath: "x.json"})
	if !ok || b != "bot" || p != "x.json" {
		t.Fatalf("got %q %q %v", b, p, ok)
	}
}
