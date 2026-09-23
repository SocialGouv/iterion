package botsource

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/SocialGouv/iterion/pkg/bundle"
)

// The store holds what launches: an author document pushed by name — whole
// bundle or one file — is refused at the one chokepoint every write crosses,
// with the typed refusal every launcher shares; the directory reader that
// feeds a push leaves a draft behind rather than carrying it to the store.
func TestTheStoreNeverHoldsADraft(t *testing.T) {
	s := validSource("team-1", "reviewer")
	s.Files["main.bot.yaml"] = "dsl: 2\n"
	if err := s.Validate(); !errors.Is(err, bundle.ErrAuthorDocument) {
		t.Fatalf("Validate with a draft: %v, want ErrAuthorDocument", err)
	}
	s = validSource("team-1", "reviewer")
	s.Files["lib/x.bot.yaml"] = "dsl: 2\n"
	if err := s.Validate(); !errors.Is(err, bundle.ErrAuthorDocument) {
		t.Fatalf("Validate with a nested draft: %v, want ErrAuthorDocument", err)
	}

	dir := t.TempDir()
	for rel, body := range map[string]string{"main.bot": "dsl: 2\n", "main.bot.yaml": "dsl: 2\n", "lib/x.bot.yaml": "dsl: 2\n"} {
		full := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	files, err := ReadBundleDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := files["main.bot"]; !ok {
		t.Fatal("the .bot was left behind")
	}
	for key := range files {
		if filepath.Ext(key) == ".yaml" {
			t.Fatalf("the reader carried a draft to the store: %s", key)
		}
	}
}

// The store's two walkers describe one bundle: ExecutableFiles leaves the
// draft behind as ReadBundleDir does — a warning about its mode would name a
// file the store never receives — and a directory named like a draft is a
// directory for both, its files members.
func TestTheStoresWalkersAgreeOnWhatADraftIs(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "x.bot.yaml"), 0o755); err != nil {
		t.Fatal(err)
	}
	for rel, mode := range map[string]os.FileMode{"main.bot": 0o644, "main.bot.yaml": 0o755, "x.bot.yaml/inner.sh": 0o755} {
		if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(rel)), []byte("dsl: 2\n"), mode); err != nil {
			t.Fatal(err)
		}
	}
	files, err := ReadBundleDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := files["main.bot.yaml"]; ok {
		t.Fatal("the reader carried a draft to the store")
	}
	if _, ok := files["x.bot.yaml/inner.sh"]; !ok {
		t.Fatalf("the reader took a directory named like a draft for one and left its files behind: %v", files)
	}
	exec := ExecutableFiles(dir)
	seen := map[string]bool{}
	for _, p := range exec {
		seen[p] = true
	}
	if seen["main.bot.yaml"] {
		t.Fatalf("ExecutableFiles lists a draft the store never receives: %v", exec)
	}
	if !seen["x.bot.yaml/inner.sh"] {
		t.Fatalf("ExecutableFiles left out an executable member under a directory named like a draft: %v", exec)
	}
}
