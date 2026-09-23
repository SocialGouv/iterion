package bundle

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Detect refuses an author document by name, with the one sentinel every
// launcher shares, so a caller that reads one (validate) tells the refusal
// apart with errors.Is and every other caller shows it as written — the
// remedy, not "unsupported extension".
func TestDetectRefusesAnAuthorDocumentByName(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "draft.bot.yaml")
	if err := os.WriteFile(path, []byte("dsl: 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Detect(path)
	if !errors.Is(err, ErrAuthorDocument) {
		t.Fatalf("Detect(%s) = %v, want ErrAuthorDocument", path, err)
	}
	if !strings.Contains(err.Error(), "write the .bot") || !strings.Contains(err.Error(), path) {
		t.Fatalf("the refusal names neither the remedy nor the path: %v", err)
	}
	if strings.Contains(err.Error(), "unsupported workflow extension") {
		t.Fatalf("an author document is not an unsupported extension: %v", err)
	}
}

// The name decides, before the disk: a directory named like a draft that
// holds a main.bot, and a path that does not exist, are refused as an author
// document — the one rule the doors that cannot stat apply, applied here too.
func TestDetectRefusesAnAuthorDocumentsNameBeforeTheDisk(t *testing.T) {
	dir := t.TempDir()
	named := filepath.Join(dir, "x.bot.yaml")
	if err := os.MkdirAll(named, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(named, "main.bot"), []byte("dsl: 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Detect(named); !errors.Is(err, ErrAuthorDocument) {
		t.Fatalf("Detect on a directory named like a draft: %v, want ErrAuthorDocument (the launchers that cannot stat refuse it by name)", err)
	}
	if _, err := Detect(filepath.Join(dir, "absent.bot.yaml")); !errors.Is(err, ErrAuthorDocument) {
		t.Fatalf("Detect on an absent draft path: %v, want ErrAuthorDocument, not a stat error", err)
	}
	if kind, err := Detect(filepath.Join(dir, "x.bot.yaml", "main.bot")); err != nil || kind != KindBot {
		t.Fatalf("the .bot inside the directory is a .bot: %v %v", kind, err)
	}
}
