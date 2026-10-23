package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/botregistry"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

const overlaySecretManifest = `name: secret-bot
version: 1.0.0
invocations:
  - kind: command
    mode: board
    args_var: scope_notes
    command:
      name: secret
      scope: any
`

// A workspace overlay its reader refuses holds the operator's visibility
// decisions unread: every door of the server that launches or dispatches by
// a bot's resolved visibility refuses while it is — the card launch, the
// slash command's discovery — and the gallery, which opts past it, lists
// every bot disabled with the refusal said. Answered as "no overlay", each
// door launched the bot the operator disabled (r4b F1).
func TestAnUnreadOverlayClosesTheServersDoors(t *testing.T) {
	t.Setenv(parser.ReadBudgetEnv, "400")
	setup := func(t *testing.T, overlay string) *Server {
		t.Helper()
		work := t.TempDir()
		writeFiles(t, work, map[string]string{
			"bots/secret-bot/main.bot":      metaBakedBot,
			"bots/secret-bot/manifest.yaml": overlaySecretManifest,
		})
		if overlay != "" {
			writeFiles(t, work, map[string]string{".iterion/bot-overrides.yaml": overlay})
		}
		s := newOrgTestServer(t)
		s.cfg.WorkDir = work
		return s
	}
	ctx := context.Background()
	t.Run("the overlay read: the decision honoured, every door agrees", func(t *testing.T) {
		s := setup(t, "")
		bot, found, err := s.resolvePipelineBot(ctx, "", "secret-bot")
		if err != nil || !found || !bot.Enabled {
			t.Fatalf("control: the card door = %+v found=%v err=%v, want the bot enabled", bot, found, err)
		}
		bot.Launch.Cleanup()
		if _, ok := s.cmdDiscoveryFor(ctx, "").LookupCommand("secret"); !ok {
			t.Fatal("control: /secret does not resolve with no overlay")
		}
	})
	t.Run("an overlay past the read budget", func(t *testing.T) {
		decisions := "bots:\n  secret-bot:\n    enabled: false\n"
		s := setup(t, decisions+"pad: {"+strings.Repeat("a,", 600)+"a}\n")
		var refused *botregistry.OverlayRefusal
		if bot, found, err := s.resolvePipelineBot(ctx, "", "secret-bot"); !errors.As(err, &refused) {
			if found {
				bot.Launch.Cleanup()
			}
			t.Errorf("the card door over a refused overlay = enabled=%v found=%v err=%v, want the overlay's refusal", bot.Enabled, found, err)
		}
		if route, ok := s.cmdDiscoveryFor(ctx, "").LookupCommand("secret"); ok {
			t.Errorf("/secret resolved to %+v over a refused overlay, want no route", route)
		}
		w := httptest.NewRecorder()
		s.handleBotsList(w, httptest.NewRequest(http.MethodGet, "/api/v1/bots", nil))
		var listed struct {
			Bots []struct {
				Name    string `json:"name"`
				Enabled bool   `json:"enabled"`
			} `json:"bots"`
			Diags []botregistry.DiscoveryError `json:"discovery_errors"`
		}
		if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &listed) != nil {
			t.Fatalf("the gallery over a refused overlay = %d %s", w.Code, w.Body.String())
		}
		if len(listed.Bots) != 1 || listed.Bots[0].Enabled {
			t.Errorf("the gallery lists %+v, want the bot listed disabled", listed.Bots)
		}
		said := false
		for _, d := range listed.Diags {
			if d.Kind == botregistry.DiscoveryErrorOverlay && strings.Contains(d.Error, parser.ReadBudgetEnv) {
				said = true
			}
		}
		if !said {
			t.Errorf("the gallery's diagnostics %+v do not say the overlay is not read", listed.Diags)
		}
	})
}
