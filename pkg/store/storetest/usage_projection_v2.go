package storetest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
	"go.mongodb.org/mongo-driver/v2/bson"
)

func UsageV2Delegate(ordinal uint64, model string, tokens int, cost float64, session bool) store.UsageComponent {
	c := store.UsageComponent{Ordinal: ordinal, Source: store.UsageDelegate, Route: &store.InvocationUsageRoute{Scope: "invocation", Backend: "claude_code", RequestedModel: model, ReportedModel: "reported-" + model}, Tokens: tokens, CostKnown: cost > 0, CostKind: store.UsageAdditiveCost, CostUSD: cost}
	if cost == 0 {
		c.UnpricedTokens = tokens
	}
	if session {
		c.CostKind = store.UsageSessionTotal
		c.Route.SessionID = "shared"
		c.Route.SessionFingerprint = "routing-fp"
	}
	return c
}
func UsageV2Receipt(t *testing.T, id string, components ...store.UsageComponent) store.OutputCorrectionReceipt {
	t.Helper()
	obs, err := store.SummarizeUsageComponents(id, components)
	if err != nil {
		t.Fatal(err)
	}
	return store.OutputCorrectionReceipt{Observation: &obs}
}
func RunUsageProjectionV2(t *testing.T, makeStore func(*testing.T) store.RunStore) {
	ctx := store.WithTenant(context.Background(), "usage-v2")
	raw := makeStore(t)
	s := raw.(observationStore)
	r, err := s.CreateRun(ctx, "projection", "wf", nil)
	if err != nil {
		t.Fatal(err)
	}
	r.TenantID = "usage-v2"
	if err = s.SaveRun(ctx, r); err != nil {
		t.Fatal(err)
	}
	r, err = s.LoadRun(ctx, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	a, ok, err := s.AcquireRunExecutionIf(ctx, r.ID, store.RunExecutionExpectation{Version: r.CASVersion, Epoch: r.ExecutionEpoch, Status: r.Status, QueuedAt: r.QueuedAt}, store.ExecutionStart)
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	origin := store.UsageOrigin{Version: 1, RunID: r.ID, TenantID: r.TenantID, Publication: "2026-11-04T00:00:00Z", OrgID: "org"}
	reserve := func(id string) store.OutputCorrectionReceiptTarget {
		res := store.OutputCorrectionReservation{ID: id, ExecutionEpoch: a.Epoch, ScopeKey: r.ID, StartedAt: time.Now(), Origin: &origin}
		if ok, e := s.ReserveInitialUsageIf(ctx, r.ID, a, res); e != nil || !ok {
			t.Fatal(ok, e)
		}
		return store.InitialUsageTarget(res)
	}
	load := func() *store.Run {
		got, e := s.LoadRun(ctx, r.ID)
		if e != nil {
			t.Fatal(e)
		}
		return got
	}
	record := func(target store.OutputCorrectionReceiptTarget, receipt store.OutputCorrectionReceipt) {
		t.Helper()
		if ok, e := s.RecordOutputCorrectionReceipt(ctx, r.ID, target, receipt); e != nil || !ok {
			t.Fatal(ok, e)
		}
	}
	extraction := store.UsageComponent{Ordinal: 3, Source: store.UsageStructuredExtraction, Route: &store.InvocationUsageRoute{Scope: "invocation", Backend: "claw", RequestedModel: "extract", Provider: "test"}, CallID: "extract-call", Outcome: "success", Tokens: 5, TokenSplit: &store.UsageTokenSplit{Input: 3, Output: 2, CacheRead: 7, CacheWrite: 1}, CostKnown: true, CostKind: store.UsageAdditiveCost, CostUSD: .5}
	generic := store.UsageComponent{Ordinal: 5, Source: store.UsageGenericReported, Tokens: 1, CostKnown: true, CostKind: store.UsageAdditiveCost, CostUSD: .25}
	first := reserve("first")
	firstBody := UsageV2Receipt(t, "collection-first", UsageV2Delegate(1, "A", 7, 10, true), UsageV2Delegate(2, "B", 8, 12, true), extraction, UsageV2Delegate(4, "C", 4, 1.25, false), generic)
	record(first, firstBody)
	row := load()
	settled := row.InitialUsageReservations["first"].Receipt
	if settled.Usage.CostUSD != 14 || settled.Usage.Tokens != 25 || settled.Projection == nil || len(settled.Projection.Lines) != 6 {
		t.Fatalf("first projection %+v", settled)
	}
	lines := settled.Projection.Lines
	if lines[0].CostUSD != 0 || lines[1].CostUSD != 0 || lines[0].Route.RequestedModel != "A" || lines[1].Route.RequestedModel != "B" || lines[2].CostUSD != .5 || lines[2].CallID != "extract-call" || lines[2].TokenSplit.CacheRead != 7 || lines[4].Route != nil {
		t.Fatalf("route attribution %+v", lines)
	}
	session := lines[5]
	if session.Kind != "sessionAggregate" || session.Route != nil || session.Ordinal != 0 || session.CostUSD != 12 || session.Session == nil || session.Session.CostUSD != 12 || session.PreviousMaxUSD != 0 {
		t.Fatalf("invented session model attribution %+v", session)
	}
	if firstBody.Projection != nil || firstBody.Usage != (store.OutputCorrectionUsage{}) {
		t.Fatal("store mutated caller request")
	}
	// Atomic rejection cannot change either the receipt or its maxima.
	for name, mutate := range map[string]func(*store.OutputCorrectionReceipt){
		"duplicate ordinal": func(x *store.OutputCorrectionReceipt) { x.Observation.Components[1].Ordinal = 1 },
		"duplicate call": func(x *store.OutputCorrectionReceipt) {
			x.Observation.Components[3] = x.Observation.Components[2]
			x.Observation.Components[3].Ordinal = 4
		},
		"forged usage":      func(x *store.OutputCorrectionReceipt) { x.Usage.CostUSD = 14 },
		"forged projection": func(x *store.OutputCorrectionReceipt) { x.Projection = &store.UsageProjection{Version: 2} },
		"incoherent total":  func(x *store.OutputCorrectionReceipt) { x.Observation.Tokens++ },
		"unattested generic route": func(x *store.OutputCorrectionReceipt) {
			x.Observation.Components[4].Route = &store.InvocationUsageRoute{Scope: "invocation", Backend: "forged"}
		},
		"nan": func(x *store.OutputCorrectionReceipt) { x.Observation.Components[0].CostUSD = math.NaN() },
	} {
		t.Run(name, func(t *testing.T) {
			body := UsageV2Receipt(t, "collection-first", store.CloneUsageComponents(firstBody.Observation.Components)...)
			mutate(&body)
			before := load()
			if ok, e := s.RecordOutputCorrectionReceipt(ctx, r.ID, first, body); ok || !errors.Is(e, store.ErrInvalidOutputCorrection) {
				t.Fatal(ok, e)
			}
			if !reflect.DeepEqual(before, load()) {
				t.Fatal("rejection changed durable state")
			}
		})
	}
	second := reserve("second")
	secondBody := UsageV2Receipt(t, "collection-second", UsageV2Delegate(1, "B", 2, 12, true))
	record(second, secondBody)
	if got := load().InitialUsageReservations["second"].Receipt; got.Usage.CostUSD != 0 || !got.Projection.Lines[0].CostKnown || got.Projection.Lines[0].UnpricedTokens != 0 || got.Projection.Lines[1].PreviousMaxUSD != 12 {
		t.Fatalf("already-paid cost zero lost known price %+v", got)
	}
	ep := store.OutputCorrectionEpisode{Generation: "g", Revision: 1, Status: "active", Budget: 2}
	if ok, e := s.SetRunOutputCorrectionIf(ctx, r.ID, a, "node", store.OutputCorrectionExpected{Absent: true}, ep); e != nil || !ok {
		t.Fatal(ok, e)
	}
	addRound := func(id string) store.OutputCorrectionReceiptTarget {
		old := load().OutputCorrections["node"]
		next := store.CloneOutputCorrectionEpisode(old)
		next.Revision++
		next.Attempts++
		res := store.OutputCorrectionReservation{ID: id, InitialUsageID: "first", ExecutionEpoch: a.Epoch, ScopeKey: r.ID, StartedAt: time.Now()}
		next.Reservations = append(next.Reservations, res)
		if ok, e := s.SetRunOutputCorrectionIf(ctx, r.ID, a, "node", store.ExpectOutputCorrection(old), next); e != nil || !ok {
			t.Fatal(ok, e)
		}
		return res.ReceiptTarget("node", next.Generation)
	}
	round := addRound("round")
	record(round, UsageV2Receipt(t, "collection-first", UsageV2Delegate(6, "B", 3, 13, true)))
	collision := addRound("collision")
	before := load()
	if ok, e := s.RecordOutputCorrectionReceipt(ctx, r.ID, collision, firstBody); ok || !errors.Is(e, store.ErrInvalidOutputCorrection) {
		t.Fatalf("component reuse on distinct target accepted %v %v", ok, e)
	}
	if !reflect.DeepEqual(before, load()) {
		t.Fatal("collision mutated run")
	}
	// Retired history still owns the ordinal; a different collection can start at1.
	row = load()
	retired := store.CloneOutputCorrectionEpisode(row.OutputCorrections["node"])
	row.OutputCorrectionHistory = append(row.OutputCorrectionHistory, retired)
	delete(row.OutputCorrections, "node")
	if err = s.SaveRun(ctx, row); err != nil {
		t.Fatal(err)
	}
	if ok, e := s.RecordOutputCorrectionReceipt(ctx, r.ID, collision, firstBody); ok || !errors.Is(e, store.ErrInvalidOutputCorrection) {
		t.Fatal("retirement lost component identity", ok, e)
	}
	record(collision, UsageV2Receipt(t, "collection-new", UsageV2Delegate(1, "new", 0, 0, false)))
	targets := []store.OutputCorrectionReceiptTarget{reserve("parallel-a"), reserve("parallel-b")}
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for i := range targets {
		body := UsageV2Receipt(t, fmt.Sprintf("parallel-%d", i), UsageV2Delegate(1, "parallel", 1, float64(14+i), true))
		wg.Go(func() {
			ok, e := s.RecordOutputCorrectionReceipt(ctx, r.ID, targets[i], body)
			if e != nil || !ok {
				errs <- fmt.Errorf("parallel %v %w", ok, e)
			}
		})
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Fatal(e)
	}
	legacy := reserve("legacy")
	record(legacy, store.OutputCorrectionReceipt{Observation: &store.UsageObservation{Version: 1, Tokens: 4, OtherCostUSD: 3}})
	if fs, ok := s.(*store.FilesystemRunStore); ok {
		reopened, e := store.New(fs.Root())
		if e != nil {
			t.Fatal(e)
		}
		s = reopened
	}
	row = load()
	receipts, e := store.ReceiptsForOrigin(row, origin)
	if e != nil {
		t.Fatal(e)
	}
	total, tokens := 0.0, 0
	for _, item := range receipts {
		total += item.Usage.CostUSD
		tokens += item.Usage.Tokens
		if item.Observation.Version == 1 && item.Projection != nil {
			t.Fatal("legacy acquired invented route")
		}
	}
	if total != 20 || tokens != 36 {
		t.Fatalf("conservation cost=%g tokens=%d", total, tokens)
	}
	if ok, e := s.RecordOutputCorrectionReceipt(ctx, r.ID, first, firstBody); e != nil || ok {
		t.Fatal("duplicate repriced", ok, e)
	}
	for key := range row.UsageSessionMarks {
		row.UsageSessionMarks[key] = 999
	}
	again, e := store.ReceiptsForOrigin(row, origin)
	if e != nil || !reflect.DeepEqual(receipts, again) {
		t.Fatalf("reader repriced against current marks: %v", e)
	}
	for _, codec := range []string{"json", "bson"} {
		var encoded []byte
		var copy store.Run
		if codec == "json" {
			encoded, e = json.Marshal(row)
			if e == nil {
				e = json.Unmarshal(encoded, &copy)
			}
		} else {
			encoded, e = bson.Marshal(row)
			if e == nil {
				e = bson.Unmarshal(encoded, &copy)
			}
		}
		if e != nil {
			t.Fatal(codec, e)
		}
		got, e := store.ReceiptsForOrigin(&copy, origin)
		if e != nil || !reflect.DeepEqual(receipts, got) {
			t.Fatalf("%s changed projection %v", codec, e)
		}
	}
	for i := range again {
		if again[i].Projection != nil && len(again[i].Projection.Lines) > 0 {
			again[i].Projection.Lines[0].CostUSD = 999
			if again[i].Projection.Lines[0].Route != nil {
				again[i].Projection.Lines[0].Route.RequestedModel = "mutated"
			}
		}
	}
	if got, e := store.ReceiptsForOrigin(row, origin); e != nil || !reflect.DeepEqual(receipts, got) {
		t.Fatal("reader projection aliases run", e)
	}
	// A caller can read the settled bill, but may not inject another route/cost.
	damaged := *row.InitialUsageReservations["first"].Receipt
	damaged.Projection = store.CloneOutputCorrectionEpisode(store.OutputCorrectionEpisode{Reservations: []store.OutputCorrectionReservation{{Receipt: &damaged}}}).Reservations[0].Receipt.Projection
	damaged.Projection.Lines[5].Route = &store.InvocationUsageRoute{Scope: "invocation", ReportedModel: "last-model"}
	if _, e := store.FreezeSettledUsageReceipt(damaged); e == nil {
		t.Fatal("reader accepted invented model on session cost")
	}
	t.Log("v2 mixed routes cost20 tokens36; shared session maximum15; current/history, concurrent settlement, v1 read, JSON/BSON, reopen/replay and copied projection preserved")
}

func RunUsageProjectionV2Upgrade(t *testing.T, makeStore func(*testing.T) store.RunStore) {
	ctx := store.WithTenant(context.Background(), "usage-upgrade")
	s := makeStore(t).(observationStore)
	r, e := s.CreateRun(ctx, "upgrade", "wf", nil)
	if e != nil {
		t.Fatal(e)
	}
	a, ok, e := s.AcquireRunExecutionIf(ctx, r.ID, store.RunExecutionExpectation{Version: r.CASVersion, Epoch: r.ExecutionEpoch, Status: r.Status, QueuedAt: r.QueuedAt}, store.ExecutionStart)
	if e != nil || !ok {
		t.Fatal(ok, e)
	}
	initial := store.OutputCorrectionReservation{ID: "legacy-initial", ExecutionEpoch: a.Epoch, ScopeKey: r.ID, StartedAt: time.Now()}
	if ok, e := s.ReserveInitialUsageIf(ctx, r.ID, a, initial); e != nil || !ok {
		t.Fatal(ok, e)
	}
	old := store.OutputCorrectionReceipt{Observation: &store.UsageObservation{Version: 1, Tokens: 1, Sessions: []store.SessionCostObservation{{Backend: "claude_code", SessionID: "shared", Fingerprint: "routing-fp", CostUSD: 10}}}}
	if ok, e := s.RecordOutputCorrectionReceipt(ctx, r.ID, store.InitialUsageTarget(initial), old); e != nil || !ok {
		t.Fatal(ok, e)
	}
	if fs, ok := s.(*store.FilesystemRunStore); ok {
		reopened, e := store.New(fs.Root())
		if e != nil {
			t.Fatal(e)
		}
		s = reopened
	}
	ep := store.OutputCorrectionEpisode{Generation: "upgrade", Revision: 1, Status: "active", Budget: 4}
	if ok, e := s.SetRunOutputCorrectionIf(ctx, r.ID, a, "node", store.OutputCorrectionExpected{Absent: true}, ep); e != nil || !ok {
		t.Fatal(ok, e)
	}
	var firstTarget store.OutputCorrectionReceiptTarget
	var firstBody store.OutputCorrectionReceipt
	for i := 1; i <= 3; i++ {
		row, e := s.LoadRun(ctx, r.ID)
		if e != nil {
			t.Fatal(e)
		}
		current := row.OutputCorrections["node"]
		next := store.CloneOutputCorrectionEpisode(current)
		next.Revision++
		next.Attempts++
		res := store.OutputCorrectionReservation{ID: fmt.Sprintf("round-%d", i), InitialUsageID: initial.ID, ExecutionEpoch: a.Epoch, ScopeKey: r.ID, StartedAt: time.Now()}
		next.Reservations = append(next.Reservations, res)
		if ok, e := s.SetRunOutputCorrectionIf(ctx, r.ID, a, "node", store.ExpectOutputCorrection(current), next); e != nil || !ok {
			t.Fatal(ok, e)
		}
		collection, ordinal := "new-collector", uint64(i)
		if i == 3 {
			collection, ordinal = "another-collector", 1
		}
		body := UsageV2Receipt(t, collection, UsageV2Delegate(ordinal, "upgraded-model", i+1, float64(11+i), true))
		target := res.ReceiptTarget("node", next.Generation)
		if ok, e := s.RecordOutputCorrectionReceipt(ctx, r.ID, target, body); e != nil || !ok {
			t.Fatalf("legacy initial blocked valid v2 round%d: %v %v", i, ok, e)
		}
		if i == 1 {
			firstBody, firstTarget = body, target
		}
	}
	before, e := s.LoadRun(ctx, r.ID)
	if e != nil {
		t.Fatal(e)
	}
	if before.InitialUsageReservations[initial.ID].Receipt.Usage.CostUSD != 10 || before.InitialUsageReservations[initial.ID].Receipt.Projection != nil || before.OutputCorrections["node"].Usage.CostUSD != 4 {
		t.Fatalf("upgrade repriced legacy or lost new calls: %+v", before.OutputCorrections["node"])
	}
	if ok, e := s.RecordOutputCorrectionReceipt(ctx, r.ID, firstTarget, firstBody); e != nil || ok {
		t.Fatal("post-upgrade duplicate changed body", ok, e)
	}
	after, e := s.LoadRun(ctx, r.ID)
	if e != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("duplicate after marks advanced changed state", e)
	}
	if _, e := store.FreezeSettledUsageReceipt(*after.OutputCorrections["node"].Reservations[0].Receipt); e != nil {
		t.Fatal(e)
	}
	t.Log("persisted v1 initial cost10 remains unrouted; new v2 collector ordinals1/2 and another collector ordinal1 settle deltas2/1/1; duplicate stable")
}
