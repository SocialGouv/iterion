package forgejo

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
)

// TestGetHook_ReadsPastTheDefaultPage: a repository whose iterion hook sits
// beyond the forge's default page must still be found by its delivery URL —
// a probe that answers "no hook" there makes the upsert register a second
// hook on the same URL, and every event is then delivered twice.
func TestGetHook_ReadsPastTheDefaultPage(t *testing.T) {
	const want = "https://iterion.example.com/api/webhooks/forgejo/w1"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		size := 30
		if v := r.URL.Query().Get("limit"); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil {
				http.Error(w, "bad limit", http.StatusBadRequest)
				return
			}
			size = n
		}
		var page []map[string]any
		for i := 0; i < 45 && i < size; i++ {
			url := fmt.Sprintf("https://other.example/hook/%d", i)
			if i == 40 {
				url = want
			}
			page = append(page, map[string]any{"id": i + 1, "active": true, "config": map[string]any{"url": url}})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(page)
	}))
	defer srv.Close()
	c := &AdminClient{HTTP: srv.Client(), BaseURL: srv.URL, Token: "t"}

	found, err := c.GetHook(context.Background(), "group/api", want)
	if err != nil {
		t.Fatalf("GetHook: %v", err)
	}
	if found == nil {
		t.Fatalf("GetHook found no hook at %s although the repository holds one at position %d — past the default page of %d", want, 40+1, 30)
	}
	hooks, err := c.ListHooks(context.Background(), "group/api")
	if err != nil {
		t.Fatalf("ListHooks: %v", err)
	}
	if len(hooks) != 45 {
		t.Errorf("ListHooks returned %d hooks, want all %d", len(hooks), 45)
	}
}
