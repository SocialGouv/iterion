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

// TestDroppedFenceRefusesEveryoneAndStaysRecoverable: when a document
// loses its fencing epoch (an older binary's full-document replace, a
// pre-lease legacy claim), the holder is REFUSED — admitting it looks
// harmless because the marker still pins ownership, and is not: a marker
// has successive generations (release, then re-claim by the same worker),
// and with the field gone every generation matches, so a superseded token
// re-stamps the fence at its own older value and locks the live holder
// out of its own card. Refusing is the safe failure. What must not happen
// is the card becoming UNRECOVERABLE, so the watchdog's un-leased arm has
// to see it.
func TestDroppedFenceRefusesEveryoneAndStaysRecoverable(t *testing.T) {
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
	// Two generations of the SAME marker — the shape that makes admitting
	// an absent epoch unsafe.
	tokGen1, err := st.Claim(iss.ID, "worker-A")
	if err != nil {
		t.Fatalf("Claim gen1: %v", err)
	}
	if err := st.ReleaseOwned(iss.ID, tokGen1); err != nil {
		t.Fatalf("ReleaseOwned gen1: %v", err)
	}
	tokGen2, err := st.Claim(iss.ID, "worker-A")
	if err != nil {
		t.Fatalf("Claim gen2: %v", err)
	}
	if tokGen2.Epoch <= tokGen1.Epoch {
		t.Fatalf("precondition: a fresh acquisition must advance the epoch (%d → %d)", tokGen1.Epoch, tokGen2.Epoch)
	}

	// An older binary's replace drops the fence field (and the lease with it).
	issues := db.Collection(boardmongo.IssuesCollection)
	if _, err := issues.UpdateOne(ctx, bson.M{"_id": iss.ID}, bson.M{"$unset": bson.M{
		"issue.claimepoch": "", "issue.claimleaseuntil": "",
	}}); err != nil {
		t.Fatalf("drop the fence fields: %v", err)
	}

	// The SUPERSEDED generation must not be able to take the card back.
	if err := st.RenewClaim(iss.ID, tokGen1); !errors.Is(err, tracker.ErrClaimConflict) {
		after, _ := st.Get(iss.ID)
		t.Fatalf("a superseded token was admitted to a card whose fence field was dropped "+
			"(err=%v, card epoch now %d) — it can re-stamp the fence at its own older value "+
			"and lock the live holder out", err, after.ClaimEpoch)
	}
	// The live one is refused too — the safe failure, not a silent pass.
	if err := st.RenewClaim(iss.ID, tokGen2); !errors.Is(err, tracker.ErrClaimConflict) {
		t.Fatalf("with no epoch on the document the fence cannot judge anyone: want a refusal, got %v", err)
	}

	// ...and the card must still be REACHABLE, or refusing everyone would
	// have created a permanently held card instead of a stolen one.
	if _, err := issues.UpdateOne(ctx, bson.M{"_id": iss.ID},
		bson.M{"$set": bson.M{"issue.updatedat": time.Now().Add(-48 * time.Hour)}}); err != nil {
		t.Fatalf("age the card: %v", err)
	}
	cands, err := st.ListExpiredClaimCandidates(time.Now(), 50)
	if err != nil {
		t.Fatalf("ListExpiredClaimCandidates: %v", err)
	}
	var found bool
	for _, c := range cands {
		if c.IssueID == iss.ID {
			found = true
		}
	}
	if !found {
		t.Fatal("a long-untouched claim carrying NO lease must be reachable by the watchdog — " +
			"otherwise it is held forever by an owner nothing can refuse and nothing can relieve")
	}
	// Reachable is not enough: the TRANSFER must accept what the listing
	// produced. When the two drifted apart the watchdog listed and refused
	// the same card on every pass — a net that looks present and is not.
	prev := tracker.ClaimToken{Marker: "worker-A", Epoch: 0}
	for _, c := range cands {
		if c.IssueID == iss.ID {
			prev = c.Prev
		}
	}
	if _, _, err := st.ReclaimExpired(iss.ID, prev, "reaper:probe", time.Now()); err != nil {
		t.Fatalf("the transfer must accept a candidate the listing produced, got %v — "+
			"listed every pass, refused every pass, is not a recovery path", err)
	}
	// A FRESH un-leased claim must NOT be listed: absence of a lease is not
	// evidence of death, so only long staleness qualifies.
	fresh, err := st.Create(native.Issue{Title: "fresh legacy", State: native.StateInProgress})
	if err != nil {
		t.Fatalf("Create fresh: %v", err)
	}
	if _, err := issues.UpdateOne(ctx, bson.M{"_id": fresh.ID},
		bson.M{"$set": bson.M{"issue.claim": "legacy-owner"}}); err != nil {
		t.Fatalf("seed legacy claim: %v", err)
	}
	cands, _ = st.ListExpiredClaimCandidates(time.Now(), 50)
	for _, c := range cands {
		if c.IssueID == fresh.ID {
			t.Fatal("a RECENT claim with no lease must not be reaped by time — absence of a lease proves nothing")
		}
	}
}

// TestEpochIsMonotoneAcrossAFamilyDrop: the fence is only a fence while
// its counter cannot repeat. Derived from the document alone it restarts
// at 1 whenever the field goes missing — and an older binary's
// full-document replace removes it — so a worker holding a card twice
// (markers are per-process, not per-claim) would be handed the SAME token
// twice, and its first, superseded one would still be accepted.
func TestEpochIsMonotoneAcrossAFamilyDrop(t *testing.T) {
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
	db := client.Database("iterion_board_mono_" + hex.EncodeToString(nonce))
	t.Cleanup(func() {
		drop, dc := context.WithTimeout(context.Background(), 10*time.Second)
		defer dc()
		_ = db.Drop(drop)
		_ = client.Disconnect(drop)
	})
	if err := boardmongo.EnsureSchema(ctx, db); err != nil {
		t.Fatalf("EnsureSchema: %v", err)
	}
	st := boardmongo.New(db, "tenant-mono")
	iss, err := st.Create(native.Issue{Title: "monotone probe", State: native.StateInProgress})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	first, err := st.Claim(iss.ID, "worker-A")
	if err != nil {
		t.Fatalf("Claim first: %v", err)
	}
	if err := st.ReleaseOwned(iss.ID, first); err != nil {
		t.Fatalf("ReleaseOwned: %v", err)
	}

	// An older binary's full-document replace takes the whole claim family
	// with it (its struct carries none of these fields).
	issues := db.Collection(boardmongo.IssuesCollection)
	if _, err := issues.UpdateOne(ctx, bson.M{"_id": iss.ID}, bson.M{"$unset": bson.M{
		"issue.claim": "", "issue.claimepoch": "", "issue.claimedat": "", "issue.claimleaseuntil": "",
	}}); err != nil {
		t.Fatalf("drop the claim family: %v", err)
	}

	// The floor is the server clock at MILLISECOND resolution: two mints
	// after a family drop within the same ms both take now_ms and tie.
	// Unreachable in production (it needs two family drops on one card
	// inside 1ms, and round 13 removed the unscoped replace that produced
	// them) — but this test CAN mint that fast, and mongo-conformance is
	// a required check: force the tick over.
	time.Sleep(2 * time.Millisecond)
	second, err := st.Claim(iss.ID, "worker-A")
	if err != nil {
		t.Fatalf("Claim second: %v", err)
	}
	if second.Epoch <= first.Epoch {
		t.Fatalf("a re-mint after the field was dropped must land AHEAD of every token ever issued: "+
			"first=%d second=%d — the two holds are indistinguishable and the superseded token still writes",
			first.Epoch, second.Epoch)
	}
	// The superseded token must be refused everywhere.
	if err := st.RenewClaim(iss.ID, first); !errors.Is(err, tracker.ErrClaimConflict) {
		t.Fatalf("superseded token renew: want ErrClaimConflict, got %v", err)
	}
	if err := st.SetLastRunOwned(iss.ID, "zombie-run", "/tmp/z", first); !errors.Is(err, tracker.ErrClaimConflict) {
		t.Fatalf("superseded token write: want ErrClaimConflict, got %v", err)
	}
}

// TestUnleasedArmDoesNotStarveTheBatch: a missing lease sorts before
// every real one, so a single query over both arms would let un-leased
// stragglers fill the batch and starve the cards the watchdog exists to
// act on — expired leases, the ones carrying positive evidence that a
// heartbeat stopped.
func TestUnleasedArmDoesNotStarveTheBatch(t *testing.T) {
	uri := os.Getenv("ITERION_TEST_MONGO_URI")
	if uri == "" {
		t.Skip("ITERION_TEST_MONGO_URI not set; skipping Mongo board suite")
	}
	client, err := mongo.Connect(options.Client().ApplyURI(uri))
	if err != nil {
		t.Fatalf("mongo connect: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	nonce := make([]byte, 4)
	_, _ = rand.Read(nonce)
	db := client.Database("iterion_board_starve_" + hex.EncodeToString(nonce))
	t.Cleanup(func() {
		drop, dc := context.WithTimeout(context.Background(), 10*time.Second)
		defer dc()
		_ = db.Drop(drop)
		_ = client.Disconnect(drop)
	})
	if err := boardmongo.EnsureSchema(ctx, db); err != nil {
		t.Fatalf("EnsureSchema: %v", err)
	}
	st := boardmongo.New(db, "tenant-starve")
	issues := db.Collection(boardmongo.IssuesCollection)

	// Five un-leased ghosts, long untouched.
	for i := 0; i < 5; i++ {
		g, err := st.Create(native.Issue{Title: "ghost", State: native.StateInProgress})
		if err != nil {
			t.Fatalf("Create ghost: %v", err)
		}
		if _, err := issues.UpdateOne(ctx, bson.M{"_id": g.ID}, bson.M{"$set": bson.M{
			"issue.claim": "old-owner", "issue.updatedat": time.Now().Add(-72 * time.Hour),
		}, "$unset": bson.M{"issue.claimleaseuntil": "", "issue.claimepoch": ""}}); err != nil {
			t.Fatalf("seed ghost: %v", err)
		}
	}
	// One genuinely expired lease — the card the watchdog must reach.
	real, err := st.Create(native.Issue{Title: "genuinely expired", State: native.StateInProgress})
	if err != nil {
		t.Fatalf("Create real: %v", err)
	}
	if _, err := st.Claim(real.ID, "dead-owner"); err != nil {
		t.Fatalf("Claim real: %v", err)
	}

	// A batch smaller than the ghost pile: the expired lease must still
	// make it in.
	cands, err := st.ListExpiredClaimCandidates(time.Now().Add(2*native.ClaimLeaseDuration), 3)
	if err != nil {
		t.Fatalf("ListExpiredClaimCandidates: %v", err)
	}
	for _, c := range cands {
		if c.IssueID == real.ID {
			return
		}
	}
	t.Fatalf("the genuinely-expired card was crowded out of a %d-card batch by un-leased stragglers — "+
		"they sort first and would monopolise every pass", len(cands))
}
