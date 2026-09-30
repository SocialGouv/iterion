package github

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/forge"
)

func graphQLServer(t *testing.T, h http.HandlerFunc) *AdminClient {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return &AdminClient{HTTP: srv.Client(), APIBase: srv.URL, Token: "tok"}
}

// GitHub answers a spent GraphQL budget with a 200 whose errors[] says
// RATE_LIMITED. It is a wait like a REST 429 — typed as one, with the reset
// the answer carries — and the errors array still travels with it.
func TestGraphQLRateLimitedIsAWait(t *testing.T) {
	c := graphQLServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(time.Now().Add(12*time.Minute).Unix(), 10))
		_, _ = w.Write([]byte(`{"data":null,"errors":[{"type":"RATE_LIMITED","message":"API rate limit exceeded for installation ID 7."}]}`))
	})

	err := c.graphQLOp(context.Background(), "list project items", "query{}", nil, &struct{}{})
	var se *forge.StatusError
	if !errors.As(err, &se) || !se.RateLimited() {
		t.Fatalf("err = %v, want a rate-limited *forge.StatusError", err)
	}
	if se.Op != "POST list project items" {
		t.Errorf("Op = %q, want the query's name", se.Op)
	}
	if wait := time.Until(se.ResetAt); wait < 11*time.Minute || wait > 12*time.Minute {
		t.Errorf("ResetAt in %v, want ~12m from X-RateLimit-Reset", wait)
	}
	if se.Code != http.StatusOK || !strings.Contains(err.Error(), "HTTP 200: rate limited until ") {
		t.Errorf("Code = %d, message = %q; want the answer's own 200, named a rate limit until its reset", se.Code, err.Error())
	}
	if se.Detail != "API rate limit exceeded for installation ID 7." {
		t.Errorf("Detail = %q, want the RATE_LIMITED entry's message", se.Detail)
	}
	var gerr *GraphQLErrors
	if !errors.As(err, &gerr) || !gerr.HasType("RATE_LIMITED") {
		t.Errorf("err = %v, want the *GraphQLErrors still reachable", err)
	}
}

// Any other errors[] entry stays a *GraphQLErrors: a missing item is not a wait.
func TestGraphQLOtherErrorsAreNotAWait(t *testing.T) {
	c := graphQLServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-RateLimit-Remaining", "4000")
		_, _ = w.Write([]byte(`{"data":null,"errors":[{"type":"NOT_FOUND","message":"Could not resolve to a ProjectV2."}]}`))
	})

	err := c.graphQLOp(context.Background(), "get project", "query{}", nil, &struct{}{})
	var se *forge.StatusError
	if errors.As(err, &se) {
		t.Fatalf("err = %v — a NOT_FOUND typed as a forge status", err)
	}
	var gerr *GraphQLErrors
	if !errors.As(err, &gerr) || !gerr.HasType("NOT_FOUND") {
		t.Fatalf("err = %v, want *GraphQLErrors carrying NOT_FOUND", err)
	}
}

// A REST-style limit on the GraphQL endpoint names the query, not the path
// every query shares.
func TestGraphQLLimited403NamesTheQuery(t *testing.T) {
	c := graphQLServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "60")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"You have exceeded a secondary rate limit."}`))
	})

	err := c.graphQLOp(context.Background(), "add project item", "mutation{}", nil, nil)
	var se *forge.StatusError
	if !errors.As(err, &se) || !se.RateLimited() {
		t.Fatalf("err = %v, want a rate-limited *forge.StatusError", err)
	}
	if wait := time.Until(se.ResetAt); se.Op != "POST add project item" || wait < 58*time.Second || wait > time.Minute {
		t.Errorf("Op = %q, ResetAt in %v; want the query's name and ~1m", se.Op, wait)
	}
}
