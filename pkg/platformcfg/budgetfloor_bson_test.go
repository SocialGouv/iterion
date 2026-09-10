package platformcfg

import (
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/SocialGouv/iterion/pkg/budgetfloor"
)

// The budget floor is the only family whose type lives outside this package,
// and it is the only one whose record is a pair of SLICES of structs rather
// than a handful of scalars — every axis carrying `omitempty`. MongoStore
// writes it through a bson.Marshal → bson.M → ReplaceOne round trip and reads
// it back with a direct Decode, so a tag typo or a dropped sub-document does
// not fail anywhere: it silently returns a policy that reserves less than the
// operator wrote, which is a starvation nobody would trace back to encoding.
//
// This exercises the same encoder the store uses, with no Mongo needed.
func TestBudgetFloorPolicySurvivesTheBSONRoundTrip(t *testing.T) {
	want := budgetfloor.Policy{
		Reservations: []budgetfloor.Reservation{
			{BotID: "review-pr", Note: "keeps the reviewer answering", Reserve: budgetfloor.Reserve{
				FiveHourPercent: 20, WeekPercent: 10, MonthlyUSD: 30.5, ConcurrentRuns: 2,
			}},
			// An all-zero reserve: `reserve` is a sub-document whose every
			// field is omitempty, so this is the shape that encodes to {} and
			// must still decode as a reservation holding nothing — not vanish,
			// and not come back as a nil struct the gates would panic on.
			{BotID: "docs-refresh"},
		},
		RepoQuotas: []budgetfloor.RepoQuota{
			{Repo: "o/hungry", MonthlyUSD: 50},
			{Repo: "o/busy", RouteSpendsPerMonth: 40},
			{Repo: "o/shared", ReserveSharePercent: 25, ShareOfBot: "review-pr"},
		},
		UpdatedAt: time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC),
	}
	if err := want.Validate(); err != nil {
		t.Fatalf("fixture: %v", err)
	}

	// The store's own encode path: marshal, re-read as a document, stamp the
	// id, then decode as the family type.
	body, err := bson.Marshal(want)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var doc bson.M
	if err := bson.Unmarshal(body, &doc); err != nil {
		t.Fatalf("to document: %v", err)
	}
	doc["_id"] = FamilyBudgetFloor
	stored, err := bson.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal document: %v", err)
	}
	var got budgetfloor.Policy
	if err := bson.Unmarshal(stored, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if len(got.Reservations) != len(want.Reservations) || len(got.RepoQuotas) != len(want.RepoQuotas) {
		t.Fatalf("shape = %d reservations / %d quotas, want %d / %d",
			len(got.Reservations), len(got.RepoQuotas), len(want.Reservations), len(want.RepoQuotas))
	}
	for i, r := range want.Reservations {
		if got.Reservations[i] != r {
			t.Errorf("reservation %d = %+v, want %+v", i, got.Reservations[i], r)
		}
	}
	for i, q := range want.RepoQuotas {
		if got.RepoQuotas[i] != q {
			t.Errorf("repo quota %d = %+v, want %+v", i, got.RepoQuotas[i], q)
		}
	}
	if !got.UpdatedAt.Equal(want.UpdatedAt) {
		// The CAS token: a write whose token did not survive storage can
		// never match, and every subsequent edit would 409 forever.
		t.Errorf("updated_at = %v, want %v", got.UpdatedAt, want.UpdatedAt)
	}
	if _, ok := got.Reserved("docs-refresh"); !ok {
		t.Error("the reservation with an empty reserve did not survive the trip")
	}

	// And the empty policy, the shape a deployment that cleared everything
	// stores: it must come back reserving and capping nothing, not nil-panic.
	var empty budgetfloor.Policy
	blank, err := bson.Marshal(empty)
	if err != nil {
		t.Fatalf("marshal empty: %v", err)
	}
	var back budgetfloor.Policy
	if err := bson.Unmarshal(blank, &back); err != nil {
		t.Fatalf("decode empty: %v", err)
	}
	if len(back.Reservations) != 0 || len(back.RepoQuotas) != 0 {
		t.Errorf("the empty policy came back as %+v", back)
	}
	if _, held := back.WindowCeiling("any", budgetfloor.WindowFiveHour, 80); held {
		t.Error("the empty policy held a window back")
	}
}
