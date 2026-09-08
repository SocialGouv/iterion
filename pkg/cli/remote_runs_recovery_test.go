package cli_test

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/cli"
)

func TestRemoteRunsGetWorkspaceCheckpoint(t *testing.T) {
	for _, known := range []bool{false, true} {
		for _, format := range []cli.OutputFormat{cli.OutputHuman, cli.OutputJSON} {
			t.Run(fmt.Sprintf("known=%t/format=%v", known, format), func(t *testing.T) {
				cp := `,"workspace_checkpoint":{"ref":"saved/work","commit":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","source":"run_workspace_checkpoint","event_seq":8,"recorded_at":"2026-09-08T10:00:00Z","fetch_command":"git fetch origin refs/heads/saved/work","warning":"Checkpoint only; validate before merging."}`
				if !known {
					cp = ""
				}
				c := remoteTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Method != http.MethodGet || r.URL.Path != "/api/runs/r" {
						t.Errorf("request = %s %s", r.Method, r.URL.Path)
					}
					fmt.Fprint(w, `{"run":{"id":"r","status":"failed_resumable"`+cp+`}}`)
				}))
				p, buf := remotePrinter(format)
				if err := cli.RemoteRunsGet(context.Background(), c, p, "r"); err != nil {
					t.Fatal(err)
				}
				out := buf.String()
				if known {
					for _, want := range []string{"saved/work", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "run_workspace_checkpoint", "2026-09-08T10:00:00Z", "git fetch origin refs/heads/saved/work", "validate before merging"} {
						if !strings.Contains(out, want) {
							t.Errorf("missing %q in %s", want, out)
						}
					}
				} else if strings.Contains(strings.ToLower(out), "checkpoint") {
					t.Fatalf("invented recovery: %s", out)
				}
			})
		}
	}
}
