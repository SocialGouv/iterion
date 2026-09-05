package cli_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/cli"
)

// A refusal's actionable field — the App's settings page — sits last in the
// JSON body, past the generic error line's 300-byte cut. It must reach the
// operator whole.
func TestRemoteForgeAvatar_RendersTheRefusal(t *testing.T) {
	manage := "https://github.com/organizations/socialgouv/settings/apps/iterion-forge-socialgouv-12345678"
	c := remoteTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"error":"GitHub exposes no API for an account's avatar or an App's logo — upload the logo on the App's settings page (Display information), which is a long sentence on purpose","logo_circle_url":"/brand/iterion-bot-circle.png","logo_url":"/brand/iterion-bot.png","manage_url":"` + manage + `"}`))
	}))
	p, out := remotePrinter(cli.OutputHuman)
	err := cli.RemoteForgeAvatar(context.Background(), c, p, "/api/teams/t1/forge/connections/c1/avatar", "", false)
	if err == nil {
		t.Fatal("a 422 must still be an error (exit 1)")
	}
	if !strings.Contains(out.String(), manage) || !strings.Contains(out.String(), "/brand/iterion-bot.png") {
		t.Fatalf("refusal fields missing from the output:\n%s", out.String())
	}

	c = remoteTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":"gitlab.example.com does not flag @iterion-bot as a bot account","needs_force":true,"account_login":"iterion-bot"}`))
	}))
	p, out = remotePrinter(cli.OutputHuman)
	if err := cli.RemoteForgeAvatar(context.Background(), c, p, "/x", "", false); err == nil || !strings.Contains(out.String(), "--force") {
		t.Fatalf("err=%v output:\n%s", err, out.String())
	}
}
