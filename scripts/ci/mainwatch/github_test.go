package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The listing asks for push runs only: a pull request from a fork's `main`
// also has head branch main, and a page of those would hide main's runs.
func TestMainRunsListsPushRunsOnly(t *testing.T) {
	var query string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query = r.URL.Query().Encode()
		_, _ = w.Write([]byte(`{"workflow_runs":[]}`))
	}))
	defer srv.Close()
	c := &restClient{api: srv.URL, repo: "SocialGouv/iterion", workflow: "tests.yml", http: srv.Client()}
	if _, err := c.MainRuns(context.Background()); err != nil {
		t.Fatal(err)
	}
	if want := "branch=main&event=push&per_page=100"; query != want {
		t.Fatalf("query = %q, want %q", query, want)
	}
}
