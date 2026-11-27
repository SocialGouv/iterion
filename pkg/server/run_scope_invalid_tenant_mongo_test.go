package server

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
	mongostore "github.com/SocialGouv/iterion/pkg/store/mongo"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// TestRunScopeInvalidTenantAnswersNotFoundNativeMongo: a run document whose
// tenant keys the strict rule refuses (an alias, a duplicate, an alias beside
// the canonical key) has no team anyone stands in. Through the production
// stack (requireAuth -> scopeRunByID -> handler) it answers every caller —
// a non-member and the super-admin alike — the 404 a missing run answers,
// never a status of its own that would tell a stranger the id exists.
// CI's mongo-conformance job supplies the URI.
func TestRunScopeInvalidTenantAnswersNotFoundNativeMongo(t *testing.T) {
	uri := os.Getenv("ITERION_TEST_MONGO_URI")
	if uri == "" {
		t.Skip("ITERION_TEST_MONGO_URI not set; Mongo run scope unverified")
	}
	_, s, hs, adminTok, userTok := newAdminRunsServer(t)
	ctx := store.WithIdentity(t.Context(), "team-b1", "owner")
	ms, err := mongostore.New(ctx, mongostore.Config{URI: uri, Database: fmt.Sprintf("run_scope_tenant_%d", time.Now().UnixNano()), Blob: wsOriginExactBlob{}, RunFilesScratchDir: filepath.Join(t.TempDir(), "scratch")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		_ = ms.DB().Drop(c)
		_ = ms.Close(c)
	})
	s.runs.Stop(t.Context())
	s.runs = newTestRunviewService(t, t.TempDir(), runview.WithRecoveryPassive(), runview.WithStore(ms))

	runs := ms.DB().Collection("runs")
	blind := store.WithoutTenantFilter(t.Context())
	seed := func(id string, tenant bson.D) {
		t.Helper()
		if _, err := ms.CreateRunWithValueFormat(ctx, store.RunCreateSpec{ID: id, WorkflowName: "wf", Format: store.LegacyValueFormat}); err != nil {
			t.Fatal(err)
		}
		if tenant == nil {
			return
		}
		raw, err := runs.FindOne(blind, bson.M{"_id": id}).Raw()
		if err != nil {
			t.Fatal(err)
		}
		var doc bson.D
		if err := bson.Unmarshal(raw, &doc); err != nil {
			t.Fatal(err)
		}
		kept := bson.D{}
		for _, e := range doc {
			if e.Key != "tenant_id" {
				kept = append(kept, e)
			}
		}
		if _, err := runs.DeleteOne(blind, bson.M{"_id": id}); err != nil {
			t.Fatal(err)
		}
		if _, err := runs.InsertOne(blind, append(kept, tenant...)); err != nil {
			t.Fatal(err)
		}
	}
	seed("ok", nil)
	seed("alias", bson.D{{Key: "TENANT_ID", Value: "team-b1"}})
	seed("dup", bson.D{{Key: "tenant_id", Value: "team-b1"}, {Key: "tenant_id", Value: "team-a1"}})
	seed("both", bson.D{{Key: "tenant_id", Value: "team-b1"}, {Key: "Tenant_Id", Value: "team-a1"}})

	// The fixture: the well-formed run is the super-admin's to read and
	// invisible to a member of another team.
	if code, body := llmDo(t, hs, "GET", "/api/runs/ok", adminTok, ""); code != 200 {
		t.Fatalf("fixture: super-admin on the well-formed run: %d %s, want 200", code, body)
	}
	for _, id := range []string{"ok", "nope", "alias", "dup", "both"} {
		for name, tok := range map[string]string{"non-member": userTok, "super-admin": adminTok} {
			if id == "ok" && name == "super-admin" {
				continue
			}
			if code, body := llmDo(t, hs, "GET", "/api/runs/"+id, tok, ""); code != 404 {
				t.Errorf("GET /api/runs/%s as %s: %d %s, want the 404 of a missing run", id, name, code, body)
			}
		}
	}
}
