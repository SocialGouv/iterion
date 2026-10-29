package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestDesktopCallbackHandler_ARefusalAnswersAtOnce is the desktop half of
// #2687: the cloud's OIDC callback redirects a REFUSED desktop sign-in to
// the loopback with ?sso_error=, and the listener hands the refusal to the
// waiting ConnectCloudSSO at once — the browser page says why, and no
// five-minute timeout is waited out.
func TestDesktopCallbackHandler_ARefusalAnswersAtOnce(t *testing.T) {
	ticketCh := make(chan string, 1)
	refusalCh := make(chan error, 1)
	srv := httptest.NewServer(desktopCallbackHandler("https://cloud.example", ticketCh, refusalCh))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/desktop-callback?sso_error=password_change_required&sso_provider=Keycloak")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("a refusal's browser page answered %d, want 400", resp.StatusCode)
	}
	if !strings.Contains(string(body), "password change") {
		t.Fatalf("the browser page does not say why: %s", body)
	}
	select {
	case err := <-refusalCh:
		if !strings.Contains(err.Error(), "https://cloud.example") || !strings.Contains(err.Error(), "password change") {
			t.Fatalf("the refusal the app surfaces reads %q — it must name the cloud and the reason", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no refusal reached the app: the cloud's sso_error redirect was dropped")
	}
	select {
	case tok := <-ticketCh:
		t.Fatalf("a refusal also delivered a ticket %q", tok)
	default:
	}
}

// TestDesktopCallbackHandler_TheTicketPathIsUnchanged: the refusal channel is
// additive — an accepted sign-in still delivers its one-time ticket and its
// success page.
func TestDesktopCallbackHandler_TheTicketPathIsUnchanged(t *testing.T) {
	ticketCh := make(chan string, 1)
	refusalCh := make(chan error, 1)
	srv := httptest.NewServer(desktopCallbackHandler("https://cloud.example", ticketCh, refusalCh))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/desktop-callback?ticket=t-123")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("the ticket path answered %d, want 200", resp.StatusCode)
	}
	select {
	case tok := <-ticketCh:
		if tok != "t-123" {
			t.Fatalf("ticket = %q", tok)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no ticket delivered")
	}
	select {
	case err := <-refusalCh:
		t.Fatalf("an accepted sign-in reported a refusal: %v", err)
	default:
	}
}

func TestDesktopSSORefusal_MapsTheCodes(t *testing.T) {
	for code, want := range map[string]string{
		"password_change_required": "password change",
		"link_required":            "already exists",
		"restricted":               "allow-listed",
		"state_expired":            "start again",
		"login_failed":             "didn't complete",
	} {
		if got := desktopSSORefusal(code, ""); !strings.Contains(got, want) {
			t.Errorf("desktopSSORefusal(%q) = %q, want it to carry %q", code, got, want)
		}
	}
}
