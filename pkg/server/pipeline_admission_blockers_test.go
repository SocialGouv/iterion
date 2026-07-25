package server

import (
	"context"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
)

// The launch loop and the dispatcher share native.CanLaunch. These tests pin
// the board-side gate that admitReadyPipelines consults so a Ready ticket with
// open blockers can never start from /pipelines.
func TestCanLaunchSharedWithAdmissionGate(t *testing.T) {
	s, err := native.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	blocker, _ := s.Create(native.Issue{Title: "mesh", State: native.StateInProgress, Bot: "mesh"})
	feature, _ := s.Create(native.Issue{
		Title: "feature", State: native.StateReady, Bot: "feature",
		Blockers: []string{blocker.ID},
	})

	// Same predicate admitReadyPipelines uses.
	if native.CanLaunch(s, feature) {
		t.Fatal("Ready + open blocker must not be CanLaunch")
	}

	// Adapter agrees.
	a := native.NewAdapter(s)
	cands, _ := a.ListCandidates(context.Background())
	for _, c := range cands {
		if c.ID == feature.ID {
			t.Fatal("dispatcher must also skip open-blocker Ready ticket")
		}
	}

	if _, err := s.SetState(blocker.ID, native.StateDone); err != nil {
		t.Fatal(err)
	}
	feature, _ = s.Get(feature.ID)
	if !native.CanLaunch(s, feature) {
		t.Fatal("after blocker done, Ready ticket must be CanLaunch")
	}
}

func TestWaitingDepsNotLaunchable(t *testing.T) {
	s, err := native.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	iss, _ := s.Create(native.Issue{
		Title: "waiting", State: native.StateWaitingDeps, Bot: "feature",
	})
	if native.CanLaunch(s, iss) {
		t.Fatal("waiting_deps must never CanLaunch")
	}
}
