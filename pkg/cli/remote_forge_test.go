package cli_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/cli"
)

// The refresh output exists to turn a permission gap into a step. It must name
// every surface the gap darkens — `statuses` is the merge-gate verdict as well
// as the card's CI panel — and it must never trail off after "on" when the
// probe could not report an installation page.
func TestRemoteForgeRefresh_RendersTheCIGapAndItsPage(t *testing.T) {
	body := func(missing, manage string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"installation_account":"acme","manage_install_url":"` + manage +
				`","granted_permissions":{"contents":"write"},"missing_ci_permissions":` + missing + `}`))
		}
	}
	page := "https://github.com/organizations/acme/settings/installations/99"

	p, out := remotePrinter(cli.OutputHuman)
	c := remoteTestClient(t, body(`["checks"]`, page))
	if err := cli.RemoteForgeRefresh(context.Background(), c, p, "/x"); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, want := range []string{"checks", "CI panel", page} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "merge-gate") {
		t.Errorf("only `checks` is missing — the merge gate posts with statuses and is unaffected:\n%s", got)
	}

	// statuses withheld: the gate is dark too, and the operator has to know.
	p, out = remotePrinter(cli.OutputHuman)
	c = remoteTestClient(t, body(`["checks","statuses"]`, page))
	if err := cli.RemoteForgeRefresh(context.Background(), c, p, "/x"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "merge-gate") {
		t.Errorf("a withheld `statuses` also darkens the revi/review verdict, which the line must say:\n%s", out.String())
	}

	// No install page (the probe could not report one): still a whole sentence.
	p, out = remotePrinter(cli.OutputHuman)
	c = remoteTestClient(t, body(`["checks"]`, ""))
	if err := cli.RemoteForgeRefresh(context.Background(), c, p, "/x"); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "on \n") || strings.HasSuffix(strings.TrimRight(out.String(), "\n"), " on") {
		t.Errorf("the line trails off after \"on\" with no page to name:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "installation page") {
		t.Errorf("with no URL the line must still name where to go:\n%s", out.String())
	}
}

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
