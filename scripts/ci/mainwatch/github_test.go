package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
)

// The listing asks for push and schedule runs, one listing each: a pull
// request from a fork's `main` also has head branch main, and a page of those
// would hide main's runs. The nightly is one of main's runs.
func TestMainRunsListsPushAndScheduleRuns(t *testing.T) {
	var queries []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		queries = append(queries, r.URL.Query().Encode())
		switch r.URL.Query().Get("event") {
		case "push":
			_, _ = w.Write([]byte(`{"workflow_runs":[{"id":2,"run_number":20,"event":"push"}]}`))
		case "schedule":
			_, _ = w.Write([]byte(`{"workflow_runs":[{"id":3,"run_number":21,"event":"schedule"}]}`))
		default:
			http.Error(w, "unexpected listing "+r.URL.RawQuery, http.StatusBadRequest)
		}
	}))
	defer srv.Close()
	c := &restClient{api: srv.URL, repo: "SocialGouv/iterion", workflow: "tests.yml", http: srv.Client()}
	runs, err := c.MainRuns(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"branch=main&event=push&per_page=100", "branch=main&event=schedule&per_page=100"}
	if !slices.Equal(queries, want) {
		t.Fatalf("queries = %q, want %q", queries, want)
	}
	var ids []int64
	for _, r := range runs {
		ids = append(ids, r.ID)
	}
	slices.Sort(ids)
	if !slices.Equal(ids, []int64{2, 3}) {
		t.Fatalf("runs = %v, want the push run 2 and the nightly 3", ids)
	}
}

// A full page of push runs is a window: a nightly older than its oldest run
// would let the last-green lookup reach past push runs the page cut off.
func TestMainRunsClipsTheNightliesToAFullPushWindow(t *testing.T) {
	var push []string
	for i := 0; i < 100; i++ {
		push = append(push, fmt.Sprintf(`{"id":%d,"run_number":%d,"event":"push"}`, 1000+i, 500+i))
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("event") {
		case "push":
			_, _ = w.Write([]byte(`{"workflow_runs":[` + strings.Join(push, ",") + `]}`))
		case "schedule":
			_, _ = w.Write([]byte(`{"workflow_runs":[{"id":7,"run_number":550,"event":"schedule"},{"id":8,"run_number":499,"event":"schedule"}]}`))
		}
	}))
	defer srv.Close()
	c := &restClient{api: srv.URL, repo: "SocialGouv/iterion", workflow: "tests.yml", http: srv.Client()}
	runs, err := c.MainRuns(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	has := func(id int64) bool { return slices.ContainsFunc(runs, func(r Run) bool { return r.ID == id }) }
	if len(runs) != 101 || !has(7) || has(8) {
		t.Fatalf("got %d runs (nightly 7 kept: %v, nightly 8 kept: %v), want the 100 push runs and nightly 7 only", len(runs), has(7), has(8))
	}
}
