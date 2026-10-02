package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/SocialGouv/iterion/pkg/store"
)

// The launch handler turns a taken run id into a 409: the id is refused
// whatever team holds it, and a launch with a fresh id does not answer 409.
func TestLaunch_ATakenRunIDIsAConflict(t *testing.T) {
	srv, hs := newTestServer(t)
	st, err := store.New(srv.cfg.StoreDir)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if _, err := st.CreateRun(context.Background(), "run-1", "wf", nil); err != nil {
		t.Fatalf("create run: %v", err)
	}
	botPath := filepath.Join(srv.cfg.WorkDir, "demo.bot")
	if err := os.WriteFile(botPath, []byte("workflow demo:\n  entry: done\n"), 0o600); err != nil {
		t.Fatalf("write bot: %v", err)
	}

	post := func(runID string) *http.Response {
		body, err := json.Marshal(map[string]any{"file_path": botPath, "run_id": runID})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		resp, err := http.Post(hs.URL+"/api/runs", "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatalf("POST: %v", err)
		}
		t.Cleanup(func() { resp.Body.Close() })
		return resp
	}

	if resp := post("run-1"); resp.StatusCode != http.StatusConflict {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("taken run id: status = %d, want 409 (body %s)", resp.StatusCode, b)
	}
	if resp := post("launch-runid-fresh"); resp.StatusCode == http.StatusConflict {
		t.Fatalf("fresh run id: status = %d, want anything but 409", resp.StatusCode)
	}
}
