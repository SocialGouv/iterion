package native_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
)

func newServerWithStore(t *testing.T) (*httptest.Server, *native.Store) {
	t.Helper()
	s, err := native.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	mux := http.NewServeMux()
	s.RegisterRoutes(mux, "")
	return httptest.NewServer(mux), s
}

func TestHTTPCreateAndList(t *testing.T) {
	srv, _ := newServerWithStore(t)
	defer srv.Close()

	body := bytes.NewBufferString(`{"title":"hello","state":"ready","priority":3,"labels":["x"]}`)
	r, err := http.Post(srv.URL+"/issues", "application/json", body)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("status %d", r.StatusCode)
	}
	var created native.Issue
	if err := json.NewDecoder(r.Body).Decode(&created); err != nil {
		t.Fatalf("decode: %v", err)
	}
	r.Body.Close()

	r2, err := http.Get(srv.URL + "/issues?state=ready")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	var list []native.Issue
	if err := json.NewDecoder(r2.Body).Decode(&list); err != nil {
		t.Fatalf("decode: %v", err)
	}
	r2.Body.Close()
	if len(list) != 1 || list[0].ID != created.ID {
		t.Fatalf("list mismatch: %+v", list)
	}
}

func TestHTTPAddCommentWithDispatch(t *testing.T) {
	srv, s := newServerWithStore(t)
	defer srv.Close()
	iss, _ := s.Create(native.Issue{Title: "Improve a11y", State: "inbox"})

	// Comment that also stamps a bot + args and moves the issue to ready,
	// mirroring the studio comment box parsing "/billy <instruction>".
	body := bytes.NewBufferString(`{"author":"operator","body":"/billy fix the contrast issues","bot":"branch-improve-loop","bot_args":{"scope_notes":"fix the contrast issues"},"transition_to":"ready"}`)
	r, err := http.Post(srv.URL+"/issues/"+iss.ID+"/comments", "application/json", body)
	if err != nil {
		t.Fatalf("POST comment: %v", err)
	}
	if r.StatusCode != http.StatusOK {
		t.Fatalf("status %d", r.StatusCode)
	}
	var updated native.Issue
	if err := json.NewDecoder(r.Body).Decode(&updated); err != nil {
		t.Fatalf("decode: %v", err)
	}
	r.Body.Close()

	if len(updated.Comments) != 1 || updated.Comments[0].Author != "operator" {
		t.Fatalf("comment not recorded: %+v", updated.Comments)
	}
	if updated.Bot != "branch-improve-loop" || updated.BotArgs["scope_notes"] != "fix the contrast issues" {
		t.Fatalf("dispatch not stamped: bot=%q args=%v", updated.Bot, updated.BotArgs)
	}
	if updated.State != "ready" {
		t.Fatalf("state = %q, want ready", updated.State)
	}
}

func TestHTTPGetAndPatchAndDelete(t *testing.T) {
	srv, s := newServerWithStore(t)
	defer srv.Close()
	iss, _ := s.Create(native.Issue{Title: "x", State: "ready"})

	r, _ := http.Get(srv.URL + "/issues/" + iss.ID)
	if r.StatusCode != 200 {
		t.Fatalf("GET status %d", r.StatusCode)
	}
	r.Body.Close()

	// PATCH title
	patch := bytes.NewBufferString(`{"title":"new title"}`)
	req, _ := http.NewRequest(http.MethodPatch, srv.URL+"/issues/"+iss.ID, patch)
	req.Header.Set("Content-Type", "application/json")
	r2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PATCH: %v", err)
	}
	var upd native.Issue
	_ = json.NewDecoder(r2.Body).Decode(&upd)
	r2.Body.Close()
	if upd.Title != "new title" {
		t.Fatalf("PATCH did not update: %s", upd.Title)
	}

	// DELETE
	req2, _ := http.NewRequest(http.MethodDelete, srv.URL+"/issues/"+iss.ID, nil)
	r3, _ := http.DefaultClient.Do(req2)
	if r3.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE status %d", r3.StatusCode)
	}
	r3.Body.Close()
}

func TestHTTPTransition(t *testing.T) {
	srv, s := newServerWithStore(t)
	defer srv.Close()
	iss, _ := s.Create(native.Issue{Title: "x", State: "ready"})

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/issues/"+iss.ID+"/transition",
		bytes.NewBufferString(`{"to":"in_progress"}`))
	req.Header.Set("Content-Type", "application/json")
	r, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST transition: %v", err)
	}
	if r.StatusCode != 200 {
		t.Fatalf("status %d", r.StatusCode)
	}
	var upd native.Issue
	_ = json.NewDecoder(r.Body).Decode(&upd)
	r.Body.Close()
	if upd.State != "in_progress" {
		t.Fatalf("state not updated: %s", upd.State)
	}

	// invalid state → 409
	req2, _ := http.NewRequest(http.MethodPost, srv.URL+"/issues/"+iss.ID+"/transition",
		bytes.NewBufferString(`{"to":"noplace"}`))
	r2, _ := http.DefaultClient.Do(req2)
	if r2.StatusCode != http.StatusConflict {
		t.Fatalf("bad transition status: %d", r2.StatusCode)
	}
	r2.Body.Close()
}

func TestHTTPBoardGetPut(t *testing.T) {
	srv, _ := newServerWithStore(t)
	defer srv.Close()

	r, _ := http.Get(srv.URL + "/board")
	if r.StatusCode != 200 {
		t.Fatalf("GET board: %d", r.StatusCode)
	}
	r.Body.Close()

	body := bytes.NewBufferString(`{"states":[{"name":"todo","eligible":true},{"name":"done","terminal":true}]}`)
	req, _ := http.NewRequest(http.MethodPut, srv.URL+"/board", body)
	req.Header.Set("Content-Type", "application/json")
	r2, _ := http.DefaultClient.Do(req)
	if r2.StatusCode != 200 {
		t.Fatalf("PUT board: %d", r2.StatusCode)
	}
	var b native.Board
	_ = json.NewDecoder(r2.Body).Decode(&b)
	r2.Body.Close()
	if len(b.States) != 2 {
		t.Fatalf("board not updated: %+v", b)
	}
}

func TestHTTPStateManagement(t *testing.T) {
	srv, s := newServerWithStore(t)
	defer srv.Close()
	do := func(method, path string, body string) *http.Response {
		var rdr *bytes.Buffer
		if body != "" {
			rdr = bytes.NewBufferString(body)
		} else {
			rdr = bytes.NewBuffer(nil)
		}
		req, _ := http.NewRequest(method, srv.URL+path, rdr)
		req.Header.Set("Content-Type", "application/json")
		r, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		return r
	}

	// Add a column.
	if r := do(http.MethodPost, "/board/states", `{"name":"triage","display":"Triage"}`); r.StatusCode != 200 {
		t.Fatalf("add state: %d", r.StatusCode)
	} else {
		r.Body.Close()
	}
	if s.Board().StateByName("triage") == nil {
		t.Fatal("triage not added")
	}

	// Rename via PATCH (cascades) — put an issue in backlog first.
	iss, _ := s.Create(native.Issue{Title: "x", State: "backlog"})
	if r := do(http.MethodPatch, "/board/states/backlog", `{"name":"todo"}`); r.StatusCode != 200 {
		t.Fatalf("rename state: %d", r.StatusCode)
	} else {
		r.Body.Close()
	}
	if got, _ := s.Get(iss.ID); got.State != "todo" {
		t.Fatalf("issue not migrated on rename: %q", got.State)
	}

	// Edit color/flags via PATCH (no rename).
	if r := do(http.MethodPatch, "/board/states/todo", `{"color":"var(--color-board-ready)","eligible":true}`); r.StatusCode != 200 {
		t.Fatalf("update state: %d", r.StatusCode)
	} else {
		r.Body.Close()
	}
	if st := s.Board().StateByName("todo"); st == nil || !st.Eligible {
		t.Fatalf("flags not updated: %+v", st)
	}

	// Delete non-empty without target → 409 + count.
	r := do(http.MethodDelete, "/board/states/todo", "")
	if r.StatusCode != http.StatusConflict {
		t.Fatalf("delete non-empty: want 409, got %d", r.StatusCode)
	}
	var conflict struct {
		Error string `json:"error"`
		Count int    `json:"count"`
	}
	_ = json.NewDecoder(r.Body).Decode(&conflict)
	r.Body.Close()
	if conflict.Count != 1 {
		t.Fatalf("conflict count = %d, want 1", conflict.Count)
	}

	// Delete with migration target.
	if r := do(http.MethodDelete, "/board/states/todo?migrate_to=ready", ""); r.StatusCode != 200 {
		t.Fatalf("delete with target: %d", r.StatusCode)
	} else {
		r.Body.Close()
	}
	if s.Board().StateByName("todo") != nil {
		t.Fatal("todo not deleted")
	}
	if got, _ := s.Get(iss.ID); got.State != "ready" {
		t.Fatalf("issue not migrated on delete: %q", got.State)
	}

	// Reorder.
	names := make([]string, 0)
	for _, st := range s.Board().States {
		names = append(names, st.Name)
	}
	rev := make([]string, len(names))
	for i := range names {
		rev[i] = names[len(names)-1-i]
	}
	order, _ := json.Marshal(map[string][]string{"order": rev})
	if r := do(http.MethodPost, "/board/states/reorder", string(order)); r.StatusCode != 200 {
		t.Fatalf("reorder: %d", r.StatusCode)
	} else {
		r.Body.Close()
	}
	if s.Board().States[0].Name != rev[0] {
		t.Fatal("reorder not applied")
	}
}

func TestHTTPIDPrefix(t *testing.T) {
	srv, s := newServerWithStore(t)
	defer srv.Close()
	iss, _ := s.Create(native.Issue{Title: "x", State: "ready"})

	// Use the bare UUID (no "native:" prefix) — Resolve should match.
	bare := iss.ID[len("native:"):]
	short := bare[:8]

	r, _ := http.Get(srv.URL + "/issues/" + short)
	if r.StatusCode != 200 {
		t.Fatalf("prefix lookup: %d", r.StatusCode)
	}
	r.Body.Close()
}
