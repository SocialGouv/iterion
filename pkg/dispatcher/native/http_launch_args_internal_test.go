package native

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The launch-context filter is a property of the MOUNTING, not of the doors
// found one review at a time. This guard drives EVERY route of the mounting
// with a probe body carrying the reserved keys, then reads the invariant off
// the STORE — no card may carry a reserved key the server did not stamp —
// so a client door that persists bot args fails whichever request shape it
// decodes.
//
// The list mirrors RegisterRoutesWithMiddleware, whose registrations stay
// literal on purpose (the pkg/server route witnesses parse that source):
// those witnesses pin the router's SHAPE — a door added there must be
// registered with them — and this walk drives each door of that shape for
// the launch-context invariant. Adding a door: add it to both.
func TestEveryBoardAPIRouteKeepsTheLaunchContextServerStamped(t *testing.T) {
	routes := []struct{ method, pattern string }{
		{http.MethodGet, "/issues"},
		{http.MethodPost, "/issues"},
		{http.MethodGet, "/issues/{id}"},
		{http.MethodPatch, "/issues/{id}"},
		{http.MethodDelete, "/issues/{id}"},
		{http.MethodPost, "/issues/{id}/transition"},
		{http.MethodPost, "/issues/{id}/comments"},
		{http.MethodGet, "/labels"},
		{http.MethodPost, "/labels/rename"},
		{http.MethodPost, "/labels/merge"},
		{http.MethodDelete, "/labels/{label}"},
		{http.MethodGet, "/board"},
		{http.MethodPut, "/board"},
		{http.MethodPost, "/board/states"},
		{http.MethodPost, "/board/states/reorder"},
		{http.MethodPatch, "/board/states/{name}"},
		{http.MethodDelete, "/board/states/{name}"},
		{http.MethodPost, "/board/fields"},
		{http.MethodPost, "/board/fields/reorder"},
		{http.MethodPatch, "/board/fields/{name}"},
		{http.MethodDelete, "/board/fields/{name}"},
		{http.MethodPost, "/board/views"},
		{http.MethodDelete, "/board/views/{name}"},
	}

	// A superset probe body: each handler decodes the fields it knows and
	// ignores the rest, so one payload drives every route.
	probe := func(botArgs string) string {
		return `{
		  "title": "probe",
		  "body": "probe body",
		  "state": "ready",
		  "to": "ready",
		  "from": "inbox",
		  "name": "ready",
		  "display": "Ready",
		  "order": ["ready"],
		  "labels": [],
		  "fields": {},
		  "bot": "probe",
		  "bot_args": ` + botArgs + `
		}`
	}
	forged := `{"member_var": "v", "__iterion_project_path": "harmless/other", "__iterion_webhook_id": "wh-x", "__iterion_key_overrides": "{}", "__iterion_secret_overrides": "{}", "__iterion_repo_url": "https://github.com/acme/widgets.git"}`

	stampWant := map[string]string{
		"__iterion_repo_url":     "https://github.com/acme/widgets.git",
		"__iterion_project_path": "acme/widgets",
		"__iterion_webhook_id":   "wh-t1",
	}

	for _, rt := range routes {
		t.Run(rt.method+" "+rt.pattern, func(t *testing.T) {
			s, err := NewStore(t.TempDir())
			if err != nil {
				t.Fatalf("NewStore: %v", err)
			}
			// A server-stamped card (what ensureBoardCard leaves behind)
			// and a bare card, so every {id} route has a target.
			stamped, err := s.Create(Issue{
				Title: "webhook card", State: "ready",
				BotArgs: map[string]string{
					"member_var":             "v1",
					"__iterion_repo_url":     stampWant["__iterion_repo_url"],
					"__iterion_project_path": stampWant["__iterion_project_path"],
					"__iterion_webhook_id":   stampWant["__iterion_webhook_id"],
				},
			})
			if err != nil {
				t.Fatalf("seed stamped card: %v", err)
			}
			bare, err := s.Create(Issue{Title: "bare card", State: "ready"})
			if err != nil {
				t.Fatalf("seed bare card: %v", err)
			}
			if _, _, err := s.AddComment(bare.ID, "seed", "seed comment"); err != nil {
				t.Fatalf("seed comment: %v", err)
			}

			path := strings.NewReplacer(
				"{id}", bare.ID,
				"{label}", "probe",
				"{name}", "ready",
			).Replace(rt.pattern)

			api := &BoardAPI{Resolve: func(*http.Request) (BoardStore, error) { return s, nil }}
			mux := http.NewServeMux()
			api.RegisterRoutesWithMiddleware(mux, "", nil)
			srv := httptest.NewServer(mux)
			defer srv.Close()

			req, err := http.NewRequest(rt.method, srv.URL+path, bytes.NewBufferString(probe(forged)))
			if err != nil {
				t.Fatalf("build request: %v", err)
			}
			req.Header.Set("Content-Type", "application/json")
			res, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("drive %s %s: %v", rt.method, path, err)
			}
			res.Body.Close()

			// The invariant, read off the store (not off a response): every
			// card's reserved keys are the server's stamp or absent, and the
			// stamped card survives every route (only the DELETE door eats a
			// card, and it eats its own bare target).
			cards, err := s.List(ListFilter{})
			if err != nil {
				t.Fatalf("list cards: %v", err)
			}
			stampedAlive := false
			for _, card := range cards {
				if card.ID == stamped.ID {
					stampedAlive = true
				}
			}
			if !stampedAlive {
				t.Fatalf("the server-stamped card did not survive %s %s — the walk lost its reference card", rt.method, rt.pattern)
			}
			for _, card := range cards {
				for _, res := range ReservedLaunchArgs {
					got, ok := card.BotArgs[res]
					if !ok {
						continue
					}
					want, stampedKey := stampWant[res]
					if !stampedKey {
						t.Errorf("%s %s left the reserved %s = %q on card %s — a client wrote launch context through the mounting",
							rt.method, rt.pattern, res, got, card.ID)
						continue
					}
					if got != want || card.ID != stamped.ID {
						t.Errorf("%s %s left the reserved %s = %q on card %s, want the server's stamp %q (stamped card only)",
							rt.method, rt.pattern, res, got, card.ID, want)
					}
				}
			}
		})
	}
}
