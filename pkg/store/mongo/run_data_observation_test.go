package mongo

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/store"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// mongoObservedTenantRun creates run id in format f under tenant "T" — the
// creation stamps the requester's tenant — appends events and run.log bytes,
// and returns the store, the run's canonical header, a team-blind ctx and that
// ctx bound to the run's observation. Header and tenant are both read back by
// the canonical parsers before the binding is built.
func mongoObservedTenantRun(t *testing.T, f store.ValueFormat, id string, events int) (*Store, store.RunDataHeader, context.Context, context.Context) {
	t.Helper()
	return mongoObservedTenantRunIn(t, f, id, "T", events)
}

// mongoObservedTenantRunIn is mongoObservedTenantRun for a run that records
// tenant: "" creates it team-blind, so it records no tenant at all — the
// shape every tenant-less run carries — and the binding observes "".
func mongoObservedTenantRunIn(t *testing.T, f store.ValueFormat, id, tenant string, events int) (*Store, store.RunDataHeader, context.Context, context.Context) {
	t.Helper()
	s, _ := valueFormatMongo(t, true)
	tenantCtx := store.WithoutTenantFilter(t.Context())
	if tenant != "" {
		tenantCtx = store.WithIdentity(t.Context(), tenant, "owner")
	}
	a, err := s.CreateRunWithValueFormat(tenantCtx, store.RunCreateSpec{ID: id, WorkflowName: "A", Format: f})
	if err != nil {
		t.Fatal(err)
	}
	writer, err := store.BindRunValueFormat(tenantCtx, a)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < events; i++ {
		if _, err = s.AppendEvent(writer, id, store.Event{Type: "tenant_witness", Data: map[string]any{"i": "v"}}); err != nil {
			t.Fatal(err)
		}
	}
	if err = s.AppendRunLog(writer, id, 0, []byte("original log")); err != nil {
		t.Fatal(err)
	}
	blind := store.WithoutTenantFilter(t.Context())
	h, err := s.ObserveRunDataHeader(blind, id)
	if err != nil {
		t.Fatal(err)
	}
	if h.BirthID == "" || h.Format != f || h.BirthID != a.BirthID {
		t.Fatalf("fixture: canonical header %+v, created run birth %q format %d", h, a.BirthID, f)
	}
	if got := mongoCanonicalTenant(t, s, id); got != tenant {
		t.Fatalf("fixture: canonical tenant %q, want the seeded %q", got, tenant)
	}
	if keys := storedTenantKeys(t, s, id); (tenant == "") != (len(keys) == 0) {
		t.Fatalf("fixture: stored tenant keys %q for tenant %q", keys, tenant)
	}
	o, err := store.CaptureRunObservation(h, tenant, true)
	if err != nil {
		t.Fatal(err)
	}
	bound, err := store.BindRunObservation(blind, o)
	if err != nil {
		t.Fatal(err)
	}
	return s, h, blind, bound
}

// mongoCanonicalTenant reads id's tenant through the retained policy: the
// projection that preserves every tenant_id alias and the strict parser.
func mongoCanonicalTenant(t *testing.T, s *Store, id string) string {
	t.Helper()
	parent, _, err := s.readRunFormatObservationPolicy(store.WithoutTenantFilter(t.Context()), id, false, true)
	if err != nil || parent == nil {
		t.Fatalf("fixture: canonical tenant of %s: %v, parent %v", id, err, parent != nil)
	}
	return parent.TenantID
}

// rewriteMongoRun replaces id's run document with edit(document): the
// original is deleted and the edited copy inserted in its place, every member
// edit does not touch kept with its BSON type.
func rewriteMongoRun(t *testing.T, s *Store, id string, edit func(bson.D) bson.D) {
	t.Helper()
	blind := store.WithoutTenantFilter(t.Context())
	raw, err := s.runs.FindOne(blind, bson.M{"_id": id}).Raw()
	if err != nil {
		t.Fatal(err)
	}
	var doc bson.D
	if err = bson.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	doc = edit(doc)
	if _, err = s.runs.DeleteOne(blind, bson.M{"_id": id}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.runs.InsertOne(blind, doc); err != nil {
		t.Fatal(err)
	}
}

// withTenantID replaces the tenant_id members of a run document with one
// holding tenant; "" leaves none, the shape a run that records no tenant
// carries.
func withTenantID(tenant string) func(bson.D) bson.D {
	return func(doc bson.D) bson.D {
		kept := bson.D{}
		for _, e := range doc {
			if e.Key != "tenant_id" {
				kept = append(kept, e)
			}
		}
		if tenant == "" {
			return kept
		}
		return append(kept, bson.E{Key: "tenant_id", Value: tenant})
	}
}

// retenantMongoRun forges B in place of A: the same document with tenant_id
// set to tenant. It asserts the canonical parsers read the same header and
// the new tenant back.
func retenantMongoRun(t *testing.T, s *Store, id, tenant string, h store.RunDataHeader) {
	t.Helper()
	rewriteMongoRun(t, s, id, withTenantID(tenant))
	got, err := s.ObserveRunDataHeader(store.WithoutTenantFilter(t.Context()), id)
	if err != nil || got != h {
		t.Fatalf("fixture: the forged successor changed the header %+v -> %+v (%v); the header path would refuse first", h, got, err)
	}
	if got := mongoCanonicalTenant(t, s, id); got != tenant {
		t.Fatalf("fixture: canonical tenant of the forged successor %q, want %q", got, tenant)
	}
}

// storedTenantKeys lists, in stored order, the top-level keys of id's run
// document that fold to tenant_id: the shape a forged fixture must hold,
// read without the parser under test.
func storedTenantKeys(t *testing.T, s *Store, id string) []string {
	t.Helper()
	raw, err := s.runs.FindOne(store.WithoutTenantFilter(t.Context()), bson.M{"_id": id}).Raw()
	if err != nil {
		t.Fatal(err)
	}
	elements, err := raw.Elements()
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for _, e := range elements {
		if strings.EqualFold(e.Key(), "tenant_id") {
			keys = append(keys, e.Key())
		}
	}
	return keys
}

func mongoScanCount(ctx context.Context, s *Store, id string) (int, error) {
	n := 0
	err := s.ScanEvents(ctx, id, func(*store.Event) bool { n++; return true })
	return n, err
}

func mongoLoadedTenant(r *store.Run) string {
	if r == nil {
		return "<no run>"
	}
	return r.TenantID
}

// TestMongoBoundReadsRefuseAnotherTenant: run A records the observed tenant
// and a team-blind ctx is bound to its observation; B replaces A with the
// same birth, format and protocol and the successor's tenant, so every header
// compare agrees and no tenant filter hides B. Each bound native read refuses
// ErrRunDataIdentityChanged and delivers nothing; the same reads unbound, or
// under a header-only binding, return B as the team-blind legacy does. ""
// is a value, not "unobserved": a run that records no tenant binds "" and
// refuses a successor recording "T", and a binding of "T" refuses a
// successor recording none.
func TestMongoBoundReadsRefuseAnotherTenant(t *testing.T) {
	for _, f := range []store.ValueFormat{store.LegacyValueFormat, store.ExactValueFormat} {
		for _, tc := range []struct{ observed, successor string }{{"T", "U"}, {"", "T"}, {"T", ""}} {
			t.Run(fmt.Sprintf("f%d/observed=%q/successor=%q", f, tc.observed, tc.successor), func(t *testing.T) {
				mongoBoundReadsRefuseAnotherTenant(t, f, tc.observed, tc.successor)
			})
		}
	}
}

func mongoBoundReadsRefuseAnotherTenant(t *testing.T, f store.ValueFormat, observed, successor string) {
	const id = "r"
	s, h, blind, bound := mongoObservedTenantRunIn(t, f, id, observed, 3)
	if r, err := s.LoadRun(bound, id); err != nil || r.TenantID != observed {
		t.Fatalf("control: bound load of the observed run = tenant %q, %v", mongoLoadedTenant(r), err)
	}
	if n, err := mongoScanCount(bound, s, id); err != nil || n != 3 {
		t.Fatalf("control: bound scan of the observed run = %d, %v", n, err)
	}
	if got, err := s.ReadRunLogRange(bound, id, 0, 0); err != nil || string(got) != "original log" {
		t.Fatalf("control: bound run.log of the observed run = %q, %v", got, err)
	}
	if sel, err := s.ResolveStreamSelection(bound, id); err != nil || sel.EventStreamObservation().TenantID() != observed {
		t.Fatalf("control: bound selection of the observed run: %v", err)
	}

	retenantMongoRun(t, s, id, successor, h)

	pair := fmt.Sprintf("bound to tenant %q, B records %q", observed, successor)
	// LoadRun (loadRunValueRecord).
	if r, err := s.LoadRun(bound, id); !errors.Is(err, store.ErrRunDataIdentityChanged) {
		t.Errorf("LoadRun: %s: loaded tenant %q, %v, want ErrRunDataIdentityChanged", pair, mongoLoadedTenant(r), err)
	}
	// Stream reads, the scan and the selection (readRunFormatObservationPolicy).
	if got, err := s.LoadEvents(bound, id); !errors.Is(err, store.ErrRunDataIdentityChanged) || got != nil {
		t.Errorf("LoadEvents: %s: %d events, %v, want ErrRunDataIdentityChanged", pair, len(got), err)
	}
	if n, err := mongoScanCount(bound, s, id); !errors.Is(err, store.ErrRunDataIdentityChanged) || n != 0 {
		t.Errorf("ScanEvents: %s: %d events, %v, want 0 and ErrRunDataIdentityChanged", pair, n, err)
	}
	if got, err := s.ReadRunLogRange(bound, id, 0, 0); !errors.Is(err, store.ErrRunDataIdentityChanged) || got != nil {
		t.Errorf("run.log: %s: %q, %v, want ErrRunDataIdentityChanged", pair, got, err)
	}
	if sel, err := s.ResolveStreamSelection(bound, id); !errors.Is(err, store.ErrRunDataIdentityChanged) || sel != nil {
		t.Errorf("ResolveStreamSelection: %s: selection %t, %v, want ErrRunDataIdentityChanged", pair, sel != nil, err)
	}
	// The header observation (ObserveRunDataHeader).
	if got, err := s.ObserveRunDataHeader(bound, id); !errors.Is(err, store.ErrRunDataIdentityChanged) {
		t.Errorf("ObserveRunDataHeader: %s: %+v, %v, want ErrRunDataIdentityChanged", pair, got, err)
	}

	headerOnly, err := store.CaptureRunDataBinding(h)
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := store.WithRunDataBinding(blind, headerOnly)
	if err != nil {
		t.Fatal(err)
	}
	if legacy, err = store.WithRunValueFormat(legacy, store.RunValueFormat{RunID: id, Format: f}); err != nil {
		t.Fatal(err)
	}
	for name, ctx := range map[string]context.Context{"unbound": blind, "header-only": legacy} {
		if r, err := s.LoadRun(ctx, id); err != nil || r.TenantID != successor {
			t.Fatalf("%s LoadRun = tenant %q, %v, want B's tenant %q", name, mongoLoadedTenant(r), err, successor)
		}
		if got, err := s.LoadEvents(ctx, id); err != nil || len(got) != 3 {
			t.Fatalf("%s LoadEvents = %d, %v, want 3 events", name, len(got), err)
		}
		if n, err := mongoScanCount(ctx, s, id); err != nil || n != 3 {
			t.Fatalf("%s ScanEvents = %d, %v, want 3 events", name, n, err)
		}
		if got, err := s.ReadRunLogRange(ctx, id, 0, 0); err != nil || string(got) != "original log" {
			t.Fatalf("%s run.log = %q, %v", name, got, err)
		}
		if got, err := s.ObserveRunDataHeader(ctx, id); err != nil || got != h {
			t.Fatalf("%s ObserveRunDataHeader = %+v, %v", name, got, err)
		}
		if sel, err := s.ResolveStreamSelection(ctx, id); err != nil || sel.EventStreamObservation().TenantID() != successor {
			t.Fatalf("%s ResolveStreamSelection: %v", name, err)
		}
	}
}

// TestMongoBoundScanRefusesAnotherTenantMidScan: more events than one scan
// batch; B (A's header, tenant "U") replaces A from the visitor at the batch
// boundary. The next batch, read under the scan's per-batch re-bind of the
// header, still compares the observed tenant and refuses: nothing of B is
// delivered.
func TestMongoBoundScanRefusesAnotherTenantMidScan(t *testing.T) {
	for _, f := range []store.ValueFormat{store.LegacyValueFormat, store.ExactValueFormat} {
		t.Run(fmt.Sprint("f", f), func(t *testing.T) {
			const id = "r"
			s, h, _, bound := mongoObservedTenantRun(t, f, id, 129)
			visited := 0
			err := s.ScanEvents(bound, id, func(*store.Event) bool {
				visited++
				if visited == 128 {
					retenantMongoRun(t, s, id, "U", h)
				}
				return true
			})
			if !errors.Is(err, store.ErrRunDataIdentityChanged) || visited != 128 {
				t.Fatalf("mid-scan: B swapped in after batch one: visited %d, %v, want 128 and ErrRunDataIdentityChanged", visited, err)
			}
		})
	}
}

// TestMongoStreamOperationComparesTheBoundTenant: a selection opened unbound
// serves an operation whose ctx observed another tenant for the same header
// only after comparing it with the document it re-reads.
func TestMongoStreamOperationComparesTheBoundTenant(t *testing.T) {
	const id = "r"
	s, h, blind, boundT := mongoObservedTenantRun(t, store.LegacyValueFormat, id, 1)
	sel, err := s.ResolveStreamSelection(blind, id)
	if err != nil {
		t.Fatal(err)
	}
	if o := sel.EventStreamObservation(); o.Header() != h || o.TenantID() != "T" {
		t.Fatalf("fixture: selection observed %+v tenant %q", o.Header(), o.TenantID())
	}
	if err = sel.Validate(boundT); err != nil {
		t.Fatalf("control: operation bound to the selection's tenant: %v", err)
	}
	oU, err := store.CaptureRunObservation(h, "U", true)
	if err != nil {
		t.Fatal(err)
	}
	boundU, err := store.BindRunObservation(blind, oU)
	if err != nil {
		t.Fatal(err)
	}
	if err = sel.Validate(boundU); !errors.Is(err, store.ErrRunDataIdentityChanged) {
		t.Fatalf("Validate: ctx observed tenant \"U\", the run records \"T\": %v, want ErrRunDataIdentityChanged", err)
	}
	emitted := 0
	err = sel.BackfillEvents(boundU, 0, false, 10, func(events []*store.Event) bool { emitted += len(events); return true })
	if !errors.Is(err, store.ErrRunDataIdentityChanged) || emitted != 0 {
		t.Fatalf("BackfillEvents: ctx observed tenant \"U\", the run records \"T\": %d events, %v, want ErrRunDataIdentityChanged", emitted, err)
	}
}

// TestMongoBoundTenantUsesTheCanonicalParser: under an observed tenant a run
// document whose tenant_id is duplicated, or spelled as a case alias, is an
// invalid identity — a lenient decode reads the observed "T" and would admit
// it. Unbound reads keep the legacy decode.
func TestMongoBoundTenantUsesTheCanonicalParser(t *testing.T) {
	for name, tc := range map[string]struct {
		forge func(bson.D) bson.D
		keys  []string
	}{
		"duplicate": {func(doc bson.D) bson.D {
			return append(append(bson.D{{Key: "tenant_id", Value: "U"}}, doc...), bson.E{Key: "tenant_id", Value: "T"})
		}, []string{"tenant_id", "tenant_id"}},
		"alias": {func(doc bson.D) bson.D {
			return append(bson.D{{Key: "TENANT_ID", Value: "T"}}, doc...)
		}, []string{"TENANT_ID"}},
	} {
		forge := tc.forge
		t.Run(name, func(t *testing.T) {
			const id = "r"
			s, h, blind, bound := mongoObservedTenantRun(t, store.LegacyValueFormat, id, 1)
			rewriteMongoRun(t, s, id, func(doc bson.D) bson.D {
				kept := doc[:0]
				for _, e := range doc {
					if e.Key != "tenant_id" {
						kept = append(kept, e)
					}
				}
				return forge(kept)
			})
			if got, err := s.ObserveRunDataHeader(blind, id); err != nil || got != h {
				t.Fatalf("fixture: forged header %+v, %v, want %+v", got, err, h)
			}
			if got := storedTenantKeys(t, s, id); fmt.Sprint(got) != fmt.Sprint(tc.keys) {
				t.Fatalf("fixture: stored tenant keys %q, want %q", got, tc.keys)
			}
			if r, err := s.LoadRun(blind, id); err != nil || r.TenantID != "T" {
				t.Fatalf("fixture: unbound LoadRun of a %s tenant_id = tenant %q, %v, want the lenient decode to read the observed \"T\"", name, mongoLoadedTenant(r), err)
			}
			if r, err := s.LoadRun(bound, id); !errors.Is(err, store.ErrInvalidRunDataIdentity) {
				t.Errorf("bound LoadRun of a %s tenant_id = tenant %q, %v, want ErrInvalidRunDataIdentity", name, mongoLoadedTenant(r), err)
			}
			if n, err := mongoScanCount(bound, s, id); !errors.Is(err, store.ErrInvalidRunDataIdentity) || n != 0 {
				t.Errorf("bound ScanEvents of a %s tenant_id = %d, %v, want 0 and ErrInvalidRunDataIdentity", name, n, err)
			}
			if got, err := s.ObserveRunDataHeader(bound, id); !errors.Is(err, store.ErrInvalidRunDataIdentity) {
				t.Errorf("bound ObserveRunDataHeader of a %s tenant_id = %+v, %v, want ErrInvalidRunDataIdentity", name, got, err)
			}
		})
	}
}

// TestMongoDocumentCanonicalTenantMatchesTheProjection: the tenant parsed
// from a whole run document (LoadRun's read) and the one parsed from the
// server-side alias projection (every stream and metadata read) agree on
// every shape, the valid and the refused ones.
func TestMongoDocumentCanonicalTenantMatchesTheProjection(t *testing.T) {
	s, _ := valueFormatMongo(t, true)
	blind := store.WithoutTenantFilter(t.Context())
	for i, tc := range []struct {
		name    string
		entries bson.D
		tenant  string
		valid   bool
	}{
		{"missing", nil, "", true},
		{"empty", bson.D{{Key: "tenant_id", Value: ""}}, "", true},
		{"string", bson.D{{Key: "tenant_id", Value: "t"}}, "t", true},
		{"near-misses", bson.D{{Key: "tenantid", Value: "a"}, {Key: "tenant_ids", Value: "b"}, {Key: "xtenant_id", Value: "c"}, {Key: "tenant_ıd", Value: "d"}, {Key: "tenant_id", Value: "t"}}, "t", true},
		{"null", bson.D{{Key: "tenant_id", Value: nil}}, "", false},
		{"number", bson.D{{Key: "tenant_id", Value: int32(2)}}, "", false},
		{"duplicate", bson.D{{Key: "tenant_id", Value: "a"}, {Key: "tenant_id", Value: "b"}}, "", false},
		{"upper", bson.D{{Key: "TENANT_ID", Value: "a"}}, "", false},
		{"mixed", bson.D{{Key: "Tenant_Id", Value: "a"}}, "", false},
		{"alias-and-canonical", bson.D{{Key: "tenant_id", Value: "a"}, {Key: "Tenant_ID", Value: "a"}}, "", false},
		{"dotted-capital-i", bson.D{{Key: "tenant_İd", Value: "a"}}, "", false},
		// Mongo's PCRE `$` matches before a final newline; neither parser
		// reads a newline-suffixed key as tenant_id.
		{"newline-alone", bson.D{{Key: "tenant_id\n", Value: "u"}}, "", true},
		{"newline-with-canonical", bson.D{{Key: "tenant_id", Value: "t"}, {Key: "tenant_id\n", Value: "u"}}, "t", true},
		{"alias-newline-alone", bson.D{{Key: "TENANT_ID\n", Value: "u"}}, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id := fmt.Sprint("tenant-shape-", i)
			doc := append(bson.D{{Key: "_id", Value: id}, {Key: "status", Value: "running"}}, tc.entries...)
			if _, err := s.runs.InsertOne(blind, doc); err != nil {
				t.Fatal(err)
			}
			raw, err := s.runs.FindOne(blind, bson.M{"_id": id}).Raw()
			if err != nil {
				t.Fatal(err)
			}
			whole, wholeErr := documentCanonicalTenant(raw)
			parent, _, projectedErr := s.readRunFormatObservationPolicy(blind, id, false, true)
			projected := ""
			if parent != nil {
				projected = parent.TenantID
			}
			if tc.valid {
				if wholeErr != nil || projectedErr != nil || whole != tc.tenant || projected != tc.tenant {
					t.Fatalf("document %q, %v; projection %q, %v; want %q", whole, wholeErr, projected, projectedErr, tc.tenant)
				}
				return
			}
			if !errors.Is(wholeErr, store.ErrInvalidRunDataIdentity) || !errors.Is(projectedErr, store.ErrInvalidRunDataIdentity) {
				t.Fatalf("document %q, %v; projection %q, %v; want ErrInvalidRunDataIdentity from both", whole, wholeErr, projected, projectedErr)
			}
		})
	}
}

// TestMongoBoundReadsIgnoreANewlineSuffixedTenantKey: a "tenant_id\n" member
// is not a tenant_id for any parser, so the run bound to its observed "T" is
// still that run on every bound read — the server-side projection counts the
// keys the Go parser counts.
func TestMongoBoundReadsIgnoreANewlineSuffixedTenantKey(t *testing.T) {
	for _, f := range []store.ValueFormat{store.LegacyValueFormat, store.ExactValueFormat} {
		t.Run(fmt.Sprint("f", f), func(t *testing.T) {
			const id = "r"
			s, h, blind, bound := mongoObservedTenantRun(t, f, id, 3)
			rewriteMongoRun(t, s, id, func(doc bson.D) bson.D { return append(doc, bson.E{Key: "tenant_id\n", Value: "U"}) })
			if got, err := s.ObserveRunDataHeader(blind, id); err != nil || got != h {
				t.Fatalf("fixture: header after the newline key %+v, %v, want %+v", got, err, h)
			}
			if got := storedTenantKeys(t, s, id); fmt.Sprint(got) != "[tenant_id]" {
				t.Fatalf("fixture: stored tenant keys %q, want only the canonical one", got)
			}
			if r, err := s.LoadRun(bound, id); err != nil || r.TenantID != "T" {
				t.Errorf("LoadRun: tenant %q, %v, want the observed run", mongoLoadedTenant(r), err)
			}
			if got, err := s.LoadEvents(bound, id); err != nil || len(got) != 3 {
				t.Errorf("LoadEvents: %d events, %v, want 3", len(got), err)
			}
			if n, err := mongoScanCount(bound, s, id); err != nil || n != 3 {
				t.Errorf("ScanEvents: %d events, %v, want 3", n, err)
			}
			if got, err := s.ReadRunLogRange(bound, id, 0, 0); err != nil || string(got) != "original log" {
				t.Errorf("run.log: %q, %v, want the observed run's log", got, err)
			}
			if got, err := s.ObserveRunDataHeader(bound, id); err != nil || got != h {
				t.Errorf("ObserveRunDataHeader: %+v, %v, want %+v", got, err, h)
			}
			if sel, err := s.ResolveStreamSelection(bound, id); err != nil || sel.EventStreamObservation().TenantID() != "T" {
				t.Errorf("ResolveStreamSelection: %v, want the observed run's selection", err)
			}
		})
	}
}

// TestMongoBoundReadsRefuseASuccessorWithAnInvalidTenant: B replaces A with a
// new birth and a tenant the strict parser refuses (canonical plus case
// alias). Under A's binding every bound read answers what any successor
// answers, ErrRunDataIdentityChanged — the header comparison comes before the
// tenant parse, so a stream read and LoadRun agree on the sentinel.
func TestMongoBoundReadsRefuseASuccessorWithAnInvalidTenant(t *testing.T) {
	const id = "r"
	s, h, blind, bound := mongoObservedTenantRun(t, store.LegacyValueFormat, id, 1)
	rewriteMongoRun(t, s, id, func(doc bson.D) bson.D {
		out := bson.D{}
		for _, e := range doc {
			switch e.Key {
			case "birth_id":
				e.Value = "successor-birth"
			case "tenant_id":
				continue
			}
			out = append(out, e)
		}
		return append(out, bson.E{Key: "tenant_id", Value: "U"}, bson.E{Key: "Tenant_Id", Value: "U"})
	})
	if got, err := s.ObserveRunDataHeader(blind, id); err != nil || got.BirthID == h.BirthID {
		t.Fatalf("fixture: successor header %+v, %v, want a new birth", got, err)
	}
	if got := storedTenantKeys(t, s, id); fmt.Sprint(got) != "[tenant_id Tenant_Id]" {
		t.Fatalf("fixture: stored tenant keys %q", got)
	}
	if r, err := s.LoadRun(bound, id); !errors.Is(err, store.ErrRunDataIdentityChanged) {
		t.Errorf("LoadRun: tenant %q, %v, want ErrRunDataIdentityChanged", mongoLoadedTenant(r), err)
	}
	if got, err := s.LoadEvents(bound, id); !errors.Is(err, store.ErrRunDataIdentityChanged) || got != nil {
		t.Errorf("LoadEvents: %d events, %v, want ErrRunDataIdentityChanged", len(got), err)
	}
	if n, err := mongoScanCount(bound, s, id); !errors.Is(err, store.ErrRunDataIdentityChanged) || n != 0 {
		t.Errorf("ScanEvents: %d events, %v, want 0 and ErrRunDataIdentityChanged", n, err)
	}
	if got, err := s.ReadRunLogRange(bound, id, 0, 0); !errors.Is(err, store.ErrRunDataIdentityChanged) || got != nil {
		t.Errorf("run.log: %q, %v, want ErrRunDataIdentityChanged", got, err)
	}
	if got, err := s.ObserveRunDataHeader(bound, id); !errors.Is(err, store.ErrRunDataIdentityChanged) {
		t.Errorf("ObserveRunDataHeader: %+v, %v, want ErrRunDataIdentityChanged", got, err)
	}
	if sel, err := s.ResolveStreamSelection(bound, id); !errors.Is(err, store.ErrRunDataIdentityChanged) || sel != nil {
		t.Errorf("ResolveStreamSelection: selection %t, %v, want ErrRunDataIdentityChanged", sel != nil, err)
	}
}
