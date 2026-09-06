package server

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	"github.com/SocialGouv/iterion/pkg/forge"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/trigger"
)

// The projection effect is the FAST path of ADR-097 §2's reflect: a native
// card move rides the durable effect outbox and reaches the bound board in
// seconds instead of at the next reconciliation pass. It must call the SAME
// reflect the pass calls — one implementation — and it must leave the board
// and the card in a state the pass then reads as "nothing to do", in both
// orders.

func projectionEffectWorld(t *testing.T, bc forge.BoardClient, board native.BoardStore) (*boardProjectionEffect, forge.BoardBindingStore) {
	t.Helper()
	binds := forge.NewMemoryBoardBindingStore()
	if err := binds.Upsert(context.Background(), *testBinding()); err != nil {
		t.Fatalf("upsert binding: %v", err)
	}
	return &boardProjectionEffect{
		Bindings:       binds,
		BoardClientFor: func(context.Context, forge.BoardBinding) (forge.BoardClient, error) { return bc, nil },
		CardsFor:       func(context.Context, string) (native.BoardStore, error) { return board, nil },
	}, binds
}

// movedCardEvent is the trigger event a `card.moved` on the native board
// normalizes to, carrying the column the card LEFT.
func movedCardEvent(cardID, from string) trigger.Event {
	return trigger.Event{
		ID: "board:b:" + cardID + ":7", Source: trigger.SourceBoard, Kind: trigger.KindCardMoved,
		TenantID: "team-a", Subject: trigger.Subject{Type: "card", ID: cardID},
		Payload: map[string]any{"from_state": from},
	}
}

func TestBoardProjection_HasBoardBinding(t *testing.T) {
	p, _ := projectionEffectWorld(t, &fakeBoardClient{project: testProject()}, newTestBoard(t))
	ctx := context.Background()
	if bound, err := p.HasBoardBinding(ctx, "team-a"); err != nil || !bound {
		t.Fatalf("bound team: %v %v", bound, err)
	}
	if bound, err := p.HasBoardBinding(ctx, "team-unbound"); err != nil || bound {
		t.Fatalf("unbound team: %v %v — a missing binding is not an error", bound, err)
	}
	if bound, err := p.HasBoardBinding(ctx, ""); err != nil || bound {
		t.Fatalf("no tenant: %v %v", bound, err)
	}
}

func TestBoardProjection_ReflectsANativeMove(t *testing.T) {
	board := newTestBoard(t)
	at := time.Date(2026, 9, 5, 10, 0, 0, 0, time.UTC)
	// Recorded status "Planned" is what `ready` maps to, so the board and the
	// card agreed until this move.
	id := seedSynced(t, board, 613, native.StateInProgress, "Planned", at)
	bc := &fakeBoardClient{project: testProject()}
	p, _ := projectionEffectWorld(t, bc, board)

	if err := p.ReflectCard(context.Background(), movedCardEvent(id, native.StateReady)); err != nil {
		t.Fatalf("ReflectCard: %v", err)
	}
	if len(bc.writes) != 1 {
		t.Fatalf("writes = %+v, want one Status write", bc.writes)
	}
	w := bc.writes[0]
	if w.ProjectID != "PVT_p" || w.ItemID != "PVTI_1" || w.FieldID != "PVTSSF_status" ||
		w.OptionID != optionID(t, testProject(), "In progress") {
		t.Fatalf("write = %+v, want the In progress option on PVTI_1", w)
	}
	// The card's own column is untouched — the reflect pushes, never pulls.
	if got := mustGet(t, board, id).State; got != native.StateInProgress {
		t.Fatalf("card state = %q, want it untouched", got)
	}
	// And the recorded status advanced, which is what makes the NEXT pass (and
	// the next projection) a no-op.
	if rec := mustGet(t, board, id).External.Project.Status; rec != "In progress" {
		t.Fatalf("recorded status = %q, want %q — the pass would re-push forever", rec, "In progress")
	}
}

// TestBoardProjection_ThenThePassIsANoop is idempotence in the first
// direction: the fast path landed, so the periodic pass over the same board
// must write nothing — to the forge OR to the card.
func TestBoardProjection_ThenThePassIsANoop(t *testing.T) {
	board := newTestBoard(t)
	at := time.Date(2026, 9, 5, 10, 0, 0, 0, time.UTC)
	id := seedSynced(t, board, 613, native.StateInProgress, "Planned", at)
	bc := &fakeBoardClient{project: testProject()}
	p, _ := projectionEffectWorld(t, bc, board)
	if err := p.ReflectCard(context.Background(), movedCardEvent(id, native.StateReady)); err != nil {
		t.Fatalf("ReflectCard: %v", err)
	}
	wrote := mustGet(t, board, id).External.Project.StatusAt

	// The pass now reads the board saying what the projection wrote.
	bc.pages = [][]forge.ProjectItem{{item("PVTI_1", 613, statusValue("In progress", wrote))}}
	before := countCardEvents(t, board)
	res, err := ImportProjectBoard(context.Background(), bc, testProjectRef, forge.ProviderGitHub, board,
		&ProjectImportOptions{Binding: testBinding()})
	if err != nil {
		t.Fatalf("ImportProjectBoard: %v", err)
	}
	if res.Reflected != 0 || res.Moved != 0 || res.Conflicts != 0 {
		t.Fatalf("the pass after a fast-path reflect = %+v, want a silent no-op", res)
	}
	if len(bc.writes) != 1 {
		t.Fatalf("the pass issued %d writes in total, want the projection's one", len(bc.writes))
	}
	if after := countCardEvents(t, board); after != before {
		t.Fatalf("the pass emitted %d card event(s) — a quiet pass relaunches every label-matching subscription", after-before)
	}
}

// TestBoardProjection_AfterThePassIsANoop is idempotence in the other
// direction: the pass already reflected this move, so the projection row that
// arrives (or is retried) afterwards must write nothing.
func TestBoardProjection_AfterThePassIsANoop(t *testing.T) {
	board := newTestBoard(t)
	at := time.Date(2026, 9, 5, 10, 0, 0, 0, time.UTC)
	id := seedSynced(t, board, 613, native.StateInProgress, "Planned", at)
	bc := &fakeBoardClient{project: testProject(), pages: [][]forge.ProjectItem{{
		item("PVTI_1", 613, statusValue("Planned", at)),
	}}}
	if _, err := ImportProjectBoard(context.Background(), bc, testProjectRef, forge.ProviderGitHub, board,
		&ProjectImportOptions{Binding: testBinding()}); err != nil {
		t.Fatalf("ImportProjectBoard: %v", err)
	}
	if len(bc.writes) != 1 {
		t.Fatalf("the pass should have reflected once, writes=%+v", bc.writes)
	}
	p, _ := projectionEffectWorld(t, bc, board)
	if err := p.ReflectCard(context.Background(), movedCardEvent(id, native.StateReady)); err != nil {
		t.Fatalf("ReflectCard: %v", err)
	}
	if len(bc.writes) != 1 {
		t.Fatalf("the projection re-wrote after the pass had already reflected: %+v", bc.writes)
	}
}

// TestBoardProjection_DefersWhenTheBoardHadAlreadyMoved is the precondition
// the fast path cannot verify for itself. reflectNativeState rests on "the
// board still says what iterion last recorded"; the pass establishes that by
// READING the board, the projection cannot. What it can check is the column
// the card LEFT: when that column does not map to the recorded status, the two
// sides had already diverged, so someone moved the card on the board — and
// pushing here would silently overwrite them. Only the pass, which reads both
// timestamps, may arbitrate that.
func TestBoardProjection_DefersWhenTheBoardHadAlreadyMoved(t *testing.T) {
	board := newTestBoard(t)
	at := time.Date(2026, 9, 5, 10, 0, 0, 0, time.UTC)
	// Recorded "Blocked" while the card was in `ready` (which maps to
	// "Planned"): the board and the card had already diverged.
	id := seedSynced(t, board, 613, native.StateInProgress, "Blocked", at)
	bc := &fakeBoardClient{project: testProject()}
	p, _ := projectionEffectWorld(t, bc, board)

	if err := p.ReflectCard(context.Background(), movedCardEvent(id, native.StateReady)); err != nil {
		t.Fatalf("ReflectCard: %v", err)
	}
	if len(bc.writes) != 0 {
		t.Fatalf("the projection pushed over a board that had moved: %+v", bc.writes)
	}
	if rec := mustGet(t, board, id).External.Project.Status; rec != "Blocked" {
		t.Fatalf("recorded status = %q, want it left stale so the pass still re-derives the conflict", rec)
	}
}

// TestBoardProjection_UnmappedPreviousStateStillReflects: a card leaving a
// state the map does not carry (`review`, …) never had its column pushed, so
// the recorded status is still the last true thing the board was told — the
// precondition holds and the reflect proceeds.
func TestBoardProjection_UnmappedPreviousStateStillReflects(t *testing.T) {
	board := newTestBoard(t)
	at := time.Date(2026, 9, 5, 10, 0, 0, 0, time.UTC)
	id := seedSynced(t, board, 613, native.StateInProgress, "Planned", at)
	bc := &fakeBoardClient{project: testProject()}
	p, _ := projectionEffectWorld(t, bc, board)
	if err := p.ReflectCard(context.Background(), movedCardEvent(id, "review")); err != nil {
		t.Fatalf("ReflectCard: %v", err)
	}
	if len(bc.writes) != 1 {
		t.Fatalf("writes = %+v, want one — an unmapped previous column tells us nothing against the record", bc.writes)
	}
}

// TestBoardProjection_BenignDeclines: each of these means "there is nothing to
// reflect", not "something failed" — the outbox must retire the row rather
// than retry it five times and dead-letter.
func TestBoardProjection_BenignDeclines(t *testing.T) {
	ctx := context.Background()
	at := time.Date(2026, 9, 5, 10, 0, 0, 0, time.UTC)

	t.Run("card deleted between the move and the reflect", func(t *testing.T) {
		board := newTestBoard(t)
		bc := &fakeBoardClient{project: testProject()}
		p, _ := projectionEffectWorld(t, bc, board)
		if err := p.ReflectCard(ctx, movedCardEvent("gone", native.StateReady)); err != nil {
			t.Fatalf("ReflectCard: %v", err)
		}
		if len(bc.writes) != 0 {
			t.Fatalf("writes = %+v", bc.writes)
		}
	})

	t.Run("binding removed between materialization and execution", func(t *testing.T) {
		board := newTestBoard(t)
		id := seedSynced(t, board, 613, native.StateInProgress, "Planned", at)
		bc := &fakeBoardClient{project: testProject()}
		p, binds := projectionEffectWorld(t, bc, board)
		if err := binds.Delete(ctx, "team-a"); err != nil {
			t.Fatalf("delete binding: %v", err)
		}
		if err := p.ReflectCard(ctx, movedCardEvent(id, native.StateReady)); err != nil {
			t.Fatalf("ReflectCard: %v", err)
		}
		if len(bc.writes) != 0 {
			t.Fatalf("writes = %+v", bc.writes)
		}
	})

	t.Run("card never joined to the board", func(t *testing.T) {
		board := newTestBoard(t)
		id := seedCard(t, board, 613, native.StateInProgress)
		bc := &fakeBoardClient{project: testProject()}
		p, _ := projectionEffectWorld(t, bc, board)
		if err := p.ReflectCard(ctx, movedCardEvent(id, native.StateReady)); err != nil {
			t.Fatalf("ReflectCard: %v", err)
		}
		if len(bc.writes) != 0 {
			t.Fatalf("writes = %+v — the import owns the join (§6), the reflect never creates it", bc.writes)
		}
	})

	t.Run("card synced against a different board", func(t *testing.T) {
		board := newTestBoard(t)
		id := seedCard(t, board, 613, native.StateInProgress)
		if _, err := board.Update(id, native.Patch{External: &native.ExternalRef{
			Provider: "github", Repo: "SocialGouv/iterion", Number: 613,
			Project: &native.ExternalProject{Owner: "SocialGouv", Number: 999, ItemID: "PVTI_other", Status: "Planned", StatusAt: at},
		}}); err != nil {
			t.Fatalf("seed foreign sync: %v", err)
		}
		bc := &fakeBoardClient{project: testProject()}
		p, _ := projectionEffectWorld(t, bc, board)
		if err := p.ReflectCard(ctx, movedCardEvent(id, native.StateReady)); err != nil {
			t.Fatalf("ReflectCard: %v", err)
		}
		if len(bc.writes) != 0 {
			t.Fatalf("writes = %+v — a re-binding must not push onto the previous board's item", bc.writes)
		}
	})
}

// TestBoardProjection_ForgeRefusalIsAnError: the row must RETRY (and
// eventually dead-letter), which only happens if the refusal reaches the
// worker. A counted-and-swallowed refusal, which is right for a 300-card pass,
// is wrong for a single-card effect that owns its own retry budget.
func TestBoardProjection_ForgeRefusalIsAnError(t *testing.T) {
	board := newTestBoard(t)
	at := time.Date(2026, 9, 5, 10, 0, 0, 0, time.UTC)
	id := seedSynced(t, board, 613, native.StateInProgress, "Planned", at)
	bc := &fakeBoardClient{project: testProject(), setErr: errors.New("403 from the forge")}
	p, _ := projectionEffectWorld(t, bc, board)
	err := p.ReflectCard(context.Background(), movedCardEvent(id, native.StateReady))
	if err == nil {
		t.Fatal("a refused status write returned nil — the outbox would retire the row and the board stays diverged")
	}
	// The record must stay stale, exactly as the pass leaves it after a failed
	// write: that is what makes the retry (and the next pass) try again.
	if rec := mustGet(t, board, id).External.Project.Status; rec != "Planned" {
		t.Fatalf("recorded status = %q after a failed write, want it untouched at %q", rec, "Planned")
	}
}

// TestBoardProjection_RefusesACardWhoseColumnIsGone: the fast path never reads
// the board, so everything it knows about the board's columns it reads off the
// binding — including the ones the last reconciliation found MISSING.
//
// That matters because a lost column KEEPS its cached option id (the evidence
// the degradation is re-derived from), so `OptionForState` still answers one.
// A fast path that consulted only the binding's option map would fire that
// dead id at the forge on every move of every card in that column, get a 422,
// and burn the outbox row's whole retry budget — for a column no retry can
// bring back.
func TestBoardProjection_RefusesACardWhoseColumnIsGone(t *testing.T) {
	board := newTestBoard(t)
	at := time.Date(2026, 9, 5, 10, 0, 0, 0, time.UTC)
	id := seedSynced(t, board, 613, native.StateInProgress, "Planned", at)
	bc := &fakeBoardClient{project: deletedColumn(t, "In progress")}
	p, binds := projectionEffectWorld(t, bc, board)
	var logs bytes.Buffer
	p.Logger = iterlog.New(iterlog.LevelWarn, &logs)
	ctx := context.Background()

	// The shape a reconciliation leaves behind when a bound column is deleted.
	lost := testBinding()
	lost.MissingStatuses = []string{"In progress"}
	lost.UnresolvedAtBind = []string{} // the bind resolved it; the board lost it
	if err := binds.Upsert(ctx, *lost); err != nil {
		t.Fatalf("upsert binding: %v", err)
	}
	if err := binds.MarkDegraded(ctx, "team-a", `the Status field no longer carries "In progress" (in_progress)`); err != nil {
		t.Fatalf("mark degraded: %v", err)
	}

	if err := p.ReflectCard(ctx, movedCardEvent(id, native.StateReady)); err != nil {
		t.Fatalf("ReflectCard = %v, want nil — no retry can make a deleted column exist, so the row retires", err)
	}
	if len(bc.writes) != 0 {
		t.Fatalf("writes = %+v, want none — the cached id names a column the board no longer carries", bc.writes)
	}
	// The record stays as it was: nothing was pushed, so nothing may claim it.
	if rec := mustGet(t, board, id).External.Project.Status; rec != "Planned" {
		t.Errorf("recorded status = %q, want it untouched at %q", rec, "Planned")
	}
	// And it is not silent: the pass folds this into a per-pass counter it
	// logs anyway, the fast path has no such line, so it says so once.
	if got := strings.Count(logs.String(), "carries no Status column"); got != 1 {
		t.Errorf("logged %d times, want exactly 1 — a single-card effect that quietly does nothing is invisible: %q", got, logs.String())
	}
}

// TestBoardProjection_SatisfiesTheTriggerSeams: one type answers both halves,
// so a deployment can never materialize projection rows it cannot execute.
func TestBoardProjection_SatisfiesTheTriggerSeams(t *testing.T) {
	var p any = &boardProjectionEffect{}
	if _, ok := p.(trigger.ProjectionBindings); !ok {
		t.Fatal("boardProjectionEffect does not implement trigger.ProjectionBindings")
	}
	if _, ok := p.(trigger.ProjectionEffect); !ok {
		t.Fatal("boardProjectionEffect does not implement trigger.ProjectionEffect")
	}
}

// recordingProjection is the wire's oracle: it answers both seams and records
// what it was asked to reflect.
type recordingProjection struct {
	bound map[string]bool
	cards []string
}

func (r *recordingProjection) HasBoardBinding(_ context.Context, tenantID string) (bool, error) {
	return r.bound[tenantID], nil
}

func (r *recordingProjection) ReflectCard(_ context.Context, ev trigger.Event) error {
	r.cards = append(r.cards, ev.Subject.ID)
	return nil
}

// TestCloudBoardSource_ProjectsACardMove is the WIRE, not the pieces: a card
// move tailed off board_events must reach the reflect through the outbox on
// the cloud source's own path — materialize, claim, execute. Without the
// bindings oracle on the source, every unit above passes and the fast path
// still never runs in production.
func TestCloudBoardSource_ProjectsACardMove(t *testing.T) {
	src, st, subs := newDrainWorld(t)
	proj := &recordingProjection{bound: map[string]bool{"t1": true}}
	src.bindings = proj
	src.eval = trigger.NewEvaluator(subs, trigger.WithProjectionEffect(proj))

	if _, err := src.drainTenant("t1", st); err != nil {
		t.Fatalf("drainTenant: %v", err)
	}
	// Two card moves, each owing one projection row on top of its launch row.
	if st.upserted != 4 {
		t.Fatalf("upserted %d rows, want 4 (2 launches + 2 projections)", st.upserted)
	}
	w := &trigger.EffectWorker{Outbox: st, Subs: subs, Evaluator: src.eval}
	w.Tick(context.Background(), 20)
	if len(proj.cards) != 2 {
		t.Fatalf("reflected %v, want both cards — the projection is materialized but never executed", proj.cards)
	}
}

// TestCloudBoardSource_NoProjectionWithoutABinding: an unbound tenant's moves
// materialize launch rows only, so a deployment with no board binding pays
// nothing for this feature.
func TestCloudBoardSource_NoProjectionWithoutABinding(t *testing.T) {
	src, st, _ := newDrainWorld(t)
	src.bindings = &recordingProjection{bound: map[string]bool{}}
	if _, err := src.drainTenant("t1", st); err != nil {
		t.Fatalf("drainTenant: %v", err)
	}
	if st.upserted != 2 {
		t.Fatalf("upserted %d rows for an unbound tenant, want 2 launch rows only", st.upserted)
	}
}
