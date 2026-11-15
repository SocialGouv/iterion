package intake

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"sync"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/SocialGouv/iterion/pkg/internal/mongotest"
)

// The one contract every twin owes. Each row runs against a fresh store; the
// Mongo twin runs only with ITERION_TEST_MONGO_URI (the CI mongo-conformance
// job supplies it).

func TestMemoryStoreConformance(t *testing.T) {
	runConformance(t, func(t *testing.T) Store { return NewMemoryStore() })
}

func TestFileStoreConformance(t *testing.T) {
	runConformance(t, func(t *testing.T) Store {
		return NewFileStore(filepath.Join(t.TempDir(), "connector", "intake.json"))
	})
}

func TestMongoStoreConformance(t *testing.T) {
	runConformance(t, func(t *testing.T) Store { return NewMongoStore(mongoDB(t)) })
}

// mongoDB opens a fresh database for one row, dropped at its end.
func mongoDB(t *testing.T) *mongo.Database {
	t.Helper()
	uri := os.Getenv("ITERION_TEST_MONGO_URI")
	if uri == "" {
		t.Skip("ITERION_TEST_MONGO_URI not set; skipping the Mongo intake store")
	}
	client, err := mongo.Connect(options.Client().ApplyURI(uri))
	if err != nil {
		t.Fatalf("mongo connect: %v", err)
	}
	nonce := make([]byte, 6)
	if _, err := rand.Read(nonce); err != nil {
		t.Fatal(err)
	}
	db := client.Database("iterion_intake_" + hex.EncodeToString(nonce))
	t.Cleanup(func() {
		ctx, cancel := mongotest.TeardownCtx()
		defer cancel()
		_ = db.Drop(ctx)
		_ = client.Disconnect(ctx)
	})
	ctx, cancel := mongotest.Ctx(t)
	defer cancel()
	if err := EnsureSchema(ctx, db); err != nil {
		t.Fatalf("EnsureSchema: %v", err)
	}
	return db
}

var t0 = time.Date(2026, 11, 14, 12, 0, 0, 123456789, time.UTC)

const ttl = time.Minute

func delivery(tenant, ingress, dedup string, at time.Time) Intake {
	return Intake{
		TenantID: tenant, ConnectionID: "conn-" + tenant, IngressID: ingress, TriggerID: "issue.updated",
		DedupKey: dedup, Event: []byte(`{"outputs":{"subject":"PRJ-1"},"payload":{"$where":"x","a.b":1}}`),
		ReceivedAt: at,
	}
}

func mustAccept(t *testing.T, s Store, in Intake) Intake {
	t.Helper()
	got, created, err := s.Accept(context.Background(), in)
	if err != nil || !created {
		t.Fatalf("Accept(%s) = (%v, %v), want a new row", in.DedupKey, created, err)
	}
	return got
}

func pending(in Intake, sub string, at time.Time) Pair {
	return Pair{TenantID: in.TenantID, IntakeID: in.ID, SubscriptionID: sub, ReceivedAt: in.ReceivedAt, State: PairPending, CreatedAt: at}
}

func ids[T any](rows []T, id func(T) string) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, id(r))
	}
	return out
}

func intakeIDs(rows []Intake) []string { return ids(rows, func(in Intake) string { return in.ID }) }
func pairSubs(rows []Pair) []string {
	return ids(rows, func(p Pair) string { return p.SubscriptionID })
}

func runConformance(t *testing.T, newStore func(*testing.T) Store) {
	ctx := context.Background()

	t.Run("accept writes a delivery once and keeps what the row became", func(t *testing.T) {
		s := newStore(t)
		in := delivery("t1", "ing-1", "digest-a", t0)
		first := mustAccept(t, s, in)
		if first.ID != IntakeID("t1", "ingress:ing-1", "digest-a") || first.State != IntakePending || first.Generation != 0 {
			t.Fatalf("accepted row = %+v, want the derived id, pending, generation 0", first)
		}
		if !first.ReceivedAt.Equal(stamp(t0)) || first.ReceivedAt.Nanosecond()%int(time.Millisecond) != 0 {
			t.Fatalf("received_at = %s, want %s at millisecond precision", first.ReceivedAt, stamp(t0))
		}
		if string(first.Event) != string(in.Event) {
			t.Fatalf("event = %s, want the bytes accepted, keys a document field cannot hold included", first.Event)
		}
		claimed, err := s.ClaimIntakes(ctx, "w1", t0, ttl, 10)
		if err != nil || len(claimed) != 1 {
			t.Fatalf("claim = (%d, %v)", len(claimed), err)
		}
		// A redelivery — the same authenticated material — meets the row
		// as it now stands and writes nothing.
		again, created, err := s.Accept(ctx, delivery("t1", "ing-1", "digest-a", t0.Add(time.Hour)))
		if err != nil || created {
			t.Fatalf("redelivery = (created %v, %v), want the existing row", created, err)
		}
		if again.State != IntakeClaimed || again.Generation != 1 || !again.ReceivedAt.Equal(first.ReceivedAt) {
			t.Fatalf("redelivery answered %+v, want the claimed row untouched", again)
		}
	})

	t.Run("the dedup identity is per tenant and per source", func(t *testing.T) {
		s := newStore(t)
		a := mustAccept(t, s, delivery("t1", "ing-1", "same", t0))
		b := mustAccept(t, s, delivery("t2", "ing-1", "same", t0))
		polled := delivery("t1", "", "same", t0)
		polled.PollerID = "ing-1"
		c := mustAccept(t, s, polled)
		if a.ID == b.ID || a.ID == c.ID || b.ID == c.ID {
			t.Fatalf("ids %s %s %s collide across tenants or sources", a.ID, b.ID, c.ID)
		}
	})

	t.Run("accept refuses a row no lane could have written", func(t *testing.T) {
		for name, mutate := range map[string]func(*Intake){
			"no tenant":      func(in *Intake) { in.TenantID = "" },
			"no connection":  func(in *Intake) { in.ConnectionID = "" },
			"two sources":    func(in *Intake) { in.PollerID = "p" },
			"no source":      func(in *Intake) { in.IngressID = "" },
			"no trigger":     func(in *Intake) { in.TriggerID = "" },
			"no dedup key":   func(in *Intake) { in.DedupKey = "" },
			"no instant":     func(in *Intake) { in.ReceivedAt = time.Time{} },
			"event not JSON": func(in *Intake) { in.Event = []byte("{") },
		} {
			t.Run(name, func(t *testing.T) {
				s := newStore(t)
				in := delivery("t1", "ing-1", "d", t0)
				mutate(&in)
				if _, _, err := s.Accept(ctx, in); !errors.Is(err, ErrInvalid) {
					t.Fatalf("Accept = %v, want ErrInvalid", err)
				}
				if rows, _ := s.ListIntakes(ctx, "t1", "", 0); len(rows) != 0 {
					t.Fatalf("a refused row was written: %v", intakeIDs(rows))
				}
			})
		}
	})

	t.Run("reads are tenant-scoped", func(t *testing.T) {
		s := newStore(t)
		in := mustAccept(t, s, delivery("t1", "ing-1", "d", t0))
		if _, err := s.GetIntake(ctx, "t2", in.ID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("GetIntake from another tenant = %v, want ErrNotFound", err)
		}
		if _, err := s.ReplayIntake(ctx, "t2", in.ID, t0); !errors.Is(err, ErrNotFound) {
			t.Fatalf("ReplayIntake from another tenant = %v, want ErrNotFound", err)
		}
		if rows, _ := s.ListIntakes(ctx, "t2", "", 0); len(rows) != 0 {
			t.Fatalf("another tenant lists %v", intakeIDs(rows))
		}
		if err := s.WritePairs(ctx, []Pair{pending(in, "s1", t0)}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.GetPair(ctx, "t2", PairID(in.ID, "s1")); !errors.Is(err, ErrNotFound) {
			t.Fatalf("GetPair from another tenant = %v, want ErrNotFound", err)
		}
		if rows, _ := s.ListPairs(ctx, "t2", PairQuery{}, 0); len(rows) != 0 {
			t.Fatalf("another tenant lists pairs %v", pairSubs(rows))
		}
	})

	t.Run("a claim takes due rows in NotBefore order, retries and expired claims alike", func(t *testing.T) {
		s := newStore(t)
		late := mustAccept(t, s, delivery("t1", "ing-1", "late", t0))
		early := mustAccept(t, s, delivery("t1", "ing-1", "early", t0))
		backoff := mustAccept(t, s, delivery("t1", "ing-1", "backoff", t0))
		// late: claimed and left to expire; backoff: retried into the future.
		got, err := s.ClaimIntakes(ctx, "w0", t0, ttl, 3)
		if err != nil || len(got) != 3 {
			t.Fatalf("seed claim = (%d, %v)", len(got), err)
		}
		gen := map[string]int64{}
		for _, in := range got {
			gen[in.ID] = in.Generation
		}
		if err := s.RetryIntake(ctx, early.ID, gen[early.ID], 1, t0, "e"); err != nil {
			t.Fatal(err)
		}
		if err := s.RetryIntake(ctx, backoff.ID, gen[backoff.ID], 1, t0.Add(time.Hour), "b"); err != nil {
			t.Fatal(err)
		}
		// Before late's claim expires only early is due.
		got, err = s.ClaimIntakes(ctx, "w1", t0.Add(ttl/2), ttl, 10)
		if err != nil || !reflect.DeepEqual(intakeIDs(got), []string{early.ID}) {
			t.Fatalf("claim at half the ttl = (%v, %v), want only the retried row", intakeIDs(got), err)
		}
		if got[0].Owner != "w1" || got[0].Attempts != 1 || got[0].Generation != gen[early.ID]+1 || !got[0].NotBefore.Equal(stamp(t0.Add(ttl/2+ttl))) {
			t.Fatalf("claimed row = %+v, want owner w1, attempts kept, next generation, expiry now+ttl", got[0])
		}
		// Past late's expiry: late is reclaimed and the reclaim spends an
		// attempt; backoff is still waiting.
		got, err = s.ClaimIntakes(ctx, "w2", t0.Add(ttl+time.Second), ttl, 10)
		if err != nil || !reflect.DeepEqual(intakeIDs(got), []string{late.ID}) {
			t.Fatalf("claim past the expiry = (%v, %v), want the expired claim", intakeIDs(got), err)
		}
		if got[0].Attempts != 1 || got[0].Generation != gen[late.ID]+1 || got[0].Owner != "w2" {
			t.Fatalf("reclaimed row = %+v, want an attempt spent and the next generation", got[0])
		}
	})

	t.Run("a claim honours its limit and orders by due instant then id", func(t *testing.T) {
		s := newStore(t)
		var all []string
		for i := 0; i < 5; i++ {
			all = append(all, mustAccept(t, s, delivery("t1", "ing-1", fmt.Sprintf("d%d", i), t0)).ID)
		}
		sort.Strings(all)
		got, err := s.ClaimIntakes(ctx, "w", t0, ttl, 2)
		if err != nil || !reflect.DeepEqual(intakeIDs(got), all[:2]) {
			t.Fatalf("first batch = (%v, %v), want %v", intakeIDs(got), err, all[:2])
		}
		got, err = s.ClaimIntakes(ctx, "w", t0, ttl, 10)
		if err != nil || !reflect.DeepEqual(intakeIDs(got), all[2:]) {
			t.Fatalf("second batch = (%v, %v), want %v", intakeIDs(got), err, all[2:])
		}
		if _, err := s.ClaimIntakes(ctx, "", t0, ttl, 1); !errors.Is(err, ErrInvalid) {
			t.Fatalf("claim without an owner = %v, want ErrInvalid", err)
		}
		if _, err := s.ClaimIntakes(ctx, "w", t0, 0, 1); !errors.Is(err, ErrInvalid) {
			t.Fatalf("claim without a ttl = %v, want ErrInvalid", err)
		}
		if _, err := s.ClaimIntakes(ctx, "w", t0, ttl, 0); !errors.Is(err, ErrInvalid) {
			t.Fatalf("claim without a limit = %v, want ErrInvalid", err)
		}
	})

	t.Run("a write under a lost claim changes nothing", func(t *testing.T) {
		s := newStore(t)
		in := mustAccept(t, s, delivery("t1", "ing-1", "d", t0))
		first, _ := s.ClaimIntakes(ctx, "w1", t0, ttl, 1)
		second, _ := s.ClaimIntakes(ctx, "w2", t0.Add(2*ttl), ttl, 1)
		if len(first) != 1 || len(second) != 1 || second[0].Generation != first[0].Generation+1 {
			t.Fatalf("claims = %v then %v, want a reclaim at the next generation", first, second)
		}
		stale := first[0].Generation
		for name, write := range map[string]func() error{
			"matched": func() error { return s.MarkIntakeMatched(ctx, in.ID, stale, t0) },
			"retry":   func() error { return s.RetryIntake(ctx, in.ID, stale, 9, t0, "late") },
			"bury":    func() error { return s.BuryIntake(ctx, in.ID, stale, 9, "late") },
		} {
			if err := write(); !errors.Is(err, ErrClaimLost) {
				t.Fatalf("%s under the lost claim = %v, want ErrClaimLost", name, err)
			}
		}
		cur, err := s.GetIntake(ctx, "t1", in.ID)
		if err != nil || cur.State != IntakeClaimed || cur.Owner != "w2" || cur.Generation != second[0].Generation || cur.Attempts != 1 || cur.LastError != "" {
			t.Fatalf("row after the late writes = %+v (%v), want w2's claim untouched", cur, err)
		}
		if err := s.MarkIntakeMatched(ctx, in.ID, cur.Generation, t0.Add(3*ttl)); err != nil {
			t.Fatalf("the current claim's write: %v", err)
		}
		if err := s.MarkIntakeMatched(ctx, in.ID, cur.Generation, t0.Add(3*ttl)); !errors.Is(err, ErrClaimLost) {
			t.Fatalf("a write on a matched row = %v, want ErrClaimLost", err)
		}
		if err := s.MarkIntakeMatched(ctx, "in-missing", 1, t0); !errors.Is(err, ErrNotFound) {
			t.Fatalf("a write on a missing row = %v, want ErrNotFound", err)
		}
	})

	t.Run("retry, dead letter and replay", func(t *testing.T) {
		s := newStore(t)
		in := mustAccept(t, s, delivery("t1", "ing-1", "d", t0))
		c, _ := s.ClaimIntakes(ctx, "w", t0, ttl, 1)
		if err := s.RetryIntake(ctx, in.ID, c[0].Generation, 1, t0.Add(time.Minute), "boom"); err != nil {
			t.Fatal(err)
		}
		if got, _ := s.ClaimIntakes(ctx, "w", t0.Add(30*time.Second), ttl, 1); len(got) != 0 {
			t.Fatalf("a row in backoff was claimed: %v", intakeIDs(got))
		}
		c, _ = s.ClaimIntakes(ctx, "w", t0.Add(time.Minute), ttl, 1)
		if len(c) != 1 || c[0].Attempts != 1 || c[0].LastError != "boom" {
			t.Fatalf("claim after the backoff = %+v", c)
		}
		if _, err := s.ReplayIntake(ctx, "t1", in.ID, t0); !errors.Is(err, ErrNotReplayable) {
			t.Fatalf("replay of a claimed row = %v, want ErrNotReplayable", err)
		}
		if err := s.BuryIntake(ctx, in.ID, c[0].Generation, 2, "spent"); err != nil {
			t.Fatal(err)
		}
		dead, _ := s.ListIntakes(ctx, "t1", IntakeDead, 0)
		if len(dead) != 1 || dead[0].LastError != "spent" || dead[0].Owner != "" || dead[0].Attempts != 2 {
			t.Fatalf("dead letters = %+v", dead)
		}
		if got, _ := s.ClaimIntakes(ctx, "w", t0.Add(time.Hour), ttl, 1); len(got) != 0 {
			t.Fatalf("a dead letter was claimed: %v", intakeIDs(got))
		}
		replayed, err := s.ReplayIntake(ctx, "t1", in.ID, t0.Add(2*time.Hour))
		if err != nil || replayed.State != IntakePending || replayed.Attempts != 0 || replayed.Generation != c[0].Generation+1 || replayed.LastError != "" || !replayed.NotBefore.Equal(stamp(t0.Add(2*time.Hour))) {
			t.Fatalf("replay = %+v (%v), want pending, attempts reset, next generation, due at the replay", replayed, err)
		}
		if err := s.BuryIntake(ctx, in.ID, c[0].Generation, 2, "late"); !errors.Is(err, ErrClaimLost) {
			t.Fatalf("a write under the pre-replay claim = %v, want ErrClaimLost", err)
		}
		if got, _ := s.ClaimIntakes(ctx, "w", t0.Add(2*time.Hour), ttl, 1); len(got) != 1 {
			t.Fatal("the replayed row is not claimable")
		}
	})

	t.Run("the backlog counts a tenant's pending and claimed intakes", func(t *testing.T) {
		s := newStore(t)
		for _, d := range []string{"a", "b", "c", "d"} {
			mustAccept(t, s, delivery("t1", "ing-1", d, t0))
		}
		c, _ := s.ClaimIntakes(ctx, "w", t0, ttl, 2)
		if err := s.MarkIntakeMatched(ctx, c[0].ID, c[0].Generation, t0); err != nil {
			t.Fatal(err)
		}
		// Another tenant's row, accepted after the claim so the claim
		// (which drains every tenant) cannot take it.
		mustAccept(t, s, delivery("t2", "ing-1", "x", t0))
		// t1: two pending, one claimed, one matched.
		if n, err := s.CountBacklog(ctx, "t1"); err != nil || n != 3 {
			t.Fatalf("t1 backlog = (%d, %v), want 3", n, err)
		}
		if n, err := s.CountBacklog(ctx, "t3"); err != nil || n != 0 {
			t.Fatalf("an empty tenant's backlog = (%d, %v), want 0", n, err)
		}
	})

	t.Run("lists newest first, filtered by state, bounded", func(t *testing.T) {
		s := newStore(t)
		a := mustAccept(t, s, delivery("t1", "ing-1", "a", t0))
		b := mustAccept(t, s, delivery("t1", "ing-1", "b", t0.Add(time.Second)))
		got, err := s.ListIntakes(ctx, "t1", "", 0)
		if err != nil || !reflect.DeepEqual(intakeIDs(got), []string{b.ID, a.ID}) {
			t.Fatalf("list = (%v, %v), want newest first", intakeIDs(got), err)
		}
		if got, _ := s.ListIntakes(ctx, "t1", "", 1); !reflect.DeepEqual(intakeIDs(got), []string{b.ID}) {
			t.Fatalf("bounded list = %v", intakeIDs(got))
		}
		if got, _ := s.ListIntakes(ctx, "t1", IntakeMatched, 0); len(got) != 0 {
			t.Fatalf("state filter = %v", intakeIDs(got))
		}
	})

	t.Run("pairs are written once whatever a later writer says", func(t *testing.T) {
		s := newStore(t)
		in := mustAccept(t, s, delivery("t1", "ing-1", "d", t0))
		first := []Pair{pending(in, "s2", t0), pending(in, "s1", t0)}
		if err := s.WritePairs(ctx, first); err != nil {
			t.Fatal(err)
		}
		// A matcher that lost its claim and judged s1 otherwise: the record
		// stands as first written.
		other := pending(in, "s1", t0.Add(time.Minute))
		other.State, other.Reason = PairNotMatched, "late opinion"
		if err := s.WritePairs(ctx, []Pair{other, pending(in, "s3", t0)}); err != nil {
			t.Fatal(err)
		}
		got, err := s.ListPairs(ctx, "t1", PairQuery{IntakeID: in.ID}, 0)
		if err != nil || !reflect.DeepEqual(pairSubs(got), []string{"s1", "s2", "s3"}) {
			t.Fatalf("pairs = (%v, %v)", pairSubs(got), err)
		}
		if got[0].State != PairPending || got[0].ID != PairID(in.ID, "s1") || !got[0].CreatedAt.Equal(stamp(t0)) {
			t.Fatalf("s1's pair = %+v, want the first write", got[0])
		}
		if got, _ := s.ListPairs(ctx, "t1", PairQuery{State: PairNotMatched}, 0); len(got) != 0 {
			t.Fatalf("the late opinion was written: %v", pairSubs(got))
		}
	})

	t.Run("write pairs refuses a pair no matcher could have written", func(t *testing.T) {
		s := newStore(t)
		in := mustAccept(t, s, delivery("t1", "ing-1", "d", t0))
		for name, mutate := range map[string]func(*Pair){
			"no tenant":                func(p *Pair) { p.TenantID = "" },
			"no intake":                func(p *Pair) { p.IntakeID = "" },
			"no subscription":          func(p *Pair) { p.SubscriptionID = "" },
			"no creation instant":      func(p *Pair) { p.CreatedAt = time.Time{} },
			"born launched":            func(p *Pair) { p.State = PairLaunched },
			"not matched without why":  func(p *Pair) { p.State = PairNotMatched },
			"refused without a reason": func(p *Pair) { p.State = PairRefused },
		} {
			t.Run(name, func(t *testing.T) {
				p := pending(in, "s1", t0)
				mutate(&p)
				if err := s.WritePairs(ctx, []Pair{p}); !errors.Is(err, ErrInvalid) {
					t.Fatalf("WritePairs = %v, want ErrInvalid", err)
				}
			})
		}
		if got, _ := s.ListPairs(ctx, "t1", PairQuery{}, 0); len(got) != 0 {
			t.Fatalf("a refused pair was written: %v", pairSubs(got))
		}
	})

	t.Run("a pair's launch, retries and epochs", func(t *testing.T) {
		s := newStore(t)
		in := mustAccept(t, s, delivery("t1", "ing-1", "d", t0))
		if err := s.WritePairs(ctx, []Pair{pending(in, "s1", t0)}); err != nil {
			t.Fatal(err)
		}
		c, err := s.ClaimPairs(ctx, "w", t0, ttl, 5)
		if err != nil || len(c) != 1 || c[0].State != PairClaimed || c[0].Generation != 1 {
			t.Fatalf("claim = (%+v, %v)", c, err)
		}
		if err := s.RetryPair(ctx, c[0].ID, c[0].Generation, PairRetry{Attempts: 1, NotBefore: t0.Add(time.Second), LastError: "spent", Epoch: 1}); err != nil {
			t.Fatal(err)
		}
		c, _ = s.ClaimPairs(ctx, "w", t0.Add(time.Second), ttl, 5)
		if len(c) != 1 || c[0].Epoch != 1 || c[0].Attempts != 1 || c[0].LastError != "spent" {
			t.Fatalf("claim after the retry = %+v, want epoch 1, one attempt", c)
		}
		if err := s.RetryPair(ctx, c[0].ID, c[0].Generation, PairRetry{Attempts: 2, NotBefore: t0, Epoch: 0}); !errors.Is(err, ErrInvalid) {
			t.Fatalf("a retry moving the epoch back = %v, want ErrInvalid", err)
		}
		if cur, _ := s.GetPair(ctx, "t1", c[0].ID); cur.State != PairClaimed || cur.Epoch != 1 || cur.Attempts != 1 {
			t.Fatalf("pair after the refused retry = %+v, want it untouched", cur)
		}
		if err := s.MarkPairLaunched(ctx, c[0].ID, c[0].Generation, "", t0); !errors.Is(err, ErrInvalid) {
			t.Fatalf("launched without a run = %v, want ErrInvalid", err)
		}
		if err := s.MarkPairLaunched(ctx, c[0].ID, c[0].Generation-1, "run-x", t0); !errors.Is(err, ErrClaimLost) {
			t.Fatalf("launched under an older generation = %v, want ErrClaimLost", err)
		}
		at := t0.Add(2 * time.Second)
		if err := s.MarkPairLaunched(ctx, c[0].ID, c[0].Generation, "run-1", at); err != nil {
			t.Fatal(err)
		}
		cur, _ := s.GetPair(ctx, "t1", c[0].ID)
		if cur.State != PairLaunched || cur.RunID != "run-1" || cur.LaunchedAt == nil || !cur.LaunchedAt.Equal(stamp(at)) || cur.Owner != "" || cur.LastError != "" {
			t.Fatalf("launched pair = %+v", cur)
		}
		if _, err := s.ReplayPair(ctx, "t1", cur.ID, at); !errors.Is(err, ErrNotReplayable) {
			t.Fatalf("replay of a launched pair = %v, want ErrNotReplayable", err)
		}
		if got, _ := s.ClaimPairs(ctx, "w", t0.Add(time.Hour), ttl, 5); len(got) != 0 {
			t.Fatalf("a launched pair was claimed")
		}
	})

	t.Run("a dead pair replays under its key, a launch-refused one under the next, a match-refused one not at all", func(t *testing.T) {
		s := newStore(t)
		in := mustAccept(t, s, delivery("t1", "ing-1", "d", t0))
		nm := pending(in, "s0", t0)
		nm.State, nm.Reason = PairNotMatched, "filters"
		mr := pending(in, "s3", t0)
		mr.State, mr.Reason = PairRefused, "expression past its budget"
		if err := s.WritePairs(ctx, []Pair{pending(in, "s1", t0), pending(in, "s2", t0), nm, mr}); err != nil {
			t.Fatal(err)
		}
		c, _ := s.ClaimPairs(ctx, "w", t0, ttl, 5)
		if len(c) != 2 {
			t.Fatalf("claimed %d pairs, want the two pending ones", len(c))
		}
		byID := map[string]Pair{}
		for _, p := range c {
			byID[p.SubscriptionID] = p
		}
		if err := s.RefusePair(ctx, byID["s1"].ID, byID["s1"].Generation, "budget"); err != nil {
			t.Fatal(err)
		}
		if err := s.BuryPair(ctx, byID["s2"].ID, byID["s2"].Generation, 3, "spent"); err != nil {
			t.Fatal(err)
		}
		if err := s.RefusePair(ctx, byID["s1"].ID, byID["s1"].Generation, "again"); !errors.Is(err, ErrClaimLost) {
			t.Fatalf("a second write on a refused pair = %v, want ErrClaimLost", err)
		}
		if dead, _ := s.GetPair(ctx, "t1", byID["s2"].ID); dead.State != PairDead || dead.Attempts != 3 {
			t.Fatalf("buried pair = %+v, want dead with the attempts it spent", dead)
		}
		// The epoch a replay submits under: the next one after a refusal, the
		// same one after a dead letter — whose last attempt may have been
		// handed on, and whose key is then answered by that run.
		for sub, epoch := range map[string]int{"s1": byID["s1"].Epoch + 1, "s2": byID["s2"].Epoch} {
			p := byID[sub]
			got, err := s.ReplayPair(ctx, "t1", p.ID, t0.Add(time.Hour))
			if err != nil || got.State != PairPending || got.Epoch != epoch || got.Attempts != 0 || got.Generation != p.Generation+1 || got.Reason != "" || got.LastError != "" || !got.NotBefore.Equal(stamp(t0.Add(time.Hour))) {
				t.Fatalf("replay of %s = %+v (%v), want pending in epoch %d, due at the replay", sub, got, err, epoch)
			}
		}
		if _, err := s.ReplayPair(ctx, "t1", PairID(in.ID, "s0"), t0); !errors.Is(err, ErrNotReplayable) {
			t.Fatalf("replay of a not-matched pair = %v, want ErrNotReplayable", err)
		}
		// Refused by matching: no filter ever said yes, so no replay may turn
		// it into a launch.
		if _, err := s.ReplayPair(ctx, "t1", PairID(in.ID, "s3"), t0); !errors.Is(err, ErrNotReplayable) {
			t.Fatalf("replay of a match-refused pair = %v, want ErrNotReplayable", err)
		}
		if cur, _ := s.GetPair(ctx, "t1", PairID(in.ID, "s3")); cur.State != PairRefused || !cur.MatchRefusal {
			t.Fatalf("match-refused pair after the refused replay = %+v", cur)
		}
		if _, err := s.ReplayPair(ctx, "t2", byID["s1"].ID, t0); !errors.Is(err, ErrNotFound) {
			t.Fatalf("replay from another tenant = %v, want ErrNotFound", err)
		}
	})

	t.Run("a match-refused pair is re-judged, and only such a pair", func(t *testing.T) {
		s := newStore(t)
		in := mustAccept(t, s, delivery("t1", "ing-1", "d", t0))
		var pairs []Pair
		for _, sub := range []string{"m", "n", "r"} {
			p := pending(in, sub, t0)
			p.State, p.Reason = PairRefused, "expression past its budget"
			pairs = append(pairs, p)
		}
		pairs = append(pairs, pending(in, "p", t0))
		if err := s.WritePairs(ctx, pairs); err != nil {
			t.Fatal(err)
		}
		at := t0.Add(time.Hour)
		before, _ := s.GetPair(ctx, "t1", PairID(in.ID, "m"))
		got, err := s.RejudgePair(ctx, "t1", before.ID, Verdict{SubscriptionID: "m", Matched: true}, at)
		if err != nil || got.State != PairPending || got.MatchRefusal || got.Reason != "" || got.Attempts != 0 ||
			got.Generation != before.Generation+1 || !got.NotBefore.Equal(stamp(at)) {
			t.Fatalf("a matching re-judgment = %+v (%v), want pending, its refusal cleared, due now", got, err)
		}
		got, err = s.RejudgePair(ctx, "t1", PairID(in.ID, "n"), Verdict{SubscriptionID: "n", Reason: "filter room: no"}, at)
		if err != nil || got.State != PairNotMatched || got.MatchRefusal || got.Reason != "filter room: no" {
			t.Fatalf("a non-matching re-judgment = %+v (%v), want not_matched with its reason", got, err)
		}
		got, err = s.RejudgePair(ctx, "t1", PairID(in.ID, "r"), Verdict{SubscriptionID: "r", Refusal: "still past its budget"}, at)
		if err != nil || got.State != PairRefused || !got.MatchRefusal || got.Reason != "still past its budget" {
			t.Fatalf("a refusing re-judgment = %+v (%v), want refused by matching, with the new reason", got, err)
		}
		// Only a match-refused pair: a pending one, a re-judged one, a
		// foreign tenant's are not.
		for _, id := range []string{PairID(in.ID, "p"), PairID(in.ID, "m")} {
			if _, err := s.RejudgePair(ctx, "t1", id, Verdict{Matched: true}, at); !errors.Is(err, ErrNotReplayable) {
				t.Fatalf("re-judging %s = %v, want ErrNotReplayable", id, err)
			}
		}
		if _, err := s.RejudgePair(ctx, "t2", PairID(in.ID, "r"), Verdict{Matched: true}, at); !errors.Is(err, ErrNotFound) {
			t.Fatalf("re-judging from another tenant = %v, want ErrNotFound", err)
		}
	})

	t.Run("claims follow arrival: a newer row never passes a retry or an expired claim", func(t *testing.T) {
		// Steady load: each tick one delivery arrives and one row is claimed.
		// A row released once, and a claim a crashed worker left, must be
		// taken again before the rows that arrived after them.
		s := newStore(t)
		released := mustAccept(t, s, delivery("t1", "ing-1", "released", t0))
		c, _ := s.ClaimIntakes(ctx, "w", t0, ttl, 1)
		if err := s.RetryIntake(ctx, c[0].ID, c[0].Generation, c[0].Attempts, t0.Add(ttl), "released unattempted"); err != nil {
			t.Fatal(err)
		}
		crashed := mustAccept(t, s, delivery("t1", "ing-1", "crashed", t0.Add(time.Second)))
		if c, _ := s.ClaimIntakes(ctx, "dead-worker", t0.Add(time.Second), ttl, 1); len(c) != 1 || c[0].ID != crashed.ID {
			t.Fatalf("the crashed worker's claim = %v", intakeIDs(c))
		}
		var order []string
		for i := 0; i < 6; i++ {
			now := t0.Add(ttl + time.Duration(2+i)*time.Second)
			mustAccept(t, s, delivery("t1", "ing-1", fmt.Sprintf("new-%d", i), now))
			got, err := s.ClaimIntakes(ctx, "w", now, ttl, 1)
			if err != nil || len(got) != 1 {
				t.Fatalf("claim = (%v, %v)", intakeIDs(got), err)
			}
			order = append(order, got[0].ID)
			if err := s.MarkIntakeMatched(ctx, got[0].ID, got[0].Generation, now); err != nil {
				t.Fatal(err)
			}
		}
		if order[0] != released.ID || order[1] != crashed.ID {
			t.Fatalf("claim order %v: want the released row, then the expired claim, before any newer delivery", order[:2])
		}
	})

	t.Run("a claim under the store's precision is refused", func(t *testing.T) {
		s := newStore(t)
		mustAccept(t, s, delivery("t1", "ing-1", "d", t0))
		if _, err := s.ClaimIntakes(ctx, "w", t0, 500*time.Microsecond, 5); !errors.Is(err, ErrInvalid) {
			t.Fatalf("a 500µs claim = %v, want ErrInvalid", err)
		}
		if _, err := s.ClaimPairs(ctx, "w", t0, 500*time.Microsecond, 5); !errors.Is(err, ErrInvalid) {
			t.Fatalf("a 500µs pair claim = %v, want ErrInvalid", err)
		}
		got, err := s.ClaimIntakes(ctx, "w", t0, MinClaimTTL, 5)
		if err != nil || len(got) != 1 {
			t.Fatalf("a claim at the precision = (%v, %v), want the row once", intakeIDs(got), err)
		}
	})

	t.Run("the event reads back as it was accepted, in one encoding on every twin", func(t *testing.T) {
		s := newStore(t)
		d := delivery("t1", "ing-1", "d", t0)
		d.Event = []byte(`{"title": "a <b> & c", "n": 1}`)
		acc := mustAccept(t, s, d)
		got, err := s.GetIntake(ctx, "t1", acc.ID)
		if err != nil {
			t.Fatal(err)
		}
		const canonical = `{"title":"a \u003cb\u003e \u0026 c","n":1}`
		if string(acc.Event) != canonical || string(got.Event) != canonical {
			t.Fatalf("event accepted as %s, read back as %s, want %s on both", acc.Event, got.Event, canonical)
		}
	})

	t.Run("two claimers never take one row", func(t *testing.T) {
		s := newStore(t)
		in := mustAccept(t, s, delivery("t1", "ing-1", "d", t0))
		var subs []Pair
		for i := 0; i < 12; i++ {
			subs = append(subs, pending(in, fmt.Sprintf("s%02d", i), t0))
		}
		if err := s.WritePairs(ctx, subs); err != nil {
			t.Fatal(err)
		}
		var mu sync.Mutex
		seen := map[string]int{}
		var wg sync.WaitGroup
		errs := make(chan error, 8)
		for w := 0; w < 4; w++ {
			wg.Add(1)
			go func(owner string) {
				defer wg.Done()
				for {
					got, err := s.ClaimPairs(ctx, owner, t0, ttl, 2)
					if err != nil {
						errs <- err
						return
					}
					if len(got) == 0 {
						return
					}
					mu.Lock()
					for _, p := range got {
						seen[p.SubscriptionID]++
					}
					mu.Unlock()
				}
			}(fmt.Sprintf("w%d", w))
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Fatalf("claim: %v", err)
		}
		if len(seen) != len(subs) {
			t.Fatalf("claimed %d distinct pairs, want %d", len(seen), len(subs))
		}
		for sub, n := range seen {
			if n != 1 {
				t.Fatalf("pair %s claimed %d times", sub, n)
			}
		}
	})

	t.Run("concurrent accepts of one delivery write one row", func(t *testing.T) {
		s := newStore(t)
		var mu sync.Mutex
		created := 0
		var wg sync.WaitGroup
		for i := 0; i < 6; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, c, err := s.Accept(ctx, delivery("t1", "ing-1", "same", t0))
				mu.Lock()
				defer mu.Unlock()
				if err != nil {
					t.Errorf("Accept: %v", err)
				}
				if c {
					created++
				}
			}()
		}
		wg.Wait()
		if created != 1 {
			t.Fatalf("%d accepts reported a new row, want exactly 1", created)
		}
	})
}
