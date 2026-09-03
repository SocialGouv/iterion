package boardmongo_test

// The scoped-replace contract (RVA round 13, CRITICAL finding): a write
// carries ONLY the fields its caller changed. The Mongo twin's replace()
// used to persist the WHOLE issue snapshot (minus the claim family), so
// any bot write or admin sweep racing a fenced owner re-applied state,
// labels, run history and the give-up stamp from a stale read —
// resurrecting terminal cards with no event, re-arming consumed
// one-shots, and erasing the run stamps the watchdog counts. These races
// are probabilistic per iteration but red with near-certainty over the
// loop when the scoping regresses (42/120, 1/120 and 36/120 observed on
// the unscoped code); the FS twin is the single-critical-section control.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"sync"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/SocialGouv/iterion/pkg/dispatcher/boardmongo"
	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	"github.com/SocialGouv/iterion/pkg/dispatcher/native/boardops"
	"github.com/SocialGouv/iterion/pkg/dispatcher/tracker"
)

func scopedMongo(t *testing.T) *boardmongo.Store {
	t.Helper()
	uri := os.Getenv("ITERION_TEST_MONGO_URI")
	if uri == "" {
		t.Skip("ITERION_TEST_MONGO_URI not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	client, err := mongo.Connect(options.Client().ApplyURI(uri))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	nonce := make([]byte, 4)
	_, _ = rand.Read(nonce)
	db := client.Database("iterion_scoped_" + hex.EncodeToString(nonce))
	t.Cleanup(func() {
		c, cc := context.WithTimeout(context.Background(), 10*time.Second)
		defer cc()
		_ = db.Drop(c)
		_ = client.Disconnect(c)
	})
	if err := boardmongo.EnsureSchema(ctx, db); err != nil {
		t.Fatalf("EnsureSchema: %v", err)
	}
	return boardmongo.New(db, "t1")
}

func scopedCaps() boardops.Capabilities {
	c := boardops.Capabilities{}
	for _, n := range boardops.AllCapabilities() {
		c[n] = true
	}
	return c
}

func scopedRaw(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// A bot's set_labels (which carries NO state intent) races the owner's
// fenced SetStateOwned into a TERMINAL sink. replace() $sets issue.state
// from the bot's stale snapshot, so the terminal filing is silently undone.
func TestScopedReplace_BotLabelWriteNeverMovesState_Mongo(t *testing.T) {
	s := scopedMongo(t)
	const N = 120
	resurrected := 0
	for i := 0; i < N; i++ {
		iss, err := s.Create(native.Issue{Title: "race", State: native.StateInProgress, Labels: []string{"a"}})
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		tok, err := s.Claim(iss.ID, "worker-1")
		if err != nil {
			t.Fatalf("claim: %v", err)
		}
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			// the owning worker files the card into the terminal sink
			if _, err := s.SetStateOwned(iss.ID, native.StateDone, tok); err != nil {
				t.Logf("iter %d: owned move: %v", i, err)
			}
		}()
		go func() {
			defer wg.Done()
			// the bot, through the production boardops surface
			if _, err := boardops.Call(s, scopedCaps(), "set_labels",
				scopedRaw(t, map[string]any{"id": iss.ID, "labels": []string{"a", "b"}})); err != nil {
				t.Logf("iter %d: set_labels: %v", i, err)
			}
		}()
		wg.Wait()
		got, err := s.Get(iss.ID)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if got.State != native.StateDone {
			resurrected++
			if resurrected <= 3 {
				t.Logf("RESURRECTED iter %d: card is %q after the owner filed it done; labels=%v", i, got.State, got.Labels)
			}
		}
	}
	if resurrected > 0 {
		t.Fatalf("%d/%d cards left OUT of the terminal sink the fenced owner filed them into — a label write carried the STATE from its stale snapshot", resurrected, N)
	}
}

// Same race on the FILESYSTEM twin — the control. Its Update reads and
// writes inside ONE critical section, so no rewind is possible.
func TestScopedReplace_BotLabelWriteNeverMovesState_FS(t *testing.T) {
	s, err := native.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	const N = 120
	resurrected := 0
	for i := 0; i < N; i++ {
		iss, _ := s.Create(native.Issue{Title: "race", State: native.StateInProgress, Labels: []string{"a"}})
		tok, err := s.Claim(iss.ID, "worker-1")
		if err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); _, _ = s.SetStateOwned(iss.ID, native.StateDone, tok) }()
		go func() {
			defer wg.Done()
			_, _ = boardops.Call(s, scopedCaps(), "set_labels", scopedRaw(t, map[string]any{"id": iss.ID, "labels": []string{"a", "b"}}))
		}()
		wg.Wait()
		got, _ := s.Get(iss.ID)
		if got.State != native.StateDone {
			resurrected++
		}
	}
	if resurrected > 0 {
		t.Fatalf("fs twin: %d/%d cards left OUT of the terminal sink", resurrected, N)
	}
}

// A bot's comment_issue races the trigger spine's ATOMIC one-shot label
// consume. replace() $sets issue.labels from the stale snapshot, so the
// consumed trigger label comes back — the one-shot re-arms itself.
func TestScopedReplace_BotCommentNeverRestoresAConsumedOneShot(t *testing.T) {
	s := scopedMongo(t)
	const N = 120
	restored := 0
	for i := 0; i < N; i++ {
		iss, err := s.Create(native.Issue{Title: "oneshot", State: native.StateReady,
			Labels: []string{native.LabelTriageAuto, "keep"}})
		if err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		var consumed bool
		wg.Add(2)
		go func() {
			defer wg.Done()
			c, err := s.ConsumeLabels(iss.ID, []string{native.LabelTriageAuto})
			if err != nil {
				t.Logf("consume: %v", err)
			}
			consumed = c
		}()
		go func() {
			defer wg.Done()
			_, _ = boardops.Call(s, scopedCaps(), "comment_issue",
				scopedRaw(t, map[string]any{"id": iss.ID, "body": "bot note"}))
		}()
		wg.Wait()
		got, _ := s.Get(iss.ID)
		still := false
		for _, l := range got.Labels {
			if l == native.LabelTriageAuto {
				still = true
			}
		}
		if consumed && still {
			restored++
			if restored <= 3 {
				t.Logf("ONE-SHOT RE-ARMED iter %d: consume reported true but %q is back: %v", i, native.LabelTriageAuto, got.Labels)
			}
		}
	}
	if restored > 0 {
		t.Fatalf("%d/%d consumed one-shot labels restored by a bot comment — the trigger re-arms itself with no actor", restored, N)
	}
}

// A bot's assign_issue races the dispatcher's SetLastRun stamp — the
// run history (Issue.Runs) is what the watchdog's LifetimeRuns ceiling
// counts.
func TestScopedReplace_BotAssignNeverDropsTheRunStamp(t *testing.T) {
	s := scopedMongo(t)
	const N = 120
	lost := 0
	for i := 0; i < N; i++ {
		iss, _ := s.Create(native.Issue{Title: "stamp", State: native.StateInProgress})
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); _ = s.SetLastRun(iss.ID, "run-abc", "/w") }()
		go func() {
			defer wg.Done()
			_, _ = boardops.Call(s, scopedCaps(), "assign_issue", scopedRaw(t, map[string]any{"id": iss.ID, "assignee": "someone"}))
		}()
		wg.Wait()
		got, _ := s.Get(iss.ID)
		if got.LastRunID != "run-abc" || len(got.Runs) == 0 {
			lost++
			if lost <= 3 {
				t.Logf("STAMP LOST iter %d: last_run=%q runs=%d", i, got.LastRunID, len(got.Runs))
			}
		}
	}
	if lost > 0 {
		t.Fatalf("%d/%d run stamps erased by a bot assign_issue — LifetimeRuns undercounts and the watchdog loses the card's run", lost, N)
	}
}

// The admin sweeps (RenameLabel / MergeLabels / DeleteLabel / the field
// rewrites / DeleteState's migrateState) take ONE listAll snapshot and then
// replace() every card from it. The snapshot for the LAST card is as old as
// the whole sweep, so any card filed into a terminal sink DURING the sweep is
// silently rewound — no EvtIssueState, no ValidateStateExit.
func TestScopedReplace_AdminLabelSweepNeverRewindsATerminalFiling(t *testing.T) {
	s := scopedMongo(t)
	const N = 400
	var victim string
	var victimTok tracker.ClaimToken
	for i := 0; i < N; i++ {
		iss, err := s.Create(native.Issue{Title: "sweep", State: native.StateInProgress, Labels: []string{"old"}})
		if err != nil {
			t.Fatal(err)
		}
		if i == N-1 {
			victim = iss.ID
			tok, cerr := s.Claim(iss.ID, "worker-1")
			if cerr != nil {
				t.Fatal(cerr)
			}
			victimTok = tok
		}
	}
	start := time.Now()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		n, err := s.RenameLabel("old", "new")
		t.Logf("RenameLabel touched %d cards in %s (err=%v)", n, time.Since(start), err)
	}()
	// The fenced owner files the victim into the terminal sink while the
	// operator's label sweep is walking its stale snapshot.
	time.Sleep(40 * time.Millisecond)
	if _, err := s.SetStateOwned(victim, native.StateDone, victimTok); err != nil {
		t.Fatalf("owned terminal filing: %v", err)
	}
	filedAt := time.Since(start)
	wg.Wait()
	got, _ := s.Get(victim)
	if got.State != native.StateDone {
		t.Fatalf("victim filed done at +%s, but after the sweep its state is %q — an operator RenameLabel pulled a card back OUT of the terminal sink with no state event", filedAt, got.State)
	}
	_ = got.Labels
}

// Full chain: the sweep's rewind puts a finished+released card back into an
// ELIGIBLE column with no claim — the dispatcher relaunches delivered work.
func TestScopedReplace_AdminSweepNeverRelaunchesDeliveredWork(t *testing.T) {
	s := scopedMongo(t)
	const N = 400
	var victim string
	for i := 0; i < N; i++ {
		iss, err := s.Create(native.Issue{Title: "sweep2", State: native.StateInProgress, Labels: []string{"old"}})
		if err != nil {
			t.Fatal(err)
		}
		if i == N-1 {
			victim = iss.ID
		}
	}
	tok, err := s.Claim(victim, "worker-1")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); _, _ = s.RenameLabel("old", "new") }()
	time.Sleep(40 * time.Millisecond)
	// the worker finishes exactly as a live one does: file, then release
	if _, err := s.SetStateOwned(victim, native.StateDone, tok); err != nil {
		t.Fatalf("file: %v", err)
	}
	if err := s.ReleaseOwned(victim, tok); err != nil {
		t.Fatalf("release: %v", err)
	}
	wg.Wait()
	got, _ := s.Get(victim)
	elig, err := s.List(native.ListFilter{States: []string{native.StateReady, native.StateInProgress}})
	if err != nil {
		t.Fatal(err)
	}
	for _, i := range elig {
		if i.ID == victim && i.Claim == "" {
			t.Fatalf("delivered work is back in an ELIGIBLE, UNCLAIMED column (state=%q) — the dispatcher would relaunch it", got.State)
		}
	}
}

// TestListUnleasedClaims_QueryFiltersAndDoesNotStarve (RVA round 13,
// HIGH): the batch cap applies at the QUERY, so the population filter
// must too. A post-listing Go filter let the conserved population —
// never written, therefore always oldest in the updatedat-ascending
// order — fill the batch of 100 and permanently starve the one
// repairable card, silently, on the exact board shape a pre-ADR-096
// deployment presents. Also pins the row's contract: claim present, no
// lease, past the 24h horizon, running column, a recorded run.
func TestListUnleasedClaims_QueryFiltersAndDoesNotStarve(t *testing.T) {
	uri := os.Getenv("ITERION_TEST_MONGO_URI")
	if uri == "" {
		t.Skip("ITERION_TEST_MONGO_URI not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	client, err := mongo.Connect(options.Client().ApplyURI(uri))
	if err != nil {
		t.Fatal(err)
	}
	nonce := make([]byte, 4)
	_, _ = rand.Read(nonce)
	db := client.Database("iterion_unleased_" + hex.EncodeToString(nonce))
	t.Cleanup(func() {
		c, cc := context.WithTimeout(context.Background(), 10*time.Second)
		defer cc()
		_ = db.Drop(c)
		_ = client.Disconnect(c)
	})
	if err := boardmongo.EnsureSchema(ctx, db); err != nil {
		t.Fatal(err)
	}
	s := boardmongo.New(db, "t1")
	coll := db.Collection(boardmongo.IssuesCollection)
	old := time.Now().UTC().Add(-48 * time.Hour) // past the 24h horizon
	strip := func(id string, extra bson.M) {
		set := bson.M{"issue.updatedat": old}
		unset := bson.M{"issue.claimleaseuntil": "", "issue.claimepoch": ""}
		for k, v := range extra {
			set[k] = v
			delete(unset, k)
		}
		if _, err := coll.UpdateOne(ctx, bson.M{"_id": id},
			bson.M{"$set": set, "$unset": unset}); err != nil {
			t.Fatal(err)
		}
	}
	// 120 CONSERVED-shape cards (no recorded run): older than everything.
	for i := 0; i < 120; i++ {
		iss, err := s.Create(native.Issue{Title: "conserved", State: native.StateInProgress})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.Claim(iss.ID, "podA-1"); err != nil {
			t.Fatal(err)
		}
		strip(iss.ID, nil)
	}
	// One repairable card (running column + a run), NEWEST of the batch.
	rep, err := s.Create(native.Issue{Title: "repairable", State: native.StateInProgress})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Claim(rep.ID, "podA-1"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetLastRun(rep.ID, "run-x", "/w"); err != nil {
		t.Fatal(err)
	}
	strip(rep.ID, nil)
	// Cards the row's contract EXCLUDES even with a run: fresh (inside
	// the horizon), leased, or parked outside the running column.
	excluded := map[string]string{}
	mk := func(label string, mutate func(id string)) {
		iss, err := s.Create(native.Issue{Title: label, State: native.StateInProgress})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.Claim(iss.ID, "podA-1"); err != nil {
			t.Fatal(err)
		}
		if err := s.SetLastRun(iss.ID, "run-y", "/w"); err != nil {
			t.Fatal(err)
		}
		mutate(iss.ID)
		excluded[iss.ID] = label
	}
	mk("fresh-inside-horizon", func(id string) {
		if _, err := coll.UpdateOne(ctx, bson.M{"_id": id},
			bson.M{"$unset": bson.M{"issue.claimleaseuntil": ""}}); err != nil {
			t.Fatal(err)
		}
	})
	mk("still-leased", func(id string) { strip(id, bson.M{"issue.claimleaseuntil": time.Now().UTC().Add(10 * time.Minute)}) })
	mk("parked-out-of-column", func(id string) { strip(id, bson.M{"issue.state": native.StateAwaitingInput}) })

	coord := boardmongo.NewCoordinator(db)
	cands, err := coord.ListUnleasedClaims(ctx, time.Now().UTC(), native.StateInProgress, 100)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, c := range cands {
		if c.Claim.IssueID == rep.ID {
			found = true
		}
		if label, bad := excluded[c.Claim.IssueID]; bad {
			t.Fatalf("the listing returned a card its contract excludes: %s", label)
		}
	}
	if !found {
		t.Fatalf("STARVED: the one repairable card is not in the batch of %d — the conserved population fills the cap and the sweep never sees it", len(cands))
	}
}

// The give-up stamp is the field that decides whether a failed pipeline
// shows in Needs attention. Naming it unconditionally in replace() wrote
// the snapshot's (usually nil) stamp on every write — a concurrent bot
// comment erased a dispatcher give-up 103/120, or resurrected a cleared
// one 108/120. It rides along only when the expiry actually fired.
func TestScopedReplace_BotWriteNeverTouchesTheGiveUpStamp(t *testing.T) {
	s := scopedMongo(t)
	const N = 120
	lost, resurrected := 0, 0
	for i := 0; i < N; i++ {
		iss, err := s.Create(native.Issue{Title: "gaveup race", State: native.StateInProgress})
		if err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			_ = s.SetGaveUp(iss.ID, &native.GiveUp{RunID: "r1", State: native.StateInProgress, Attempts: 3})
		}()
		go func() {
			defer wg.Done()
			asg := "someone"
			_, _ = s.Update(iss.ID, native.Patch{Assignee: &asg})
		}()
		wg.Wait()
		if got, _ := s.Get(iss.ID); got.GaveUp == nil {
			lost++
		}
	}
	if lost > 0 {
		t.Fatalf("%d/%d give-up stamps LOST to a concurrent bot write — the failed pipeline files as Closed instead of Needs attention", lost, N)
	}
	for i := 0; i < N; i++ {
		iss, err := s.Create(native.Issue{Title: "gaveup clear race", State: native.StateInProgress})
		if err != nil {
			t.Fatal(err)
		}
		if err := s.SetGaveUp(iss.ID, &native.GiveUp{RunID: "r1", State: native.StateInProgress, Attempts: 3}); err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); _ = s.SetGaveUp(iss.ID, nil) }()
		go func() {
			defer wg.Done()
			asg := "someone"
			_, _ = s.Update(iss.ID, native.Patch{Assignee: &asg})
		}()
		wg.Wait()
		if got, _ := s.Get(iss.ID); got.GaveUp != nil {
			resurrected++
		}
	}
	if resurrected > 0 {
		t.Fatalf("%d/%d CLEARED give-up stamps resurrected by a concurrent bot write — the acknowledged card is stuck in Needs attention", resurrected, N)
	}
}

// Two concurrent comments both land: the append is an atomic
// $concatArrays pipeline, not a read-modify-$set of the whole array
// (which lost one of every concurrent pair, 60/60 — a /command posted
// while a bot wrote its trace vanished with no error).
func TestScopedReplace_ConcurrentCommentsBothLand(t *testing.T) {
	s := scopedMongo(t)
	const N = 60
	lost := 0
	for i := 0; i < N; i++ {
		iss, err := s.Create(native.Issue{Title: "comment race", State: native.StateInProgress})
		if err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); _, _, _ = s.AddComment(iss.ID, "op", "/billy") }()
		go func() { defer wg.Done(); _, _, _ = s.AddComment(iss.ID, "bot", "trace") }()
		wg.Wait()
		if got, _ := s.Get(iss.ID); len(got.Comments) != 2 {
			lost++
		}
	}
	if lost > 0 {
		t.Fatalf("a comment was LOST in %d/%d concurrent pairs", lost, N)
	}
}

// The claim session cancels its renewal context at Stop(); a renew that
// ignored the cancel held Stop hostage for the full op timeout — on the
// dispatcher actor locally, inside the drain's WaitGroup in cloud. The
// store's renew must honour the CALLER's context.
func TestRenewClaimCtx_HonoursTheCallersContext(t *testing.T) {
	s := scopedMongo(t)
	iss, err := s.Create(native.Issue{Title: "renewed", State: native.StateInProgress})
	if err != nil {
		t.Fatal(err)
	}
	tok, err := s.Claim(iss.ID, "worker-1")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	err = s.RenewClaimCtx(ctx, iss.ID, tok)
	if err == nil {
		t.Fatal("a renew on a CANCELLED context reported success — the session's Stop() cancel reaches nothing")
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("the cancelled renew still took %s — Stop() is hostage to it", time.Since(start))
	}
}

// A comment body is FREE TEXT evaluated nowhere: "$"-leading agent
// output ("$ go test ./...", "$issue.labels") must persist verbatim.
// Without $literal the pipeline $set read it as a field path — an empty
// body, a hard write error, or a poisoned document that broke decoding
// of the ENTIRE tenant listing (List returned 0 cards and an error).
func TestAddComment_DollarBodiesAreDataNotExpressions(t *testing.T) {
	s := scopedMongo(t)
	iss, err := s.Create(native.Issue{Title: "victim", State: native.StateInProgress})
	if err != nil {
		t.Fatal(err)
	}
	witness, err := s.Create(native.Issue{Title: "witness", State: native.StateReady})
	if err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{"$ go test ./...", "$nope please review", "$issue.labels"} {
		if _, _, err := s.AddComment(iss.ID, "agent", body); err != nil {
			t.Fatalf("AddComment(%q): %v — free text hard-failed the write", body, err)
		}
	}
	got, err := s.Get(iss.ID)
	if err != nil {
		t.Fatalf("the commented card is no longer decodable: %v", err)
	}
	if len(got.Comments) != 3 {
		t.Fatalf("want 3 comments, got %d", len(got.Comments))
	}
	for i, want := range []string{"$ go test ./...", "$nope please review", "$issue.labels"} {
		if got.Comments[i].Body != want {
			t.Fatalf("comment %d body = %q, want %q — evaluated as an expression", i, got.Comments[i].Body, want)
		}
	}
	// The blast radius that made this CRITICAL: one poisoned comment
	// broke the whole tenant's listing.
	all, err := s.List(native.ListFilter{})
	if err != nil {
		t.Fatalf("tenant listing broken: %v", err)
	}
	seen := false
	for _, i := range all {
		if i.ID == witness.ID {
			seen = true
		}
	}
	if !seen {
		t.Fatal("the witness card vanished from the tenant listing")
	}
}
