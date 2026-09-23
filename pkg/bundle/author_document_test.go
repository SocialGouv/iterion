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
