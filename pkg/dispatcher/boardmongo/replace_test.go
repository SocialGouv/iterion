package boardmongo_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/SocialGouv/iterion/pkg/dispatcher/boardmongo"
	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	"github.com/SocialGouv/iterion/pkg/dispatcher/tracker"
)

// TestReplacePreservesUnknownIssueFields pins the mixed-fleet contract of
// the store's full-issue write path: a binary must never erase issue
// subdocument fields it does not know about. Under the historical
// ReplaceOne this test is RED — a newer binary's field (here the stand-in
// zz_future_field, tomorrow a claim lease) vanished on the older binary's
// first SetLastRun/SetState/Update, silently disarming whatever feature
// owned it.
func TestReplacePreservesUnknownIssueFields(t *testing.T) {
	uri := os.Getenv("ITERION_TEST_MONGO_URI")
	if uri == "" {
		t.Skip("ITERION_TEST_MONGO_URI not set; skipping Mongo board suite")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	client, err := mongo.Connect(options.Client().ApplyURI(uri))
	if err != nil {
		t.Fatalf("mongo connect: %v", err)
	}
	nonce := make([]byte, 4)
	_, _ = rand.Read(nonce)
	db := client.Database("iterion_board_replace_" + hex.EncodeToString(nonce))
	t.Cleanup(func() {
		drop, dc := context.WithTimeout(context.Background(), 10*time.Second)
		defer dc()
		_ = db.Drop(drop)
		_ = client.Disconnect(drop)
	})
	if err := boardmongo.EnsureSchema(ctx, db); err != nil {
		t.Fatalf("EnsureSchema: %v", err)
	}
	st := boardmongo.New(db, "tenant-mixed-fleet")

	iss, err := st.Create(native.Issue{Title: "mixed-fleet probe", Body: "b"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// A NEWER binary wrote a field this one's struct does not carry.
	issues := db.Collection(boardmongo.IssuesCollection)
	if _, err := issues.UpdateOne(ctx,
		bson.M{"_id": iss.ID},
		bson.M{"$set": bson.M{"issue.zz_future_field": "must-survive"}}); err != nil {
		t.Fatalf("inject future field: %v", err)
	}

	// This binary performs ordinary full-issue mutations through the
	// write path under test.
	if err := st.SetLastRun(iss.ID, "run-123", "/tmp/wd"); err != nil {
		t.Fatalf("SetLastRun: %v", err)
	}
	if _, err := st.SetState(iss.ID, "ready"); err != nil {
		t.Fatalf("SetState: %v", err)
	}

	var doc bson.Raw
	if err := issues.FindOne(ctx, bson.M{"_id": iss.ID}).Decode(&doc); err != nil {
		t.Fatalf("read raw doc: %v", err)
	}
	if got, ok := doc.Lookup("issue", "zz_future_field").StringValueOK(); !ok || got != "must-survive" {
		t.Fatalf("newer binary's field erased by this binary's write (got %q, ok=%t) — the ReplaceOne regression", got, ok)
	}

	// And the mutations themselves landed with full semantics preserved.
	got, err := st.Get(iss.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.LastRunID != "run-123" || got.State != "ready" {
		t.Fatalf("mutations lost: %+v", got)
	}
}

// TestOrdinaryWritesNeverRewindTheFence is the concurrency canary for the
// other half of the same write path: an ordinary (unfenced) mutation is a
// read-modify-write, so persisting the claim family along with it
// re-applies the ownership as it stood when the document was READ. Under
// the fence that is not a stale field, it is a rewind: a reaper's transfer
// is undone, the epoch moves BACKWARDS, and the card returns to the owner
// the watchdog just evicted — with a fresh lease, so nothing lists it
// again for a full lease period.
//
// The defect only exists between a read and its write, so the assertion is
// an INVARIANT measured under real concurrency rather than a sequential
// replay: the epoch a claim writer was granted must never be observed to
// decrease, whatever ordinary writes interleave. With the claim family
// excluded from the write path the invariant holds on every interleaving
// (no flake); with it re-included this reproduces in the first handful of
// iterations.
func TestOrdinaryWritesNeverRewindTheFence(t *testing.T) {
	uri := os.Getenv("ITERION_TEST_MONGO_URI")
	if uri == "" {
		t.Skip("ITERION_TEST_MONGO_URI not set; skipping Mongo board suite")
	}
	client, err := mongo.Connect(options.Client().ApplyURI(uri))
	if err != nil {
		t.Fatalf("mongo connect: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	nonce := make([]byte, 4)
	_, _ = rand.Read(nonce)
	db := client.Database("iterion_board_fence_" + hex.EncodeToString(nonce))
	t.Cleanup(func() {
		drop, dc := context.WithTimeout(context.Background(), 10*time.Second)
		defer dc()
		_ = db.Drop(drop)
		_ = client.Disconnect(drop)
	})
	if err := boardmongo.EnsureSchema(ctx, db); err != nil {
		t.Fatalf("EnsureSchema: %v", err)
	}
	st := boardmongo.New(db, "tenant-fence")

	iss, err := st.Create(native.Issue{Title: "fence rewind probe", State: native.StateInProgress})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// A worker holds the card; ordinary writes hammer it throughout.
	tok, err := st.Claim(iss.ID, "owner-live")
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 60; i++ {
			_ = st.SetLastRun(iss.ID, "run-ordinary", "/tmp/ordinary")
			if _, _, err := st.AddComment(iss.ID, "op", "note"); err != nil {
				return
			}
		}
	}()
	// Meanwhile the reaper transfers the card away, repeatedly: each
	// transfer is a fresh grant whose epoch must stand.
	granted := tok.Epoch
	prev := tok
	for i := 0; i < 60; i++ {
		future := time.Now().Add(2 * native.ClaimLeaseDuration)
		next, _, rerr := st.ReclaimExpired(iss.ID, prev, "reaper:probe", future)
		if rerr != nil {
			// A transfer may legitimately race an ordinary write's timing;
			// re-read the truth and carry on.
			cur, gerr := st.Get(iss.ID)
			if gerr != nil {
				t.Fatalf("Get during reap loop: %v", gerr)
			}
			if cur.ClaimEpoch < granted {
				t.Fatalf("FENCE REWOUND by an ordinary write: epoch %d < granted %d (claim %q) — "+
					"a read-modify-write re-applied a stale claim", cur.ClaimEpoch, granted, cur.Claim)
			}
			prev = tracker.ClaimToken{Marker: cur.Claim, Epoch: cur.ClaimEpoch}
			continue
		}
		if next.Epoch < granted {
			t.Fatalf("transfer granted a DECREASING epoch: %d < %d", next.Epoch, granted)
		}
		granted, prev = next.Epoch, next
		cur, gerr := st.Get(iss.ID)
		if gerr != nil {
			t.Fatalf("Get after transfer: %v", gerr)
		}
		if cur.ClaimEpoch < granted || cur.Claim != next.Marker {
			t.Fatalf("the transfer was undone: card is claim=%q epoch=%d, the grant was %q epoch=%d "+
				"(a stale ordinary write re-applied the claim it had read)",
				cur.Claim, cur.ClaimEpoch, next.Marker, granted)
		}
	}
	<-done

	// Final truth: the last grant still owns the card, and its lease is
	// intact — a rewind would have restored the evicted owner.
	final, err := st.Get(iss.ID)
	if err != nil {
		t.Fatalf("Get final: %v", err)
	}
	if final.Claim != prev.Marker || final.ClaimEpoch != granted {
		t.Fatalf("after the ordinary-write storm the card must still belong to the last grant: "+
			"claim=%q epoch=%d, want %q epoch=%d", final.Claim, final.ClaimEpoch, prev.Marker, granted)
	}
	if final.ClaimLeaseUntil.IsZero() {
		t.Fatal("the recovery owner's lease was cleared by an ordinary write — the card is now invisible to the watchdog")
	}
}

// TestTokenlessReleaseCannotStealFromTheReaper is the Release half of the
// same class. Release(id, marker) is the tokenless, marker-scoped release
// of the BoardStore contract — reached by the dispatcher's ordinary
// give-back paths. Written as check-then-act it reads "the card is mine",
// then writes unconditionally; if a watchdog TRANSFERS the card in that
// gap, the write lands on the recovery owner's claim, clears it, and the
// card becomes instantly re-dispatchable in the middle of its own
// disposition — the double-launch window the transfer exists to close.
//
// The window opens only while the caller genuinely holds the claim, so
// each round re-arms it: claim, then race a release against a transfer.
func TestTokenlessReleaseCannotStealFromTheReaper(t *testing.T) {
	uri := os.Getenv("ITERION_TEST_MONGO_URI")
	if uri == "" {
		t.Skip("ITERION_TEST_MONGO_URI not set; skipping Mongo board suite")
	}
	client, err := mongo.Connect(options.Client().ApplyURI(uri))
	if err != nil {
		t.Fatalf("mongo connect: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	nonce := make([]byte, 4)
	_, _ = rand.Read(nonce)
	db := client.Database("iterion_board_relsteal_" + hex.EncodeToString(nonce))
	t.Cleanup(func() {
		drop, dc := context.WithTimeout(context.Background(), 10*time.Second)
		defer dc()
		_ = db.Drop(drop)
		_ = client.Disconnect(drop)
	})
	if err := boardmongo.EnsureSchema(ctx, db); err != nil {
		t.Fatalf("EnsureSchema: %v", err)
	}
	st := boardmongo.New(db, "tenant-relsteal")
	iss, err := st.Create(native.Issue{Title: "release steal probe", State: native.StateInProgress})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	for round := 0; round < 200; round++ {
		tokA, cerr := st.Claim(iss.ID, "owner-live")
		if cerr != nil {
			t.Fatalf("round %d: Claim: %v", round, cerr)
		}
		start := make(chan struct{})
		relDone := make(chan struct{})
		go func() {
			defer close(relDone)
			<-start
			_ = st.Release(iss.ID, "owner-live")
		}()
		close(start)
		future := time.Now().Add(2 * native.ClaimLeaseDuration)
		rec, _, rerr := st.ReclaimExpired(iss.ID, tokA, "reaper:probe", future)
		<-relDone
		if rerr != nil {
			continue // the release won the race outright: nothing was transferred
		}
		// The transfer succeeded, so the recovery owner OWNS the card. A
		// release issued by the evicted owner must not be able to undo it.
		cur, gerr := st.Get(iss.ID)
		if gerr != nil {
			t.Fatalf("round %d: Get: %v", round, gerr)
		}
		if cur.Claim != rec.Marker || cur.ClaimEpoch != rec.Epoch {
			t.Fatalf("round %d: the evicted owner's tokenless Release STOLE the card from the reaper: "+
				"claim=%q epoch=%d, the transfer granted %q epoch=%d",
				round, cur.Claim, cur.ClaimEpoch, rec.Marker, rec.Epoch)
		}
		if err := st.ReleaseOwned(iss.ID, rec); err != nil {
			t.Fatalf("round %d: cleanup ReleaseOwned: %v", round, err)
		}
	}
}

// TestRenewHealsADroppedFence: the fencing epoch is admitted when ABSENT
// (an older binary's full-document replace drops it, and refusing the
// live holder there would fence it out of its own card with no recovery).
// That admission must not outlive the first heartbeat: nothing else in
// the system ever re-creates the field, so a document left healed-at-read
// would accept ANY epoch for the whole hold — the fence, silently off.
func TestRenewHealsADroppedFence(t *testing.T) {
	uri := os.Getenv("ITERION_TEST_MONGO_URI")
	if uri == "" {
		t.Skip("ITERION_TEST_MONGO_URI not set; skipping Mongo board suite")
	}
	client, err := mongo.Connect(options.Client().ApplyURI(uri))
	if err != nil {
		t.Fatalf("mongo connect: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	nonce := make([]byte, 4)
	_, _ = rand.Read(nonce)
	db := client.Database("iterion_board_heal_" + hex.EncodeToString(nonce))
	t.Cleanup(func() {
		drop, dc := context.WithTimeout(context.Background(), 10*time.Second)
		defer dc()
		_ = db.Drop(drop)
		_ = client.Disconnect(drop)
	})
	if err := boardmongo.EnsureSchema(ctx, db); err != nil {
		t.Fatalf("EnsureSchema: %v", err)
	}
	st := boardmongo.New(db, "tenant-heal")
	iss, err := st.Create(native.Issue{Title: "heal probe", State: native.StateInProgress})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	tok, err := st.Claim(iss.ID, "pod-new")
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}

	// An older binary's replace drops the fence field.
	issues := db.Collection(boardmongo.IssuesCollection)
	if _, err := issues.UpdateOne(ctx, bson.M{"_id": iss.ID},
		bson.M{"$unset": bson.M{"issue.claimepoch": ""}}); err != nil {
		t.Fatalf("drop the fence field: %v", err)
	}

	// The live holder is admitted — that is the point of the nil arm.
	if err := st.RenewClaim(iss.ID, tok); err != nil {
		t.Fatalf("the live holder must be admitted to its own healed card: %v", err)
	}
	// ...and the beat must have RE-STAMPED the fence, so a bogus epoch is
	// refused from here on.
	bogus := tracker.ClaimToken{Marker: "pod-new", Epoch: tok.Epoch + 424242}
	if err := st.RenewClaim(iss.ID, bogus); !errors.Is(err, tracker.ErrClaimConflict) {
		t.Fatalf("after healing, an arbitrary epoch must be refused: got %v — the fence is open for the whole hold", err)
	}
	after, _ := st.Get(iss.ID)
	if after.ClaimEpoch != tok.Epoch {
		t.Fatalf("the heartbeat must re-stamp the holder's own epoch: got %d, want %d", after.ClaimEpoch, tok.Epoch)
	}
}
