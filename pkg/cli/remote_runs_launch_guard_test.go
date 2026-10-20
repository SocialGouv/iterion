package cli_test

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/cli"
)

// A launch cannot carry a workflow file and --bot together: the file ships
// as inline source and the server resolves no bot for an inline source, so
// the combination would silently launch unnamed work — no scoped key, bot
// binding, pledge or bot-scoped memory would apply to it.
func TestRemoteRunsLaunchRefusesAFileAndABotTogether(t *testing.T) {
	called := false
	c := remoteTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		fmt.Fprint(w, `{"run_id":"r1","status":"running"}`)
	}))
	dir := t.TempDir()
	file := dir + "/x.bot"
	if err := os.WriteFile(file, []byte("workflow demo:\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	p, _ := remotePrinter(cli.OutputHuman)
	err := cli.RemoteRunsLaunch(context.Background(), c, p, cli.RemoteRunsLaunchOptions{
		FilePath: file,
		BotID:    "review-pr",
	})
	if err == nil {
		t.Fatal("launch accepted a workflow file and --bot together, want a mutual-exclusion error")
	}
	if !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("err = %v, want the mutual-exclusion wording", err)
	}
	if called {
		t.Fatal("the refused launch reached the server")
	}
}
