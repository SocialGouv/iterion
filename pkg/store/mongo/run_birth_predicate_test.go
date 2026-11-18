package mongo

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// TestCanonicalBirthPredicateMatchesTheParser: the merge CAS birth predicate,
// evaluated by the mutating command itself, matches a document for an
// expected birth exactly when store.RunDataHeaderFromBSON reads that birth
// from the same stored document.
func TestCanonicalBirthPredicateMatchesTheParser(t *testing.T) {
	s, ctx := valueFormatMongo(t, true)
	tenant, _ := store.TenantFromContext(ctx)
	cases := []struct {
		name   string
		births bson.D
	}{
		{"absent", nil},
		{"empty", bson.D{{Key: "birth_id", Value: ""}}},
		{"x", bson.D{{Key: "birth_id", Value: "x"}}},
		{"y", bson.D{{Key: "birth_id", Value: "y"}}},
		{"dollar", bson.D{{Key: "birth_id", Value: "$x"}}},
		{"int", bson.D{{Key: "birth_id", Value: int32(7)}}},
		{"null", bson.D{{Key: "birth_id", Value: nil}}},
		{"array", bson.D{{Key: "birth_id", Value: bson.A{"x"}}}},
		{"document", bson.D{{Key: "birth_id", Value: bson.D{{Key: "v", Value: "x"}}}}},
		{"duplicate-same", bson.D{{Key: "birth_id", Value: "x"}, {Key: "birth_id", Value: "x"}}},
		{"duplicate-different", bson.D{{Key: "birth_id", Value: "x"}, {Key: "birth_id", Value: "y"}}},
		{"alias-upper", bson.D{{Key: "BIRTH_ID", Value: "x"}}},
		{"alias-mixed", bson.D{{Key: "Birth_Id", Value: "x"}}},
		{"alias-dotted-i", bson.D{{Key: "BİRTH_İD", Value: "x"}}},
		{"alias-empty", bson.D{{Key: "BIRTH_ID", Value: ""}}},
		{"canonical-and-alias", bson.D{{Key: "birth_id", Value: "x"}, {Key: "BIRTH_ID", Value: "x"}}},
		{"near-miss", bson.D{{Key: "birth_idx", Value: "x"}}},
		{"dotless-i", bson.D{{Key: "bırth_id", Value: "x"}}},
	}
	expectations := []string{"", "x", "y", "$x"}
	refused, matched := 0, 0
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id := fmt.Sprintf("birth-%d", i)
			doc := append(bson.D{{Key: "_id", Value: id}, {Key: "tenant_id", Value: tenant}}, tc.births...)
			if _, err := s.runs.InsertOne(ctx, doc); err != nil {
				t.Fatal(err)
			}
			raw, err := s.runs.FindOne(ctx, bson.M{"_id": id}).Raw()
			if err != nil {
				t.Fatal(err)
			}
			header, parseErr := store.RunDataHeaderFromBSON(ctx, raw)
			if parseErr != nil {
				refused++
			}
			for j, birth := range expectations {
				want := parseErr == nil && header.BirthID == birth
				filter := bson.M{"_id": id, "$and": bson.A{canonicalBirthPredicate(birth)}}
				res, err := s.runs.UpdateOne(ctx, filter, bson.M{"$set": bson.M{fmt.Sprintf("probe_%d", j): true}})
				if err != nil {
					t.Fatal(birth, err)
				}
				if res.MatchedCount == 1 {
					matched++
				}
				if (res.MatchedCount == 1) != want {
					t.Errorf("expected birth %q: predicate matched=%d, parser (birth %q, err %v) wants %t", birth, res.MatchedCount, header.BirthID, parseErr, want)
				}
			}
		})
	}
	// Non-vacuous: the matrix exercises both parser refusals and matches.
	if refused < 8 || matched < 6 {
		t.Fatalf("matrix too thin: %d parser refusals, %d matches", refused, matched)
	}
}

// TestMergeExpectedHistoricalBirthMongo: "" is the historical empty birth,
// never "unset". An expectation of it lands on a document without birth_id
// and is refused, typed, by a successor holding birth "x" — on both merge
// doors, the refusal decided by the mutating command.
func TestMergeExpectedHistoricalBirthMongo(t *testing.T) {
	s, ctx := valueFormatMongo(t, true)
	const id = "historical"
	if _, err := s.CreateRun(ctx, id, "wf", nil); err != nil {
		t.Fatal(err)
	}
	setBirth := func(birth string) {
		t.Helper()
		update := bson.M{"$unset": bson.M{"birth_id": ""}}
		if birth != "" {
			update = bson.M{"$set": bson.M{"birth_id": birth}}
		}
		if _, err := s.runs.UpdateOne(ctx, bson.M{"_id": id}, update); err != nil {
			t.Fatal(err)
		}
		r, err := s.LoadRun(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if r.BirthID != birth {
			t.Fatalf("fixture: rewritten birth reads back as %q, want %q", r.BirthID, birth)
		}
	}
	setBirth("")
	a, err := s.LoadRun(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	expect := store.ExpectBirthOf(a)
	if expect != (store.ExpectedBirth{ID: "", Check: true}) {
		t.Fatalf("fixture: expectation of a historical run = %+v", expect)
	}
	if changed, err := s.UpdateRunMergeIf(ctx, id, store.RunMergeUpdate{Status: store.MergeStatusPending, ExpectBirth: expect}, []store.MergeStatus{""}); err != nil || !changed {
		t.Fatalf("exit expecting the historical birth on a historical run = (%t, %v), want (true, nil)", changed, err)
	}
	claimed, _, token, err := s.ClaimMerge(ctx, id, time.Now().Add(-15*time.Minute), expect)
	if err != nil || !claimed {
		t.Fatalf("claim expecting the historical birth on a historical run = (%t, %v), want (true, nil)", claimed, err)
	}

	setBirth("x")
	changed, err := s.UpdateRunMergeIf(ctx, id, store.RunMergeUpdate{Status: store.MergeStatusFailed, ExpectBirth: expect, ExpectClaimedAt: token}, []store.MergeStatus{store.MergeStatusMerging})
	r, loadErr := s.LoadRun(ctx, id)
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if r.MergeStatus != store.MergeStatusMerging || !r.MergeClaimedAt.Equal(token) {
		t.Fatalf("an exit expecting the historical birth wrote into birth %q: merge_status=%q claimed_at=%v", r.BirthID, r.MergeStatus, r.MergeClaimedAt)
	}
	if !errors.Is(err, store.ErrRunDataIdentityChanged) || changed {
		t.Fatalf("exit expecting the historical birth on birth \"x\" = (%t, %v), want ErrRunDataIdentityChanged", changed, err)
	}
	if claimed, _, _, err = s.ClaimMerge(ctx, id, time.Now().Add(time.Hour), expect); !errors.Is(err, store.ErrRunDataIdentityChanged) || claimed {
		t.Fatalf("claim expecting the historical birth on birth \"x\" = (%t, %v), want ErrRunDataIdentityChanged", claimed, err)
	}
	if r, err = s.LoadRun(ctx, id); err != nil {
		t.Fatal(err)
	}
	if !r.MergeClaimedAt.Equal(token) {
		t.Fatalf("a refused claim restamped birth \"x\": claimed_at=%v, want %v", r.MergeClaimedAt, token)
	}
}
