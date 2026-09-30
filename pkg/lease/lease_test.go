package lease

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/SocialGouv/iterion/pkg/internal/mongotest"
)

// forEachStore runs one conformance row against both twins: the memory store
// always, the Mongo store when ITERION_TEST_MONGO_URI names a replica set (the
// CI mongo-conformance job does). The row gets a fresh store.
func forEachStore(t *testing.T, row func(t *testing.T, ctx context.Context, st Store)) {
	t.Run("memory", func(t *testing.T) {
		row(t, context.Background(), NewMemoryStore())
	})
	t.Run("mongo", func(t *testing.T) {
		ctx, db := testDB(t)
		row(t, ctx, NewMongoStore(db))
	})
}

// testDB opens a fresh database on the replica set ITERION_TEST_MONGO_URI
// names, dropped at cleanup; it skips the test when the variable is unset.
func testDB(t *testing.T) (context.Context, *mongo.Database) {
	t.Helper()
	uri := os.Getenv("ITERION_TEST_MONGO_URI")
	if uri == "" {
		t.Skip("ITERION_TEST_MONGO_URI not set; skipping the Mongo lease suite")
	}
	ctx, cancel := mongotest.Ctx(t)
	t.Cleanup(cancel)
	client, err := mongo.Connect(options.Client().ApplyURI(uri))
	if err != nil {
		t.Fatalf("mongo connect: %v", err)
	}
	nonce := make([]byte, 4)
	_, _ = rand.Read(nonce)
	db := client.Database("iterion_lease_" + hex.EncodeToString(nonce))
	t.Cleanup(func() {
		drop, dc := mongotest.TeardownCtx()
		defer dc()
		_ = db.Drop(drop)
		_ = client.Disconnect(drop)
	})
	return ctx, db
}

// t0 is millisecond-aligned: BSON datetimes are, so an instant with finer
// precision would sit on a different side of an expiry boundary in each twin.
var t0 = time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)

const ttl = time.Minute

func mustRenew(t *testing.T, ctx context.Context, st Store, owner string, now time.Time, want bool) {
	t.Helper()
	got, err := st.Renew(ctx, "sweeper", owner, now, ttl)
	if err != nil {
		t.Fatalf("Renew(%s, %s): %v", owner, now.Format(time.RFC3339Nano), err)
	}
	if got != want {
		t.Fatalf("Renew(%s, %s) = %v, want %v", owner, now.Format(time.RFC3339Nano), got, want)
	}
}

func mustAcquire(t *testing.T, ctx context.Context, st Store, owner string, now time.Time, want bool) {
	t.Helper()
	got, err := st.Acquire(ctx, "sweeper", owner, now, ttl)
	if err != nil {
		t.Fatalf("Acquire(%s, %s): %v", owner, now.Format(time.RFC3339Nano), err)
	}
	if got != want {
		t.Fatalf("Acquire(%s, %s) = %v, want %v", owner, now.Format(time.RFC3339Nano), got, want)
	}
}

func TestAcquire_AFreeLeaseIsTaken(t *testing.T) {
	forEachStore(t, func(t *testing.T, ctx context.Context, st Store) {
		mustAcquire(t, ctx, st, "a", t0, true)
	})
}

// The election itself: while the holder's lease runs, nobody else gets it.
func TestAcquire_AHeldLeaseRefusesAnotherOwner(t *testing.T) {
	forEachStore(t, func(t *testing.T, ctx context.Context, st Store) {
		mustAcquire(t, ctx, st, "a", t0, true)
		mustAcquire(t, ctx, st, "b", t0.Add(ttl/2), false)
		mustAcquire(t, ctx, st, "b", t0.Add(ttl-time.Millisecond), false)
	})
}

// Stickiness: the holder renews, and the renewal moves the expiry, so a
// challenger arriving at the ORIGINAL expiry still loses.
func TestAcquire_TheHolderRenewsItsLease(t *testing.T) {
	forEachStore(t, func(t *testing.T, ctx context.Context, st Store) {
		mustAcquire(t, ctx, st, "a", t0, true)
		mustAcquire(t, ctx, st, "a", t0.Add(ttl/2), true)
		mustAcquire(t, ctx, st, "b", t0.Add(ttl), false)
	})
}

// A renewal stamping the instant already stored changes no field. It is still
// a renewal: an implementation reading "modified" as "won" loses its own
// lease on the second call within a millisecond.
func TestAcquire_ARenewalAtTheSameInstantIsNotALoss(t *testing.T) {
	forEachStore(t, func(t *testing.T, ctx context.Context, st Store) {
		mustAcquire(t, ctx, st, "a", t0, true)
		mustAcquire(t, ctx, st, "a", t0, true)
	})
}

// Takeover: a holder that stops renewing is outlived by its TTL — expiry
// inclusive — and the new owner then holds the lease against the old one.
func TestAcquire_AnExpiredLeaseIsTakenOver(t *testing.T) {
	forEachStore(t, func(t *testing.T, ctx context.Context, st Store) {
		mustAcquire(t, ctx, st, "a", t0, true)
		mustAcquire(t, ctx, st, "b", t0.Add(ttl), true)
		mustAcquire(t, ctx, st, "a", t0.Add(ttl), false)
	})
}

// Hand-over: a release frees the lease at once, which is what spares a
// successor the TTL on every rollout.
func TestRelease_FreesTheLeaseAtOnce(t *testing.T) {
	forEachStore(t, func(t *testing.T, ctx context.Context, st Store) {
		mustAcquire(t, ctx, st, "a", t0, true)
		if err := st.Release(ctx, "sweeper", "a"); err != nil {
			t.Fatalf("Release: %v", err)
		}
		mustAcquire(t, ctx, st, "b", t0.Add(time.Second), true)
	})
}

// A holder that overran its TTL must not free its SUCCESSOR's lease on the
// way out: the release is refused, typed, and the successor keeps it.
func TestRelease_ByAnotherOwnerIsRefusedAndChangesNothing(t *testing.T) {
	forEachStore(t, func(t *testing.T, ctx context.Context, st Store) {
		mustAcquire(t, ctx, st, "a", t0, true)
		mustAcquire(t, ctx, st, "b", t0.Add(ttl), true)
		if err := st.Release(ctx, "sweeper", "a"); !errors.Is(err, ErrLost) {
			t.Fatalf("Release by the overrun holder = %v, want ErrLost", err)
		}
		mustAcquire(t, ctx, st, "c", t0.Add(ttl+time.Second), false)
	})
}

// Renewal extends the holder's lease, like the holder's Acquire does.
func TestRenew_ExtendsTheHoldersLease(t *testing.T) {
	forEachStore(t, func(t *testing.T, ctx context.Context, st Store) {
		mustAcquire(t, ctx, st, "a", t0, true)
		mustRenew(t, ctx, st, "a", t0.Add(ttl/2), true)
		mustAcquire(t, ctx, st, "b", t0.Add(ttl), false)
		mustAcquire(t, ctx, st, "b", t0.Add(ttl/2+ttl), true)
	})
}

// A renewal never takes: not a lease another owner holds, not a free one.
func TestRenew_NeverTakesALease(t *testing.T) {
	forEachStore(t, func(t *testing.T, ctx context.Context, st Store) {
		mustRenew(t, ctx, st, "a", t0, false)
		mustAcquire(t, ctx, st, "b", t0, true)
		mustRenew(t, ctx, st, "a", t0.Add(ttl*2), false)
		mustAcquire(t, ctx, st, "a", t0.Add(ttl/2), false)
	})
}

// The reason Renew exists: a renewal that lands after its holder released
// the lease must not re-create it — the log says "released", the successor
// must find the lease free.
func TestRenew_AfterAReleaseLeavesTheLeaseFree(t *testing.T) {
	forEachStore(t, func(t *testing.T, ctx context.Context, st Store) {
		mustAcquire(t, ctx, st, "a", t0, true)
		if err := st.Release(ctx, "sweeper", "a"); err != nil {
			t.Fatalf("Release: %v", err)
		}
		mustRenew(t, ctx, st, "a", t0.Add(time.Second), false)
		mustAcquire(t, ctx, st, "b", t0.Add(2*time.Second), true)
	})
}

// A renewal stamping the instant already stored changes no field; it is still
// the holder's renewal (an implementation reading "modified" loses it).
func TestRenew_AtTheSameInstantIsNotALoss(t *testing.T) {
	forEachStore(t, func(t *testing.T, ctx context.Context, st Store) {
		mustAcquire(t, ctx, st, "a", t0, true)
		mustRenew(t, ctx, st, "a", t0, true)
	})
}

// A lease past its expiry that nobody took is still its owner's: the owner may
// renew it, and until someone takes it over, nobody else holds it.
func TestRenew_AnExpiredLeaseNobodyTookIsStillTheOwners(t *testing.T) {
	forEachStore(t, func(t *testing.T, ctx context.Context, st Store) {
		mustAcquire(t, ctx, st, "a", t0, true)
		mustRenew(t, ctx, st, "a", t0.Add(2*ttl), true)
		mustAcquire(t, ctx, st, "b", t0.Add(2*ttl+ttl/2), false)
	})
}

// An older stamp that lands late — a renewal held up in the store, overtaken
// by the next one — never moves the expiry backward: the holder's step-down
// is counted from the later one, so a shortened lease would let a successor
// in while the holder still serves.
func TestExpiry_NeverMovesBackward(t *testing.T) {
	forEachStore(t, func(t *testing.T, ctx context.Context, st Store) {
		mustAcquire(t, ctx, st, "a", t0, true)
		mustRenew(t, ctx, st, "a", t0.Add(40*time.Second), true)
		mustRenew(t, ctx, st, "a", t0.Add(20*time.Second), true)
		mustAcquire(t, ctx, st, "b", t0.Add(85*time.Second), false)
		mustAcquire(t, ctx, st, "a", t0.Add(10*time.Second), true)
		mustAcquire(t, ctx, st, "b", t0.Add(95*time.Second), false)
		mustAcquire(t, ctx, st, "b", t0.Add(100*time.Second), true)
	})
}

// A call on a done context fails and changes nothing — a store round-trip
// cannot land on a context already cancelled, and Run's stop paths are built
// on which calls still reach the store.
func TestCalls_OnADoneContextFailAndChangeNothing(t *testing.T) {
	forEachStore(t, func(t *testing.T, ctx context.Context, st Store) {
		done, cancel := context.WithCancel(ctx)
		cancel()
		if ok, err := st.Acquire(done, "sweeper", "a", t0, ttl); err == nil || ok {
			t.Fatalf("Acquire on a done context = %v, %v; want an error", ok, err)
		}
		mustAcquire(t, ctx, st, "b", t0, true)
		if ok, err := st.Renew(done, "sweeper", "b", t0.Add(ttl/2), ttl); err == nil || ok {
			t.Fatalf("Renew on a done context = %v, %v; want an error", ok, err)
		}
		mustAcquire(t, ctx, st, "c", t0.Add(ttl), true) // b's lease was not extended
		if err := st.Release(done, "sweeper", "c"); err == nil {
			t.Fatal("Release on a done context returned nil, want an error")
		}
		mustAcquire(t, ctx, st, "d", t0.Add(ttl+time.Second), false) // c still holds it
	})
}

func TestRelease_OfALeaseNobodyHoldsIsANoOp(t *testing.T) {
	forEachStore(t, func(t *testing.T, ctx context.Context, st Store) {
		if err := st.Release(ctx, "sweeper", "a"); err != nil {
			t.Fatalf("Release of a free lease = %v, want nil", err)
		}
	})
}

// Leases are independent by name.
func TestAcquire_LeasesAreKeyedByName(t *testing.T) {
	forEachStore(t, func(t *testing.T, ctx context.Context, st Store) {
		for _, name := range []string{"one", "two"} {
			if ok, err := st.Acquire(ctx, name, "a", t0, ttl); err != nil || !ok {
				t.Fatalf("Acquire(%s, a) = %v, %v", name, ok, err)
			}
		}
		if ok, err := st.Acquire(ctx, "two", "b", t0, ttl); err != nil || ok {
			t.Fatalf("Acquire(two, b) = %v, %v; want false, nil", ok, err)
		}
	})
}

// The races the store exists to settle: many candidates, one lease, the same
// instant — for a lease that does not exist yet (they race to create it) and
// for one whose holder's TTL ran out (they race to take it over). Exactly one
// wins, every round.
func TestAcquire_ConcurrentCandidatesElectExactlyOne(t *testing.T) {
	forEachStore(t, func(t *testing.T, ctx context.Context, st Store) {
		const candidates, rounds = 8, 10
		for round := 0; round < rounds; round++ {
			name := fmt.Sprintf("race-%d", round)
			at := t0
			if round%2 == 1 {
				if ok, err := st.Acquire(ctx, name, "former-holder", t0, ttl); err != nil || !ok {
					t.Fatalf("round %d: seeding the expired lease: %v, %v", round, ok, err)
				}
				at = t0.Add(ttl + time.Second)
			}
			var (
				wg    sync.WaitGroup
				mu    sync.Mutex
				wins  int
				errs  []error
				start = make(chan struct{})
			)
			for i := 0; i < candidates; i++ {
				wg.Add(1)
				go func(owner string) {
					defer wg.Done()
					<-start
					ok, err := st.Acquire(ctx, name, owner, at, ttl)
					mu.Lock()
					defer mu.Unlock()
					if err != nil {
						errs = append(errs, err)
					} else if ok {
						wins++
					}
				}(fmt.Sprintf("candidate-%d", i))
			}
			close(start)
			wg.Wait()
			if len(errs) > 0 {
				t.Fatalf("round %d: store errors %v — a lost race must read as \"not elected\", never as an error", round, errs)
			}
			if wins != 1 {
				t.Fatalf("round %d: %d candidates elected, want exactly 1", round, wins)
			}
		}
	})
}

// A TTL under the stores' millisecond would be expired as it is written.
func TestAcquire_RejectsUnusableArguments(t *testing.T) {
	forEachStore(t, func(t *testing.T, ctx context.Context, st Store) {
		for _, c := range []struct {
			name, owner string
			ttl         time.Duration
		}{
			{"", "a", ttl},
			{"sweeper", " ", ttl},
			{"sweeper", "a", 0},
			{"sweeper", "a", 2},
			{"sweeper", "a", time.Millisecond - 1},
		} {
			if ok, err := st.Acquire(ctx, c.name, c.owner, t0, c.ttl); err == nil || ok {
				t.Errorf("Acquire(%q, %q, %s) = %v, %v; want an error", c.name, c.owner, c.ttl, ok, err)
			}
			if ok, err := st.Renew(ctx, c.name, c.owner, t0, c.ttl); err == nil || ok {
				t.Errorf("Renew(%q, %q, %s) = %v, %v; want an error", c.name, c.owner, c.ttl, ok, err)
			}
		}
	})
}

// Mongo keeps instants to the millisecond, so both twins do: at sub-millisecond
// instants and durations they answer as their millisecond, and alike.
func TestAcquire_BothTwinsMeasureTimeToTheMillisecond(t *testing.T) {
	forEachStore(t, func(t *testing.T, ctx context.Context, st Store) {
		sub := time.Duration(400) * time.Microsecond
		if ok, err := st.Acquire(ctx, "sweeper", "a", t0.Add(sub), ttl); err != nil || !ok {
			t.Fatalf("a: %v, %v", ok, err)
		}
		// a's lease runs to t0+ttl (its instant truncated); b arrives within
		// that same millisecond past the expiry, which counts as expired.
		if ok, err := st.Acquire(ctx, "sweeper", "b", t0.Add(ttl+sub/2), ttl); err != nil || !ok {
			t.Errorf("b at the expiry's millisecond = %v, %v; want the lease (expired to the millisecond)", ok, err)
		}
		if ok, err := st.Acquire(ctx, "short", "a", t0, time.Millisecond+sub); err != nil || !ok {
			t.Fatalf("short a: %v, %v", ok, err)
		}
		if ok, err := st.Acquire(ctx, "short", "b", t0.Add(time.Millisecond-time.Microsecond), ttl); err != nil || ok {
			t.Errorf("b inside a 1.4ms lease's first millisecond = %v, %v; want refused", ok, err)
		}
		// A lease taken at t0+0.4ms runs from its millisecond, t0: with a 1.7ms
		// TTL it ends at t0+1ms — not at t0+2ms, where the unrounded sum lands.
		if ok, err := st.Acquire(ctx, "offset", "a", t0.Add(sub), time.Millisecond+700*time.Microsecond); err != nil || !ok {
			t.Fatalf("offset a: %v, %v", ok, err)
		}
		if ok, err := st.Acquire(ctx, "offset", "b", t0.Add(1500*time.Microsecond), ttl); err != nil || !ok {
			t.Errorf("b at t0+1.5ms = %v, %v; want the lease, which ended at t0+1ms", ok, err)
		}
	})
}

// The zero value is a usable store, like sync.Mutex's.
func TestMemoryStore_TheZeroValueWorks(t *testing.T) {
	var st MemoryStore
	if ok, err := st.Acquire(context.Background(), "sweeper", "a", t0, ttl); err != nil || !ok {
		t.Fatalf("zero-value Acquire = %v, %v", ok, err)
	}
	if ok, err := st.Renew(context.Background(), "sweeper", "a", t0, ttl); err != nil || !ok {
		t.Fatalf("zero-value Renew = %v, %v", ok, err)
	}
}

// The production shape of a renewal that outlives its term, on a real replica
// set: the holder's renewal is held up in the server (here, behind an open
// transaction on the lease document — a lock, a slow disk or a failover would
// do the same) when its replica shuts down and releases. The renewal lands
// after the release; it must not re-create the lease for a process that is
// gone.
func TestMongoStore_ARenewalHeldUpPastTheReleaseDoesNotRecreateTheLease(t *testing.T) {
	ctx, db := testDB(t)
	st := NewMongoStore(db)
	coll := db.Collection(Collection)
	log := &logSink{}
	const leaseTTL = 6 * time.Second // renewal due at 2s
	actx, acancel := context.WithCancel(ctx)
	elected := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		var once sync.Once
		_ = Run(actx, st, spec("a", leaseTTL, log), func(c context.Context) {
			once.Do(func() { close(elected) })
			<-c.Done()
		})
	}()
	<-elected
	sess, err := db.Client().StartSession()
	if err != nil {
		t.Fatal(err)
	}
	defer sess.EndSession(ctx)
	if err := sess.StartTransaction(); err != nil {
		t.Fatal(err)
	}
	sctx := mongo.NewSessionContext(ctx, sess)
	if _, err := coll.UpdateOne(sctx, bson.M{"_id": "sweeper"}, bson.M{"$set": bson.M{"blocker": 1}}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2500 * time.Millisecond) // a's renewal is sent at ≈2s and waits in the server
	acancel()                           // a's replica shuts down
	time.Sleep(300 * time.Millisecond)  // a's release is sent and waits too
	if err := sess.AbortTransaction(ctx); err != nil {
		t.Fatal(err)
	}
	<-done
	if !log.has("released") {
		t.Fatalf("a did not report its release; log: %v", log.all())
	}
	// Give the held-up renewal every chance to land before looking.
	time.Sleep(time.Second)
	if ok, err := st.Acquire(ctx, "sweeper", "b", time.Now(), leaseTTL); err != nil || !ok {
		var doc bson.M
		_ = coll.FindOne(ctx, bson.M{"_id": "sweeper"}).Decode(&doc)
		t.Errorf("after a released, the successor is refused (%v, %v): the lease document is %v", ok, err, doc)
	}
}
