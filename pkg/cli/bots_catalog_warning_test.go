package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/botregistry"
)

// breakCatalogOwner vendors a catalog owner under <workdir>/bots/nexie whose
// hand-authored template is a self-referencing symlink: ELOOP, which no
// privilege reads, so the refresh fails whoever runs the test. It returns
// the bundle root every regeneration error names.
func breakCatalogOwner(t *testing.T, workdir string) string {
	t.Helper()
	dir := filepath.Join(workdir, "bots", "nexie")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte("name: nexie\nschema_version: 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.bot"), []byte("workflow nexie {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tmpl := filepath.Join(dir, "iterion-bot-catalog-static.md")
	if err := os.Symlink(tmpl, tmpl); err != nil {
		t.Fatalf("symlink unavailable: %v", err)
	}
	botregistry.ClearSchemaCache()
	return dir
}

// TestBotsCreate_ReportsTheCatalogRegenFailureInBothFormats: `iterion bots
// create` scaffolds a valid bundle whatever the catalog refresh does, so the
// failure has to ride on the output — in JSON too, where a machine consumer
// is the only reader there is.
func TestBotsCreate_ReportsTheCatalogRegenFailureInBothFormats(t *testing.T) {
	t.Run("human", func(t *testing.T) {
		workdir := inTempWorkspace(t)
		owner := breakCatalogOwner(t, workdir)
		p, out := testPrinter()

		if err := BotsCreate(BotsCreateOptions{Slug: "my-bot", Workdir: workdir}, p); err != nil {
			t.Fatalf("BotsCreate: %v", err)
		}
		got := out.String()
		if !strings.Contains(got, "Bot created") {
			t.Fatalf("the scaffold itself must still be reported as done:\n%s", got)
		}
		if !strings.Contains(got, owner) {
			t.Errorf("the output does not name the catalog it could not refresh (%s):\n%s", owner, got)
		}
		if !strings.Contains(got, "iterion bots regen-catalog") {
			t.Errorf("the output does not tell the operator how to refresh the catalog:\n%s", got)
		}
	})

	t.Run("json", func(t *testing.T) {
		workdir := inTempWorkspace(t)
		owner := breakCatalogOwner(t, workdir)
		p, out := jsonPrinter()

		if err := BotsCreate(BotsCreateOptions{Slug: "my-bot", Workdir: workdir}, p); err != nil {
			t.Fatalf("BotsCreate: %v", err)
		}
		var got map[string]any
		if err := json.Unmarshal(out.Bytes(), &got); err != nil {
			t.Fatalf("decode: %v; out=%s", err, out.String())
		}
		if dir, _ := got["dir"].(string); dir == "" {
			t.Fatalf("the scaffold result must keep its own fields: %s", out.String())
		}
		msg, _ := got["catalog_error"].(string)
		if msg == "" {
			t.Fatalf("the JSON create result hides the catalog regeneration failure: %s", out.String())
		}
		if !strings.Contains(msg, owner) {
			t.Errorf("catalog_error %q does not name the catalog owner it could not refresh (%s)", msg, owner)
		}
	})
}

// TestPrintCatalogWarning_RendersARelayedCauseInertOnOneLine: the cause is
// relayed text — it joins one failure per catalog owner (errors.Join,
// newline-separated) and embeds the parse error of whatever discovery read,
// which echoes a foreign manifest's own keys. The warning is one line of
// cause plus one line of remedy, whatever the cause carries.
func TestPrintCatalogWarning_RendersARelayedCauseInertOnOneLine(t *testing.T) {
	p, out := testPrinter()
	forged := "installed  evil-bot  " + strings.Repeat("0", 64)
	cause := "botregistry: first owner unreadable\n" + forged + "\n\x1b]0;owned\x07second owner unreadable"

	PrintCatalogWarning(p, cause)

	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("the warning rendered %d lines, want the cause line and the remedy:\n%q", len(lines), out.String())
	}
	if !strings.Contains(lines[0], forged) {
		t.Errorf("the cause line drops what the cause carried:\n%q", lines[0])
	}
	if !strings.Contains(lines[0], "second owner unreadable") {
		t.Errorf("the cause line drops the second owner's failure:\n%q", lines[0])
	}
	if !strings.Contains(lines[1], "iterion bots regen-catalog") {
		t.Errorf("the remedy line is not the second line:\n%q", lines[1])
	}
	if strings.ContainsAny(out.String(), "\x1b\x07") {
		t.Errorf("the warning sent a control character to the terminal raw:\n%q", out.String())
	}
}

// TestBotsCreate_SilentWhenTheCatalogRefreshSucceeds: a workspace with no
// catalog owner has nothing to refresh, and nothing to report.
func TestBotsCreate_SilentWhenTheCatalogRefreshSucceeds(t *testing.T) {
	workdir := inTempWorkspace(t)
	p, out := testPrinter()

	if err := BotsCreate(BotsCreateOptions{Slug: "my-bot", Workdir: workdir}, p); err != nil {
		t.Fatalf("BotsCreate: %v", err)
	}
	if strings.Contains(out.String(), "not refreshed") {
		t.Errorf("a refresh with nothing to write must report no failure:\n%s", out.String())
	}
}
