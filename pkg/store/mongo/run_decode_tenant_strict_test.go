package mongo

import (
	"errors"
	"fmt"
	"testing"

	"github.com/SocialGouv/iterion/pkg/store"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// replaceTenantMembers drops every tenant_id member of a run document and
// appends entries in their place, so a fixture holds exactly the tenant keys
// it names.
func replaceTenantMembers(entries bson.D) func(bson.D) bson.D {
	return func(doc bson.D) bson.D {
		kept := bson.D{}
		for _, e := range doc {
			if e.Key != "tenant_id" {
				kept = append(kept, e)
			}
		}
		return append(kept, entries...)
	}
}

// TestMongoUnboundRunDecodeAppliesTheStrictTenantRule: decodeRunValueRecord
// reads a run's tenant by the strict canonical rule on every read, bound or
// not (#3830). The struct decoder alone matches keys case-insensitively, so a
// document stored under an alias or a duplicate tenant key would decode under
// a tenant Mongo never filtered on. LoadRun and LoadRunForListing refuse such
// a document with ErrInvalidRunDataIdentity, and the window listing reports it
// as a skipped run with the same sentinel; the legitimate shapes — no tenant
// key (a tenantless run), one exact tenant_id string, empty included — keep
// decoding to the stored tenant on all three reads.
func TestMongoUnboundRunDecodeAppliesTheStrictTenantRule(t *testing.T) {
	for _, f := range []store.ValueFormat{store.LegacyValueFormat, store.ExactValueFormat} {
		t.Run(fmt.Sprint("f", f), func(t *testing.T) {
			s, _ := valueFormatMongo(t, true)
			blind := store.TeamBlind(t.Context())
			cases := []struct {
				name    string
				entries bson.D
				tenant  string
				valid   bool
			}{
				{"canonical", bson.D{{Key: "tenant_id", Value: "T"}}, "T", true},
				{"missing", nil, "", true},
				{"empty", bson.D{{Key: "tenant_id", Value: ""}}, "", true},
				{"alias-only", bson.D{{Key: "TENANT_ID", Value: "T"}}, "", false},
				{"canonical-then-alias", bson.D{{Key: "tenant_id", Value: "T"}, {Key: "Tenant_Id", Value: "U"}}, "", false},
				{"duplicate", bson.D{{Key: "tenant_id", Value: "U"}, {Key: "tenant_id", Value: "T"}}, "", false},
				{"null", bson.D{{Key: "tenant_id", Value: nil}}, "", false},
				{"number", bson.D{{Key: "tenant_id", Value: int32(7)}}, "", false},
			}
			byID := map[string]int{}
			for i, tc := range cases {
				id := fmt.Sprintf("strict-%d-%s", i, tc.name)
				byID[id] = i
				if _, err := s.CreateRunWithValueFormat(store.WithIdentity(t.Context(), "T", "owner"), store.RunCreateSpec{ID: id, WorkflowName: "wf", Format: f}); err != nil {
					t.Fatalf("create %s: %v", id, err)
				}
				rewriteMongoRun(t, s, id, replaceTenantMembers(tc.entries))
			}
			for id, i := range byID {
				tc := cases[i]
				for name, load := range map[string]func() (*store.Run, error){
					"LoadRun":           func() (*store.Run, error) { return s.LoadRun(blind, id) },
					"LoadRunForListing": func() (*store.Run, error) { return s.LoadRunForListing(blind, id) },
				} {
					r, err := load()
					if tc.valid {
						if err != nil || r.TenantID != tc.tenant {
							t.Errorf("%s of %s: tenant %q, %v, want %q", name, id, mongoLoadedTenant(r), err, tc.tenant)
						}
						continue
					}
					if !errors.Is(err, store.ErrInvalidRunDataIdentity) {
						t.Errorf("%s of %s: tenant %q, %v, want ErrInvalidRunDataIdentity", name, id, mongoLoadedTenant(r), err)
					}
				}
			}

			for _, w := range []store.RunWindow{{}, {TenantIDs: []string{"T", "U"}}} {
				runs, skipped, err := s.ListRunWindow(blind, w)
				if err != nil {
					t.Fatalf("ListRunWindow %+v: %v", w, err)
				}
				for _, r := range runs {
					i, ok := byID[r.ID]
					if !ok {
						continue
					}
					if tc := cases[i]; !tc.valid || r.TenantID != tc.tenant {
						t.Errorf("ListRunWindow %+v listed %s under tenant %q, want valid=%t tenant %q", w, r.ID, r.TenantID, tc.valid, tc.tenant)
					}
				}
				refused := map[string]bool{}
				for _, sk := range skipped {
					if !errors.Is(sk.Err, store.ErrInvalidRunDataIdentity) {
						t.Errorf("ListRunWindow %+v skipped %s with %v, want ErrInvalidRunDataIdentity", w, sk.ID, sk.Err)
					}
					refused[sk.ID] = true
				}
				if len(w.TenantIDs) > 0 {
					continue
				}
				for id, i := range byID {
					if !cases[i].valid && !refused[id] {
						t.Errorf("ListRunWindow %+v did not report %s as skipped", w, id)
					}
				}
			}
		})
	}
}
