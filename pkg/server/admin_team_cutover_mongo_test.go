package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/SocialGouv/iterion/pkg/internal/mongotest"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/store/blob"
	mongostore "github.com/SocialGouv/iterion/pkg/store/mongo"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// Artifact methods are deliberately unavailable: this test exercises only real
// Mongo metadata and the real mapping handler, which must never read a blob.
type cutoverMetadataOnlyBlob struct{ blob.Client }

func TestAdminTeamCutoverMongoTenantMetadata(t *testing.T) {
	uri := os.Getenv("ITERION_TEST_MONGO_URI")
	if uri == "" {
		t.Skip("ITERION_TEST_MONGO_URI not set; Mongo cutover metadata unverified")
	}
	for _, shape := range []string{"array-running", "array-finished", "scalar-running", "scalar-finished", "foreign-array"} {
		for _, force := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s-force%t", shape, force), func(t *testing.T) {
				s, _, done := newPoolMappingTestServer(t)
				defer done()
				ctx, cancel := mongotest.Ctx(t)
				defer cancel()
				db := "iterion_cutover_tenant_" + bson.NewObjectID().Hex()
				runs, e := mongostore.New(ctx, mongostore.Config{URI: uri, Database: db, Blob: &cutoverMetadataOnlyBlob{}})
				if e != nil {
					t.Fatal(e)
				}
				defer func() {
					clean, c := mongotest.TeardownCtx()
					defer c()
					_ = runs.DB().Drop(clean)
					_ = runs.Close(clean)
				}()
				var tenant any = "t1"
				status := "running"
				unknown := false
				switch shape {
				case "array-running":
					tenant = bson.A{"t1", "foreign"}
					unknown = true
				case "array-finished":
					tenant = bson.A{"t1", "foreign"}
					status = "finished"
					unknown = true
				case "scalar-finished":
					status = "finished"
				case "foreign-array":
					tenant = bson.A{"foreign", "other"}
				}
				doc := bson.M{"_id": "candidate", "tenant_id": tenant, "status": status}
				if _, e := runs.DB().Collection("runs").InsertOne(ctx, doc); e != nil {
					t.Fatal(e)
				}
				ids, readErr := runs.ListAliveRunsForTeam(store.WithTenant(ctx, "t1"), "t1")
				t.Logf("shape=%s doc=%v ids=%v readErr=%v", shape, doc, ids, readErr)
				if unknown && readErr == nil {
					t.Errorf("non-scalar matching tenant became known metadata")
				}
				if !unknown && readErr != nil {
					t.Errorf("legitimate control refused: %v", readErr)
				}
				s.cfg.Store = runs
				path := "/api/admin/teams/t1/runner-pool"
				if force {
					path += "?force=1"
				}
				w := httptest.NewRecorder()
				s.handleAdminSetTeamRunnerPool(w, orgReq(superAdminCtx(), http.MethodPut, path, `{"runner_pool":"alpha"}`, "t1"))
				team, e := s.authStore().GetTeam(ctx, "t1")
				if e != nil {
					t.Fatal(e)
				}
				want := http.StatusOK
				if unknown {
					want = http.StatusServiceUnavailable
				} else if shape == "scalar-running" && !force {
					want = http.StatusConflict
				}
				t.Logf("shape=%s force=%t HTTP=%d mapping=%q", shape, force, w.Code, team.RunnerPool)
				if w.Code != want {
					t.Errorf("cutover unknown/known classification HTTP=%d want=%d mapping=%q", w.Code, want, team.RunnerPool)
				}
				if want != http.StatusOK && team.RunnerPool != "" {
					t.Errorf("refused cutover changed mapping to %q", team.RunnerPool)
				}
			})
		}
	}
}
