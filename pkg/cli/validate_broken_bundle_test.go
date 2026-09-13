package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/bundle"
)

// TestRunValidate_BareMainBotOfABundleThatDoesNotOpenIsRefused: the file
// and the directory forms give ONE verdict on a bundle whose manifest does
// not decode — both refuse it, naming the manifest. A bare fall-through
// used to print VALID for the file form: a false green in exactly the state
// validate exists to catch, and the form an agent loop validates.
func TestRunValidate_BareMainBotOfABundleThatDoesNotOpenIsRefused(t *testing.T) {
	inTempWorkspace(t)
	p, _ := testPrinter()
	if err := BotsCreate(BotsCreateOptions{Slug: "mf", Template: "multi-file", Model: "anthropic/claude-opus-4-8", Backend: "claude_code"}, p); err != nil {
		t.Fatalf("BotsCreate: %v", err)
	}
	dir := filepath.Join("bots", "mf")
	if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte("schema_version: 99\nname: [broken\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := bundle.OpenDir(dir); err == nil {
		t.Fatal("the fixture manifest still opens; the case it guards is not exercised")
	}
	for _, target := range []string{dir, filepath.Join(dir, "main.bot")} {
		jp, out := jsonPrinter()
		err := RunValidate(target, jp)
		if err == nil || !strings.Contains(err.Error(), "manifest") {
			t.Errorf("validate %s: err=%v, want the bundle refused by its manifest\n%s", target, err, out.String())
		}
	}
	// The same refusal on the opener run and resume share with validate.
	if _, _, _, cleanup, err := openBundleOrFile(filepath.Join(dir, "main.bot")); err == nil {
		_ = cleanup()
		t.Fatal("openBundleOrFile promoted a main.bot whose bundle does not open")
	}
}
