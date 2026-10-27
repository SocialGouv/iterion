package server

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The localize path refuses an unclaimed consumer (#2537): the landed copy
// goes under plugins/<name> INSIDE the consumer's directory, and discovery
// only skips the subtree of a directory it CLAIMS — manifest.yaml (exact
// spelling) AND main.bot. A consumer failing that test would have its copy
// leak into the routing namespace, shadow the .botz materialization, and
// drop the consumer from the catalogue. Both defects are refused BY NAME,
// before anything moves; a healthy consumer localizes exactly as before.

// A consumer saved under the manifest.yml spelling is another tool's
// manifest to discovery: the localize refuses naming the spelling.
func TestLocalizeRefusesAManifestYmlConsumer(t *testing.T) {
	s, root, _ := assistantDependencyLocalizeFixture(t)
	consumer := filepath.Join(root, "bots", "planner")
	manifestYaml := filepath.Join(consumer, "manifest.yaml")
	manifestBody, err := os.ReadFile(manifestYaml)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(consumer, "manifest.yml"), manifestBody, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(manifestYaml); err != nil {
		t.Fatal(err)
	}
	rec := assistantDependencyLocalizeCall(t, s, assistantDependencyBotsLocalizeRequest{
		EditorPath: "bots/planner/main.bot", Name: "shared-planner", Message: "chore(planner): vendor shared planner source",
	})
	if rec.Code != 409 {
		t.Fatalf("manifest.yml consumer localize = %d %s — want 409", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "manifest.yaml") || !strings.Contains(rec.Body.String(), "discovery claims") {
		t.Fatalf("body %s: the refusal must name the exact spelling and the discovery claim", rec.Body.String())
	}
	if _, err := os.Stat(filepath.Join(root, "bots", "planner", "plugins", "shared-planner")); !os.IsNotExist(err) {
		t.Fatalf("the localize moved material under an unclaimed consumer: %v", err)
	}
}

// A consumer whose main.bot is absent is not claimed either — the localize
// refuses naming the missing entrypoint.
func TestLocalizeRefusesAConsumerWithoutAMainBot(t *testing.T) {
	s, root, _ := assistantDependencyLocalizeFixture(t)
	if err := os.Remove(filepath.Join(root, "bots", "planner", "main.bot")); err != nil {
		t.Fatal(err)
	}
	rec := assistantDependencyLocalizeCall(t, s, assistantDependencyBotsLocalizeRequest{
		EditorPath: "bots/planner/main.bot", Name: "shared-planner", Message: "chore(planner): vendor shared planner source",
	})
	if rec.Code != 409 {
		t.Fatalf("consumer without main.bot localize = %d %s — want 409", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "main.bot") {
		t.Fatalf("body %s: the refusal must name the missing entrypoint", rec.Body.String())
	}
	if _, err := os.Stat(filepath.Join(root, "bots", "planner", "plugins", "shared-planner")); !os.IsNotExist(err) {
		t.Fatalf("the localize moved material under an unclaimed consumer: %v", err)
	}
}
