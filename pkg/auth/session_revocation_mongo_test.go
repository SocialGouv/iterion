package auth

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/event"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"go.mongodb.org/mongo-driver/v2/mongo/readpref"

	"github.com/SocialGouv/iterion/pkg/identity"
)

// The Mongo twin revokes in two commands — the revocation's record, then its
// pass over the sessions — where the memory twin holds one lock around both.
// Only the Mongo twin can interleave a rotation between them, so the cases
// below run on Mongo alone and hold the pass apart from the record: a suite
// run on both twins stays green on a race the memory twin cannot have.

// heldPassTag marks the context of the revocation whose pass a heldPassGate
// holds; only commands issued under it are ever paused.
type heldPassTag struct{}

// heldPassGate pauses, before it is sent, the first `update` command on the
// sessions collection issued under a heldPassTag context — the detached pass
// of a lineage or account revocation (Service.runPass keeps its caller's
// context values), which runs after the revocation's record landed. It
// pauses, it changes nothing: every write is the store's.
type heldPassGate struct {
	once    sync.Once
	reached chan struct{}
	release chan struct{}
}

func newHeldPassGate() *heldPassGate {
	return &heldPassGate{reached: make(chan struct{}), release: make(chan struct{})}
}

func (g *heldPassGate) monitor() *event.CommandMonitor {
	return &event.CommandMonitor{Started: func(ctx context.Context, e *event.CommandStartedEvent) {
		if ctx.Value(heldPassTag{}) == nil || e.CommandName != "update" {
			return
		}
		if coll, ok := e.Command.Lookup("update").StringValueOK(); !ok || coll != SessionsCollectionName {
			return
		}
		g.once.Do(func() {
			close(g.reached)
			<-g.release
		})
	}}
}

// holdingSessions holds, once armed, the next successor write
// (CreateSession) or the next successor check (SessionRevoked) until
// released — a pause, the write and the read are the store's own.
type holdingSessions struct {
	SessionStore
	holdCreate, holdCheck atomic.Bool
	entered, proceed      chan struct{}
}

func newHoldingSessions(st SessionStore) *holdingSessions {
	return &holdingSessions{SessionStore: st, entered: make(chan struct{}), proceed: make(chan struct{})}
}

func (h *holdingSessions) CreateSession(ctx context.Context, s Session) error {
	if h.holdCreate.CompareAndSwap(true, false) {
		close(h.entered)
		<-h.proceed
	}
	return h.SessionStore.CreateSession(ctx, s)
}

func (h *holdingSessions) SessionRevoked(ctx context.Context, s Session) (bool, error) {
	if h.holdCheck.CompareAndSwap(true, false) {
		close(h.entered)
		<-h.proceed
	}
	return h.SessionStore.SessionRevoked(ctx, s)
}

// wait fails the test unless ch closes within a generous bound.
func wait(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(20 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

// liveRows lists the sessions of userID nothing revoked, on Mongo itself.
func liveRows(t *testing.T, st *MongoSessionStore, userID string) []Session {
	t.Helper()
	cur, err := st.coll.Find(context.Background(), bson.M{"user_id": userID, "revoked_at": bson.M{"$exists": false}})
	if err != nil {
		t.Fatalf("find live sessions: %v", err)
	}
	var out []Session
	if err := cur.All(context.Background(), &out); err != nil {
		t.Fatalf("decode live sessions: %v", err)
	}
	return out
}

// TestMongoSessionStore_ARevocationHeldBeforeItsPassOutrunsNoRotation holds
// each revocation — "sign out everywhere", a sign-out, reuse detection —
// between its record and its pass, and lands a rotation of the family it
// revokes at every point around that gap: the rotation inside the gap with
// its successor written after the revocation returned; a whole refresh
// inside the gap; a successor written before the revocation and checked
// after it; a successor rotated again inside the gap. No schedule may leave
// a session of the revoked family live, and none may touch another lineage
// a lineage revocation does not name.
func TestMongoSessionStore_ARevocationHeldBeforeItsPassOutrunsNoRotation(t *testing.T) {
	type fixture struct {
		st            *MongoSessionStore
		hold          *holdingSessions
		holder        *Service // answers the refreshes
		victim        *Service // answers the reuse: no window, so a retired token is reuse at once
		userID        string
		retired, live string // two tokens of one lineage: live is retired's successor
		other         string // the account's other device: another lineage
	}
	setup := func(t *testing.T, gate *heldPassGate) *fixture {
		t.Helper()
		ctx := context.Background()
		st := mongoSessionStoreWith(t, func(o *options.ClientOptions) { o.SetMonitor(gate.monitor()) })
		ids := identity.NewMemoryStore()
		signer := newTestSigner(t)
		f := &fixture{st: st, hold: newHoldingSessions(st)}
		var err error
		if f.holder, err = NewService(Config{Store: ids, Sessions: f.hold, Signer: signer, SignupMode: SignupOpen, RefreshTTL: time.Hour}); err != nil {
			t.Fatalf("NewService: %v", err)
		}
		zero := time.Duration(0)
		if f.victim, err = NewService(Config{Store: ids, Sessions: st, Signer: signer, SignupMode: SignupOpen, RefreshTTL: time.Hour, RefreshReuseGrace: &zero}); err != nil {
			t.Fatalf("NewService: %v", err)
		}
		reg, err := f.holder.Register(ctx, "held-pass@example.org", "correct-horse-9", "", "", "ua", "ip")
		if err != nil {
			t.Fatalf("Register: %v", err)
		}
		next, err := f.holder.Refresh(ctx, reg.RefreshToken, "ua", "ip")
		if err != nil {
			t.Fatalf("the lineage's first rotation: %v", err)
		}
		other, err := f.holder.Login(ctx, "held-pass@example.org", "correct-horse-9", "other-device", "ip2")
		if err != nil {
			t.Fatalf("Login (other device): %v", err)
		}
		f.userID, f.retired, f.live, f.other = reg.User.ID, reg.RefreshToken, next.RefreshToken, other.RefreshToken
		return f
	}
	revocations := []struct {
		name    string
		account bool
		revoke  func(ctx context.Context, f *fixture) error
	}{
		{"sign out everywhere", true, func(ctx context.Context, f *fixture) error {
			return f.holder.RevokeUserSessions(ctx, f.userID)
		}},
		{"a sign-out", false, func(ctx context.Context, f *fixture) error {
			return f.holder.Logout(ctx, f.retired)
		}},
		{"reuse detection", false, func(ctx context.Context, f *fixture) error {
			if _, err := f.victim.Refresh(ctx, f.retired, "ua", "ip"); !errors.Is(err, ErrSessionRevoked) {
				return err
			}
			return nil
		}},
	}
	// verdict checks the family is gone and, for a lineage revocation, that
	// the other lineage is not.
	verdict := func(t *testing.T, f *fixture, account bool) {
		t.Helper()
		f.holder.WaitRevocationPasses()
		f.victim.WaitRevocationPasses()
		otherHash := HashRefreshToken(f.other)
		otherLive := false
		for _, s := range liveRows(t, f.st, f.userID) {
			if s.TokenHash == otherHash {
				otherLive = true
				continue
			}
			t.Errorf("session %s (lineage %s, rotated from %q) is live after the revocation of its family", s.ID, s.LineageID, s.RotatedFromID)
		}
		if account && otherLive {
			t.Error("the other device's session is live after \"sign out everywhere\"")
		}
		if !account && !otherLive {
			t.Error("a lineage revocation revoked the account's other device")
		}
	}
	for _, rv := range revocations {
		t.Run(rv.name, func(t *testing.T) {
			t.Run("a rotation inside the gap, its successor written after the revocation returned", func(t *testing.T) {
				gate := newHeldPassGate()
				f := setup(t, gate)
				revoked := make(chan error, 1)
				go func() { revoked <- rv.revoke(context.WithValue(context.Background(), heldPassTag{}, true), f) }()
				wait(t, gate.reached, "the revocation's pass")
				f.hold.holdCreate.Store(true)
				answered := make(chan error, 1)
				go func() {
					_, err := f.holder.Refresh(context.Background(), f.live, "ua", "ip")
					answered <- err
				}()
				wait(t, f.hold.entered, "the successor's write")
				close(gate.release)
				if err := <-revoked; err != nil {
					t.Fatalf("the revocation: %v", err)
				}
				close(f.hold.proceed)
				if err := <-answered; !errors.Is(err, ErrSessionRevoked) {
					t.Fatalf("the refresh whose rotation landed inside the revocation: got %v, want ErrSessionRevoked", err)
				}
				verdict(t, f, rv.account)
			})
			t.Run("a whole refresh inside the gap", func(t *testing.T) {
				gate := newHeldPassGate()
				f := setup(t, gate)
				revoked := make(chan error, 1)
				go func() { revoked <- rv.revoke(context.WithValue(context.Background(), heldPassTag{}, true), f) }()
				wait(t, gate.reached, "the revocation's pass")
				_, err := f.holder.Refresh(context.Background(), f.live, "ua", "ip")
				close(gate.release)
				if rerr := <-revoked; rerr != nil {
					t.Fatalf("the revocation: %v", rerr)
				}
				if !errors.Is(err, ErrSessionRevoked) {
					t.Fatalf("a refresh inside the revocation: got %v, want ErrSessionRevoked", err)
				}
				verdict(t, f, rv.account)
			})
			t.Run("a successor written before the revocation, checked after it", func(t *testing.T) {
				gate := newHeldPassGate()
				f := setup(t, gate)
				close(gate.release) // this schedule holds the refresh, not the pass
				f.hold.holdCheck.Store(true)
				answered := make(chan error, 1)
				go func() {
					_, err := f.holder.Refresh(context.Background(), f.live, "ua", "ip")
					answered <- err
				}()
				wait(t, f.hold.entered, "the successor's check")
				if err := rv.revoke(context.WithValue(context.Background(), heldPassTag{}, true), f); err != nil {
					t.Fatalf("the revocation: %v", err)
				}
				close(f.hold.proceed)
				if err := <-answered; !errors.Is(err, ErrSessionRevoked) {
					t.Fatalf("a refresh whose check ran after the revocation: got %v, want ErrSessionRevoked", err)
				}
				verdict(t, f, rv.account)
			})
			t.Run("a successor rotated again inside the gap", func(t *testing.T) {
				gate := newHeldPassGate()
				f := setup(t, gate)
				chained, err := f.holder.Refresh(context.Background(), f.live, "ua", "ip")
				if err != nil {
					t.Fatalf("the rotation before the revocation: %v", err)
				}
				revoked := make(chan error, 1)
				go func() { revoked <- rv.revoke(context.WithValue(context.Background(), heldPassTag{}, true), f) }()
				wait(t, gate.reached, "the revocation's pass")
				_, err = f.holder.Refresh(context.Background(), chained.RefreshToken, "ua", "ip")
				close(gate.release)
				if rerr := <-revoked; rerr != nil {
					t.Fatalf("the revocation: %v", rerr)
				}
				if !errors.Is(err, ErrSessionRevoked) {
					t.Fatalf("a rotation of the chain inside the revocation: got %v, want ErrSessionRevoked", err)
				}
				verdict(t, f, rv.account)
			})
		})
	}
}

// TestMongoSessionStore_ConformanceUnderASecondaryReadPreference runs the
// suite and a refresh through the window on a client whose URI would send
// every read to a secondary. The store reads back its own writes — the
// rotation's stamp against the store's clock, a successor's check against
// the revocations recorded before it — so its reads must stay on the
// primary. A single-node replica set has no secondary: a read that follows
// the client's preference finds no server at all.
func TestMongoSessionStore_ConformanceUnderASecondaryReadPreference(t *testing.T) {
	st := mongoSessionStoreWith(t, func(o *options.ClientOptions) {
		o.SetReadPreference(readpref.Secondary()).SetServerSelectionTimeout(3 * time.Second)
	})
	runSessionStoreSuite(t, st)
	ctx := context.Background()
	svc := newTestServiceOn(t, identity.NewMemoryStore(), st)
	reg, err := svc.Register(ctx, "secondary@example.org", "correct-horse-9", "", "", "ua", "ip")
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	first, err := svc.Refresh(ctx, reg.RefreshToken, "ua", "ip")
	if err != nil {
		t.Fatalf("the rotation under a secondary read preference: %v", err)
	}
	again, err := svc.Refresh(ctx, reg.RefreshToken, "ua", "ip")
	if err != nil || again.RefreshToken != first.RefreshToken {
		t.Fatalf("a retry inside the window under a secondary read preference: err=%v, same successor=%t", err, again.RefreshToken == first.RefreshToken)
	}
}

// checkHook runs a hook once, at the start of the next SessionRevoked — the
// successor's check — before the store's own read: another presentation
// landing between a writer's write and its check. The hook is taken before it
// runs, so the presentation it drives checks through the store unhooked.
type checkHook struct {
	SessionStore
	hook atomic.Pointer[func()]
}

func (h *checkHook) arm(f func()) { h.hook.Store(&f) }

func (h *checkHook) SessionRevoked(ctx context.Context, s Session) (bool, error) {
	if f := h.hook.Swap(nil); f != nil {
		(*f)()
	}
	return h.SessionStore.SessionRevoked(ctx, s)
}

// TestMongoSessionStore_ASuccessorHandedOutOutlivesItsWritersFailure drives,
// on Mongo, a refresh W whose client leaves during the check of the successor
// it wrote, after another presentation P of the same token — a second tab, or
// the desktop presenting again at once — was handed that successor: P's
// session must stay live and claimed, and refresh. The same with a successor
// never written, completed by two presentations at once.
func TestMongoSessionStore_ASuccessorHandedOutOutlivesItsWritersFailure(t *testing.T) {
	for _, shape := range []string{"a fresh rotation", "a completion"} {
		t.Run(shape, func(t *testing.T) {
			ctx := context.Background()
			st := mongoSessionStoreForTest(t)
			hooked := &checkHook{SessionStore: st}
			ids := identity.NewMemoryStore()
			svc, err := NewService(Config{Store: ids, Sessions: hooked, Signer: newTestSigner(t), SignupMode: SignupOpen, RefreshTTL: time.Hour})
			if err != nil {
				t.Fatalf("NewService: %v", err)
			}
			reg, err := svc.Register(ctx, "handed-out@example.org", "correct-horse-9", "", "", "ua", "ip")
			if err != nil {
				t.Fatalf("Register: %v", err)
			}
			if shape == "a completion" {
				// Retire the token with no successor written: a rotation whose
				// write never ran.
				prev, err := st.GetSessionByTokenHash(ctx, HashRefreshToken(reg.RefreshToken))
				if err != nil {
					t.Fatalf("read the root: %v", err)
				}
				rot, _, err := svc.newRotation(reg.RefreshToken, "ua", "ip")
				if err != nil {
					t.Fatalf("newRotation: %v", err)
				}
				if _, ok, err := st.RotateSession(ctx, prev.ID, rot); err != nil || !ok {
					t.Fatalf("RotateSession: ok=%t err=%v", ok, err)
				}
			}
			wctx, leave := context.WithCancel(ctx)
			var p LoginResult
			var perr error
			hooked.arm(func() {
				p, perr = svc.Refresh(ctx, reg.RefreshToken, "ua", "ip")
				leave()
			})
			if _, err := svc.Refresh(wctx, reg.RefreshToken, "ua", "ip"); !errors.Is(err, context.Canceled) {
				t.Fatalf("writer W: got %v, want context.Canceled", err)
			}
			if perr != nil || p.RefreshToken == "" {
				t.Fatalf("presentation P: err=%v, a successor handed out: %t — the witness did not drive the race it pins", perr, p.RefreshToken != "")
			}
			got, err := st.GetSessionByTokenHash(ctx, HashRefreshToken(p.RefreshToken))
			if err != nil {
				t.Fatalf("the successor P was handed is gone after W's failure: %v", err)
			}
			if got.RevokedAt != nil || got.DeliveredAt == nil {
				t.Fatalf("the successor P was handed reads revoked_at=%v delivered_at=%v: want live and claimed", got.RevokedAt, got.DeliveredAt)
			}
			if _, err := svc.Refresh(ctx, p.RefreshToken, "ua", "ip"); err != nil {
				t.Fatalf("the successor P was handed no longer refreshes: %v", err)
			}
		})
	}
}

// TestMongoSessionStore_ARevocationRecordIsNeverReaped: a lineage's record is
// what covers a session its pass did not reach, for as long as that session
// lives, under whatever refresh TTL it was written; so no record carries an
// expiry and no TTL index reaps the collection.
func TestMongoSessionStore_ARevocationRecordIsNeverReaped(t *testing.T) {
	ctx := context.Background()
	st := mongoSessionStoreForTest(t)
	now := time.Now().UTC()
	if err := st.RecordLineageRevocation(ctx, "user-kept", "lineage-kept", now); err != nil {
		t.Fatalf("RecordLineageRevocation: %v", err)
	}
	if _, err := st.RecordAccountRevocation(ctx, "user-kept", now); err != nil {
		t.Fatalf("RecordAccountRevocation: %v", err)
	}
	cur, err := st.revocations.Find(ctx, bson.M{})
	if err != nil {
		t.Fatalf("read the revocations: %v", err)
	}
	var rows []bson.M
	if err := cur.All(ctx, &rows); err != nil {
		t.Fatalf("decode the revocations: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("the two revocations wrote %d record(s), want 2", len(rows))
	}
	for _, r := range rows {
		if _, has := r["expires_at"]; has {
			t.Fatalf("the revocation record %v carries an expiry: a TTL index would reap it before the sessions it covers", r["_id"])
		}
	}
	icur, err := st.revocations.Indexes().List(ctx)
	if err != nil {
		t.Fatalf("list the revocations' indexes: %v", err)
	}
	var idx []bson.M
	if err := icur.All(ctx, &idx); err != nil {
		t.Fatalf("decode the indexes: %v", err)
	}
	for _, ix := range idx {
		if _, ttl := ix["expireAfterSeconds"]; ttl {
			t.Fatalf("the revocations collection carries the TTL index %v: a record must outlive every session it covers", ix["name"])
		}
	}
}
