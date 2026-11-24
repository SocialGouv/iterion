package mongo

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// bindMongoRun binds ctx to the observation of the run id holds now, as the
// server binds an admitted action: its header and its stored tenant.
func bindMongoRun(t *testing.T, s *Store, ctx context.Context, id string) (context.Context, *store.Run) {
	t.Helper()
	r, err := s.LoadRun(ctx, id)
	if err != nil {
		t.Fatalf("LoadRun %s: %v", id, err)
	}
	header := store.RunDataHeader{RunID: r.ID, Format: store.ValueFormat(r.FormatVersion), Protocol: r.DataProtocol, BirthID: r.BirthID}
	o, err := store.CaptureRunObservation(header, r.TenantID, true)
	if err != nil {
		t.Fatal(err)
	}
	bound, err := store.BindRunObservation(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	return bound, r
}

// rawRun reads id's stored document as raw BSON.
func rawRun(t *testing.T, s *Store, ctx context.Context, id string) bson.Raw {
	t.Helper()
	raw, err := s.runs.FindOne(ctx, bson.M{"_id": id}).Raw()
	if err != nil {
		t.Fatalf("read %s: %v", id, err)
	}
	return append(bson.Raw(nil), raw...)
}

// setStoredBirth rewrites id's birth_id: "" removes the key, the historical
// empty birth.
func setStoredBirth(t *testing.T, s *Store, ctx context.Context, id, birth string) {
	t.Helper()
	update := bson.M{"$unset": bson.M{"birth_id": ""}}
	if birth != "" {
		update = bson.M{"$set": bson.M{"birth_id": birth}}
	}
	if _, err := s.runs.UpdateOne(ctx, bson.M{"_id": id}, update); err != nil {
		t.Fatal(err)
	}
}

// boundMetadataRefusal reports whether err is a typed refusal of a bound
// metadata write: another birth, or a header the canonical parser refuses.
func boundMetadataRefusal(err error) bool {
	return errors.Is(err, store.ErrRunDataIdentityChanged) || errors.Is(err, store.ErrInvalidRunDataIdentity)
}

// TestBoundMetadataWriteMatchesTheParser: under a run data binding, a write
// through updateRunMetadata or findOneAndUpdateRunMetadata lands exactly when
// store.RunDataHeaderFromBSON reads the bound birth from the document the
// write meets, the document swapped after the guard's own read; every miss is
// a typed refusal, never a silent no-op. "" is the historical empty birth.
func TestBoundMetadataWriteMatchesTheParser(t *testing.T) {
	s, ctx := valueFormatMongo(t, true)
	shapes := []struct {
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
		{"newline-alone", bson.D{{Key: "birth_id\n", Value: "x"}}},
		{"newline-with-canonical", bson.D{{Key: "birth_id", Value: "x"}, {Key: "birth_id\n", Value: "y"}}},
		{"alias-newline-with-canonical", bson.D{{Key: "birth_id", Value: "x"}, {Key: "BIRTH_ID\n", Value: "x"}}},
	}
	births := []string{"", "x", "y", "$x"}
	entries := []struct {
		name  string
		write func(ctx context.Context, id string) error
	}{
		{"updateRunMetadata", func(ctx context.Context, id string) error {
			_, err := s.updateRunMetadata(ctx, id, bson.M{"_id": id}, bson.M{"$set": bson.M{"probe": true}})
			return err
		}},
		{"findOneAndUpdateRunMetadata", func(ctx context.Context, id string) error {
			res, err := s.findOneAndUpdateRunMetadata(ctx, id, bson.M{"_id": id}, bson.M{"$set": bson.M{"probe": true}})
			if err != nil {
				return err
			}
			return res.Err()
		}},
	}
	t.Cleanup(func() { s.afterRunMetadataPrepare = nil })
	landed, identity, parser, n := 0, 0, 0, 0
	for i, shape := range shapes {
		for j, birth := range births {
			for k, entry := range entries {
				n++
				id := fmt.Sprintf("bound-meta-%d-%d-%d", i, j, k)
				t.Run(fmt.Sprintf("%s/birth=%q/%s", shape.name, birth, entry.name), func(t *testing.T) {
					if _, err := s.CreateRun(ctx, id, "wf", nil); err != nil {
						t.Fatal(err)
					}
					setStoredBirth(t, s, ctx, id, birth)
					bound, a := bindMongoRun(t, s, ctx, id)
					if a.BirthID != birth {
						t.Fatalf("fixture: %s reads back birth %q, want %q", id, a.BirthID, birth)
					}
					var header store.RunDataHeader
					var parseErr error
					fired := false
					s.afterRunMetadataPrepare = func(runID string) {
						if runID != id || fired {
							return
						}
						fired = true
						current := rawRun(t, s, ctx, id)
						elements, err := current.Elements()
						if err != nil {
							t.Fatal(err)
						}
						swapped := bson.D{}
						for _, el := range elements {
							if el.Key() != "birth_id" {
								swapped = append(swapped, bson.E{Key: el.Key(), Value: el.Value()})
							}
						}
						swapped = append(swapped, shape.births...)
						if _, err := s.runs.ReplaceOne(ctx, bson.M{"_id": id}, swapped); err != nil {
							t.Fatal(err)
						}
						header, parseErr = store.RunDataHeaderFromBSON(ctx, rawRun(t, s, ctx, id))
					}
					err := entry.write(bound, id)
					s.afterRunMetadataPrepare = nil
					if !fired {
						t.Fatal("the seam never ran: the matrix row is vacuous")
					}
					want := parseErr == nil && header.BirthID == birth
					_, probed := rawRun(t, s, ctx, id).LookupErr("probe")
					wrote := probed == nil
					if wrote != want {
						t.Fatalf("bound birth %q on shape %s: write landed=%t, parser (birth %q, err %v) wants %t (write err %v)",
							birth, shape.name, wrote, header.BirthID, parseErr, want, err)
					}
					switch {
					case want:
						if err != nil {
							t.Fatalf("a landed bound write answered %v", err)
						}
						landed++
					case !boundMetadataRefusal(err):
						t.Fatalf("a refused bound write on shape %s answered %v, want a typed refusal", shape.name, err)
					case errors.Is(err, store.ErrRunDataIdentityChanged):
						identity++
					default:
						parser++
					}
				})
			}
		}
	}
	// Non-vacuous: the matrix lands, refuses another birth and refuses
	// shapes the parser refuses, on both entry points.
	if n != len(shapes)*len(births)*len(entries) || landed < 12 || identity < 24 || parser < 24 {
		t.Fatalf("matrix too thin: %d rows, %d landed, %d identity refusals, %d parser refusals", n, landed, identity, parser)
	}
}

// boundMetadataOrigin creates the original run A of a W12b row.
type boundMetadataOrigin struct {
	name   string
	create func(t *testing.T, s *Store, ctx context.Context, id string)
}

func boundMetadataOrigins() []boundMetadataOrigin {
	return []boundMetadataOrigin{
		{"legacy", func(t *testing.T, s *Store, ctx context.Context, id string) {
			if _, err := s.CreateRun(ctx, id, "wf", nil); err != nil {
				t.Fatal(err)
			}
		}},
		{"exact", func(t *testing.T, s *Store, ctx context.Context, id string) {
			if _, err := s.CreateRunWithValueFormat(ctx, store.RunCreateSpec{ID: id, WorkflowName: "wf", Format: 2}); err != nil {
				t.Fatal(err)
			}
		}},
		{"historical", func(t *testing.T, s *Store, ctx context.Context, id string) {
			if _, err := s.CreateRun(ctx, id, "wf", nil); err != nil {
				t.Fatal(err)
			}
			setStoredBirth(t, s, ctx, id, "")
		}},
	}
}

// boundMetadataCaller is one store method the server calls under an admitted
// run's binding and that writes through the metadata guard.
type boundMetadataCaller struct {
	name string
	// seed puts the successor in the state the call needs to land, unbound.
	seed func(t *testing.T, s *Store, ctx context.Context, id string)
	call func(ctx context.Context, s *Store, id string) error
	// landed reports the call's effect on the stored document.
	landed func(raw bson.Raw) bool
}

func stringField(raw bson.Raw, key string) string {
	v, err := raw.LookupErr(key)
	if err != nil {
		return ""
	}
	str, _ := v.StringValueOK()
	return str
}

func watchedIssueIDs(raw bson.Raw) []string {
	v, err := raw.LookupErr("watched_issue_ids")
	if err != nil {
		return nil
	}
	values, err := v.Array().Values()
	if err != nil {
		return nil
	}
	ids := make([]string, 0, len(values))
	for _, value := range values {
		ids = append(ids, value.StringValue())
	}
	return ids
}

func boundMetadataCallers() []boundMetadataCaller {
	none := func(*testing.T, *Store, context.Context, string) {}
	return []boundMetadataCaller{
		{"UpdateRunOutcome", none, func(ctx context.Context, s *Store, id string) error {
			changed, err := s.UpdateRunOutcome(ctx, id, store.RunStatusCancelled, "stopped", store.RunOutcomeMeta{}, []store.RunStatus{store.RunStatusQueued})
			if err == nil && !changed {
				return errors.New("outcome not written")
			}
			return err
		}, func(raw bson.Raw) bool { return stringField(raw, "status") == string(store.RunStatusCancelled) }},
		{"AddWatchedIssues", none, func(ctx context.Context, s *Store, id string) error {
			_, err := s.AddWatchedIssues(ctx, id, []string{"issue-1"})
			return err
		}, func(raw bson.Raw) bool { ids := watchedIssueIDs(raw); return len(ids) == 1 && ids[0] == "issue-1" }},
		{"RemoveWatchedIssues", func(t *testing.T, s *Store, ctx context.Context, id string) {
			if _, err := s.AddWatchedIssues(ctx, id, []string{"issue-1"}); err != nil {
				t.Fatal(err)
			}
		}, func(ctx context.Context, s *Store, id string) error {
			_, err := s.RemoveWatchedIssues(ctx, id, []string{"issue-1"})
			return err
		}, func(raw bson.Raw) bool { return len(watchedIssueIDs(raw)) == 0 }},
		{"FlipToQueued", func(t *testing.T, s *Store, ctx context.Context, id string) {
			if err := s.UpdateRunStatus(ctx, id, store.RunStatusFailed, "boom"); err != nil {
				t.Fatal(err)
			}
		}, func(ctx context.Context, s *Store, id string) error {
			_, flipped, err := s.FlipToQueued(ctx, id, store.RunStatusFailed, time.Now())
			if err == nil && !flipped {
				return errors.New("not flipped")
			}
			return err
		}, func(raw bson.Raw) bool { return stringField(raw, "status") == string(store.RunStatusQueued) }},
		{"SetRunBudgetOverrides", none, func(ctx context.Context, s *Store, id string) error {
			return s.SetRunBudgetOverrides(ctx, id, &store.RunBudgetOverrides{MaxTokens: 77})
		}, func(raw bson.Raw) bool {
			v, err := raw.LookupErr("budget_overrides", "max_tokens")
			return err == nil && v.AsInt64() == 77
		}},
		{"SetRunBudgetSnapshot", none, func(ctx context.Context, s *Store, id string) error {
			return s.SetRunBudgetSnapshot(ctx, id, &store.RunBudget{MaxTokens: 88})
		}, func(raw bson.Raw) bool {
			v, err := raw.LookupErr("budget", "max_tokens")
			return err == nil && v.AsInt64() == 88
		}},
		{"ClaimMerge", none, func(ctx context.Context, s *Store, id string) error {
			claimed, _, _, err := s.ClaimMerge(ctx, id, time.Now().Add(-15*time.Minute), store.ExpectedBirth{})
			if err == nil && !claimed {
				return errors.New("not claimed")
			}
			return err
		}, func(raw bson.Raw) bool { return stringField(raw, "merge_status") == string(store.MergeStatusMerging) }},
		{"UpdateRunMergeIf", func(t *testing.T, s *Store, ctx context.Context, id string) {
			if changed, err := s.UpdateRunMergeIf(ctx, id, store.RunMergeUpdate{Status: store.MergeStatusPending}, []store.MergeStatus{""}); err != nil || !changed {
				t.Fatalf("seed pending = (%t, %v)", changed, err)
			}
		}, func(ctx context.Context, s *Store, id string) error {
			changed, err := s.UpdateRunMergeIf(ctx, id, store.RunMergeUpdate{Status: store.MergeStatusSkipped}, []store.MergeStatus{store.MergeStatusPending})
			if err == nil && !changed {
				return errors.New("merge exit not written")
			}
			return err
		}, func(raw bson.Raw) bool { return stringField(raw, "merge_status") == string(store.MergeStatusSkipped) }},
	}
}

// TestBoundMetadataCallersRefuseASuccessor: every store method the server
// calls under an admitted run's binding and that writes through the metadata
// guard refuses, typed, a successor recreated under the same ID after the
// guard's own read — the successor's document byte-equal — for both value
// formats and for a historical (empty birth) original. The merge doors are
// driven without their own expected birth, so the binding alone decides. The
// same calls unbound land on the successor, as before the bound predicate.
func TestBoundMetadataCallersRefuseASuccessor(t *testing.T) {
	s, ctx := valueFormatMongo(t, true)
	t.Cleanup(func() { s.afterRunMetadataPrepare = nil })
	for _, origin := range boundMetadataOrigins() {
		for _, caller := range boundMetadataCallers() {
			for _, bound := range []bool{true, false} {
				mode := "unbound"
				if bound {
					mode = "bound"
				}
				id := fmt.Sprintf("bound-caller-%s-%s-%s", origin.name, caller.name, mode)
				t.Run(origin.name+"/"+caller.name+"/"+mode, func(t *testing.T) {
					origin.create(t, s, ctx, id)
					callCtx, a := bindMongoRun(t, s, ctx, id)
					if !bound {
						callCtx = ctx
					}
					var successor bson.Raw
					fired := false
					s.afterRunMetadataPrepare = func(runID string) {
						if runID != id || fired {
							return
						}
						fired = true
						if err := s.DeleteRun(ctx, id); err != nil {
							t.Fatalf("DeleteRun: %v", err)
						}
						if _, err := s.PruneDeletionMarkers(ctx, time.Now().Add(time.Hour)); err != nil {
							t.Fatalf("PruneDeletionMarkers: %v", err)
						}
						b, err := s.CreateRunWithValueFormat(ctx, store.RunCreateSpec{ID: id, WorkflowName: "wf", Format: store.ValueFormat(a.FormatVersion)})
						if err != nil {
							t.Fatalf("recreate %s: %v", id, err)
						}
						if b.BirthID == "" || b.BirthID == a.BirthID {
							t.Fatalf("fixture: the successor's birth %q does not tell it from the original's %q", b.BirthID, a.BirthID)
						}
						caller.seed(t, s, ctx, id)
						successor = rawRun(t, s, ctx, id)
					}
					err := caller.call(callCtx, s, id)
					s.afterRunMetadataPrepare = nil
					if !fired {
						t.Fatal("the seam never ran: the row is vacuous")
					}
					after := rawRun(t, s, ctx, id)
					if bound {
						if !bytes.Equal(after, successor) {
							t.Fatalf("%s bound to the original changed its successor's document (err %v)", caller.name, err)
						}
						if !errors.Is(err, store.ErrRunDataIdentityChanged) {
							t.Fatalf("%s bound to the original on its successor = %v, want ErrRunDataIdentityChanged", caller.name, err)
						}
						return
					}
					if err != nil || !caller.landed(after) {
						t.Fatalf("unbound %s after the replacement = %v, landed %t; want it to land on the successor", caller.name, err, caller.landed(after))
					}
				})
			}
		}
	}
}
