package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// TestRemoteAdminUsersHelp_NamesTheRefusalOnOnesOwnAccount: the instance
// refuses `admin users reset-password <self>` (403) when the credential
// presented is a personal access token — which is what `remote login` stores,
// so it is what the CLI presents on every run. The help has to say so and
// point at the path that does work: the operator reading only this text would
// otherwise read the 403 as a permission problem on a super-admin account
// that has every permission.
func TestRemoteAdminUsersHelp_NamesTheRefusalOnOnesOwnAccount(t *testing.T) {
	// The text is hard-wrapped for a terminal, so the sentences are matched
	// on one line: a phrase broken across two source lines is the same
	// sentence to the reader.
	long := strings.Join(strings.Fields(remoteAdminUsersCmd.Long), " ")
	for _, want := range []string{
		"personal access token",
		"refuses it (403) on YOUR OWN account",
		"Change password",
		// The erasure is irreversible and not self-service: the help says
		// both before an operator types it.
		"cannot be undone",
		"cannot be run on your own account",
		"It asks for --yes",
	} {
		if !strings.Contains(long, want) {
			t.Errorf("`admin users` help does not carry %q:\n%s", want, long)
		}
	}
}

// TestRemoteAdminUsersErase_AsksForYes: `admin users erase <id>` sends
// nothing without --yes — an id pasted wrong would erase another person —
// and sends the one POST with it.
func TestRemoteAdminUsersErase_AsksForYes(t *testing.T) {
	var mu sync.Mutex
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		got = append(got, r.Method+" "+r.URL.Path)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"already_erased":false}`))
	}))
	t.Cleanup(srv.Close)
	t.Setenv("ITERION_REMOTE_URL", srv.URL)
	t.Setenv("ITERION_REMOTE_TOKEN", "test-token")
	t.Cleanup(func() { remoteAdminEraseYes = false })

	cmd := remoteAdminUsersCmd
	cmd.SetContext(context.Background())
	remoteAdminEraseYes = false
	if err := cmd.RunE(cmd, []string{"erase", "u-1"}); err == nil || !strings.Contains(err.Error(), "refusing to erase account u-1 without --yes") {
		t.Fatalf("erase without --yes = %v, want the refusal", err)
	}
	mu.Lock()
	sent := len(got)
	mu.Unlock()
	if sent != 0 {
		t.Fatalf("erase without --yes sent %v", got)
	}
	remoteAdminEraseYes = true
	if err := cmd.RunE(cmd, []string{"get", "u-1"}); err == nil || !strings.Contains(err.Error(), "--yes confirms") {
		t.Fatalf("--yes on another action = %v, want a refusal", err)
	}
	if err := cmd.RunE(cmd, []string{"erase", "u-1"}); err != nil {
		t.Fatalf("erase --yes: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 || got[0] != "POST /api/admin/users/u-1/erase" {
		t.Fatalf("erase --yes sent %v, want the one POST", got)
	}
}
