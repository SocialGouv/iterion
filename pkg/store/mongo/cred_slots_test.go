package mongo

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/SocialGouv/iterion/pkg/internal/mongotest"
	"github.com/SocialGouv/iterion/pkg/store"
)

// The one eviction the cross-backend row cannot reach without a clock: a
// holder whose run document never came keeps its slot inside
// store.CredSlotGrace and loses it past it. Written straight into the row,
// the way a launch that died between its two writes leaves it. Skipped
// without ITERION_TEST_MONGO_URI, like the conformance suite.
func TestCredSlots_AHolderWithNoRunDocumentIsEvictedPastTheGrace(t *testing.T) {
	uri := os.Getenv("ITERION_TEST_MONGO_URI")
	if uri == "" {
		t.Skip("ITERION_TEST_MONGO_URI not set; skipping Mongo cred-slot test")
	}
	ctx, cancel := mongotest.Ctx(t)
	defer cancel()
	s, err := New(ctx, Config{URI: uri, Database: "iterion_credslots_" + bsonNonce(t), Blob: newInMemoryBlob()})
	if err != nil {
		t.Fatalf("mongo New: %v", err)
	}
	t.Cleanup(func() {
		drop, dropCancel := mongotest.TeardownCtx()
		defer dropCancel()
		_ = s.db.Drop(drop)
		_ = s.Close(drop)
	})
	const fp = "fp-capped"
	stale := time.Now().UTC().Add(-store.CredSlotGrace - time.Minute)
	if _, err := s.credSlots.InsertOne(ctx, bson.M{"_id": fp, "holders": bson.A{bson.M{"run_id": "ghost-old", "at": stale}}}); err != nil {
		t.Fatalf("seed row: %v", err)
	}
	tctx := store.WithIdentity(context.Background(), "_test", "_test")
	if _, err := s.CreateRun(tctx, "live", "demo", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if err := s.SetRunCredStamp(tctx, "live", store.RunCredStamp{Fingerprints: []string{fp}}); err != nil {
		t.Fatalf("SetRunCredStamp: %v", err)
	}
	adm, err := s.ReserveCredSlot(tctx, fp, "live", 1)
	if err != nil {
		t.Fatalf("ReserveCredSlot: %v", err)
	}
	if !adm.Granted || adm.Held != 1 {
		t.Fatalf("a holder with no document past the grace kept its slot: %+v", adm)
	}
	// A fresh ghost (inside the grace) holds its slot against the next run.
	if _, err := s.credSlots.UpdateOne(ctx, bson.M{"_id": fp}, bson.M{"$push": bson.M{"holders": bson.M{"run_id": "ghost-fresh", "at": time.Now().UTC()}}}); err != nil {
		t.Fatalf("seed fresh ghost: %v", err)
	}
	if _, err := s.CreateRun(tctx, "late", "demo", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if err := s.SetRunCredStamp(tctx, "late", store.RunCredStamp{Fingerprints: []string{fp}}); err != nil {
		t.Fatalf("SetRunCredStamp: %v", err)
	}
	if adm, err := s.ReserveCredSlot(tctx, fp, "late", 2); err != nil || adm.Granted {
		t.Fatalf("a holder inside the grace was evicted: %+v %v", adm, err)
	}
	// Deleting a run frees its slot at once, not at the grace.
	if err := s.DeleteRun(tctx, "live"); err != nil {
		t.Fatalf("DeleteRun: %v", err)
	}
	var row credSlotDoc
	if err := s.credSlots.FindOne(ctx, bson.M{"_id": fp}).Decode(&row); err != nil {
		t.Fatalf("read row: %v", err)
	}
	if row.holds("live") {
		t.Fatalf("a deleted run still holds a slot: %+v", row.Holders)
	}
}

// The cap's headline property on the production ledger: of N reservations
// racing for a key with K free slots, exactly K are granted, whichever
// connection each ran on. The filesystem twin proves this under one mutex;
// here the conditional update itself — `holders.<max-1>` absent, `$push`
// in the same write — is what decides, so this is the witness the deployed
// guarantee rests on. Every racer has a stamped, queued run document, as a
// launch's does once it persists, so no holder is evictable. Red when the
// room check leaves the update's filter (every racer is granted).
func TestCredSlots_RacingReservationsAdmitExactlyTheCap(t *testing.T) {
	uri := os.Getenv("ITERION_TEST_MONGO_URI")
	if uri == "" {
		t.Skip("ITERION_TEST_MONGO_URI not set; skipping Mongo cred-slot test")
	}
	ctx, cancel := mongotest.Ctx(t)
	defer cancel()
	s, err := New(ctx, Config{URI: uri, Database: "iterion_credslots_race_" + bsonNonce(t), Blob: newInMemoryBlob()})
	if err != nil {
		t.Fatalf("mongo New: %v", err)
	}
	t.Cleanup(func() {
		drop, dropCancel := mongotest.TeardownCtx()
		defer dropCancel()
		_ = s.db.Drop(drop)
		_ = s.Close(drop)
	})
	const (
		fp     = "fp-raced"
		cap    = 3
		racers = 16
	)
	tctx := store.WithIdentity(context.Background(), "_test", "_test")
	ids := make([]string, racers)
	for i := range ids {
		ids[i] = fmt.Sprintf("racer-%02d", i)
		if _, err := s.CreateRun(tctx, ids[i], "demo", nil); err != nil {
			t.Fatalf("CreateRun %s: %v", ids[i], err)
		}
		if err := s.SetRunCredStamp(tctx, ids[i], store.RunCredStamp{Fingerprints: []string{fp}}); err != nil {
			t.Fatalf("SetRunCredStamp %s: %v", ids[i], err)
		}
	}
	race := func(ids []string) (granted []string) {
		var mu sync.Mutex
		var wg sync.WaitGroup
		start := make(chan struct{})
		errs := make([]error, len(ids))
		for i, id := range ids {
			wg.Add(1)
			go func(i int, id string) {
				defer wg.Done()
				<-start
				adm, err := s.ReserveCredSlot(tctx, fp, id, cap)
				if err != nil {
					errs[i] = err
					return
				}
				mu.Lock()
				defer mu.Unlock()
				if adm.Granted {
					granted = append(granted, id)
				} else if adm.Held != cap {
					errs[i] = fmt.Errorf("refused %s reports %d holders, want the cap %d", id, adm.Held, cap)
				}
			}(i, id)
		}
		close(start)
		wg.Wait()
		for _, err := range errs {
			if err != nil {
				t.Fatalf("ReserveCredSlot: %v", err)
			}
		}
		return granted
	}
	granted := race(ids)
	if len(granted) != cap {
		t.Fatalf("%d of %d racers were granted a slot on a cap of %d: %v", len(granted), racers, cap, granted)
	}
	var row credSlotDoc
	if err := s.credSlots.FindOne(ctx, bson.M{"_id": fp}).Decode(&row); err != nil {
		t.Fatalf("read row: %v", err)
	}
	if len(row.Holders) != cap {
		t.Fatalf("the row names %d holders after the race, want %d: %+v", len(row.Holders), cap, row.Holders)
	}
	for _, id := range granted {
		if !row.holds(id) {
			t.Fatalf("granted run %s is not a holder of the row: %+v", id, row.Holders)
		}
	}
	// One slot given back, the refused racers race again: exactly one more.
	if err := s.ReleaseCredSlot(tctx, fp, granted[0]); err != nil {
		t.Fatalf("ReleaseCredSlot: %v", err)
	}
	var refused []string
	for _, id := range ids {
		if !row.holds(id) {
			refused = append(refused, id)
		}
	}
	if again := race(refused); len(again) != 1 {
		t.Fatalf("%d of %d refused racers were granted the one freed slot: %v", len(again), len(refused), again)
	}
}

// The window the eviction's CAS exists for, on the production ledger: a
// launch's reconcile judges a paused holder stale, and before its $pull
// lands another replica resumes that run — flips it to queued and admits it
// again in place. The pull must miss the re-admitted entry: taken, the run
// executes with no slot while the launch that evicted it takes the slot, two
// runs on a cap of one. The interleaving is driven through the seam between
// the judgment and the pull, so it is the same on every run. Red when the
// re-admission leaves the entry as the reconcile judged it.
func TestCredSlots_AnEvictionJudgedBeforeAReadmissionMissesTheReadmittedHolder(t *testing.T) {
	uri := os.Getenv("ITERION_TEST_MONGO_URI")
	if uri == "" {
		t.Skip("ITERION_TEST_MONGO_URI not set; skipping Mongo cred-slot test")
	}
	ctx, cancel := mongotest.Ctx(t)
	defer cancel()
	s, err := New(ctx, Config{URI: uri, Database: "iterion_credslots_evict_" + bsonNonce(t), Blob: newInMemoryBlob()})
	if err != nil {
		t.Fatalf("mongo New: %v", err)
	}
	t.Cleanup(func() {
		drop, dropCancel := mongotest.TeardownCtx()
		defer dropCancel()
		_ = s.db.Drop(drop)
		_ = s.Close(drop)
	})
	const fp = "fp-evicted-in-flight"
	tctx := store.WithIdentity(context.Background(), "_test", "_test")
	for _, id := range []string{"run-a", "run-b"} {
		if _, err := s.CreateRun(tctx, id, "demo", nil); err != nil {
			t.Fatalf("CreateRun %s: %v", id, err)
		}
		if err := s.SetRunCredStamp(tctx, id, store.RunCredStamp{Fingerprints: []string{fp}}); err != nil {
			t.Fatalf("SetRunCredStamp %s: %v", id, err)
		}
	}
	if adm, err := s.ReserveCredSlot(tctx, fp, "run-a", 1); err != nil || !adm.Granted {
		t.Fatalf("admitting run-a: %+v %v", adm, err)
	}
	// run-a pauses: it spends nothing, so any reconcile may judge it stale.
	paused, err := s.LoadRun(tctx, "run-a")
	if err != nil {
		t.Fatalf("LoadRun run-a: %v", err)
	}
	paused.Status = store.RunStatusPausedWaitingHuman
	if err := s.SaveRun(tctx, paused); err != nil {
		t.Fatalf("pause run-a: %v", err)
	}
	resumed := false
	s.beforeCredSlotEvict = func(runID string) {
		if runID != "run-a" || resumed {
			return
		}
		resumed = true
		// Another replica resumes run-a between the judgment and the pull,
		// the way SubmitResume does: the flip first, then the admission.
		if _, ok, err := s.FlipToQueued(tctx, "run-a", store.RunStatusPausedWaitingHuman, time.Now()); err != nil || !ok {
			t.Fatalf("resume run-a: flipped=%v err=%v", ok, err)
		}
		if adm, err := s.ReserveCredSlot(tctx, fp, "run-a", 1); err != nil || !adm.Granted {
			t.Fatalf("re-admitting run-a on its own slot: %+v %v", adm, err)
		}
	}
	adm, err := s.ReserveCredSlot(tctx, fp, "run-b", 1)
	if err != nil {
		t.Fatalf("ReserveCredSlot run-b: %v", err)
	}
	if !resumed {
		t.Fatal("the reconcile never judged the paused run-a stale: the interleaving under test did not happen")
	}
	if adm.Granted {
		t.Fatalf("run-b was admitted (%+v): the pull took run-a's re-admitted entry, and both runs execute on a cap of one", adm)
	}
	var row credSlotDoc
	if err := s.credSlots.FindOne(ctx, bson.M{"_id": fp}).Decode(&row); err != nil {
		t.Fatalf("read row: %v", err)
	}
	if len(row.Holders) != 1 || !row.holds("run-a") {
		t.Fatalf("the row names %+v, want run-a alone", row.Holders)
	}
}
