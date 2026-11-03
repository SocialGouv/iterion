package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"unicode"

	"github.com/SocialGouv/iterion/internal/stdiotest"
)

// captureStdout runs run with os.Stdout read into the returned string.
func captureStdout(t *testing.T, run func() error) string {
	t.Helper()
	return stdiotest.Run(t, &os.Stdout, func() {
		if err := run(); err != nil {
			t.Fatal(err)
		}
	})
}

// TestRemoteLoginAndStatus_showTheServersAccountInert: the account `remote
// login` and `remote status` print comes from the server; a terminal shows
// it as text, and never acts on it.
func TestRemoteLoginAndStatus_showTheServersAccountInert(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"user": map[string]any{"id": "u1", "email": "ops@example.test\x1b]0;owned\x07\x1b[2J\u202e"}})
	}))
	defer srv.Close()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ITERION_TOKEN", "iap_test")
	for _, env := range []string{"ITERION_REMOTE_URL", "ITERION_REMOTE_TOKEN", "ITERION_REMOTE_TEAM", "ITERION_REMOTE_ORG"} {
		t.Setenv(env, "")
	}
	ctx := context.Background()
	remoteLoginCmd.SetContext(ctx)
	remoteStatusCmd.SetContext(ctx)
	for _, tc := range []struct {
		name string
		run  func() error
	}{
		{"login", func() error { return runRemoteLogin(remoteLoginCmd, []string{srv.URL}) }},
		{"status", func() error { return remoteStatusCmd.RunE(remoteStatusCmd, nil) }},
	} {
		out := captureStdout(t, tc.run)
		for _, r := range out {
			if r != '\n' && !unicode.IsPrint(r) {
				t.Fatalf("%s sends %U raw to the terminal: %q", tc.name, r, out)
			}
		}
		if !strings.Contains(out, `ops@example.test\x1b`) {
			t.Fatalf("%s does not show the account escaped: %q", tc.name, out)
		}
	}
}
