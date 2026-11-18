package poller

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
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
		return NewFileStore(filepath.Join(t.TempDir(), "connector", "pollers.json"))
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
		t.Skip("ITERION_TEST_MONGO_URI not set; skipping the Mongo poller store")
	}
	client, err := mongo.Connect(options.Client().ApplyURI(uri))
	if err != nil {
		t.Fatalf("mongo connect: %v", err)
	}
	nonce := make([]byte, 6)
	if _, err := rand.Read(nonce); err != nil {
		t.Fatal(err)
	}
	db := client.Database("iterion_poller_" + hex.EncodeToString(nonce))
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

var t0 = time.Date(2026, 11, 18, 22, 0, 0, 123456789, time.UTC)

func record(tenant, conn string, query map[string]string) Poller {
	return Poller{TenantID: tenant, ConnectionID: conn, TriggerID: "jira.issue.changed", Query: query, CreatedAt: t0}
}

func mustEnsure(t *testing.T, s Store, p Poller) Poller {
	t.Helper()
	got, created, err := s.Ensure(context.Background(), p)
	if err != nil || !created {
		t.Fatalf("Ensure = (%v, %v), want a new poller", created, err)
	}
	return got
}

func instant(d time.Duration) *time.Time {
	at := t0.Add(d)
	return &at
}

func tick(next time.Duration) Tick {
	return Tick{At: t0, NextTickAt: t0.Add(next)}
}

func runConformance(t *testing.T, newStore func(*testing.T) Store) {
	ctx := context.Background()

	t.Run("ensure writes a poller once, due at once, at generation zero", func(t *testing.T) {
		s := newStore(t)
		p := mustEnsure(t, s, record("t1", "c1", map[string]string{"jql": "project = ENG"}))
		if p.ID != ID("t1", "c1", "jira.issue.changed", map[string]string{"jql": "project = ENG"}) ||
			p.Generation != 0 || p.Primed || p.Sequence != 0 || p.Checkpoint != nil || !p.NextTickAt.Equal(stamp(t0)) {
			t.Fatalf("ensured = %+v, want the derived id, generation 0, unprimed, due at creation", p)
		}
		taken, err := s.Acquire(ctx, p.ID, 0, "w1", t0)
		if err != nil {
			t.Fatal(err)
		}
		again, created, err := s.Ensure(ctx, record("t1", "c1", map[string]string{"jql": "project = ENG"}))
		if err != nil || created || again.Generation != taken.Generation {
			t.Fatalf("re-ensure = (%+v, %v, %v), want the stored poller untouched", again, created, err)
		}
	})

	t.Run("a poller's identity is its tenant, connection, trigger and query", func(t *testing.T) {
		base := ID("t1", "c1", "tr", map[string]string{"a": "1", "b": "2"})
		if ID("t1", "c1", "tr", map[string]string{"b": "2", "a": "1"}) != base {
			t.Fatal("one query typed in another order is another poller")
		}
		for name, other := range map[string]string{
			"another tenant":     ID("t2", "c1", "tr", map[string]string{"a": "1", "b": "2"}),
			"another connection": ID("t1", "c2", "tr", map[string]string{"a": "1", "b": "2"}),
			"another trigger":    ID("t1", "c1", "tx", map[string]string{"a": "1", "b": "2"}),
			"another value":      ID("t1", "c1", "tr", map[string]string{"a": "1", "b": "3"}),
			"a shifted boundary": ID("t1", "c1", "tr", map[string]string{"a": "12", "b": ""}),
		} {
			if other == base {
				t.Errorf("%s shares the poller id", name)
			}
		}
	})

	t.Run("ensure refuses a poller nobody could tick", func(t *testing.T) {
		for name, mutate := range map[string]func(*Poller){
			"no tenant":     func(p *Poller) { p.TenantID = "" },
			"no connection": func(p *Poller) { p.ConnectionID = "" },
			"no trigger":    func(p *Poller) { p.TriggerID = "" },
			"no instant":    func(p *Poller) { p.CreatedAt = time.Time{} },
		} {
			t.Run(name, func(t *testing.T) {
				s := newStore(t)
				p := record("t1", "c1", nil)
				mutate(&p)
				if _, _, err := s.Ensure(ctx, p); !errors.Is(err, ErrInvalid) {
					t.Fatalf("Ensure = %v, want ErrInvalid", err)
				}
				if got, _ := s.List(ctx, "t1"); len(got) != 0 {
					t.Fatalf("a refused poller was written: %+v", got)
				}
			})
		}
	})

	t.Run("reads are tenant-scoped", func(t *testing.T) {
		s := newStore(t)
		p := mustEnsure(t, s, record("t1", "c1", nil))
		mustEnsure(t, s, record("t2", "c1", nil))
		if _, err := s.Get(ctx, "t2", p.ID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("Get from another tenant = %v, want ErrNotFound", err)
		}
		got, err := s.List(ctx, "t1")
		if err != nil || len(got) != 1 || got[0].ID != p.ID {
			t.Fatalf("List(t1) = (%+v, %v), want t1's poller only", got, err)
		}
	})

	t.Run("a lost update is refused: two takers of one generation, one wins", func(t *testing.T) {
		// Both orders: the verdict is the store's, never the call order's.
		for _, order := range [][2]string{{"w1", "w2"}, {"w2", "w1"}} {
			t.Run(order[0]+" first", func(t *testing.T) {
				s := newStore(t)
				p := mustEnsure(t, s, record("t1", "c1", nil))
				won, err := s.Acquire(ctx, p.ID, p.Generation, order[0], t0)
				if err != nil || won.Generation != 1 || won.Holder != order[0] {
					t.Fatalf("first taker = (%+v, %v), want generation 1 held by %s", won, err, order[0])
				}
				if _, err := s.Acquire(ctx, p.ID, p.Generation, order[1], t0); !errors.Is(err, ErrFenced) {
					t.Fatalf("second taker of generation %d = %v, want ErrFenced", p.Generation, err)
				}
				if got, _ := s.Get(ctx, "t1", p.ID); got.Generation != 1 || got.Holder != order[0] {
					t.Fatalf("stored = generation %d holder %q, want the winner's", got.Generation, got.Holder)
				}
			})
		}
	})

	t.Run("two overlapping holders: the older generation's writes are refused", func(t *testing.T) {
		for _, olderFirst := range []bool{true, false} {
			t.Run(fmt.Sprintf("older writes first=%v", olderFirst), func(t *testing.T) {
				s := newStore(t)
				p := mustEnsure(t, s, record("t1", "c1", nil))
				older, _ := s.Acquire(ctx, p.ID, 0, "stalled", t0)
				newer, _ := s.Acquire(ctx, p.ID, older.Generation, "successor", t0)
				olderWrites := func() {
					if _, err := s.Reserve(ctx, p.ID, older.Generation, 3); !errors.Is(err, ErrFenced) {
						t.Errorf("older Reserve = %v, want ErrFenced", err)
					}
					tk := tick(time.Minute)
					tk.Checkpoint = &Checkpoint{Kind: KindTimestamp, At: instant(time.Hour)}
					tk.Primed = true
					if err := s.Complete(ctx, p.ID, older.Generation, tk); !errors.Is(err, ErrFenced) {
						t.Errorf("older Complete = %v, want ErrFenced", err)
					}
				}
				newerWrites := func() {
					first, err := s.Reserve(ctx, p.ID, newer.Generation, 2)
					if err != nil || first != 1 {
						t.Errorf("newer Reserve = (%d, %v), want 1", first, err)
					}
					tk := tick(5 * time.Minute)
					tk.Checkpoint = &Checkpoint{Kind: KindTimestamp, At: instant(time.Minute)}
					if err := s.Complete(ctx, p.ID, newer.Generation, tk); err != nil {
						t.Errorf("newer Complete = %v", err)
					}
				}
				if olderFirst {
					olderWrites()
					newerWrites()
				} else {
					newerWrites()
					olderWrites()
				}
				got, _ := s.Get(ctx, "t1", p.ID)
				if got.Sequence != 2 || got.Primed || got.Checkpoint == nil || !got.Checkpoint.At.Equal(stamp(t0.Add(time.Minute))) {
					t.Fatalf("stored = sequence %d primed %v checkpoint %+v, want only the successor's writes", got.Sequence, got.Primed, got.Checkpoint)
				}
			})
		}
	})

	t.Run("reserve hands out contiguous sequence numbers", func(t *testing.T) {
		s := newStore(t)
		p := mustEnsure(t, s, record("t1", "c1", nil))
		p, _ = s.Acquire(ctx, p.ID, 0, "w", t0)
		for _, want := range []struct {
			n     int
			first int64
		}{{3, 1}, {1, 4}, {5, 5}} {
			first, err := s.Reserve(ctx, p.ID, p.Generation, want.n)
			if err != nil || first != want.first {
				t.Fatalf("Reserve(%d) = (%d, %v), want %d", want.n, first, err, want.first)
			}
		}
		if got, _ := s.Get(ctx, "t1", p.ID); got.Sequence != 9 {
			t.Fatalf("sequence = %d, want 9", got.Sequence)
		}
		if _, err := s.Reserve(ctx, p.ID, p.Generation, 0); !errors.Is(err, ErrInvalid) {
			t.Fatalf("Reserve(0) = %v, want ErrInvalid", err)
		}
	})

	t.Run("a timestamp checkpoint never moves back, and an equal one is no regression", func(t *testing.T) {
		s := newStore(t)
		p := mustEnsure(t, s, record("t1", "c1", nil))
		p, _ = s.Acquire(ctx, p.ID, 0, "w", t0)
		at := func(d time.Duration) Tick {
			tk := tick(time.Minute)
			tk.Checkpoint = &Checkpoint{Kind: KindTimestamp, At: instant(d)}
			return tk
		}
		if err := s.Complete(ctx, p.ID, p.Generation, at(time.Hour)); err != nil {
			t.Fatal(err)
		}
		if err := s.Complete(ctx, p.ID, p.Generation, at(time.Hour-time.Millisecond)); !errors.Is(err, ErrCheckpointBehind) {
			t.Fatalf("a checkpoint a millisecond back = %v, want ErrCheckpointBehind", err)
		}
		if err := s.Complete(ctx, p.ID, p.Generation, at(time.Hour)); err != nil {
			t.Fatalf("an equal checkpoint = %v, want it accepted", err)
		}
		got, _ := s.Get(ctx, "t1", p.ID)
		if !got.Checkpoint.At.Equal(stamp(t0.Add(time.Hour))) {
			t.Fatalf("checkpoint = %s, want the hour", got.Checkpoint.At)
		}
		// A cursor whose kind changed replaces the old one: an opaque token
		// is not ordered against an instant.
		tk := tick(time.Minute)
		tk.Checkpoint = &Checkpoint{Kind: KindOpaque, Token: "c-1"}
		if err := s.Complete(ctx, p.ID, p.Generation, tk); err != nil {
			t.Fatalf("a kind change = %v, want it accepted", err)
		}
		if err := s.Complete(ctx, p.ID, p.Generation, at(-time.Hour)); err != nil {
			t.Fatalf("an instant after a token = %v, want it accepted", err)
		}
	})

	t.Run("complete records the tick, keeps the checkpoint it does not name, never unprimes", func(t *testing.T) {
		s := newStore(t)
		p := mustEnsure(t, s, record("t1", "c1", nil))
		p, _ = s.Acquire(ctx, p.ID, 0, "w", t0)
		first := tick(5 * time.Minute)
		first.Checkpoint = &Checkpoint{Kind: KindTimestamp, At: instant(time.Minute)}
		first.Primed = true
		if err := s.Complete(ctx, p.ID, p.Generation, first); err != nil {
			t.Fatal(err)
		}
		incomplete := tick(10 * time.Minute)
		incomplete.Error = "page 2 answered 502"
		if err := s.Complete(ctx, p.ID, p.Generation, incomplete); err != nil {
			t.Fatal(err)
		}
		got, _ := s.Get(ctx, "t1", p.ID)
		if !got.Primed || got.Checkpoint == nil || !got.Checkpoint.At.Equal(stamp(t0.Add(time.Minute))) ||
			got.LastError != "page 2 answered 502" || !got.NextTickAt.Equal(stamp(t0.Add(10*time.Minute))) {
			t.Fatalf("stored = %+v, want primed, the first checkpoint, the second tick's error and due time", got)
		}
		if err := s.Complete(ctx, p.ID, p.Generation, tick(time.Minute)); err != nil {
			t.Fatal(err)
		}
		if got, _ := s.Get(ctx, "t1", p.ID); got.LastError != "" {
			t.Fatalf("an error a clean tick did not clear: %q", got.LastError)
		}
	})

	t.Run("an idle tick unprimes; priming again reaches further, never back", func(t *testing.T) {
		s := newStore(t)
		p := mustEnsure(t, s, record("t1", "c1", nil))
		p, _ = s.Acquire(ctx, p.ID, 0, "w", t0)
		first := tick(time.Minute)
		first.Primed, first.PrimedThrough = true, 7
		first.Error = "$where: a vendor's text, not a field path"
		first.Checkpoint = &Checkpoint{Kind: KindOpaque, Token: "$cursor"}
		if err := s.Complete(ctx, p.ID, p.Generation, first); err != nil {
			t.Fatal(err)
		}
		behind := tick(time.Minute)
		behind.Primed, behind.PrimedThrough = true, 3
		if err := s.Complete(ctx, p.ID, p.Generation, behind); err != nil {
			t.Fatal(err)
		}
		got, _ := s.Get(ctx, "t1", p.ID)
		if !got.Primed || got.PrimedThrough != 7 {
			t.Fatalf("primed %v through %d, want primed through 7: a priming never moves it back", got.Primed, got.PrimedThrough)
		}
		if got.Checkpoint == nil || got.Checkpoint.Token != "$cursor" {
			t.Fatalf("checkpoint = %+v, want the token as written", got.Checkpoint)
		}
		idle := tick(24 * time.Hour)
		idle.Idle = true
		idle.Checkpoint = &Checkpoint{Kind: KindTimestamp, At: instant(time.Hour)}
		if err := s.Complete(ctx, p.ID, p.Generation, idle); err != nil {
			t.Fatal(err)
		}
		if got, _ := s.Get(ctx, "t1", p.ID); got.Primed || got.PrimedThrough != 7 || got.Checkpoint == nil || !got.Checkpoint.At.Equal(stamp(t0.Add(time.Hour))) {
			t.Fatalf("after an idle tick = primed %v through %d checkpoint %+v, want unprimed, 7 kept, the idle instant", got.Primed, got.PrimedThrough, got.Checkpoint)
		}
		again := tick(time.Minute)
		again.Primed, again.PrimedThrough = true, 12
		if err := s.Complete(ctx, p.ID, p.Generation, again); err != nil {
			t.Fatal(err)
		}
		got, _ = s.Get(ctx, "t1", p.ID)
		if !got.Primed || got.PrimedThrough != 12 {
			t.Fatalf("re-primed %v through %d, want primed through 12", got.Primed, got.PrimedThrough)
		}
		for name, tk := range map[string]Tick{
			"idle and primed":   {At: t0, NextTickAt: t0, Idle: true, Primed: true},
			"idle with a token": {At: t0, NextTickAt: t0, Idle: true, Checkpoint: &Checkpoint{Kind: KindOpaque, Token: "x"}},
		} {
			if err := s.Complete(ctx, p.ID, p.Generation, tk); !errors.Is(err, ErrInvalid) {
				t.Errorf("%s: Complete = %v, want ErrInvalid", name, err)
			}
		}
		first2 := tick(time.Minute)
		first2.Error = "$where"
		if err := s.Complete(ctx, p.ID, p.Generation, first2); err != nil {
			t.Fatal(err)
		}
		if got, _ := s.Get(ctx, "t1", p.ID); got.LastError != "$where" {
			t.Fatalf("last error = %q, want the text as written", got.LastError)
		}
	})

	t.Run("complete refuses a tick it cannot record", func(t *testing.T) {
		s := newStore(t)
		p := mustEnsure(t, s, record("t1", "c1", nil))
		for name, tk := range map[string]Tick{
			"no instant":                  {NextTickAt: t0},
			"no due time":                 {At: t0},
			"a timestamp without instant": {At: t0, NextTickAt: t0, Checkpoint: &Checkpoint{Kind: KindTimestamp}},
			"an opaque without token":     {At: t0, NextTickAt: t0, Checkpoint: &Checkpoint{Kind: KindOpaque}},
			"an unknown kind":             {At: t0, NextTickAt: t0, Checkpoint: &Checkpoint{Kind: "sequence", Token: "1"}},
		} {
			if err := s.Complete(ctx, p.ID, p.Generation, tk); !errors.Is(err, ErrInvalid) {
				t.Errorf("%s: Complete = %v, want ErrInvalid", name, err)
			}
		}
		if err := s.Complete(ctx, "pl-missing", 0, tick(time.Minute)); !errors.Is(err, ErrNotFound) {
			t.Errorf("Complete of a missing poller = %v, want ErrNotFound", err)
		}
	})

	t.Run("due pollers list by due instant then id, across tenants, bounded", func(t *testing.T) {
		s := newStore(t)
		a := mustEnsure(t, s, record("t1", "c1", nil))
		b := mustEnsure(t, s, record("t2", "c1", nil))
		c := mustEnsure(t, s, record("t1", "c2", nil))
		// c is pushed an hour out; a and b stay due at t0.
		cg, _ := s.Acquire(ctx, c.ID, 0, "w", t0)
		if err := s.Complete(ctx, c.ID, cg.Generation, tick(time.Hour)); err != nil {
			t.Fatal(err)
		}
		due, err := s.ListDue(ctx, t0, 0)
		want := []string{a.ID, b.ID}
		if a.ID > b.ID {
			want = []string{b.ID, a.ID}
		}
		if err != nil || !reflect.DeepEqual(pollerIDs(due), want) {
			t.Fatalf("due at t0 = (%v, %v), want %v", pollerIDs(due), err, want)
		}
		if due, _ := s.ListDue(ctx, t0.Add(time.Hour), 0); len(due) != 3 || due[2].ID != c.ID {
			t.Fatalf("due at t0+1h = %v, want the later one last", pollerIDs(due))
		}
		if due, _ := s.ListDue(ctx, t0.Add(time.Hour), 1); len(due) != 1 {
			t.Fatalf("bounded due = %v", pollerIDs(due))
		}
		if due, _ := s.ListDue(ctx, t0.Add(-time.Millisecond), 0); len(due) != 0 {
			t.Fatalf("due before anything = %v", pollerIDs(due))
		}
	})

	t.Run("the seen set holds what was observed, moves forward only, prunes what left", func(t *testing.T) {
		s := newStore(t)
		a, b, c := ItemKey{"ENG-1", "v1"}, ItemKey{"ENG-1", "v2"}, ItemKey{"ENG-2", "v1"}
		if err := s.MarkSeen(ctx, "t1", "pl-1", []ItemKey{a, c}, t0); err != nil {
			t.Fatal(err)
		}
		if err := s.MarkSeen(ctx, "t1", "pl-2", []ItemKey{b}, t0); err != nil {
			t.Fatal(err)
		}
		got, err := s.Seen(ctx, "pl-1", []ItemKey{a, b, c})
		if err != nil || !reflect.DeepEqual(got, map[ItemKey]bool{a: true, c: true}) {
			t.Fatalf("seen = (%v, %v), want a and c: a revision is its own key, another poller's set is its own", got, err)
		}
		// c observed again later, then a stale observation arrives: the
		// later one stands.
		if err := s.MarkSeen(ctx, "t1", "pl-1", []ItemKey{c}, t0.Add(2*time.Hour)); err != nil {
			t.Fatal(err)
		}
		if err := s.MarkSeen(ctx, "t1", "pl-1", []ItemKey{c}, t0.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		n, err := s.PruneSeen(ctx, "pl-1", t0.Add(90*time.Minute))
		if err != nil || n != 1 {
			t.Fatalf("prune = (%d, %v), want a alone", n, err)
		}
		if got, _ := s.Seen(ctx, "pl-1", []ItemKey{a, c}); !reflect.DeepEqual(got, map[ItemKey]bool{c: true}) {
			t.Fatalf("after prune = %v, want c, observed after the cut", got)
		}
		if got, _ := s.Seen(ctx, "pl-2", []ItemKey{b}); !got[b] {
			t.Fatalf("pruning one poller touched another's set")
		}
	})

	t.Run("wake makes a dormant poller due, never later, in its own tenant", func(t *testing.T) {
		s := newStore(t)
		p := mustEnsure(t, s, record("t1", "c1", nil))
		p, _ = s.Acquire(ctx, p.ID, 0, "w", t0)
		if err := s.Complete(ctx, p.ID, p.Generation, tick(24*time.Hour)); err != nil {
			t.Fatal(err)
		}
		if err := s.Wake(ctx, "t2", p.ID, t0); !errors.Is(err, ErrNotFound) {
			t.Fatalf("Wake from another tenant = %v, want ErrNotFound", err)
		}
		if err := s.Wake(ctx, "t1", p.ID, t0.Add(time.Minute)); err != nil {
			t.Fatal(err)
		}
		if got, _ := s.Get(ctx, "t1", p.ID); !got.NextTickAt.Equal(stamp(t0.Add(time.Minute))) {
			t.Fatalf("next tick = %s, want the wake instant", got.NextTickAt)
		}
		if err := s.Wake(ctx, "t1", p.ID, t0.Add(48*time.Hour)); err != nil {
			t.Fatal(err)
		}
		if got, _ := s.Get(ctx, "t1", p.ID); !got.NextTickAt.Equal(stamp(t0.Add(time.Minute))) {
			t.Fatalf("a later wake moved the due instant to %s", got.NextTickAt)
		}
	})

	t.Run("a seen row needs its tenant", func(t *testing.T) {
		s := newStore(t)
		if err := s.MarkSeen(ctx, "", "pl-1", []ItemKey{{"a", "v"}}, t0); !errors.Is(err, ErrInvalid) {
			t.Fatalf("MarkSeen without a tenant = %v, want ErrInvalid", err)
		}
	})

	t.Run("a large seen set crosses the store's batches whole", func(t *testing.T) {
		s := newStore(t)
		keys := make([]ItemKey, 1203)
		for i := range keys {
			keys[i] = ItemKey{ID: fmt.Sprintf("item-%d", i), Version: "v"}
		}
		if err := s.MarkSeen(ctx, "t1", "pl-1", keys, t0); err != nil {
			t.Fatal(err)
		}
		got, err := s.Seen(ctx, "pl-1", append(keys, ItemKey{"absent", "v"}))
		if err != nil || len(got) != len(keys) || got[ItemKey{"absent", "v"}] {
			t.Fatalf("seen = (%d keys, %v), want %d", len(got), err, len(keys))
		}
	})

	t.Run("concurrent takers of one generation: exactly one wins", func(t *testing.T) {
		s := newStore(t)
		p := mustEnsure(t, s, record("t1", "c1", nil))
		var (
			wg   sync.WaitGroup
			mu   sync.Mutex
			wins []string
		)
		for i := range 8 {
			wg.Add(1)
			go func(owner string) {
				defer wg.Done()
				if _, err := s.Acquire(ctx, p.ID, 0, owner, t0); err == nil {
					mu.Lock()
					wins = append(wins, owner)
					mu.Unlock()
				} else if !errors.Is(err, ErrFenced) {
					t.Errorf("%s: Acquire = %v, want success or ErrFenced", owner, err)
				}
			}(fmt.Sprintf("w%d", i))
		}
		wg.Wait()
		if len(wins) != 1 {
			t.Fatalf("winners = %v, want exactly one", wins)
		}
		if got, _ := s.Get(ctx, "t1", p.ID); got.Generation != 1 || got.Holder != wins[0] {
			t.Fatalf("stored = generation %d holder %q, want 1 held by %s", got.Generation, got.Holder, wins[0])
		}
	})

	t.Run("a holder must be named", func(t *testing.T) {
		s := newStore(t)
		p := mustEnsure(t, s, record("t1", "c1", nil))
		if _, err := s.Acquire(ctx, p.ID, 0, "", t0); !errors.Is(err, ErrInvalid) {
			t.Fatalf("anonymous Acquire = %v, want ErrInvalid", err)
		}
	})
}

func pollerIDs(ps []Poller) []string {
	out := make([]string, 0, len(ps))
	for _, p := range ps {
		out = append(out, p.ID)
	}
	return out
}
