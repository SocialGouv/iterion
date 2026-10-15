package gitlab

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
	const want = "https://iterion.example.com/api/webhooks/gitlab/w1"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		size := 20
		if v := r.URL.Query().Get("per_page"); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil {
				http.Error(w, "bad per_page", http.StatusBadRequest)
				return
			}
			size = n
		}
		var page []map[string]any
		for i := 0; i < 25 && i < size; i++ {
			url := fmt.Sprintf("https://other.example/hook/%d", i)
			if i == 22 {
				url = want
			}
			page = append(page, map[string]any{"id": i + 1, "url": url})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(page)
	}))
	defer srv.Close()
	c := New(srv.Client(), srv.URL, "t")

	found, err := c.GetHook(context.Background(), "group/api", want)
	if err != nil {
		t.Fatalf("GetHook: %v", err)
	}
	if found == nil {
		t.Fatalf("GetHook found no hook at %s although the repository holds one at position %d — past the default page of %d", want, 22+1, 20)
	}
	hooks, err := c.ListHooks(context.Background(), "group/api")
	if err != nil {
		t.Fatalf("ListHooks: %v", err)
	}
	if len(hooks) != 25 {
		t.Errorf("ListHooks returned %d hooks, want all %d", len(hooks), 25)
	}
}
