package secrets

import (
	"context"
	"errors"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/SocialGouv/iterion/pkg/store"
)

// run_secrets holds the sealed credential bundles and is addressed by a
// ref that is the document `_id`, so the tenant clause is the only thing
// keeping a read or a delete inside one team. These tests judge the
// clause the store BUILDS, and judge the memory store against the same
// table, so a caller that forgot its stamp cannot be proved innocent by
// a kinder fixture.

// ctxShape is one context the stores can be called with, and the verdict
// both of them owe it.
type ctxShape struct {
	name string
	ctx  func() context.Context
	// wantErr is the refusal both stores must answer with ("" = served).
	wantErr error
	// wantTenantClause is the tenant_id the Mongo filter must carry
	// ("" = no clause, which only an explicitly unscoped ctx earns).
	wantTenantClause string
}

func runSecretsCtxShapes() []ctxShape {
	return []ctxShape{
		{
			name:    "bare ctx",
			ctx:     func() context.Context { return context.Background() },
			wantErr: ErrRunSecretsTenantMissing,
		},
		{
			name:    "tenant stamped empty",
			ctx:     func() context.Context { return store.WithTenant(context.Background(), "") },
			wantErr: ErrRunSecretsTenantMissing,
		},
		{
			name:             "tenant stamped",
			ctx:              func() context.Context { return store.WithTenant(context.Background(), "team-a") },
			wantTenantClause: "team-a",
		},
		{
			name: "explicitly unscoped (the greppable privileged door)",
			ctx:  func() context.Context { return store.WithoutTenantFilter(context.Background()) },
		},
		{
			name: "team-blind (unscoped and tenant cleared)",
			ctx:  func() context.Context { return store.TeamBlind(context.Background()) },
		},
	}
}

// TestTheRunSecretsFilterFailsClosedOnATenantlessCtx judges the clause the
// Mongo store sends, not a log line: a served shape must carry the
// tenant_id, a refused shape must build no filter at all.
func TestTheRunSecretsFilterFailsClosedOnATenantlessCtx(t *testing.T) {
	for _, s := range runSecretsCtxShapes() {
		t.Run(s.name, func(t *testing.T) {
			filter, err := withRunSecretsTenantFilter(s.ctx(), bson.M{"_id": "ref-1"})
			if s.wantErr != nil {
				if !errors.Is(err, s.wantErr) {
					t.Fatalf("withRunSecretsTenantFilter = (%v, %v), want error %v", filter, err, s.wantErr)
				}
				if filter != nil {
					t.Fatalf("a refused ctx still produced a filter: %v", filter)
				}
				return
			}
			if err != nil {
				t.Fatalf("withRunSecretsTenantFilter: %v", err)
			}
			if filter["_id"] != "ref-1" {
				t.Fatalf("the base clause was dropped: %v", filter)
			}
			got, has := filter["tenant_id"]
			if s.wantTenantClause == "" {
				if has {
					t.Fatalf("an explicitly unscoped ctx produced tenant_id=%v", got)
				}
				return
			}
			if !has || got != s.wantTenantClause {
				t.Fatalf("filter tenant_id = %v (present=%v), want %q", got, has, s.wantTenantClause)
			}
		})
	}
}

// TestTheMemoryRunSecretsStoreRefusesWhatMongoRefuses holds the half that
// makes the fixture usable as evidence: for every ctx shape above, the
// memory store's Get and Delete answer the refusal the Mongo filter
// answers. A memory store kinder than production would let a caller that
// forgot its tenant stamp pass its own test suite — the class this
// ticket already paid for once (item 26).
func TestTheMemoryRunSecretsStoreRefusesWhatMongoRefuses(t *testing.T) {
	for _, s := range runSecretsCtxShapes() {
		t.Run(s.name, func(t *testing.T) {
			st := NewMemoryRunSecretsStore()
			if err := st.Put(context.Background(), RunSecretsRecord{ID: "ref-1", TenantID: "team-a", RunID: "run-1"}); err != nil {
				t.Fatalf("Put: %v", err)
			}

			_, getErr := st.Get(s.ctx(), "ref-1")
			delErr := st.Delete(s.ctx(), "ref-1")

			if s.wantErr != nil {
				if !errors.Is(getErr, s.wantErr) {
					t.Fatalf("memory Get = %v, want %v (Mongo refuses this ctx)", getErr, s.wantErr)
				}
				if !errors.Is(delErr, s.wantErr) {
					t.Fatalf("memory Delete = %v, want %v (Mongo refuses this ctx)", delErr, s.wantErr)
				}
				// The refusal is not a silent no-op: the record survives.
				if _, err := st.Get(store.WithTenant(context.Background(), "team-a"), "ref-1"); err != nil {
					t.Fatalf("a refused Delete removed the record anyway: %v", err)
				}
				return
			}
			if errors.Is(getErr, ErrRunSecretsTenantMissing) || errors.Is(delErr, ErrRunSecretsTenantMissing) {
				t.Fatalf("memory store refused a ctx Mongo serves: get=%v delete=%v", getErr, delErr)
			}
		})
	}
}

// TestEveryRunSecretsStoreImplementationIsJudgedHere keeps the table
// above from going blind to a third implementation: both stores of this
// package are driven by the shapes, so a new RunSecretsStore has to join
// them rather than inherit silence.
func TestEveryRunSecretsStoreImplementationIsJudgedHere(t *testing.T) {
	var _ RunSecretsStore = (*MemoryRunSecretsStore)(nil)
	var _ RunSecretsStore = (*MongoRunSecretsStore)(nil)
	if len(runSecretsCtxShapes()) < 5 {
		t.Fatalf("the ctx shape table shrank to %d: the refused shapes are the point", len(runSecretsCtxShapes()))
	}
}
