package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/store"
)

// TestRunTags_Lifecycle pins the /api/runs/:id/tags contract: GET on a run
// with none → empty array (200, not 404); PUT sets the full list and GET
// reads it back (survives reload); over-limit input is a 400; unknown run
// is 404 on both verbs.
func TestRunTags_Lifecycle(t *testing.T) {
	srv, hs := newTestServer(t)
	const runID = "tag-run"
	seedRun(t, srv, runID, "wf", store.RunStatusFinished)

	t.Run("empty when none set", func(t *testing.T) {
		if got := getTags(t, hs.URL, runID); len(got) != 0 {
			t.Fatalf("expected empty tags, got %v", got)
		}
	})

	t.Run("PUT sets and dedups/trims", func(t *testing.T) {
		got := putTags(t, hs.URL, runID, []string{"release", " release ", "flaky", ""})
		want := []string{"release", "flaky"}
		if !equalTags(got, want) {
			t.Fatalf("PUT returned %v, want %v", got, want)
		}
		// Survives a reload (fresh GET).
		if reread := getTags(t, hs.URL, runID); !equalTags(reread, want) {
			t.Fatalf("GET after PUT = %v, want %v", reread, want)
		}
	})

	t.Run("PUT overwrites whole set", func(t *testing.T) {
		got := putTags(t, hs.URL, runID, []string{"customer-x"})
		if !equalTags(got, []string{"customer-x"}) {
			t.Fatalf("overwrite = %v, want [customer-x]", got)
		}
	})

	t.Run("PUT empty clears", func(t *testing.T) {
		got := putTags(t, hs.URL, runID, []string{})
		if len(got) != 0 {
			t.Fatalf("clear = %v, want empty", got)
		}
	})

	t.Run("tag over length cap is 400", func(t *testing.T) {
		body, _ := json.Marshal(tagsRequest{Tags: []string{strings.Repeat("x", store.MaxTagLen+1)}})
		resp := tagsPutReq(t, hs.URL+"/api/runs/"+runID+"/tags", body)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", resp.StatusCode)
		}
	})

	t.Run("GET unknown run is 404", func(t *testing.T) {
		resp, err := http.Get(hs.URL + "/api/runs/nope/tags")
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", resp.StatusCode)
		}
	})

	t.Run("PUT unknown run is 404", func(t *testing.T) {
		body, _ := json.Marshal(tagsRequest{Tags: []string{"x"}})
		resp := tagsPutReq(t, hs.URL+"/api/runs/nope/tags", body)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", resp.StatusCode)
		}
	})
}

func getTags(t *testing.T, base, runID string) []string {
	t.Helper()
	resp, err := http.Get(base + "/api/runs/" + runID + "/tags")
	if err != nil {
		t.Fatalf("GET tags: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET status = %d, want 200", resp.StatusCode)
	}
	return decodeTags(t, resp)
}

func putTags(t *testing.T, base, runID string, tags []string) []string {
	t.Helper()
	body, _ := json.Marshal(tagsRequest{Tags: tags})
	resp := tagsPutReq(t, base+"/api/runs/"+runID+"/tags", body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT status = %d, want 200", resp.StatusCode)
	}
	return decodeTags(t, resp)
}

func tagsPutReq(t *testing.T, url string, body []byte) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPut, url, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PUT: %v", err)
	}
	return resp
}

func decodeTags(t *testing.T, resp *http.Response) []string {
	t.Helper()
	var out struct {
		Tags []string `json:"tags"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return out.Tags
}

func equalTags(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
