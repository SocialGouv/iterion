package dispatcher

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/SocialGouv/iterion/pkg/bundle"
)

// The dispatcher's configuration names the workflows it will launch: an
// author document there is refused when the configuration is validated,
// by the one sentinel every launcher shares, never at the first ticket.
func TestDispatcherConfigRefusesAnAuthorDocument(t *testing.T) {
	dir := t.TempDir()
	draft := filepath.Join(dir, "main.bot.yaml")
	if err := os.WriteFile(draft, []byte("dsl: 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	bot := filepath.Join(dir, "main.bot")
	if err := os.WriteFile(bot, []byte("dsl: 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := &Config{Workflow: draft, Tracker: TrackerConfig{Kind: "fake"}}
	if err := cfg.Validate(); !errors.Is(err, bundle.ErrAuthorDocument) {
		t.Fatalf("workflow as a draft: %v, want ErrAuthorDocument", err)
	}
	cfg = &Config{Workflow: bot, AssigneeWorkflows: map[string]string{"triage": draft}, Tracker: TrackerConfig{Kind: "fake"}}
	if err := cfg.Validate(); !errors.Is(err, bundle.ErrAuthorDocument) {
		t.Fatalf("assignee workflow as a draft: %v, want ErrAuthorDocument", err)
	}
}
