package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/webhooks"
	"github.com/SocialGouv/iterion/pkg/webhooks/gitlab"
)

// The label door's visibility gate (#2676): a label starts a bot the
// catalogue ENABLES — the same judgment the card launch door makes. A bot
// disabled by the workspace overlay does not start; a refused overlay
// starts nothing (fail closed); an enabled bot launches as before.

// labelVisibilityServer builds the webhook server over a workspace whose
// catalogue carries one bot (bots/feature-dev.bot, named by its stem), so
// the overlay at .iterion/bot-overrides.yaml composes over it.
func labelVisibilityServer(t *testing.T) *Server {
	t.Helper()
	s := newWebhookTestServer(t)
	s.cfg.WorkDir = t.TempDir()
	if err := os.MkdirAll(filepath.Join(s.cfg.WorkDir, "bots"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.cfg.WorkDir, "bots", "feature-dev.bot"), []byte(testBotMain), 0o644); err != nil {
		t.Fatal(err)
	}
	return s
}

func launchCapture(t *testing.T, s *Server) *int {
	t.Helper()
	calls := 0
	s.webhookLaunchBot = func(_ context.Context, botID string, vars map[string]string, repoURL, repoRef, _ string, _, _ map[string]string) (string, error) {
		calls++
		return "run-x", nil
	}
	return &calls
}

func postLabeled(t *testing.T, s *Server) (int, string) {
	t.Helper()
	w := httptest.NewRecorder()
	s.handleGitLabWebhook(w, glReq(gitlabCtx(glIssueConfig()), glLabeledIssue, gitlab.EventHeaderIssue))
	return w.Code, w.Body.String()
}

// A bot the workspace overlay disabled does not start on its label: the
// delivery is filtered naming the bot, and no launch is composed.
func TestLabelDoorRefusesAnOverlayDisabledBot(t *testing.T) {
	s := labelVisibilityServer(t)
	calls := launchCapture(t, s)
	overlay := filepath.Join(s.cfg.WorkDir, ".iterion", "bot-overrides.yaml")
	if err := os.MkdirAll(filepath.Dir(overlay), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(overlay, []byte("bots:\n  feature-dev:\n    enabled: false\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, body := postLabeled(t, s)
	if code != http.StatusOK || !strings.Contains(body, webhooks.StatusFiltered) {
		t.Fatalf("disabled bot on its label: %d %s — want the filtered answer", code, body)
	}
	if !strings.Contains(body, "feature-dev") {
		t.Fatalf("body %q: the refusal must name the bot", body)
	}
	if *calls != 0 {
		t.Fatalf("launches=%d: a disabled bot must not start on its label", *calls)
	}
}

// A refused overlay starts nothing — fail closed, the card door's rule.
func TestLabelDoorStartsNothingOnARefusedOverlay(t *testing.T) {
	s := labelVisibilityServer(t)
	calls := launchCapture(t, s)
	overlay := filepath.Join(s.cfg.WorkDir, ".iterion", "bot-overrides.yaml")
	if err := os.MkdirAll(filepath.Dir(overlay), 0o755); err != nil {
		t.Fatal(err)
	}
	// Past the size cap: the overlay is refused before any decode.
	if err := os.WriteFile(overlay, make([]byte, 11<<20), 0o644); err != nil {
		t.Fatal(err)
	}
	code, body := postLabeled(t, s)
	if code != http.StatusOK || !strings.Contains(body, webhooks.StatusFiltered) {
		t.Fatalf("refused overlay on a label: %d %s — want the filtered answer", code, body)
	}
	if *calls != 0 {
		t.Fatalf("launches=%d: a refused overlay starts nothing", *calls)
	}
}

// The door stays open for an enabled bot — and the delivery is the launch
// the label always carried.
func TestLabelDoorStillLaunchesAnEnabledBot(t *testing.T) {
	s := labelVisibilityServer(t)
	calls := launchCapture(t, s)
	code, body := postLabeled(t, s)
	if code != http.StatusAccepted {
		t.Fatalf("enabled bot on its label: %d %s — want accepted", code, body)
	}
	if *calls != 1 {
		t.Fatalf("launches=%d: an enabled bot launches as before", *calls)
	}
}
