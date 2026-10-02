package botinstall

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// An install from a directory (or a clone) lands the tree the archive path
// would: an author document beside the sources is never a member of the
// installed bundle.
func TestAnInstallFromADirectoryLeavesTheDraftBehind(t *testing.T) {
	src := t.TempDir()
	for rel, body := range map[string]string{
		"main.bot":      "schema empty:\n  ok: bool\n\ntool t:\n  command: \"true\"\n  output: empty\n\nworkflow w:\n  entry: t\n  t -> done\n",
		"manifest.yaml": "name: drafted\nversion: 0.1.0\n",
		"main.bot.yaml": "dsl: 2\n",
	} {
		if err := os.WriteFile(filepath.Join(src, rel), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	res, err := Install(context.Background(), Options{Source: src, Name: "drafted", Workdir: t.TempDir(), SkipCatalog: true})
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if _, err := os.Stat(filepath.Join(res.InstalledPath, "main.bot")); err != nil {
		t.Fatalf("the .bot was not installed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(res.InstalledPath, "main.bot.yaml")); err == nil {
		t.Fatal("the install from a directory copied a draft into the installed bundle")
	}
}
