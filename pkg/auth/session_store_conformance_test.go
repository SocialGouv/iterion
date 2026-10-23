package auth

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/SocialGouv/iterion/pkg/identity"
	"github.com/SocialGouv/iterion/pkg/internal/mongotest"
)

// runSessionStoreSuite exercises the SessionStore contract both twins must
// satisfy. It matters more than it looks: the memory store mutates a copied
// struct, Mongo writes a `$set` of hand-written keys and filters on another
// — so a key that does not match its bson tag writes a NEW document key the
// read never decodes, silently. Only a suite run against both sees that.
func runSessionStoreSuite(t *testing.T, st SessionStore) {
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)
	seed := func(t *testing.T, id, hash string, s Session) Session {
		t.Helper()
		s.ID, s.UserID, s.TokenHash = id, "user-"+id, hash
		if s.IssuedAt.IsZero() {
			s.IssuedAt = now
		}
		if s.ExpiresAt.IsZero() {
			s.ExpiresAt = now.Add(time.Hour)
		}
		if err := st.CreateSession(ctx, s); err != nil {
			t.Fatalf("CreateSession %s: %v", id, err)
		}
		return s
	}
	read := func(t *testing.T, hash string) Session {
		t.Helper()
		s, err := st.GetSessionByTokenHash(ctx, hash)
		if err != nil {
			t.Fatalf("GetSessionByTokenHash %s: %v", hash, err)
		}
		return s
	}

	t.Run("a session round-trips, its lineage included", func(t *testing.T) {
		seed(t, "s-lineage", "h-lineage", Session{RotatedFromID: "s-before", UserAgent: "ua", IP: "ip"})
		got := read(t, "h-lineage")
		if got.RotatedFromID != "s-before" || got.UserID != "user-s-lineage" || got.UserAgent != "ua" || got.RevokedAt != nil {
			t.Fatalf("session read back as %+v", got)
		}
		if !got.ExpiresAt.Equal(now.Add(time.Hour)) {
			t.Fatalf("expires_at read back as %v, want %v", got.ExpiresAt, now.Add(time.Hour))
		}
	})

	t.Run("an unknown token hash is ErrSessionNotFound", func(t *testing.T) {
		if _, err := st.GetSessionByTokenHash(ctx, "h-never"); !errors.Is(err, ErrSessionNotFound) {
			t.Fatalf("got %v, want ErrSessionNotFound", err)
		}
	})

	t.Run("a token hash the store holds is refused as ErrSessionExists", func(t *testing.T) {
		seed(t, "s-dup-1", "h-dup", Session{})
		err := st.CreateSession(ctx, Session{ID: "s-dup-2", UserID: "u", TokenHash: "h-dup", IssuedAt: now, ExpiresAt: now.Add(time.Hour)})
		if !errors.Is(err, ErrSessionExists) {
			t.Fatalf("got %v, want ErrSessionExists", err)
		}
	})

	storeNow := func(t *testing.T) time.Time {
		t.Helper()
		at, err := st.Now(ctx)
		if err != nil || at.IsZero() {
			t.Fatalf("Now: %v (%v)", at, err)
		}
		return at
	}

	t.Run("the rotation is a compare-and-set that records itself with revoked_at, stamped by the store's clock", func(t *testing.T) {
		seed(t, "s-rot", "h-rot", Session{})
		first := Rotation{Nonce: "nonce-1", KeyID: "k0", SuccessorHash: "succ-1", UserAgent: "ua-rot", IP: "203.0.113.7"}
		before := storeNow(t)
		at, ok, err := st.RotateSession(ctx, "s-rot", first)
		if err != nil || !ok {
			t.Fatalf("first rotation: ok=%v err=%v", ok, err)
		}
		after := storeNow(t)
		if at.Before(before) || at.After(after) {
			t.Fatalf("the rotation was stamped %v, outside the store's clock read around it [%v, %v]", at, before, after)
		}
		got := read(t, "h-rot")
		if got.RevokedAt == nil || !got.RevokedAt.Equal(at) {
			t.Fatalf("revoked_at read back as %v, want the stamp the rotation returned %v", got.RevokedAt, at)
		}
		if got.rotation() != first {
			t.Fatalf("rotation record read back as %+v, want %+v", got.rotation(), first)
		}
		_, ok, err = st.RotateSession(ctx, "s-rot", Rotation{Nonce: "nonce-2", KeyID: "k1", SuccessorHash: "succ-2"})
		if err != nil || ok {
			t.Fatalf("second rotation of a rotated session: ok=%v err=%v, want false", ok, err)
		}
		again := read(t, "h-rot")
		if again.rotation() != first || !again.RevokedAt.Equal(at) {
			t.Fatalf("a refused rotation overwrote the first: %+v", again)
		}
	})

	t.Run("a rotation value is written as a value, never read as an expression", func(t *testing.T) {
		seed(t, "s-lit", "h-lit", Session{})
		rot := Rotation{Nonce: "$revoked_at", KeyID: "$$NOW", SuccessorHash: "$user_id", UserAgent: "$token_hash", IP: "$_id"}
		if _, ok, err := st.RotateSession(ctx, "s-lit", rot); err != nil || !ok {
			t.Fatalf("rotation: ok=%v err=%v", ok, err)
		}
		if got := read(t, "h-lit").rotation(); got != rot {
			t.Fatalf("rotation record read back as %+v, want %+v", got, rot)
		}
	})

	t.Run("a sign-out of a rotated session keeps its revoked_at and clears its rotation record", func(t *testing.T) {
		seed(t, "s-out-rot", "h-out-rot", Session{})
		rot := Rotation{Nonce: "n-out", KeyID: "k0", SuccessorHash: "succ-out", UserAgent: "ua", IP: "ip"}
		at, ok, err := st.RotateSession(ctx, "s-out-rot", rot)
		if err != nil || !ok {
			t.Fatalf("rotation: ok=%v err=%v", ok, err)
		}
		cleared, err := st.RevokeSession(ctx, "s-out-rot", at.Add(time.Hour))
		if err != nil {
			t.Fatalf("RevokeSession: %v", err)
		}
		if cleared != rot {
			t.Fatalf("the sign-out reported clearing %+v, want the rotation's record %+v", cleared, rot)
		}
		got := read(t, "h-out-rot")
		if got.RevokedAt == nil || !got.RevokedAt.Equal(at) {
			t.Fatalf("the sign-out moved revoked_at from the rotation's %v to %v", at, got.RevokedAt)
		}
		if got.rotation() != (Rotation{}) {
			t.Fatalf("the sign-out left a rotation record: %+v", got.rotation())
		}
		if again, err := st.RevokeSession(ctx, "s-out-rot", at.Add(2*time.Hour)); err != nil || again != (Rotation{}) {
			t.Fatalf("a second sign-out: cleared=%+v err=%v, want nothing cleared", again, err)
		}
	})

	t.Run("a session revoked by a sign-out cannot be rotated and carries no rotation record", func(t *testing.T) {
		seed(t, "s-out", "h-out", Session{})
		if cleared, err := st.RevokeSession(ctx, "s-out", now); err != nil || cleared != (Rotation{}) {
			t.Fatalf("RevokeSession: cleared=%+v err=%v", cleared, err)
		}
		if got := read(t, "h-out"); got.RevokedAt == nil || !got.RevokedAt.Equal(now) {
			t.Fatalf("a sign-out of a live session wrote revoked_at %v, want %v", got.RevokedAt, now)
		}
		_, ok, err := st.RotateSession(ctx, "s-out", Rotation{Nonce: "n", KeyID: "k0", SuccessorHash: "s"})
		if err != nil || ok {
			t.Fatalf("rotation of a signed-out session: ok=%v err=%v, want false", ok, err)
		}
		if got := read(t, "h-out"); got.rotation() != (Rotation{}) {
			t.Fatalf("a signed-out session gained a rotation record: %+v", got)
		}
	})

	t.Run("a sign-out of a session that is gone is ErrSessionNotFound", func(t *testing.T) {
		if _, err := st.RevokeSession(ctx, "s-never", now); !errors.Is(err, ErrSessionNotFound) {
			t.Fatalf("got %v, want ErrSessionNotFound", err)
		}
	})

	t.Run("a session that is gone cannot be rotated", func(t *testing.T) {
		_, ok, err := st.RotateSession(ctx, "s-gone", Rotation{Nonce: "n", KeyID: "k0", SuccessorHash: "s"})
		if err != nil || ok {
			t.Fatalf("rotation of a missing session: ok=%v err=%v, want false", ok, err)
		}
	})

	seedFor := func(t *testing.T, userID, id string, s Session) Session {
		t.Helper()
		s.ID, s.UserID, s.TokenHash = id, userID, "h-"+id
		if s.IssuedAt.IsZero() {
			s.IssuedAt = now
		}
		if s.ExpiresAt.IsZero() {
			s.ExpiresAt = now.Add(time.Hour)
		}
		if err := st.CreateSession(ctx, s); err != nil {
			t.Fatalf("CreateSession %s: %v", id, err)
		}
		return s
	}
	revoked := func(t *testing.T, s Session) bool {
		t.Helper()
		got, err := st.SessionRevoked(ctx, s)
		if err != nil {
			t.Fatalf("SessionRevoked %s: %v", s.ID, err)
		}
		return got
	}

	t.Run("a session round-trips its lineage and generation; a row without a lineage is its own root", func(t *testing.T) {
		seed(t, "s-lin", "h-lin", Session{LineageID: "root-lin", Generation: 3})
		got := read(t, "h-lin")
		if got.LineageID != "root-lin" || got.Generation != 3 || got.lineage() != "root-lin" {
			t.Fatalf("lineage read back as %q (generation %d)", got.LineageID, got.Generation)
		}
		seed(t, "s-legacy", "h-legacy", Session{})
		if legacy := read(t, "h-legacy"); legacy.LineageID != "" || legacy.lineage() != "s-legacy" || legacy.Generation != 0 {
			t.Fatalf("a row without a lineage read back as lineage %q / %q, generation %d", legacy.LineageID, legacy.lineage(), legacy.Generation)
		}
	})

	t.Run("a rotation and a sign-out stamp the lineage of a row written before lineages, and keep one already there", func(t *testing.T) {
		seed(t, "s-stamp-rot", "h-stamp-rot", Session{})
		if _, ok, err := st.RotateSession(ctx, "s-stamp-rot", Rotation{Nonce: "n", KeyID: "k0", SuccessorHash: "succ-stamp"}); err != nil || !ok {
			t.Fatalf("rotation: ok=%v err=%v", ok, err)
		}
		if got := read(t, "h-stamp-rot"); got.LineageID != "s-stamp-rot" {
			t.Fatalf("a rotation of a row without a lineage stamped %q, want its own id", got.LineageID)
		}
		seed(t, "s-stamp-out", "h-stamp-out", Session{})
		if _, err := st.RevokeSession(ctx, "s-stamp-out", now); err != nil {
			t.Fatalf("RevokeSession: %v", err)
		}
		if got := read(t, "h-stamp-out"); got.LineageID != "s-stamp-out" {
			t.Fatalf("a sign-out of a row without a lineage stamped %q, want its own id", got.LineageID)
		}
		seed(t, "s-stamp-kept", "h-stamp-kept", Session{LineageID: "root-kept"})
		if _, ok, err := st.RotateSession(ctx, "s-stamp-kept", Rotation{Nonce: "n", KeyID: "k0", SuccessorHash: "succ-kept"}); err != nil || !ok {
			t.Fatalf("rotation: ok=%v err=%v", ok, err)
		}
		if got := read(t, "h-stamp-kept"); got.LineageID != "root-kept" {
			t.Fatalf("a rotation moved the lineage from root-kept to %q", got.LineageID)
		}
	})

	t.Run("a lineage revocation's record covers every session of it, and its pass revokes the live ones and nothing else", func(t *testing.T) {
		const user = "user-fam"
		root := seedFor(t, user, "fam-root", Session{}) // written before lineages: its own root
		retiredAt, ok, err := st.RotateSession(ctx, "fam-root", Rotation{Nonce: "n-root", KeyID: "k0", SuccessorHash: "h-fam-a"})
		if err != nil || !ok {
			t.Fatalf("rotation of the root: ok=%v err=%v", ok, err)
		}
		famA := seedFor(t, user, "fam-a", Session{LineageID: "fam-root", RotatedFromID: "fam-root"})
		other := seedFor(t, user, "other", Session{LineageID: "other"})
		otherRot := Rotation{Nonce: "n-other", KeyID: "k0", SuccessorHash: "h-other-next", UserAgent: "ua", IP: "ip"}
		if _, ok, err := st.RotateSession(ctx, "other", otherRot); err != nil || !ok {
			t.Fatalf("rotation of the other lineage: ok=%v err=%v", ok, err)
		}
		otherNext := seedFor(t, user, "other-next", Session{LineageID: "other", RotatedFromID: "other"})
		at := now.Add(time.Minute)
		if err := st.RecordLineageRevocation(ctx, user, "fam-root", at); err != nil {
			t.Fatalf("RecordLineageRevocation: %v", err)
		}
		// The record alone covers the lineage: no pass has run.
		if read(t, famA.TokenHash).RevokedAt != nil {
			t.Fatal("the record wrote a session row: the pass is its own step")
		}
		if !revoked(t, famA) || !revoked(t, read(t, root.TokenHash)) {
			t.Fatal("SessionRevoked does not cover the recorded lineage before its pass")
		}
		n, err := st.RevokeLineageSessions(ctx, user, "fam-root", at)
		if err != nil {
			t.Fatalf("RevokeLineageSessions: %v", err)
		}
		if n != 1 {
			t.Fatalf("the pass matched %d row(s), want the lineage's one live row", n)
		}
		gotRoot := read(t, root.TokenHash)
		if gotRoot.RevokedAt == nil || !gotRoot.RevokedAt.Equal(retiredAt) || gotRoot.rotation().Nonce != "n-root" {
			t.Fatalf("the lineage's retired root reads %+v: the pass leaves a retired row as it is (rotation stamp %v, record kept)", gotRoot, retiredAt)
		}
		if gotA := read(t, famA.TokenHash); gotA.RevokedAt == nil || !gotA.RevokedAt.Equal(at) {
			t.Fatalf("the lineage's live session reads revoked_at %v, want %v", gotA.RevokedAt, at)
		}
		if gotOther := read(t, other.TokenHash); gotOther.rotation() != otherRot {
			t.Fatalf("a revocation of another lineage touched this one's rotation record: %+v", gotOther.rotation())
		}
		if gotNext := read(t, otherNext.TokenHash); gotNext.RevokedAt != nil {
			t.Fatal("a revocation of another lineage revoked this one's live session")
		}
		if revoked(t, otherNext) {
			t.Fatal("SessionRevoked covers a lineage nobody revoked")
		}
		// The record outlives the pass: a session of the lineage written
		// after it is covered, though the pass never saw it.
		late := seedFor(t, user, "fam-late", Session{LineageID: "fam-root", RotatedFromID: "fam-a"})
		if read(t, late.TokenHash).RevokedAt != nil {
			t.Fatal("the fixture is wrong: a session written after the pass reads revoked")
		}
		if !revoked(t, late) {
			t.Fatal("a session of a revoked lineage written after its revocation is not covered by it")
		}
		// A live root written before lineages is its own lineage: the pass of
		// that lineage reaches it by its id, and stamps it.
		legacyRoot := seedFor(t, user, "legacy-root", Session{})
		if err := st.RecordLineageRevocation(ctx, user, "legacy-root", at); err != nil {
			t.Fatalf("RecordLineageRevocation of a pre-lineage root: %v", err)
		}
		if n, err := st.RevokeLineageSessions(ctx, user, "legacy-root", at); err != nil || n != 1 {
			t.Fatalf("the pass of a pre-lineage root's lineage matched %d row(s) (err %v), want 1", n, err)
		}
		if got := read(t, legacyRoot.TokenHash); got.RevokedAt == nil || got.LineageID != "legacy-root" {
			t.Fatalf("the pre-lineage root reads %+v after its lineage's pass: revoked, its own lineage, expected", got)
		}
		// The revocation is the user's: a lineage id another account names
		// revokes nothing of this one.
		if err := st.RecordLineageRevocation(ctx, "user-elsewhere", "other-elsewhere", at); err != nil {
			t.Fatalf("RecordLineageRevocation of another user: %v", err)
		}
		if n, err := st.RevokeLineageSessions(ctx, "user-elsewhere", "other", at); err != nil || n != 0 {
			t.Fatalf("a pass in another user's name matched %d row(s) (err %v), want 0", n, err)
		}
		if read(t, otherNext.TokenHash).RevokedAt != nil {
			t.Fatal("a revocation in another user's name revoked this user's session")
		}
	})

	t.Run("the lineage's live session is the one row the revocation pass would match", func(t *testing.T) {
		const user = "user-live"
		liveSession := func(t *testing.T, userID, lineage string) (Session, bool) {
			t.Helper()
			sess, found, err := st.LiveLineageSession(ctx, userID, lineage)
			if err != nil {
				t.Fatalf("LiveLineageSession(%s, %s): %v", userID, lineage, err)
			}
			return sess, found
		}
		live := seedFor(t, user, "live-a", Session{LineageID: "live-a"})
		got, found := liveSession(t, user, "live-a")
		if !found || got.ID != live.ID || got.TokenHash != live.TokenHash {
			t.Fatalf("the lineage's live session read back as %+v (found=%t), want %s", got, found, live.ID)
		}
		// A lineage another user names answers nothing: the match is the
		// user's, like the revocation pass it mirrors.
		if _, found := liveSession(t, "user-elsewhere", "live-a"); found {
			t.Fatal("another user's name answered this lineage's live session")
		}
		if _, found := liveSession(t, user, "live-unknown"); found {
			t.Fatal("an unknown lineage answered a live session")
		}
		// A retired row is not live: the rotation that delivers the
		// successor retires its predecessor in the same write.
		if _, ok, err := st.RotateSession(ctx, "live-a", Rotation{Nonce: "n-live", KeyID: "k0", SuccessorHash: "h-live-b"}); err != nil || !ok {
			t.Fatalf("rotation: ok=%v err=%v", ok, err)
		}
		if _, found := liveSession(t, user, "live-a"); found {
			t.Fatal("a retired row answered as the lineage's live session")
		}
		// A root written before lineages is its own lineage.
		legacy := seedFor(t, user, "legacy-live", Session{})
		got, found = liveSession(t, user, "legacy-live")
		if !found || got.ID != legacy.ID {
			t.Fatalf("the pre-lineage root read back as %+v (found=%t), want %s", got, found, legacy.ID)
		}
		// A lineage holds one live row — a rotation retires its predecessor
		// in the write that delivers the successor — but should a store ever
		// hold two, the newest by issued_at answers, whichever twin reads.
		older := seedFor(t, user, "live-two-older", Session{LineageID: "live-two", IssuedAt: now.Add(-time.Hour)})
		newer := seedFor(t, user, "live-two-newer", Session{LineageID: "live-two", IssuedAt: now})
		got, found = liveSession(t, user, "live-two")
		if !found || got.ID != newer.ID {
			t.Fatalf("a lineage with two live rows answered %s (found=%t), want the newest %s (not %s)", got.ID, found, newer.ID, older.ID)
		}
		// After the lineage's revocation pass, nothing is live.
		if _, err := st.RevokeLineageSessions(ctx, user, "live-two", now.Add(time.Minute)); err != nil {
			t.Fatalf("RevokeLineageSessions: %v", err)
		}
		if _, found := liveSession(t, user, "live-two"); found {
			t.Fatal("a lineage whose pass ran answered a live session")
		}
		// No lineage id is no read.
		if _, _, err := st.LiveLineageSession(ctx, user, ""); err == nil {
			t.Fatal("a read without a lineage id reported success")
		}
	})

	t.Run("revoking every session of a user raises its generation and covers every session born under a lower one", func(t *testing.T) {
		const user = "user-gen"
		gen := func(t *testing.T) int64 {
			t.Helper()
			g, err := st.SessionGeneration(ctx, user)
			if err != nil {
				t.Fatalf("SessionGeneration: %v", err)
			}
			return g
		}
		if g := gen(t); g != 0 {
			t.Fatalf("an account never signed out everywhere reads generation %d, want 0", g)
		}
		before := seedFor(t, user, "gen-before", Session{LineageID: "gen-before"})
		legacy := seedFor(t, user, "gen-legacy", Session{})
		at := now.Add(2 * time.Minute)
		raised, err := st.RecordAccountRevocation(ctx, user, at)
		if err != nil {
			t.Fatalf("RecordAccountRevocation: %v", err)
		}
		if g := gen(t); g != 1 || raised != 1 {
			t.Fatalf("after a revoke-all the generation reads %d and the revocation answered %d, want 1 and 1", g, raised)
		}
		if !revoked(t, before) {
			t.Fatal("a session born before the revoke-all is not covered by its record")
		}
		// A session the caller opens under the raised generation, before the
		// pass runs: the pass must leave it live.
		opened := seedFor(t, user, "gen-opened", Session{LineageID: "gen-opened", Generation: raised})
		if n, err := st.RevokeAccountSessions(ctx, user, raised, at); err != nil || n != 2 {
			t.Fatalf("the pass matched %d row(s) (err %v), want the live row and the pre-lineage row born below", n, err)
		}
		if got := read(t, legacy.TokenHash); got.RevokedAt == nil || got.LineageID != "gen-legacy" {
			t.Fatalf("the revoke-all left a row without a lineage as %+v: revoked, its own root, expected", got)
		}
		if got := read(t, before.TokenHash); got.RevokedAt == nil || !got.RevokedAt.Equal(at) {
			t.Fatalf("the pass left the live session born below as %+v", got)
		}
		if got := read(t, opened.TokenHash); got.RevokedAt != nil {
			t.Fatal("the pass revoked a session born under the generation its record raised")
		}
		late := seedFor(t, user, "gen-late-old", Session{LineageID: "gen-before", Generation: 0})
		if !revoked(t, late) {
			t.Fatal("a session born under the old generation, written after the revoke-all, is not covered by it")
		}
		fresh := seedFor(t, user, "gen-fresh", Session{LineageID: "gen-fresh", Generation: 1})
		if revoked(t, fresh) {
			t.Fatal("a login born under the raised generation is covered by the revoke-all before it")
		}
		raised, err = st.RecordAccountRevocation(ctx, user, at.Add(time.Minute))
		if err != nil {
			t.Fatalf("second RecordAccountRevocation: %v", err)
		}
		if g := gen(t); g != 2 || raised != 2 {
			t.Fatalf("after two revoke-alls the generation reads %d and the revocation answered %d, want 2 and 2", g, raised)
		}
		if g, err := st.SessionGeneration(ctx, "user-untouched"); err != nil || g != 0 {
			t.Fatalf("another account's generation reads %d (err %v), want 0", g, err)
		}
	})

	t.Run("a claim stamps delivered_at once, and only on a session that is not revoked", func(t *testing.T) {
		seed(t, "s-claim", "h-claim", Session{LineageID: "s-claim"})
		if got := read(t, "h-claim"); got.DeliveredAt != nil {
			t.Fatalf("a session never claimed reads delivered_at %v", got.DeliveredAt)
		}
		claimed, err := st.ClaimSession(ctx, "s-claim")
		if err != nil || !claimed {
			t.Fatalf("the first claim: claimed=%t err=%v, want true", claimed, err)
		}
		first := read(t, "h-claim").DeliveredAt
		if first == nil {
			t.Fatal("a claimed session reads no delivered_at")
		}
		time.Sleep(5 * time.Millisecond)
		if claimed, err := st.ClaimSession(ctx, "s-claim"); err != nil || !claimed {
			t.Fatalf("a second claim of a live session: claimed=%t err=%v, want true", claimed, err)
		}
		if again := read(t, "h-claim").DeliveredAt; again == nil || !again.Equal(*first) {
			t.Fatalf("a second claim moved delivered_at from %v to %v", first, again)
		}
		seed(t, "s-claim-rev", "h-claim-rev", Session{LineageID: "s-claim-rev"})
		if _, err := st.RevokeSession(ctx, "s-claim-rev", now); err != nil {
			t.Fatalf("RevokeSession: %v", err)
		}
		if claimed, err := st.ClaimSession(ctx, "s-claim-rev"); err != nil || claimed {
			t.Fatalf("a claim of a revoked session: claimed=%t err=%v, want false", claimed, err)
		}
		if got := read(t, "h-claim-rev"); got.DeliveredAt != nil {
			t.Fatalf("a refused claim wrote delivered_at %v", got.DeliveredAt)
		}
		if claimed, err := st.ClaimSession(ctx, "s-claim-missing"); err != nil || claimed {
			t.Fatalf("a claim of a session that is not there: claimed=%t err=%v, want false", claimed, err)
		}
	})

	t.Run("the pass of an account revocation revokes the live sessions and leaves a retired one as it is", func(t *testing.T) {
		seed(t, "s-all-rot", "h-all-rot", Session{})
		seed(t, "s-all-live", "h-all-live", Session{LineageID: "s-all-live"})
		rotAt, ok, err := st.RotateSession(ctx, "s-all-rot", Rotation{Nonce: "n-all", KeyID: "k0", SuccessorHash: "succ-all", UserAgent: "ua", IP: "ip"})
		if err != nil || !ok {
			t.Fatalf("rotation: ok=%v err=%v", ok, err)
		}
		for _, uid := range []string{"user-s-all-rot", "user-s-all-live"} {
			g, err := st.RecordAccountRevocation(ctx, uid, now.Add(time.Minute))
			if err != nil {
				t.Fatalf("RecordAccountRevocation: %v", err)
			}
			if _, err := st.RevokeAccountSessions(ctx, uid, g, now.Add(time.Minute)); err != nil {
				t.Fatalf("RevokeAccountSessions: %v", err)
			}
		}
		rot := read(t, "h-all-rot")
		if !rot.RevokedAt.Equal(rotAt) {
			t.Fatalf("a revoke-all moved a rotated session's revoked_at from %v to %v", rotAt, rot.RevokedAt)
		}
		if rot.rotation().Nonce != "n-all" {
			t.Fatalf("the pass touched the rotation record of a retired session: %+v", rot.rotation())
		}
		if !revoked(t, rot) {
			t.Fatal("the account's record does not cover the retired session")
		}
		live := read(t, "h-all-live")
		if live.RevokedAt == nil || !live.RevokedAt.Equal(now.Add(time.Minute)) {
			t.Fatalf("a revoke-all left the live session %+v", live)
		}
	})

	t.Run("one credential-change claim per account at a time", func(t *testing.T) {
		const user = "user-claim"
		got, err := st.ClaimCredentialChange(ctx, user, "change-a", time.Minute)
		if err != nil || !got {
			t.Fatalf("the first claim: claimed=%t err=%v, want true", got, err)
		}
		if got, err := st.ClaimCredentialChange(ctx, user, "change-b", time.Minute); err != nil || got {
			t.Fatalf("a second owner's claim while the first holds: claimed=%t err=%v, want false", got, err)
		}
		if got, err := st.ClaimCredentialChange(ctx, "user-claim-other", "change-b", time.Minute); err != nil || !got {
			t.Fatalf("a claim of another account: claimed=%t err=%v, want true", got, err)
		}
		if err := st.ReleaseCredentialChange(ctx, user, "change-a"); err != nil {
			t.Fatalf("the holder's release: %v", err)
		}
		if got, err := st.ClaimCredentialChange(ctx, user, "change-b", time.Minute); err != nil || !got {
			t.Fatalf("a claim once the holder released: claimed=%t err=%v, want true", got, err)
		}
		if err := st.ReleaseCredentialChange(ctx, user, "change-a"); err == nil {
			t.Fatal("a release by an owner that does not hold the claim reported success")
		}
		if got, err := st.ClaimCredentialChange(ctx, user, "change-c", time.Minute); err != nil || got {
			t.Fatalf("the stale owner's release freed the claim: claimed=%t err=%v, want false", got, err)
		}
	})

	// Last: DeleteExpired sweeps the whole store.
	t.Run("the sweep of expired sessions keeps every revocation record", func(t *testing.T) {
		const user = "user-sweep"
		row := Session{ID: "sweep-row", UserID: user, LineageID: "sweep-lin"}
		if err := st.RecordLineageRevocation(ctx, user, "sweep-lin", now); err != nil {
			t.Fatalf("RecordLineageRevocation: %v", err)
		}
		if _, err := st.RecordAccountRevocation(ctx, user, now); err != nil {
			t.Fatalf("RecordAccountRevocation: %v", err)
		}
		if !revoked(t, row) {
			t.Fatal("the lineage's revocation does not cover it")
		}
		// Years later: a session of the lineage a pass never reached may
		// have been written under a longer refresh TTL than any revocation
		// knows of; its record must still cover it.
		if _, err := st.DeleteExpired(ctx, now.Add(10*365*24*time.Hour)); err != nil {
			t.Fatalf("DeleteExpired: %v", err)
		}
		row.Generation = 1 // born under the raised generation: only the lineage covers it
		if !revoked(t, row) {
			t.Fatal("the sweep of expired sessions dropped a lineage's revocation record")
		}
		if g, err := st.SessionGeneration(ctx, user); err != nil || g != 1 {
			t.Fatalf("the sweep moved the account's generation to %d (err %v), want 1", g, err)
		}
	})
}

func TestMemorySessionStore_Conformance(t *testing.T) {
	runSessionStoreSuite(t, NewMemorySessionStore())
}

// TestMongoSessionStore_Conformance runs the same suite against a real Mongo
// (same gating as the pkg/store/mongo conformance harness).
func TestMongoSessionStore_Conformance(t *testing.T) {
	runSessionStoreSuite(t, mongoSessionStoreForTest(t))
}

// TestMongoSessionStore_RefreshThroughTheWindow drives Refresh itself on the
// Mongo twin, where the work is the store's: the rotation's compare-and-set
// is an UpdateOne filtered on revoked_at, and a successor two refreshes
// derive alike collides on the unique token_hash index. Concurrent
// presentations of one token must share one successor; a successor whose
// write failed after the rotation must be written by the next presentation.
func TestMongoSessionStore_RefreshThroughTheWindow(t *testing.T) {
	ctx := context.Background()
	st := &failOnceSessions{SessionStore: mongoSessionStoreForTest(t)}
	svc := newTestServiceOn(t, identity.NewMemoryStore(), st)
	reg, err := svc.Register(ctx, "mongo-window@example.org", "correct-horse-9", "", "", "ua", "ip")
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	other, err := svc.Login(ctx, "mongo-window@example.org", "correct-horse-9", "other", "ip2")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}

	const callers = 4
	var (
		wg   sync.WaitGroup
		res  [callers]LoginResult
		errs [callers]error
	)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			res[i], errs[i] = svc.Refresh(ctx, reg.RefreshToken, "ua", "ip")
		}(i)
	}
	wg.Wait()
	for i := 0; i < callers; i++ {
		if errs[i] != nil || res[i].RefreshToken != res[0].RefreshToken {
			t.Fatalf("concurrent refresh %d on Mongo: err=%v, same successor=%t", i, errs[i], res[i].RefreshToken == res[0].RefreshToken)
		}
	}

	st.failCreate.Store(true)
	if _, err := svc.Refresh(ctx, res[0].RefreshToken, "ua", "ip"); err == nil {
		t.Fatal("the refresh whose successor write failed reported success")
	}
	next, err := svc.Refresh(ctx, res[0].RefreshToken, "ua", "ip")
	if err != nil {
		t.Fatalf("the presentation after the lost successor on Mongo: %v", err)
	}
	if s, err := st.GetSessionByTokenHash(ctx, HashRefreshToken(next.RefreshToken)); err != nil || s.RevokedAt != nil {
		t.Fatalf("the written successor reads back as %+v (err %v)", s, err)
	}
	if s, err := st.GetSessionByTokenHash(ctx, HashRefreshToken(other.RefreshToken)); err != nil || s.RevokedAt != nil {
		t.Fatalf("the account's other session was revoked on Mongo: %+v (err %v)", s, err)
	}
}

// TestMongoSessionStore_TheWindowIsMeasuredOnTheDatabaseClock drives the
// window across replicas whose clocks disagree, on the real driver: the
// rotation is stamped by the database ($$NOW) and the presentation compared
// with the database's clock, so a replica 45 s ahead answers a prompt retry
// with the successor, and one 60 s behind still refuses a presentation past
// the window.
func TestMongoSessionStore_TheWindowIsMeasuredOnTheDatabaseClock(t *testing.T) {
	ctx := context.Background()
	st := mongoSessionStoreForTest(t)
	ids := identity.NewMemoryStore()
	signer := newTestSigner(t)
	replica := func(skew, grace time.Duration) *Service {
		svc, err := NewService(Config{Store: ids, Sessions: st, Signer: signer, SignupMode: SignupOpen, RefreshTTL: time.Hour, RefreshReuseGrace: &grace})
		if err != nil {
			t.Fatalf("NewService: %v", err)
		}
		svc.now = func() time.Time { return time.Now().Add(skew) }
		return svc
	}
	a, ahead := replica(0, 2*time.Second), replica(45*time.Second, 2*time.Second)
	reg, err := a.Register(ctx, "mongo-clock@example.org", "correct-horse-9", "", "", "ua", "ip")
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	other, err := a.Login(ctx, "mongo-clock@example.org", "correct-horse-9", "other", "ip2")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	first, err := a.Refresh(ctx, reg.RefreshToken, "ua", "ip")
	if err != nil {
		t.Fatalf("the rotation: %v", err)
	}
	again, err := ahead.Refresh(ctx, reg.RefreshToken, "ua", "ip")
	if err != nil || again.RefreshToken != first.RefreshToken {
		t.Fatalf("a prompt retry on a replica 45 s ahead: err=%v, same successor=%t", err, again.RefreshToken == first.RefreshToken)
	}
	if s, err := st.GetSessionByTokenHash(ctx, HashRefreshToken(other.RefreshToken)); err != nil || s.RevokedAt != nil {
		t.Fatalf("a prompt retry on a replica ahead revoked the account's other session: %+v (err %v)", s, err)
	}
	time.Sleep(2500 * time.Millisecond)
	if _, err := replica(-60*time.Second, 2*time.Second).Refresh(ctx, reg.RefreshToken, "ua", "ip"); !errors.Is(err, ErrSessionRevoked) {
		t.Fatalf("a presentation past the window, on a replica 60 s behind: got %v, want ErrSessionRevoked", err)
	}
}

// failOnceSessions fails the next CreateSession when armed.
type failOnceSessions struct {
	SessionStore
	failCreate atomic.Bool
}

func (s *failOnceSessions) CreateSession(ctx context.Context, x Session) error {
	if s.failCreate.CompareAndSwap(true, false) {
		return errors.New("auth: insert session: connection reset by peer")
	}
	return s.SessionStore.CreateSession(ctx, x)
}

// mongoSessionStoreForTest is a MongoSessionStore on a throwaway database
// of the Mongo ITERION_TEST_MONGO_URI names; the test is skipped without one.
func mongoSessionStoreForTest(t *testing.T) *MongoSessionStore {
	t.Helper()
	return mongoSessionStoreWith(t, nil)
}

// mongoSessionStoreWith is mongoSessionStoreForTest on a client configure
// adjusts (a read preference, a command monitor).
func mongoSessionStoreWith(t *testing.T, configure func(*options.ClientOptions)) *MongoSessionStore {
	t.Helper()
	uri := os.Getenv("ITERION_TEST_MONGO_URI")
	if uri == "" {
		t.Skip("ITERION_TEST_MONGO_URI not set; skipping Mongo session store suite")
	}
	ctx, cancel := mongotest.Ctx(t)
	defer cancel()
	opts := options.Client().ApplyURI(uri)
	if configure != nil {
		configure(opts)
	}
	client, err := mongo.Connect(opts)
	if err != nil {
		t.Fatalf("mongo connect: %v", err)
	}
	nonce := make([]byte, 4)
	_, _ = rand.Read(nonce)
	db := client.Database("iterion_sessions_" + hex.EncodeToString(nonce))
	t.Cleanup(func() {
		drop, dc := mongotest.TeardownCtx()
		defer dc()
		_ = db.Drop(drop)
		_ = client.Disconnect(drop)
	})
	st := NewMongoSessionStore(db)
	if err := st.EnsureSchema(ctx); err != nil {
		t.Fatalf("EnsureSchema: %v", err)
	}
	return st
}
