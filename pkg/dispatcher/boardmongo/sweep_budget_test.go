package boardmongo

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/event"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
)

// A CASCADE (RenameLabel / MergeLabels / DeleteLabel / RenameState /
// DeleteState / RenameField / DeleteField) rewrites every card of the board.
// opTimeout prices ONE Mongo round-trip; a walk that threads a single
// opTimeout context through listAll plus a write per card spends that one
// budget on the WHOLE walk, so it renames the first cards, errors on the
// rest, and leaves a half-applied board with no rollback.
//
// The oracle here is the deadline each per-card write actually carries on
// the wire. The driver renders the context deadline as `maxTimeMS`, so a
// shared deadline is directly observable: it BURNS DOWN across the walk (the
// last card's budget is short by the walk's whole duration), while a
// per-round-trip deadline is the same on every card. Reading the wire instead
// of racing a stopwatch keeps the assertion exact — a loaded runner's
// latency spike moves neither figure.

// budgetSpread is the tolerated variation, in milliseconds, between the
// largest and smallest wire deadline of one cascade. Per-round-trip
// derivation lands every write within a millisecond or two of opTimeout; a
// shared deadline burns down by the walk's own duration, seconds over a
// board of sweepCards.
const budgetSpread = 250

// sweepCards is the fixture size. It only has to make the walk's duration
// exceed budgetSpread by a wide margin, which a few hundred round-trips do
// on any host.
const sweepCards = 400

// deadlineProbe records the wire deadline of every `update` the cascade
// issues against the issues collection — one per rewritten card.
type deadlineProbe struct {
	mu       sync.Mutex
	watching bool
	seen     []int64
}

func (p *deadlineProbe) monitor() *event.CommandMonitor {
	return &event.CommandMonitor{
		Started: func(_ context.Context, e *event.CommandStartedEvent) {
			if e.CommandName != "update" {
				return
			}
			if coll, ok := e.Command.Lookup("update").StringValueOK(); !ok || coll != IssuesCollection {
				return
			}
			ms, ok := e.Command.Lookup("maxTimeMS").AsInt64OK()
			if !ok {
				return
			}
			p.mu.Lock()
			defer p.mu.Unlock()
			if p.watching {
				p.seen = append(p.seen, ms)
			}
		},
	}
}

// watch arms the probe for the cascade about to run, discarding anything the
// fixture seeding produced.
func (p *deadlineProbe) watch() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.watching, p.seen = true, nil
}

// assertPerCallBudget fails unless the cascade rewrote every card under a
// deadline it did not share with the rest of the walk.
func (p *deadlineProbe) assertPerCallBudget(t *testing.T, wantWrites int) {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	p.watching = false
	if len(p.seen) < wantWrites {
		t.Fatalf("cascade issued %d card writes, want at least %d — the fixture did not exercise the walk", len(p.seen), wantWrites)
	}
	lo, hi := p.seen[0], p.seen[0]
	for _, ms := range p.seen {
		if ms < lo {
			lo = ms
		}
		if ms > hi {
			hi = ms
		}
	}
	if spread := hi - lo; spread > budgetSpread {
		t.Fatalf("per-card write deadlines span %d ms over %d writes (%d..%d): the walk shares ONE budget and burns it down — a board large enough exhausts it mid-cascade and half-applies",
			spread, len(p.seen), lo, hi)
	}
}

// sweepBudgetStore opens a throwaway database with the probe attached.
func sweepBudgetStore(t *testing.T, prefix string) (*Store, *deadlineProbe) {
	t.Helper()
	uri := os.Getenv("ITERION_TEST_MONGO_URI")
	if uri == "" {
		t.Skip("ITERION_TEST_MONGO_URI not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	probe := &deadlineProbe{}
	client, err := mongo.Connect(options.Client().ApplyURI(uri).SetMonitor(probe.monitor()))
	if err != nil {
		t.Fatal(err)
	}
	nonce := make([]byte, 4)
	_, _ = rand.Read(nonce)
	db := client.Database(prefix + hex.EncodeToString(nonce))
	t.Cleanup(func() {
		c, cc := context.WithTimeout(context.Background(), 20*time.Second)
		defer cc()
		_ = db.Drop(c)
		_ = client.Disconnect(c)
	})
	if err := EnsureSchema(ctx, db); err != nil {
		t.Fatal(err)
	}
	return New(db, "t1"), probe
}

func seedCards(t *testing.T, s *Store, n int, decorate func(iss *native.Issue)) {
	t.Helper()
	for i := 0; i < n; i++ {
		iss := native.Issue{Title: fmt.Sprintf("card-%03d", i), State: native.StateReady}
		decorate(&iss)
		if _, err := s.Create(iss); err != nil {
			t.Fatalf("seed card %d: %v", i, err)
		}
	}
}

func labelled(label string) func(*native.Issue) {
	return func(iss *native.Issue) { iss.Labels = []string{label} }
}

func TestRenameLabelSweepBudgetsEachCallSeparately(t *testing.T) {
	s, probe := sweepBudgetStore(t, "sweepbudget_rename_")
	seedCards(t, s, sweepCards, labelled("old"))

	probe.watch()
	touched, err := s.RenameLabel("old", "new")
	if err != nil {
		t.Fatalf("RenameLabel touched %d/%d cards and failed: %v", touched, sweepCards, err)
	}
	if touched != sweepCards {
		t.Fatalf("RenameLabel touched %d cards, want %d", touched, sweepCards)
	}
	probe.assertPerCallBudget(t, sweepCards)
}

// DeleteLabel is the cascade a consume_labels trigger depends on: a card that
// keeps a label the operator retired stays armed.
func TestDeleteLabelSweepBudgetsEachCallSeparately(t *testing.T) {
	s, probe := sweepBudgetStore(t, "sweepbudget_delete_")
	seedCards(t, s, sweepCards, labelled("retire-me"))

	probe.watch()
	touched, err := s.DeleteLabel("retire-me")
	if err != nil {
		t.Fatalf("DeleteLabel touched %d/%d cards and failed: %v", touched, sweepCards, err)
	}
	if touched != sweepCards {
		t.Fatalf("DeleteLabel touched %d cards, want %d", touched, sweepCards)
	}
	probe.assertPerCallBudget(t, sweepCards)
}

func TestMergeLabelsSweepBudgetsEachCallSeparately(t *testing.T) {
	s, probe := sweepBudgetStore(t, "sweepbudget_merge_")
	seedCards(t, s, sweepCards, labelled("from"))

	probe.watch()
	touched, err := s.MergeLabels("from", "into")
	if err != nil {
		t.Fatalf("MergeLabels touched %d/%d cards and failed: %v", touched, sweepCards, err)
	}
	if touched != sweepCards {
		t.Fatalf("MergeLabels touched %d cards, want %d", touched, sweepCards)
	}
	probe.assertPerCallBudget(t, sweepCards)
}

// RenameState drives migrateState, the second walk family.
func TestRenameStateSweepBudgetsEachCallSeparately(t *testing.T) {
	s, probe := sweepBudgetStore(t, "sweepbudget_state_")
	seedCards(t, s, sweepCards, func(*native.Issue) {})

	probe.watch()
	touched, err := s.RenameState(native.StateReady, "queued")
	if err != nil {
		t.Fatalf("RenameState touched %d/%d cards and failed: %v", touched, sweepCards, err)
	}
	if touched != sweepCards {
		t.Fatalf("RenameState touched %d cards, want %d", touched, sweepCards)
	}
	probe.assertPerCallBudget(t, sweepCards)
}

// DeleteState drives migrateState behind an extra listAll of its own.
func TestDeleteStateSweepBudgetsEachCallSeparately(t *testing.T) {
	s, probe := sweepBudgetStore(t, "sweepbudget_statedel_")
	seedCards(t, s, sweepCards, func(*native.Issue) {})

	probe.watch()
	touched, err := s.DeleteState(native.StateReady, native.StateInProgress)
	if err != nil {
		t.Fatalf("DeleteState touched %d/%d cards and failed: %v", touched, sweepCards, err)
	}
	if touched != sweepCards {
		t.Fatalf("DeleteState touched %d cards, want %d", touched, sweepCards)
	}
	probe.assertPerCallBudget(t, sweepCards)
}

// RenameField drives applyFieldRewrite, the third walk family.
func TestRenameFieldSweepBudgetsEachCallSeparately(t *testing.T) {
	s, probe := sweepBudgetStore(t, "sweepbudget_field_")
	if err := s.AddField(native.Field{Name: "sprint", Type: native.FieldText}); err != nil {
		t.Fatalf("AddField: %v", err)
	}
	seedCards(t, s, sweepCards, func(iss *native.Issue) {
		iss.Fields = map[string]any{"sprint": "s1"}
	})

	probe.watch()
	touched, err := s.RenameField("sprint", "iteration")
	if err != nil {
		t.Fatalf("RenameField touched %d/%d cards and failed: %v", touched, sweepCards, err)
	}
	if touched != sweepCards {
		t.Fatalf("RenameField touched %d cards, want %d", touched, sweepCards)
	}
	probe.assertPerCallBudget(t, sweepCards)
}

// DeleteField is applyFieldRewrite's second entry point.
func TestDeleteFieldSweepBudgetsEachCallSeparately(t *testing.T) {
	s, probe := sweepBudgetStore(t, "sweepbudget_fielddel_")
	if err := s.AddField(native.Field{Name: "sprint", Type: native.FieldText}); err != nil {
		t.Fatalf("AddField: %v", err)
	}
	seedCards(t, s, sweepCards, func(iss *native.Issue) {
		iss.Fields = map[string]any{"sprint": "s1"}
	})

	probe.watch()
	touched, err := s.DeleteField("sprint")
	if err != nil {
		t.Fatalf("DeleteField touched %d/%d cards and failed: %v", touched, sweepCards, err)
	}
	if touched != sweepCards {
		t.Fatalf("DeleteField touched %d cards, want %d", touched, sweepCards)
	}
	probe.assertPerCallBudget(t, sweepCards)
}

// TestSweepErrorNamesItsProgress asserts a cascade that fails reports the
// operation and how far it got, not a bare driver error: a cascade is a
// partial write with no rollback, so an operator who cannot name the boundary
// cannot finish it by hand. Runs against an unreachable server, so the read
// fails for certain and no harness is needed.
func TestSweepErrorNamesItsProgress(t *testing.T) {
	client, err := mongo.Connect(options.Client().
		ApplyURI("mongodb://127.0.0.1:1/").
		SetServerSelectionTimeout(200 * time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		c, cc := context.WithTimeout(context.Background(), 5*time.Second)
		defer cc()
		_ = client.Disconnect(c)
	})
	s := New(client.Database("unreachable"), "t1")

	touched, err := s.RenameLabel("old", "new")
	if err == nil {
		t.Fatal("expected the cascade to fail against an unreachable server")
	}
	if touched != 0 {
		t.Fatalf("touched=%d, want 0 (the read never returned)", touched)
	}
	for _, want := range []string{"label_rename", "0 card(s)", "read the board"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error does not report %q: %v", want, err)
		}
	}
}
