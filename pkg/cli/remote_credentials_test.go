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
	for _, tt := range []struct{ name, bot, webhook, reviewMode string }{
		{"personal", "review-pr", "", ""},
		// The review topology rides the preview: a mono pin is a credential
		// demand, so the CLI must carry it or the preview answers a plan the
		// launch never seals.
		{"personal-mono", "review-pr", "", "mono"},
		{"webhook", "", "hook-17", ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			calls := 0
			c := remoteTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.Method != "POST" || r.URL.Path != "/api/teams/selected-team/credentials/preview" {
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
				}
				var in struct {
					Source     struct{ Kind, ID string }
					BotID      string `json:"bot_id"`
					ReviewMode string `json:"review_mode"`
				}
				if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
					t.Fatal(err)
				}
				// The case name may carry a suffix ("personal-mono"); the
				// launch source kind is its first segment.
				kind, _, _ := strings.Cut(tt.name, "-")
				if in.Source.Kind != kind || in.Source.ID != tt.webhook || in.BotID != tt.bot {
					t.Errorf("source changed: %+v", in)
				}
				if in.ReviewMode != tt.reviewMode {
					t.Errorf("review_mode = %q, want %q", in.ReviewMode, tt.reviewMode)
				}
				fmt.Fprint(w, `{"observed_at":"2026-09-14T10:00:00Z","candidates":[{"state":"unknown"}],"warnings":["capacity is not reserved"]}`)
			}))
			p, buf := remotePrinter(cli.OutputJSON)
			if err := cli.RemoteCredentialsPreview(t.Context(), c, p, "selected-team", tt.bot, tt.webhook, tt.reviewMode); err != nil {
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
	if err := cli.RemoteCredentialsPreview(t.Context(), c, p, "selected-team", " ", "", ""); err == nil || !strings.Contains(err.Error(), "--bot") {
		t.Fatalf("missing bot error=%v", err)
	}
}
