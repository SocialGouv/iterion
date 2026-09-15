package server

import (
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// The cloud twin of the grant's post-run life. The in-memory registry and this
// store are the two backends the SAME expiry lane drives, so an invariant that
// holds on one and not the other holds on a laptop and not in production —
// which is the shape CLAUDE.md requires a durable seam to ship both halves of.
//
// It is worth its own test because the two halves are written in DIFFERENT
// primitives: shortening is ExpireLT (clamped by Redis itself), re-anchoring is
// a plain EXPIRE. Getting that second one wrong — reaching for ExpireLT or
// ExpireGT out of symmetry — reproduces exactly the bug this lane had, a
// silent no-op on the runs that need it most.
func TestValkeyForgePublishTokenStore_ExpiryBothDirections(t *testing.T) {
	newStore := func(t *testing.T) (*miniredis.Miniredis, *valkeyForgePublishTokenStore) {
		t.Helper()
		mr, rdb := newTestRedis(t)
		return mr, newValkeyForgePublishTokenStore(rdb, iterlog.New(iterlog.LevelError, nil))
	}
	grant := ForgePublishGrant{TeamID: "team1", ConnectionID: "conn1", Repo: "o/r"}

	t.Run("expireIn shortens and cannot extend", func(t *testing.T) {
		mr, s := newStore(t)
		if err := s.Register("tok", grant); err != nil {
			t.Fatalf("register: %v", err)
		}
		s.expireIn("tok", forgePublishDeadRunGrace)

		mr.FastForward(forgePublishDeadRunGrace + time.Minute)
		if _, ok := s.lookup("tok"); ok {
			t.Error("a shortened grant outlived its new expiry — terminal eviction is not happening on the cloud backend")
		}

		// And the other direction is refused, which is what keeps a caller
		// that only means to retire a grant from ever widening one.
		if err := s.Register("tok2", grant); err != nil {
			t.Fatalf("register: %v", err)
		}
		s.expireIn("tok2", forgePublishDeadRunGrace)
		s.expireIn("tok2", forgePublishPostRunGrace)
		mr.FastForward(forgePublishDeadRunGrace + time.Minute)
		if _, ok := s.lookup("tok2"); ok {
			t.Error("expireIn extended a grant — a shorten-only primitive must stay shorten-only")
		}
	})

	t.Run("reanchorIn extends past the launch-stamped TTL", func(t *testing.T) {
		mr, s := newStore(t)
		if err := s.Register("tok", grant); err != nil {
			t.Fatalf("register: %v", err)
		}
		// The run parked on a usage window: almost all of the launch-stamped
		// TTL is already spent when it finally dies.
		mr.FastForward(forgePublishDefaultTTL - time.Hour)
		s.reanchorIn("tok", forgePublishPostRunGrace)

		// Past where the ORIGINAL TTL would have ended, and the grant is still
		// there — which is the whole point: the merge-gate net keeps offering
		// this run for the horizon measured from its death.
		mr.FastForward(2 * time.Hour)
		if _, ok := s.lookup("tok"); !ok {
			t.Fatal("the grant died at its launch-stamped TTL despite being re-anchored on the run's death — every later sweep pass can only abstain")
		}
		mr.FastForward(forgePublishPostRunGrace)
		if _, ok := s.lookup("tok"); ok {
			t.Error("the re-anchored grant outlived the repair window that is its only reader")
		}
	})

	t.Run("reanchorIn cannot grant an unbounded life", func(t *testing.T) {
		mr, s := newStore(t)
		if err := s.Register("tok", grant); err != nil {
			t.Fatalf("register: %v", err)
		}
		s.reanchorIn("tok", 10*365*24*time.Hour)

		mr.FastForward(forgePublishPostRunGrace + time.Minute)
		if _, ok := s.lookup("tok"); ok {
			t.Error("an unbounded re-anchor stuck — the clamp is what stops an extending primitive uncapping a forge-write credential")
		}
	})

	t.Run("reanchorIn never resurrects a reaped grant", func(t *testing.T) {
		mr, s := newStore(t)
		if err := s.Register("tok", grant); err != nil {
			t.Fatalf("register: %v", err)
		}
		mr.FastForward(forgePublishDefaultTTL + time.Minute)
		s.reanchorIn("tok", forgePublishPostRunGrace)
		if _, ok := s.lookup("tok"); ok {
			t.Error("a grant Redis had already reaped came back — a dead run's forge-write token must not get a second life")
		}
	})
}
