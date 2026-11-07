package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/spf13/cobra"
)

// priceFeedsRecorder runs the root command against a recording server and
// returns the requests it received.
func priceFeedsRecorder(t *testing.T, args ...string) ([]string, error) {
	t.Helper()
	resetPriceFeedsFlags()
	var mu sync.Mutex
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		got = append(got, r.Method+" "+r.URL.Path+" "+strings.TrimSpace(string(b)))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"scope":"team","origin":"db","source_types":["modelsdev","scaleway","litellm","exec"],
			"feeds":[{"feed":{"name":"scw","type":"exec","enabled":true,"interval":3600,"timeout":10},
			"status":"ok","entries":2,"refreshed_at":"2026-11-07T10:00:00Z","fresh_for":7200}]}`))
	}))
	defer srv.Close()
	t.Setenv("ITERION_REMOTE_URL", srv.URL)
	t.Setenv("ITERION_REMOTE_TOKEN", "test-token-not-a-secret")
	t.Setenv("ITERION_REMOTE_TEAM", "team-a")
	rootCmd.SetArgs(args)
	rootCmd.SetOut(io.Discard)
	rootCmd.SetErr(io.Discard)
	err := rootCmd.Execute()
	mu.Lock()
	defer mu.Unlock()
	return append([]string(nil), got...), err
}

// Cobra keeps flag values between executions in one process; each case
// starts from the defaults.
func resetPriceFeedsFlags() {
	remotePriceFeedsOrgFlag, remotePriceFeedsTeamFlag, remotePriceFeedsFile = "", "", ""
	for _, name := range []string{"org", "team", "file"} {
		for _, cmd := range []*cobra.Command{remotePriceFeedsListCmd, remotePriceFeedsSetCmd, remotePriceFeedsDeleteCmd, remotePriceFeedsSyncCmd} {
			if f := cmd.Flags().Lookup(name); f != nil {
				f.Changed = false
			}
		}
	}
}

func TestRemotePriceFeedsCommands(t *testing.T) {
	got, err := priceFeedsRecorder(t, "remote", "price-feeds", "list")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "GET /api/teams/team-a/price-feeds " {
		t.Fatalf("list hit %v", got)
	}

	got, err = priceFeedsRecorder(t, "remote", "price-feeds", "list", "--org", "org-9")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "GET /api/orgs/org-9/price-feeds " {
		t.Fatalf("org-scoped list hit %v", got)
	}

	got, err = priceFeedsRecorder(t, "remote", "price-feeds", "sync", "scw")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "POST /api/teams/team-a/price-feeds/sync {\"name\":\"scw\"}" {
		t.Fatalf("named sync hit %v", got)
	}

	got, err = priceFeedsRecorder(t, "remote", "price-feeds", "delete")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || !strings.Contains(got[0], "PUT /api/teams/team-a/price-feeds {\"feeds\":null}") {
		t.Fatalf("delete hit %v", got)
	}
}
