package github

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/SocialGouv/iterion/pkg/forge"
)

func TestFileClient_GetPutConflict(t *testing.T) {
	const repo = "o/r"
	var putBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/contents/feed-watch.json":
			if r.URL.Query().Get("ref") != "main" {
				t.Errorf("ref = %q, want main", r.URL.Query().Get("ref"))
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"type": "file", "sha": "sha-1",
				"content": base64.StdEncoding.EncodeToString([]byte(`{"k":1}`)),
			})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/contents/missing.json":
			w.WriteHeader(http.StatusNotFound)
		case r.Method == http.MethodPut && r.URL.Path == "/repos/o/r/contents/feed-watch.json":
			_ = json.NewDecoder(r.Body).Decode(&putBody)
			if putBody["sha"] == "stale" {
				w.WriteHeader(http.StatusConflict)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"content": map[string]any{"sha": "sha-2"}})
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer srv.Close()
	c := &AdminClient{HTTP: srv.Client(), APIBase: srv.URL, Token: "t"}
	ctx := context.Background()

	// GetFile decodes content + sha.
	fr, err := c.GetFile(ctx, repo, "feed-watch.json", "main")
	if err != nil {
		t.Fatalf("GetFile: %v", err)
	}
	if string(fr.Content) != `{"k":1}` || fr.SHA != "sha-1" {
		t.Fatalf("GetFile = %q / %q", fr.Content, fr.SHA)
	}

	// Missing path → ErrFileNotFound (not a silent empty read).
	if _, err := c.GetFile(ctx, repo, "missing.json", "main"); err != forge.ErrFileNotFound {
		t.Fatalf("missing GetFile err = %v, want ErrFileNotFound", err)
	}

	// PutFile with the current sha → new sha; the wire carries base64 content,
	// the pinned branch, the fixed bot author, and the server message.
	fr2, err := c.PutFile(ctx, repo, forge.PutFile{
		Path: "feed-watch.json", Content: []byte(`{"k":2}`), Message: "chore: edit",
		Branch: "main", PrevSHA: "sha-1", AuthorName: "iterion-share-editor[bot]",
		AuthorEmail: "share@bot.iterion.invalid",
	})
	if err != nil {
		t.Fatalf("PutFile: %v", err)
	}
	if fr2.SHA != "sha-2" {
		t.Fatalf("PutFile sha = %q, want sha-2", fr2.SHA)
	}
	if putBody["branch"] != "main" || putBody["message"] != "chore: edit" {
		t.Fatalf("put body branch/message = %v / %v", putBody["branch"], putBody["message"])
	}
	if got, _ := base64.StdEncoding.DecodeString(putBody["content"].(string)); string(got) != `{"k":2}` {
		t.Fatalf("put content = %q", got)
	}
	if a, _ := putBody["author"].(map[string]any); a["name"] != "iterion-share-editor[bot]" {
		t.Fatalf("author = %v", putBody["author"])
	}

	// Stale sha → ErrFileConflict (never a silent overwrite).
	if _, err := c.PutFile(ctx, repo, forge.PutFile{
		Path: "feed-watch.json", Content: []byte("x"), Message: "m", Branch: "main", PrevSHA: "stale",
	}); err != forge.ErrFileConflict {
		t.Fatalf("stale PutFile err = %v, want ErrFileConflict", err)
	}
}
