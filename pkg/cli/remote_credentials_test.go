package cli_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/cli"
)

func TestRemoteCredentialsPreviewPreservesLaunchSource(t *testing.T) {
	for _, tt := range []struct{ name, bot, webhook string }{
		{"personal", "review-pr", ""}, {"webhook", "", "hook-17"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			calls := 0
			c := remoteTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.Method != "POST" || r.URL.Path != "/api/teams/selected-team/credentials/preview" {
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
				}
				var in struct {
					Source struct{ Kind, ID string }
					BotID  string `json:"bot_id"`
				}
				if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
					t.Fatal(err)
				}
				if in.Source.Kind != tt.name || in.Source.ID != tt.webhook || in.BotID != tt.bot {
					t.Errorf("source changed: %+v", in)
				}
				fmt.Fprint(w, `{"observed_at":"2026-09-14T10:00:00Z","candidates":[{"state":"unknown"}],"warnings":["capacity is not reserved"]}`)
			}))
			p, buf := remotePrinter(cli.OutputJSON)
			if err := cli.RemoteCredentialsPreview(t.Context(), c, p, "selected-team", tt.bot, tt.webhook); err != nil {
				t.Fatal(err)
			}
			if calls != 1 || !strings.Contains(buf.String(), `"unknown"`) || !strings.Contains(buf.String(), "capacity is not reserved") {
				t.Fatalf("calls=%d output=%s", calls, buf)
			}
		})
	}
}

func TestRemoteCredentialsPreviewRequiresPersonalBotBeforeNetwork(t *testing.T) {
	c := remoteTestClient(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("invalid preview made a network request") }))
	p, _ := remotePrinter(cli.OutputJSON)
	if err := cli.RemoteCredentialsPreview(t.Context(), c, p, "selected-team", " ", ""); err == nil || !strings.Contains(err.Error(), "--bot") {
		t.Fatalf("missing bot error=%v", err)
	}
}
