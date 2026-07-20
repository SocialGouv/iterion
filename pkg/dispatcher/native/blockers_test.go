package native_test

import (
	"testing"

	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
)

func TestBlockerSatisfiedOnlyDone(t *testing.T) {
	cases := []struct {
		state string
		want  bool
	}{
		{native.StateDone, true},
		{native.StateBlocked, false}, // terminal but not success
		{native.StateReady, false},
		{native.StateInProgress, false},
		{native.StateWaitingDeps, false},
		{"", false},
	}
	for _, c := range cases {
		got := native.BlockerSatisfied(&native.Issue{State: c.state})
		if got != c.want {
			t.Errorf("BlockerSatisfied(state=%q)=%v want %v", c.state, got, c.want)
		}
	}
	if native.BlockerSatisfied(nil) {
		t.Error("nil issue must not be satisfied")
	}
}

func TestBlockersSatisfiedMissingIsOpen(t *testing.T) {
	s, err := native.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	done, _ := s.Create(native.Issue{Title: "done", State: native.StateDone})
	ok, open := native.BlockersSatisfied(s, []string{done.ID, "native:missing"})
	if ok {
		t.Fatal("missing blocker must fail closed")
	}
	if len(open) != 1 || open[0].ID != "native:missing" || open[0].Satisfied {
		t.Fatalf("open = %+v", open)
	}
}

func TestBlockersSatisfiedAllDone(t *testing.T) {
	s, err := native.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a, _ := s.Create(native.Issue{Title: "a", State: native.StateDone})
	b, _ := s.Create(native.Issue{Title: "b", State: native.StateDone})
	ok, open := native.BlockersSatisfied(s, []string{a.ID, b.ID})
	if !ok || len(open) != 0 {
		t.Fatalf("ok=%v open=%+v", ok, open)
	}
}

func TestBlockersSatisfiedBlockedDoesNotCount(t *testing.T) {
	s, err := native.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// StateBlocked is terminal but must NOT satisfy a hard dep.
	blocked, _ := s.Create(native.Issue{Title: "abandoned", State: native.StateBlocked})
	ok, open := native.BlockersSatisfied(s, []string{blocked.ID})
	if ok {
		t.Fatal("blocked (won't-do) must not satisfy a dep")
	}
	if len(open) != 1 || open[0].Satisfied {
		t.Fatalf("open = %+v", open)
	}
}

func TestCanLaunch(t *testing.T) {
	s, err := native.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	blocker, _ := s.Create(native.Issue{Title: "mesh", State: native.StateInProgress, Bot: "mesh-bot"})
	gated, _ := s.Create(native.Issue{
		Title:    "feature",
		State:    native.StateReady,
		Bot:      "feature-bot",
		Blockers: []string{blocker.ID},
	})
	if native.CanLaunch(s, gated) {
		t.Fatal("ready + open blocker must not launch")
	}
	if got := native.LaunchBlockedReason(s, gated); got != "open_blockers" {
		t.Fatalf("reason = %q, want open_blockers", got)
	}

	if _, err := s.SetState(blocker.ID, native.StateDone); err != nil {
		t.Fatal(err)
	}
	// Reload gated — state unchanged.
	gated, _ = s.Get(gated.ID)
	if !native.CanLaunch(s, gated) {
		t.Fatal("ready + all blockers done must launch")
	}
	if got := native.LaunchBlockedReason(s, gated); got != "" {
		t.Fatalf("reason = %q, want empty", got)
	}
}

func TestCanLaunchWaitingDepsNever(t *testing.T) {
	s, err := native.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	iss, _ := s.Create(native.Issue{
		Title: "waiting",
		State: native.StateWaitingDeps,
		Bot:   "feature-bot",
	})
	if native.CanLaunch(s, iss) {
		t.Fatal("waiting_deps must never be launchable")
	}
	if got := native.LaunchBlockedReason(s, iss); got != "waiting_deps" {
		t.Fatalf("reason = %q, want waiting_deps", got)
	}
}

func TestCanLaunchNoBot(t *testing.T) {
	s, err := native.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	iss, _ := s.Create(native.Issue{Title: "no bot", State: native.StateReady})
	if native.CanLaunch(s, iss) {
		t.Fatal("ticket without bot must not launch")
	}
	if got := native.LaunchBlockedReason(s, iss); got != "no_bot" {
		t.Fatalf("reason = %q, want no_bot", got)
	}
}

func TestValidateBlockersCycle(t *testing.T) {
	s, err := native.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a, _ := s.Create(native.Issue{Title: "a", State: native.StateBacklog})
	b, _ := s.Create(native.Issue{Title: "b", State: native.StateBacklog, Blockers: []string{a.ID}})

	// Setting a.blockers = [b] creates A→B→A.
	if err := native.ValidateBlockers(s, a.ID, []string{b.ID}); err == nil {
		t.Fatal("expected cycle error")
	}
	// Self-ref.
	if err := native.ValidateBlockers(s, a.ID, []string{a.ID}); err == nil {
		t.Fatal("expected self-cycle error")
	}
	// Acyclic is fine.
	c, _ := s.Create(native.Issue{Title: "c", State: native.StateBacklog})
	if err := native.ValidateBlockers(s, a.ID, []string{c.ID}); err != nil {
		t.Fatalf("acyclic should pass: %v", err)
	}
}

func TestNormalizeBlockers(t *testing.T) {
	got := native.NormalizeBlockers([]string{"  a  ", "", "b", "a", " b "})
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("got %v", got)
	}
}

func TestReverseBlockers(t *testing.T) {
	all := []*native.Issue{
		{ID: "a", Title: "A", Blockers: []string{"x"}},
		{ID: "b", Title: "B", Blockers: []string{"y", "x"}},
		{ID: "c", Title: "C"},
	}
	got := native.ReverseBlockers(all, "x")
	if len(got) != 2 {
		t.Fatalf("got %+v", got)
	}
	if got[0].ID != "a" || got[1].ID != "b" {
		t.Fatalf("order/ids = %+v", got)
	}
}

func TestAutoReadyFromArgs(t *testing.T) {
	if !native.AutoReadyFromArgs(map[string]string{"auto_ready": "true"}) {
		t.Fatal("true")
	}
	if !native.AutoReadyFromArgs(map[string]string{"auto_ready": "1"}) {
		t.Fatal("1")
	}
	if native.AutoReadyFromArgs(map[string]string{"auto_ready": "false"}) {
		t.Fatal("false")
	}
	if native.AutoReadyFromArgs(nil) {
		t.Fatal("nil")
	}
}

func TestUnblockTarget(t *testing.T) {
	board := native.DefaultBoard()
	iss := &native.Issue{BotArgs: map[string]string{}}
	if got := native.UnblockTarget(board, iss); got != native.StateBacklog {
		t.Fatalf("default = %q, want backlog", got)
	}
	iss.BotArgs = map[string]string{"auto_ready": "true"}
	if got := native.UnblockTarget(board, iss); got != native.StateReady {
		t.Fatalf("auto_ready = %q, want ready", got)
	}
}
