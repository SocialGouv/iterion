package github

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/SocialGouv/iterion/pkg/forge"
)

// TestHookWrite_CarriesTheForgesOwnRefusal: a refused hook create or update
// reaches the orchestrator with the forge's own reason on the *StatusError —
// the reason is what tells an address refusal from a hook limit or a
// rejected field, and the operator's remedy differs.
func TestHookWrite_CarriesTheForgesOwnRefusal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"message":"Invalid url given"}`))
	}))
	defer srv.Close()
	c := &AdminClient{HTTP: srv.Client(), APIBase: srv.URL, Token: "t"}
	spec := forge.HookSpec{URL: "https://iterion.example.com/api/webhooks/github/w1", Secret: "s", Active: true}

	_, createErr := c.CreateHook(context.Background(), "group/api", spec)
	_, updateErr := c.UpdateHook(context.Background(), "group/api", "7", spec)
	for op, err := range map[string]error{"create": createErr, "update": updateErr} {
		var se *forge.StatusError
		if !errors.As(err, &se) {
			t.Fatalf("%s: want a *forge.StatusError, got %T: %v", op, err, err)
		}
		if se.Code != http.StatusUnprocessableEntity || se.Detail != "Invalid url given" {
			t.Errorf("%s: StatusError = {Code:%d Detail:%q}, want {422 \"Invalid url given\"}", op, se.Code, se.Detail)
		}
	}
}
