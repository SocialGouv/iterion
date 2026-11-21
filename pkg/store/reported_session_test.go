package store_test

import (
	"encoding/json"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/store/storetest"
	"go.mongodb.org/mongo-driver/v2/bson"
	"math"
	"reflect"
	"testing"
	"time"
)

func reportedSessionComponent() store.UsageComponent {
	return store.UsageComponent{Ordinal: 1, Source: store.UsageGenericReported, ReportedSession: &store.ReportedEconomicSession{Backend: "declared", SessionID: "A", Fingerprint: "fp"}, Tokens: 2, CostKnown: true, CostKind: store.UsageSessionTotal, CostUSD: 1}
}
func TestReportedSessionDeclaredSessionValidationAndClones(t *testing.T) {
	good := reportedSessionComponent()
	if err := store.ValidateUsageComponent(good); err != nil {
		t.Fatal(err)
	}
	cloned := store.CloneUsageComponents([]store.UsageComponent{good})
	cloned[0].ReportedSession.SessionID = "changed"
	if good.ReportedSession.SessionID != "A" {
		t.Fatal("declared identity clone aliases caller")
	}
	for _, kind := range []string{"route", "delegate", "additive", "emptybackend", "emptysession", "badutf8", "unknowncost", "nan"} {
		t.Run(kind, func(t *testing.T) {
			c := store.CloneUsageComponents([]store.UsageComponent{good})[0]
			switch kind {
			case "route":
				c.Route = &store.InvocationUsageRoute{Scope: "invocation", Backend: "x", SessionID: "A"}
			case "delegate":
				c.Source = store.UsageDelegate
				c.Route = &store.InvocationUsageRoute{Scope: "invocation", Backend: "x", SessionID: "A"}
			case "additive":
				c.CostKind = store.UsageAdditiveCost
			case "emptybackend":
				c.ReportedSession.Backend = ""
			case "emptysession":
				c.ReportedSession.SessionID = ""
			case "badutf8":
				c.ReportedSession.Fingerprint = string([]byte{255})
			case "unknowncost":
				c.CostKnown = false
				c.UnpricedTokens = c.Tokens
			case "nan":
				c.CostUSD = math.NaN()
			}
			if err := store.ValidateUsageComponent(c); err == nil {
				t.Fatalf("accepted %s", kind)
			}
		})
	}
	for _, known := range []bool{false, true} {
		c := reportedSessionComponent()
		c.CostUSD = 0
		c.CostKnown = known
		if !known {
			c.UnpricedTokens = c.Tokens
		}
		s, err := store.SummarizeUsageComponents("lot", []store.UsageComponent{c})
		if err != nil {
			t.Fatal(err)
		}
		if s.Components[0].CostKnown != known || s.OtherCostUSD != 0 || len(s.Sessions) != 1 {
			t.Fatal("known zero lost", s)
		}
	}
	for _, wire := range []string{"json", "bson"} {
		var raw []byte
		var err error
		var got store.UsageComponent
		if wire == "json" {
			raw, err = json.Marshal(good)
			if err == nil {
				err = json.Unmarshal(raw, &got)
			}
		} else {
			raw, err = bson.Marshal(good)
			if err == nil {
				err = bson.Unmarshal(raw, &got)
			}
		}
		if err != nil || !reflect.DeepEqual(good, got) {
			t.Fatalf("%s roundtrip: %+v %v", wire, got, err)
		}
	}
}
func reportedSessionReceipt(t *testing.T, components []store.UsageComponent) *store.OutputCorrectionReceipt {
	t.Helper()
	s, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	r, err := s.CreateRun(ctx, "r", "wf", nil)
	if err != nil {
		t.Fatal(err)
	}
	a, ok, err := s.AcquireRunExecutionIf(ctx, r.ID, store.RunExecutionExpectation{Version: r.CASVersion, Epoch: r.ExecutionEpoch, Status: r.Status, QueuedAt: r.QueuedAt}, store.ExecutionStart)
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	res := store.OutputCorrectionReservation{ID: "initial", ExecutionEpoch: a.Epoch, ScopeKey: r.ID, StartedAt: time.Now()}
	if ok, err = s.ReserveInitialUsageIf(ctx, r.ID, a, res); err != nil || !ok {
		t.Fatal(ok, err)
	}
	receipt := storetest.UsageV2Receipt(t, "lot", components...)
	if ok, err = s.RecordOutputCorrectionReceipt(ctx, r.ID, store.InitialUsageTarget(res), receipt); err != nil || !ok {
		t.Fatal(ok, err)
	}
	loaded, err := s.LoadRun(ctx, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	got := loaded.InitialUsageReservations[res.ID].Receipt
	if _, err = store.FreezeSettledUsageReceipt(*got); err != nil {
		t.Fatal(err)
	}
	return got
}
func TestReportedSessionProjectionRetainsDeclaredSessionAssociation(t *testing.T) {
	a, b := reportedSessionComponent(), reportedSessionComponent()
	b.Ordinal = 2
	b.Tokens = 3
	b.ReportedSession.SessionID = "B"
	first := reportedSessionReceipt(t, []store.UsageComponent{a, b})
	a.ReportedSession.SessionID = "B"
	b.ReportedSession.SessionID = "A"
	second := reportedSessionReceipt(t, []store.UsageComponent{a, b})
	if reflect.DeepEqual(first.Observation, second.Observation) {
		t.Fatal("bad fixture")
	}
	if reflect.DeepEqual(first.Projection, second.Projection) {
		t.Fatal("projection loses which declared session owns each component's tokens")
	}
}
func TestReportedSessionProjectionCloneAndTamper(t *testing.T) {
	got := reportedSessionReceipt(t, []store.UsageComponent{reportedSessionComponent()})
	frozen, err := store.FreezeSettledUsageReceipt(*got)
	if err != nil {
		t.Fatal(err)
	}
	if frozen.Projection.Lines[0].ReportedSession == nil {
		t.Fatal("projection lost declared identity")
	}
	frozen.Projection.Lines[0].ReportedSession.SessionID = "changed"
	if got.Projection.Lines[0].ReportedSession.SessionID != "A" || frozen.Observation.Components[0].ReportedSession.SessionID != "A" {
		t.Fatal("projection clone aliases original or observation")
	}
	if _, err = store.FreezeSettledUsageReceipt(frozen); err == nil {
		t.Fatal("forged projection identity accepted")
	}
	// Once on disk, the same v2 declared identity survives both serialization seams.
	for _, wire := range []string{"json", "bson"} {
		var raw []byte
		var e error
		var decoded store.OutputCorrectionReceipt
		if wire == "json" {
			raw, e = json.Marshal(got)
			if e == nil {
				e = json.Unmarshal(raw, &decoded)
			}
		} else {
			raw, e = bson.Marshal(got)
			if e == nil {
				e = bson.Unmarshal(raw, &decoded)
			}
		}
		if e != nil {
			t.Fatal(e)
		}
		if _, e = store.FreezeSettledUsageReceipt(decoded); e != nil {
			t.Fatalf("%s froze: %v", wire, e)
		}
		if !reflect.DeepEqual(got, &decoded) {
			t.Fatalf("%s receipt changed", wire)
		}
	}
}
