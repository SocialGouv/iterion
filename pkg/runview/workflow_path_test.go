package runview

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/SocialGouv/iterion/pkg/store"
)

// The regression: a catalog-bot run persists a repo-relative FilePath
// ("bots/app-dev/main.bot"). A cloud server pod has a different working dir and
// ships the catalog at $ITERION_BOTS_PATH, so the studio's workflow view 404'd
// on every catalog-bot run in cloud mode.
func TestResolveWorkflowPath_FallsBackToBotCatalog(t *testing.T) {
	catalog := t.TempDir()
	botDir := filepath.Join(catalog, "app-dev")
	if err := os.MkdirAll(botDir, 0o755); err != nil {
		t.Fatal(err)
	}
	main := filepath.Join(botDir, "main.bot")
	if err := os.WriteFile(main, []byte("workflow x:\n  entry: a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A bundle is only discoverable by botregistry.List with its manifest.
	if err := os.WriteFile(filepath.Join(botDir, "manifest.yaml"),
		[]byte("name: app-dev\ndisplay_name: Appy\nschema_version: 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ITERION_BOTS_PATH", catalog)

	got := resolveWorkflowPath(&store.Run{FilePath: "bots/app-dev/main.bot", BotID: "app-dev"})
	if got != main {
		t.Errorf("got %q, want the catalog path %q", got, main)
	}
}

// An existing path must win untouched — local runs and absolute paths keep
// their current behaviour.
func TestResolveWorkflowPath_ExistingPathWins(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "main.bot")
	if err := os.WriteFile(real, []byte("workflow x:\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ITERION_BOTS_PATH", t.TempDir())

	if got := resolveWorkflowPath(&store.Run{FilePath: real, BotID: "app-dev"}); got != real {
		t.Errorf("got %q, want the persisted path %q", got, real)
	}
}

// Unresolvable stays as-is so the caller surfaces the real compile error
// instead of a masked one.
func TestResolveWorkflowPath_UnresolvableKeepsOriginal(t *testing.T) {
	t.Setenv("ITERION_BOTS_PATH", t.TempDir())
	in := &store.Run{FilePath: "bots/nope/main.bot", BotID: "nope"}
	if got := resolveWorkflowPath(in); got != in.FilePath {
		t.Errorf("got %q, want the original %q", got, in.FilePath)
	}
	// No BotID (a plain .bot run) must not consult the catalog either.
	plain := &store.Run{FilePath: "missing.bot"}
	if got := resolveWorkflowPath(plain); got != "missing.bot" {
		t.Errorf("got %q, want %q", got, "missing.bot")
	}
}

func TestBotsPathsFromEnv(t *testing.T) {
	t.Setenv("ITERION_BOTS_PATH", "")
	if got := botsPathsFromEnv(); got != nil {
		t.Errorf("unset env should yield nil, got %v", got)
	}
	t.Setenv("ITERION_BOTS_PATH", "/opt/iterion/bots:/srv/bots/")
	got := botsPathsFromEnv()
	if len(got) != 2 || got[0] != "/opt/iterion/bots" || got[1] != "/srv/bots" {
		t.Errorf("got %v, want the two cleaned paths", got)
	}
}
