package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/floortest"
	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

// writeContractedBundle is a bundle whose main declares a contract, under a
// floor far below any release that reads one.
func writeContractedBundle(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	bot := "## the bot\n\nvars:\n  goal: string\n\ncontract pub:\n  inputs:\n    goal: string\n\nworkflow w:\n  contract: pub\n  entry: done\n"
	if err := os.WriteFile(filepath.Join(dir, "main.bot"), []byte(bot), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte("name: contracted\ndisplay_name: Contracted\nrequires:\n  iterion: \">= 1.0.0\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// `dsl migrate` raises a bundle's floor to what its sources need, not only to
// the profile's release or this build's: a bundle that declares a contract
// gets a floor that reaches the contract release, so the migration never
// leaves a floor `validate` then asks to raise (C252).
//
// The `--floor` the probe gives is derived from the contract release, and
// what the raise is held against is what the migrated sources ask for, so
// neither depends on which pin is the newest — including the day two floors
// are cut in the same release, where the aligner writes that version into
// both and a `--floor` written as the lower pin would already be the contract
// release.
func TestMigrateDSLRaisesTheFloorToWhatTheSourcesNeed(t *testing.T) {
	floortest.EachPinOrder(t, parser.ContractSince, func(t *testing.T) {
		low := floortest.Below(t, parser.ContractSince)
		if cmp, ok := bundle.CompareVersions(low, parser.ContractSince); !ok || cmp >= 0 {
			t.Fatalf("--floor %q does not sit below the contract release %q: the raise this case exercises cannot happen", low, parser.ContractSince)
		}
		dir := writeContractedBundle(t)
		var out bytes.Buffer
		res, err := MigrateDSL(t.Context(), MigrateDSLOptions{Paths: []string{dir}, Floor: "v" + low, Printer: &Printer{W: &out, Format: OutputHuman}})
		if err != nil {
			t.Fatalf("MigrateDSL: %v\n%s", err, out.String())
		}
		// What the migrated sources read — the profile `dsl migrate` wrote and
		// the contract the main declares — decides the floor, and it has to
		// reach the contract release the probe is about.
		need, reason := bundle.RequiredRelease(bundle.MaxSyntaxRequirementsDir(dir))
		if cmp, ok := bundle.CompareVersions(need, parser.ContractSince); !ok || cmp < 0 {
			t.Fatalf("the migrated sources ask for %q (%s), which does not reach the contract release %q", need, reason, parser.ContractSince)
		}
		if len(res.Manifests) != 1 || !res.Manifests[0].Written || res.Manifests[0].To != ">= "+need {
			t.Fatalf("manifests: %+v (the floor must reach %s, the release the migrated sources need, not the one given)", res.Manifests, need)
		}
		m, err := bundle.LoadManifest(filepath.Join(dir, "manifest.yaml"))
		if err != nil || m.Requires == nil || !strings.Contains(m.Requires.Iterion, need) {
			t.Fatalf("rewritten manifest: %v %+v", err, m)
		}
	})
}
