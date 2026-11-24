package runtime

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/authoringlock"
	"github.com/SocialGouv/iterion/pkg/botregistry"
)

// catalogOwnerWorkspace vendors one bundle that owns a generated catalog,
// with a committed copy holding bytes this build never renders.
func catalogOwnerWorkspace(t *testing.T) (ws, generated, committed string) {
	t.Helper()
	ws = t.TempDir()
	owner := filepath.Join(ws, "bots", "whats-next")
	if err := os.MkdirAll(filepath.Join(owner, "skills"), 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(path, body string) {
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(owner, "manifest.yaml"), "name: whats-next\ndisplay_name: P\ndescription: Routes work.\n")
	write(filepath.Join(owner, "main.bot"), "agent x:\n  model: \"test\"\n")
	write(filepath.Join(owner, "iterion-bot-catalog-static.md"),
		"---\nname: iterion-bot-catalog\n---\nPREAMBLE\n\n<!-- ITERION:CATALOG:GENERATED:BEGIN -->\n<!-- ITERION:CATALOG:GENERATED:END -->\n")
	committed = "---\nname: iterion-bot-catalog\n---\nAN EDITOR DECIDED ON THESE BYTES\n"
	generated = filepath.Join(owner, "skills", "iterion-bot-catalog.md")
	write(generated, committed)
	botregistry.ClearSchemaCache()
	return ws, generated, committed
}

// TestARunStartDoesNotSitBehindASaveOfTheCatalog is the hot-path half of
// the authoring lock: the catalog is regenerated at EVERY run start, and
// it is now written under a lock an editor's save holds too. A run must
// not wait on that save for longer than the bound this package sets —
// the refresh is best effort, and a run start is not a gate an editor
// gets to hold. Without the bound the engine passes the run's own
// context, which carries no deadline: the wait would be forever.
func TestARunStartDoesNotSitBehindASaveOfTheCatalog(t *testing.T) {
	ws, generated, committed := catalogOwnerWorkspace(t)
	lock, err := authoringlock.Take(context.Background(), generated)
	if err != nil {
		t.Fatalf("stand in for an editor's save: %v", err)
	}
	defer func() {
		if err := lock.Release(); err != nil {
			t.Errorf("release: %v", err)
		}
	}()

	previous := catalogLockWait
	catalogLockWait = 200 * time.Millisecond
	t.Cleanup(func() { catalogLockWait = previous })

	type outcome struct {
		path string
		err  error
	}
	// The call runs in a goroutine so a wait with no bound fails this
	// test's own assertion instead of the harness's global timeout. The
	// deferred release above ends the goroutine in every case.
	done := make(chan outcome, 1)
	go func() {
		path, err := (&Engine{workDir: ws}).regenerateBotCatalog(context.Background(), "")
		done <- outcome{path, err}
	}()
	select {
	case got := <-done:
		if got.err == nil {
			t.Fatalf("a refresh over a save in flight = %q, nil; want the bounded refusal", got.path)
		}
		if !strings.Contains(got.err.Error(), "authoring lock was not taken") {
			t.Errorf("the refusal does not name the lock as its cause: %v", got.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the catalog refresh did not come back within 5s though the bound is 200ms: a run start can sit behind an editor's save for as long as the save lasts")
	}

	body, err := os.ReadFile(generated)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != committed {
		t.Errorf("the bytes the editor decided on were replaced:\n%s", body)
	}
}
