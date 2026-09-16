package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

// `dsl migrate` raises a bundle's floor to what its sources need, not only
// to the profile's release or this build's: a bundle that declares a
// contract gets the contract release, so the migration never leaves a
// floor `validate` then asks to raise (C252).
func TestMigrateDSLRaisesTheFloorToWhatTheSourcesNeed(t *testing.T) {
	dir := t.TempDir()
	bot := "## the bot\n\nvars:\n  goal: string\n\ncontract pub:\n  inputs:\n    goal: string\n\nworkflow w:\n  contract: pub\n  entry: done\n"
	if err := os.WriteFile(filepath.Join(dir, "main.bot"), []byte(bot), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte("name: contracted\ndisplay_name: Contracted\nrequires:\n  iterion: \">= 1.0.0\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	res, err := MigrateDSL(MigrateDSLOptions{Paths: []string{dir}, Floor: "v3.141.0", Printer: &Printer{W: &out, Format: OutputHuman}})
	if err != nil {
		t.Fatalf("MigrateDSL: %v\n%s", err, out.String())
	}
	if len(res.Manifests) != 1 || !res.Manifests[0].Written || res.Manifests[0].To != ">= "+parser.ContractSince {
		t.Fatalf("manifests: %+v (the floor must reach the contract release, not the one given)", res.Manifests)
	}
	m, err := bundle.LoadManifest(filepath.Join(dir, "manifest.yaml"))
	if err != nil || m.Requires == nil || !strings.Contains(m.Requires.Iterion, parser.ContractSince) {
		t.Fatalf("rewritten manifest: %v %+v", err, m)
	}
}
