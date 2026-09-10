package server

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"testing"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
)

// TestArtifactEndpointsWithoutLocalDirectory is the cloud server pod at the
// HTTP surface: the run and its artifacts live in the shared store, but the
// pod's own store directory has no runs/<id>/artifacts tree (the runner that
// wrote them has it). Both artifact endpoints must serve from the store —
// the aggregate listing from the run's artifact_index, and the per-node
// version list the studio drills into from the store's version enumeration.
// Serving either as empty is what made the Artifacts view read as "this run
// published nothing", or its cards fail to open.
func TestArtifactEndpointsWithoutLocalDirectory(t *testing.T) {
	srv, hs := newTestServer(t)

	// The shared store the run actually lives in — a different root from
	// the service's own store directory, which is what makes the local
	// artifact directory absent.
	shared, err := store.New(t.TempDir(), store.WithLogger(iterlog.New(iterlog.LevelError, os.Stderr)))
	if err != nil {
		t.Fatalf("shared store: %v", err)
	}
	ctx := context.Background()
	if _, err := shared.CreateRun(ctx, "cloudrun", "wf", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	for _, v := range []int{0, 3} {
		if err := shared.WriteArtifact(ctx, &store.Artifact{
			RunID: "cloudrun", NodeID: "report", Version: v,
			Labels: []string{"plan"}, Data: map[string]any{"title": "Migration plan"},
		}); err != nil {
			t.Fatalf("WriteArtifact v%d: %v", v, err)
		}
	}

	// The service's own store directory is a fresh, empty tree — so every
	// artifact read must go through the injected shared store, exactly as
	// on a pod that never wrote these files.
	stopRunviewOnCleanup(t, srv.runs) // the one newTestServer built, now replaced
	srv.runs = newTestRunviewService(t, t.TempDir(),
		runview.WithLogger(iterlog.New(iterlog.LevelError, os.Stderr)),
		runview.WithStore(shared))

	// The aggregate listing: one card for the report node, at its latest
	// version — not an empty list.
	var all struct {
		Artifacts []runview.RunArtifactSummary `json:"artifacts"`
	}
	getJSON(t, hs.URL+"/api/runs/cloudrun/artifacts", &all)
	if len(all.Artifacts) != 1 {
		t.Fatalf("aggregate listing = %+v, want 1 entry", all.Artifacts)
	}
	if all.Artifacts[0].NodeID != "report" || all.Artifacts[0].Version != 3 {
		t.Errorf("aggregate entry = %+v, want report v3", all.Artifacts[0])
	}

	// The per-node drill-in behind that card: both versions, so the studio
	// picker cannot fall back to a v1 that does not exist.
	var versions struct {
		Artifacts []runview.ArtifactSummary `json:"artifacts"`
	}
	getJSON(t, hs.URL+"/api/runs/cloudrun/artifacts/report", &versions)
	if len(versions.Artifacts) != 2 || versions.Artifacts[0].Version != 0 || versions.Artifacts[1].Version != 3 {
		t.Fatalf("version list = %+v, want v0 and v3", versions.Artifacts)
	}
}

// getJSON performs a GET, asserts 200, and decodes the body into out.
func getJSON(t *testing.T, url string, out any) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: status %d, want 200", url, resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		t.Fatalf("decode %s: %v", url, err)
	}
}
