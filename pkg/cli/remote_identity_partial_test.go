package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRemoteIdentityPreservesUnavailableOrganizations(t *testing.T) {
	t.Setenv("ITERION_REMOTE_URL", "")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/auth/me" {
			t.Errorf("unexpected request: %s", r.URL.Path)
		}
		_, _ = io.WriteString(w, `{"user":{"id":"u1"},"active_org_id":"bad","orgs":[{"org_id":"bad","unavailable":true,"teams":[]},{"org_id":"good","org_name":"Healthy","org_role":"member","teams":[{"team_id":"11111111-1111-1111-1111-111111111111","team_slug":"healthy"}]}]}`)
	}))
	defer srv.Close()
	c := NewRemoteClientFor(RemoteConfig{BaseURL: srv.URL, Token: "iap_test"})
	var out bytes.Buffer
	p := &Printer{W: &out, Format: OutputJSON}
	if err := RemoteOrgsList(context.Background(), c, p); err != nil {
		t.Fatal(err)
	}
	var body struct {
		Orgs []struct {
			ID          string `json:"id"`
			Unavailable bool   `json:"unavailable"`
		} `json:"orgs"`
	}
	if err := json.Unmarshal(out.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Orgs) != 2 || !body.Orgs[0].Unavailable || body.Orgs[1].Unavailable {
		t.Fatalf("CLI hides partial identity: %s", out.String())
	}
	if err := RemoteOrgsSwitch(context.Background(), c, p, "bad"); err == nil || !strings.Contains(err.Error(), "unavailable") {
		t.Fatalf("CLI accepts unavailable org: %v", err)
	}
	for _, slug := range []string{"healthy", "missing"} {
		if _, err := resolveRemoteTeamIDAs(context.Background(), c, "", slug); err == nil || !strings.Contains(err.Error(), "incomplete identity") {
			t.Fatalf("CLI guesses from incomplete identity for %s: %v", slug, err)
		}
	}
}
