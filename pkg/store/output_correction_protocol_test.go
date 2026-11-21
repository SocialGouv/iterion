package store

import (
	"encoding/json"
	"errors"
	"math"
	"os"
	"reflect"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

func TestOutputCorrectionFilesystemLegacyAcquisition(t *testing.T) {
	s := tmpStore(t)
	mustCreateRun(t, s, "legacy")
	r, err := s.LoadRun(t.Context(), "legacy")
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	var old map[string]any
	if err := json.Unmarshal(data, &old); err != nil {
		t.Fatal(err)
	}
	delete(old, "execution_epoch")
	delete(old, "cas_version")
	data, err = json.Marshal(old)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.runJSONPath(r.ID), data, 0600); err != nil {
		t.Fatal(err)
	}
	r, err = s.LoadRun(t.Context(), r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if r.CASVersion != 0 || r.ExecutionEpoch != "" {
		t.Fatal("fixture not legacy")
	}
	a, ok, err := s.AcquireRunExecutionIf(t.Context(), r.ID, RunExecutionExpectation{Status: r.Status}, ExecutionStart)
	if err != nil || !ok || a.Epoch == "" {
		t.Fatalf("legacy claim=%+v applied=%v err=%v", a, ok, err)
	}
}

func TestOutputCorrectionProtocolWireAndLegacy(t *testing.T) {
	now := time.Unix(1800000000, 0).UTC()
	want := Run{ID: "r", ExecutionEpoch: NewExecutionEpoch(), OutputCorrections: map[string]OutputCorrectionEpisode{"key": {
		Generation: "generation", Revision: 3, LedgerKey: "key", Status: "exhausted", Attempts: 1, Budget: 1, TerminalKind: "round_failed",
		Usage:        OutputCorrectionUsage{Tokens: 9, CostUSD: 1.5, UnreportedCalls: 2},
		Cause:        &OutputCorrectionCause{Version: 1, Kind: "usage_window", Provider: "provider", WindowKind: "weekly", ResetAt: now, SelfImposed: true, NodeID: "node", TimeoutNanos: 123},
		Reservations: []OutputCorrectionReservation{{ID: "attempt", ExecutionEpoch: "old", ScopeKey: "scope", StartedAt: now, Receipt: &OutputCorrectionReceipt{Usage: OutputCorrectionUsage{Tokens: 9, CostUSD: 1.5, UnreportedCalls: 2}}}},
	}}}
	for _, codec := range []struct {
		name      string
		marshal   func(any) ([]byte, error)
		unmarshal func([]byte, any) error
	}{{"json", json.Marshal, json.Unmarshal}, {"bson", bson.Marshal, bson.Unmarshal}} {
		t.Run(codec.name, func(t *testing.T) {
			data, err := codec.marshal(want)
			if err != nil {
				t.Fatal(err)
			}
			var got Run
			if err := codec.unmarshal(data, &got); err != nil {
				t.Fatal(err)
			}
			if got.ExecutionEpoch != want.ExecutionEpoch || !reflect.DeepEqual(got.OutputCorrections, want.OutputCorrections) {
				t.Fatalf("wire lost protocol: %+v", got.OutputCorrections)
			}
			data, err = codec.marshal(map[string]any{"id": "legacy", "status": "running", "output_corrections": map[string]any{"node": map[string]any{"attempts": 2, "status": "exhausted"}}})
			if err != nil {
				t.Fatal(err)
			}
			got = Run{}
			if err := codec.unmarshal(data, &got); err != nil {
				t.Fatal(err)
			}
			if got.ExecutionEpoch != "" || got.OutputCorrections["node"].Attempts != 2 || (RunExecutionAuthority{RunID: got.ID}).Matches(&got) {
				t.Fatal("legacy gained authority or lost budget")
			}
		})
	}
}

func TestOutputCorrectionCapabilitiesRemainSeparate(t *testing.T) {
	s := tmpStore(t)
	if AsOutputCorrectionCASStore(decorator{decorator{s}}) == nil || AsRunExecutionStore(decorator{s}) == nil {
		t.Fatal("wrapped durable capability hidden")
	}
	if AsOutputCorrectionCASStore(opaqueDecorator{s}) != nil || AsRunExecutionStore(opaqueDecorator{s}) != nil {
		t.Fatal("opaque wrapper exposed new capability")
	}
	if AsOutputCorrectionStore(correctingDecorator{decorator{s}}) == nil {
		t.Fatal("legacy setter disappeared")
	}
	if AsOutputCorrectionCASStore(nil) != nil || AsRunExecutionStore(nil) != nil {
		t.Fatal("nil exposes capability")
	}
}

func TestOutputCorrectionDecisionRejectsForgedAccounting(t *testing.T) {
	a := RunExecutionAuthority{RunID: "r", Epoch: "epoch"}
	base := OutputCorrectionEpisode{Generation: "g", Revision: 1, Status: "active", Budget: 1}
	for _, tc := range []struct {
		name   string
		change func(*OutputCorrectionEpisode)
	}{
		{"paid-without-reservation", func(e *OutputCorrectionEpisode) { e.Attempts = 1 }},
		{"forged-usage", func(e *OutputCorrectionEpisode) { e.Usage.CostUSD = 1 }},
		{"unknown-cause", func(e *OutputCorrectionEpisode) { e.Cause = &OutputCorrectionCause{Version: 2, Kind: "auth"} }},
		{"new-generation-without-retirement", func(e *OutputCorrectionEpisode) { e.Generation = "new" }},
		{"unknown-status", func(e *OutputCorrectionEpisode) { e.Status = "forged" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &Run{ID: "r", Status: RunStatusRunning, ExecutionEpoch: "epoch", OutputCorrections: map[string]OutputCorrectionEpisode{"node": base}}
			next := base
			next.Revision++
			tc.change(&next)
			if ok, err := ApplyOutputCorrectionDecision(r, a, "node", ExpectOutputCorrection(base), next); ok || !errors.Is(err, ErrInvalidOutputCorrection) {
				t.Fatalf("accepted invalid transition %v %v", ok, err)
			}
			if !reflect.DeepEqual(r.OutputCorrections["node"], base) {
				t.Fatal("rejected decision mutated episode")
			}
		})
	}
}

func TestOutputCorrectionReceiptRejectsInvalidUsage(t *testing.T) {
	for _, usage := range []OutputCorrectionUsage{{Tokens: -1}, {UnreportedCalls: -1}, {CostUSD: -1}, {CostUSD: math.NaN()}, {CostUSD: math.Inf(1)}} {
		r := &Run{OutputCorrections: map[string]OutputCorrectionEpisode{"key": {Generation: "g", Revision: 1, Reservations: []OutputCorrectionReservation{{ID: "id", ExecutionEpoch: "original"}}}}}
		if ok, err := ApplyOutputCorrectionReceipt(r, OutputCorrectionReceiptTarget{LedgerKey: "key", Generation: "g", ReservationID: "id", ExecutionEpoch: "original"}, OutputCorrectionReceipt{Usage: usage}); ok || !errors.Is(err, ErrInvalidOutputCorrection) {
			t.Fatalf("invalid usage accepted: %+v %v %v", usage, ok, err)
		}
	}
}
