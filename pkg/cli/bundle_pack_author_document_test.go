package cli_test

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/cli"
)

// `bundle pack` leaves an author document out of the archive and says so —
// the count in JSON, the line in the human form — without prescribing a
// command.
func TestBundlePackSaysHowManyDraftsItLeftOut(t *testing.T) {
	src := t.TempDir()
	writeFixture(t, src, "main.bot", "tool t:\n  command: \"true\"\n  output: out\n\nschema out:\n  ok: bool\n\nworkflow w:\n  entry: t\n  t -> done\n")
	writeFixture(t, src, "main.bot.yaml", "dsl: 2\n")

	p, buf := newTestPrinter(cli.OutputJSON)
	if err := cli.RunBundlePack(src, filepath.Join(t.TempDir(), "x.botz"), false, p); err != nil {
		t.Fatalf("pack (json): %v", err)
	}
	var res cli.BundlePackResult
	if err := json.Unmarshal(buf.Bytes(), &res); err != nil {
		t.Fatalf("%v\n%s", err, buf.String())
	}
	if res.DraftsLeftOut != 1 {
		t.Fatalf("drafts_left_out = %d, want 1:\n%s", res.DraftsLeftOut, buf.String())
	}

	p, buf = newTestPrinter(cli.OutputHuman)
	if err := cli.RunBundlePack(src, filepath.Join(t.TempDir(), "y.botz"), false, p); err != nil {
		t.Fatalf("pack (human): %v", err)
	}
	if !strings.Contains(buf.String(), "Drafts left out") {
		t.Fatalf("the human form does not say what it left out:\n%s", buf.String())
	}
	if strings.Contains(buf.String(), "--to") {
		t.Fatalf("the notice prescribes a flag this build does not have:\n%s", buf.String())
	}
}
