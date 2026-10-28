package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/SocialGouv/iterion/pkg/botregistry"
	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/cli"
)

// `iterion bots install` and `iterion bots sync` refresh the catalog Nexie
// routes on. The gesture succeeds whatever the refresh does, so the only
// place the operator can learn their catalog went stale is the command's
// own output.

// writeCatalogTestBundle writes a minimal bundle (manifest + main.bot).
func writeCatalogTestBundle(t *testing.T, dir, name string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"),
		[]byte("name: "+name+"\nversion: 0.1.0\nschema_version: 1\ndescription: test bot\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.bot"), []byte("workflow w:\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// breakCatalogOwner vendors a catalog owner under <workdir>/bots/nexie whose
// hand-authored template is a self-referencing symlink: ELOOP, which no
// privilege reads, so the regeneration fails whoever runs the test. It
// returns the bundle root every regeneration error names.
func breakCatalogOwner(t *testing.T, workdir string) string {
	t.Helper()
	dir := filepath.Join(workdir, "bots", "nexie")
	writeCatalogTestBundle(t, dir, "nexie")
	tmpl := filepath.Join(dir, "iterion-bot-catalog-static.md")
	// Replace a readable template the same fixture may have written: the
	// loop must be what the regeneration reads.
	if err := os.Remove(tmpl); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if err := os.Symlink(tmpl, tmpl); err != nil {
		t.Fatalf("symlink unavailable: %v", err)
	}
	botregistry.ClearSchemaCache()
	return dir
}

// healthyCatalogOwner vendors an owner whose template reads cleanly, so the
// regeneration succeeds.
func healthyCatalogOwner(t *testing.T, workdir string) {
	t.Helper()
	dir := filepath.Join(workdir, "bots", "nexie")
	writeCatalogTestBundle(t, dir, "nexie")
	if err := os.WriteFile(filepath.Join(dir, "iterion-bot-catalog-static.md"),
		[]byte("---\nname: iterion-bot-catalog\n---\nPREAMBLE\n\n<!-- ITERION:CATALOG:GENERATED:BEGIN -->\n<!-- ITERION:CATALOG:GENERATED:END -->\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	botregistry.ClearSchemaCache()
}

// runBotsCommand drives one real command of the cobra tree with the string
// flags an operator would type, and returns what it printed. Cobra parses
// into package-level state, so every flag set here is restored.
func runBotsCommand(t *testing.T, cmd *cobra.Command, flags map[string]string, args ...string) string {
	t.Helper()
	cmd.SetContext(context.Background())
	for name, value := range flags {
		if err := cmd.Flags().Set(name, value); err != nil {
			t.Fatalf("set --%s: %v", name, err)
		}
		flag := name
		t.Cleanup(func() { _ = cmd.Flags().Set(flag, "") })
	}
	return captureStdout(t, func() error { return cmd.RunE(cmd, args) })
}

// lockOneDependency writes a bots.lock pinning one local source bundle at
// its real content hash, the shape `iterion bots sync` materializes.
func lockOneDependency(t *testing.T, workdir, name string) {
	t.Helper()
	source := filepath.Join(workdir, "source")
	writeCatalogTestBundle(t, source, name)
	hash, err := bundle.ContentHashDir(source)
	if err != nil {
		t.Fatal(err)
	}
	lock := fmt.Sprintf("version: 1\ndependencies:\n  %s:\n    source: ./source\n    ref: local-test\n    bundle_sha256: %s\n", name, hash)
	if err := os.WriteFile(filepath.Join(workdir, "bots.lock"), []byte(lock), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestBotsInstallCmd_WarnsWhenTheCatalogCouldNotBeRefreshed(t *testing.T) {
	workdir := t.TempDir()
	owner := breakCatalogOwner(t, workdir)
	source := t.TempDir()
	writeCatalogTestBundle(t, source, "newbot")

	out := runBotsCommand(t, botsInstallCmd, map[string]string{"workdir": workdir}, source)

	if !strings.Contains(out, "Bot installed") {
		t.Fatalf("the install itself must still be reported as done:\n%s", out)
	}
	// The exact prefix docs/cli-reference.md quotes.
	if !strings.Contains(out, "! catalog not refreshed:") {
		t.Errorf("the output does not carry the documented warning prefix:\n%s", out)
	}
	if !strings.Contains(out, owner) {
		t.Errorf("the output does not name the catalog it could not refresh (%s):\n%s", owner, out)
	}
	if !strings.Contains(out, "iterion bots regen-catalog") {
		t.Errorf("the output does not tell the operator how to refresh the catalog:\n%s", out)
	}
}

func TestBotsInstallCmd_SilentWhenTheCatalogRefreshSucceeds(t *testing.T) {
	workdir := filepath.Join(t.TempDir(), "catalog")
	healthyCatalogOwner(t, workdir)
	source := t.TempDir()
	writeCatalogTestBundle(t, source, "newbot")

	out := runBotsCommand(t, botsInstallCmd, map[string]string{"workdir": workdir}, source)

	out = strings.ReplaceAll(out, source, "<source>")
	out = strings.ReplaceAll(out, workdir, "<workdir>")
	if strings.Contains(out, "! catalog not refreshed:") {
		t.Errorf("a successful refresh must add nothing to the output:\n%s", out)
	}
}

func TestBotsInstallCmd_SilentWhenTheCatalogRefreshSucceedsWithNestedSource(t *testing.T) {
	workdir := filepath.Clean(t.TempDir())
	healthyCatalogOwner(t, workdir)
	source := filepath.Join(workdir, "nested", "! catalog not refreshed:")
	writeCatalogTestBundle(t, source, "newbot")

	out := runBotsCommand(t, botsInstallCmd, map[string]string{"workdir": workdir}, source)

	out = strings.ReplaceAll(out, source, "<source>")
	out = strings.ReplaceAll(out, workdir, "<workdir>")
	if strings.Contains(out, "! catalog not refreshed:") {
		t.Errorf("a successful refresh must add nothing to the output:\n%s", out)
	}
}

func TestBotsInstallCmd_JSONCarriesTheCatalogError(t *testing.T) {
	workdir := t.TempDir()
	owner := breakCatalogOwner(t, workdir)
	source := t.TempDir()
	writeCatalogTestBundle(t, source, "newbot")
	jsonOutput = true
	t.Cleanup(func() { jsonOutput = false })

	out := runBotsCommand(t, botsInstallCmd, map[string]string{"workdir": workdir}, source)

	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("decode: %v; out=%s", err, out)
	}
	msg, _ := got["catalog_error"].(string)
	if msg == "" {
		t.Fatalf("the JSON install result hides the catalog regeneration failure: %s", out)
	}
	if !strings.Contains(msg, owner) {
		t.Errorf("catalog_error %q does not name the catalog owner it could not refresh (%s)", msg, owner)
	}
}

// seedMarketplaceEntry indexes a local bundle in a fresh registry and
// returns the store directory both the submit and the command under test
// resolve from.
func seedMarketplaceEntry(t *testing.T, name string) string {
	t.Helper()
	t.Setenv("ITERION_HOME", t.TempDir())
	storeDir := t.TempDir()
	repo := t.TempDir()
	writeCatalogTestBundle(t, repo, name)
	if _, err := cli.MarketplaceSubmit(context.Background(), cli.MarketplaceSubmitOptions{StoreDir: storeDir, Source: repo}); err != nil {
		t.Fatalf("seed the registry: %v", err)
	}
	return storeDir
}

func TestMarketplaceInstallCmd_WarnsWhenTheCatalogCouldNotBeRefreshed(t *testing.T) {
	workdir := t.TempDir()
	owner := breakCatalogOwner(t, workdir)
	storeDir := seedMarketplaceEntry(t, "mybot")

	out := runBotsCommand(t, marketplaceInstallCmd, map[string]string{"store-dir": storeDir, "workdir": workdir}, "mybot")

	if !strings.Contains(out, "Bot installed") {
		t.Fatalf("the install itself must still be reported as done:\n%s", out)
	}
	if !strings.Contains(out, owner) {
		t.Errorf("the output does not name the catalog it could not refresh (%s):\n%s", owner, out)
	}
	if !strings.Contains(out, "iterion bots regen-catalog") {
		t.Errorf("the output does not tell the operator how to refresh the catalog:\n%s", out)
	}
}

func TestMarketplaceUninstallCmd_WarnsWhenTheCatalogCouldNotBeRefreshed(t *testing.T) {
	workdir := t.TempDir()
	healthyCatalogOwner(t, workdir)
	storeDir := seedMarketplaceEntry(t, "mybot")
	runBotsCommand(t, marketplaceInstallCmd, map[string]string{"store-dir": storeDir, "workdir": workdir}, "mybot")
	// Break the catalog only now: the install stays a clean success, so the
	// uninstall is the one gesture under test.
	owner := breakCatalogOwner(t, workdir)

	out := runBotsCommand(t, marketplaceUninstallCmd, map[string]string{"store-dir": storeDir, "workdir": workdir}, "mybot")

	if !strings.Contains(out, "uninstalled bot") {
		t.Fatalf("the removal itself must still be reported as done:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(workdir, ".botz", "mybot")); !os.IsNotExist(err) {
		t.Fatalf("the removal itself must still happen: %v", err)
	}
	if !strings.Contains(out, owner) {
		t.Errorf("the output does not name the catalog it could not refresh (%s):\n%s", owner, out)
	}
	if !strings.Contains(out, "iterion bots regen-catalog") {
		t.Errorf("the output does not tell the operator how to refresh the catalog:\n%s", out)
	}
}

// TestMarketplaceCmds_JSONCarryTheCatalogError: docs/cli-reference.md says
// `--json` carries the cause as `catalog_error` for the marketplace commands
// too. The install result marshals the field; the uninstall payload is built
// by hand, so only a test keeps that key honest.
func TestMarketplaceCmds_JSONCarryTheCatalogError(t *testing.T) {
	workdir := t.TempDir()
	owner := breakCatalogOwner(t, workdir)
	storeDir := seedMarketplaceEntry(t, "mybot")
	jsonOutput = true
	t.Cleanup(func() { jsonOutput = false })

	installed := runBotsCommand(t, marketplaceInstallCmd, map[string]string{"store-dir": storeDir, "workdir": workdir}, "mybot")

	var install struct {
		Install map[string]any `json:"install"`
	}
	if err := json.Unmarshal([]byte(installed), &install); err != nil {
		t.Fatalf("decode install: %v; out=%s", err, installed)
	}
	msg, _ := install.Install["catalog_error"].(string)
	if msg == "" {
		t.Fatalf("the JSON install payload hides the catalog regeneration failure: %s", installed)
	}
	if !strings.Contains(msg, owner) {
		t.Errorf("install catalog_error %q does not name the catalog owner (%s)", msg, owner)
	}

	removed := runBotsCommand(t, marketplaceUninstallCmd, map[string]string{"store-dir": storeDir, "workdir": workdir}, "mybot")

	var uninstall map[string]any
	if err := json.Unmarshal([]byte(removed), &uninstall); err != nil {
		t.Fatalf("decode uninstall: %v; out=%s", err, removed)
	}
	if uninstall["entry"] == nil {
		t.Errorf("the uninstall payload no longer carries the entry it removed: %s", removed)
	}
	msg, _ = uninstall["catalog_error"].(string)
	if msg == "" {
		t.Fatalf("the JSON uninstall payload hides the catalog regeneration failure: %s", removed)
	}
	if !strings.Contains(msg, owner) {
		t.Errorf("uninstall catalog_error %q does not name the catalog owner (%s)", msg, owner)
	}
}

func TestBotsSyncCmd_WarnsWhenTheCatalogCouldNotBeRefreshed(t *testing.T) {
	workdir := t.TempDir()
	owner := breakCatalogOwner(t, workdir)
	lockOneDependency(t, workdir, "shared-planner")

	out := runBotsCommand(t, botsSyncCmd, map[string]string{"workdir": workdir})

	if !strings.Contains(out, "installed  shared-planner") {
		t.Fatalf("the materialization itself must still be reported as done:\n%s", out)
	}
	if !strings.Contains(out, owner) {
		t.Errorf("the output does not name the catalog it could not refresh (%s):\n%s", owner, out)
	}
	if !strings.Contains(out, "iterion bots regen-catalog") {
		t.Errorf("the output does not tell the operator how to refresh the catalog:\n%s", out)
	}
}

// runBotsCommandExpectingError drives a command that must fail, and returns
// what it printed before failing — the rows and warnings an operator still
// owes a failed gesture.
func runBotsCommandExpectingError(t *testing.T, cmd *cobra.Command, flags map[string]string, args ...string) (string, error) {
	t.Helper()
	cmd.SetContext(context.Background())
	for name, value := range flags {
		if err := cmd.Flags().Set(name, value); err != nil {
			t.Fatalf("set --%s: %v", name, err)
		}
		flag := name
		t.Cleanup(func() { _ = cmd.Flags().Set(flag, "") })
	}
	var runErr error
	out := captureStdout(t, func() error {
		runErr = cmd.RunE(cmd, args)
		return nil
	})
	if runErr == nil {
		t.Fatalf("the command was expected to fail; it printed:\n%s", out)
	}
	return out, runErr
}

// lockTwoDependencies pins one materializable dependency and one whose source
// cannot match its pin, the shape that aborts a sync AFTER a dependency (and
// its catalog refresh) already landed.
func lockTwoDependencies(t *testing.T, workdir string) {
	t.Helper()
	good := filepath.Join(workdir, "source-alpha")
	writeCatalogTestBundle(t, good, "alpha")
	hash, err := bundle.ContentHashDir(good)
	if err != nil {
		t.Fatal(err)
	}
	writeCatalogTestBundle(t, filepath.Join(workdir, "source-zbeta"), "zbeta")
	lock := fmt.Sprintf("version: 1\ndependencies:\n"+
		"  alpha:\n    source: ./source-alpha\n    ref: local-test\n    bundle_sha256: %s\n"+
		"  zbeta:\n    source: ./source-zbeta\n    ref: local-test\n    bundle_sha256: %s\n",
		hash, strings.Repeat("0", 64))
	if err := os.WriteFile(filepath.Join(workdir, "bots.lock"), []byte(lock), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestBotsSyncCmd_AnAbortedSyncStillReportsTheStaleCatalog: the sync aborts
// on the second dependency, but the first one IS materialized and its
// refresh failed. Dropping that cause with the error leaves a stale routing
// table nothing will mention again — the next sync finds the dependency at
// its pin and installs nothing.
func TestBotsSyncCmd_AnAbortedSyncStillReportsTheStaleCatalog(t *testing.T) {
	workdir := t.TempDir()
	owner := breakCatalogOwner(t, workdir)
	lockTwoDependencies(t, workdir)

	out, err := runBotsCommandExpectingError(t, botsSyncCmd, map[string]string{"workdir": workdir})

	if !strings.Contains(err.Error(), "zbeta") {
		t.Fatalf("the abort must still be the command's error: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(workdir, ".botz", "alpha", "main.bot")); statErr != nil {
		t.Fatalf("test premise: alpha must have materialized before the abort: %v", statErr)
	}
	if !strings.Contains(out, "installed  alpha") {
		t.Errorf("the output hides the dependency that did materialize:\n%s", out)
	}
	if !strings.Contains(out, owner) {
		t.Errorf("the output does not name the catalog the aborted sync left stale (%s):\n%s", owner, out)
	}
	if !strings.Contains(out, "iterion bots regen-catalog") {
		t.Errorf("the output does not tell the operator how to refresh the catalog:\n%s", out)
	}
	// zbeta is pinned and NOT in the workspace: no sentence here may claim
	// the workspace is up to date — the rows above are the only state this
	// warning vouches for.
	if strings.Contains(out, "up to date") {
		t.Errorf("an aborted sync leaves a pinned dependency missing; the warning must not claim the workspace is up to date:\n%s", out)
	}
}

// TestBotsSyncCmd_JSONCarriesTheStaleCatalogOfAnAbortedSync: in --json the
// payload is the machine consumer's only reader, so an abort emits ONE
// document carrying both the dependencies that materialized and the abort —
// a second document after the first breaks every parser reading the stream —
// and the error leaves marked reported, so the CLI exits non-zero without
// printing anything after it.
func TestBotsSyncCmd_JSONCarriesTheStaleCatalogOfAnAbortedSync(t *testing.T) {
	workdir := t.TempDir()
	owner := breakCatalogOwner(t, workdir)
	lockTwoDependencies(t, workdir)
	jsonOutput = true
	t.Cleanup(func() { jsonOutput = false })

	out, err := runBotsCommandExpectingError(t, botsSyncCmd, map[string]string{"workdir": workdir})

	if !errors.Is(err, cli.ErrReported) {
		t.Fatalf("an abort whose document is already on stdout must leave marked reported, got %v", err)
	}
	if !strings.Contains(err.Error(), "zbeta") {
		t.Fatalf("the abort must still be the command's error: %v", err)
	}
	// Exactly ONE document on the stream: a second one after the first is
	// what made the payload unparseable.
	dec := json.NewDecoder(strings.NewReader(out))
	var got struct {
		Results []map[string]any `json:"results"`
		Error   string           `json:"error"`
	}
	if decodeErr := dec.Decode(&got); decodeErr != nil {
		t.Fatalf("decode: %v; out=%s", decodeErr, out)
	}
	if dec.More() {
		t.Fatalf("the abort wrote more than one JSON document; a parser reading the stream dies on the second:\n%s", out)
	}
	if len(got.Results) != 1 {
		t.Fatalf("the payload drops what the aborted sync materialized: %s", out)
	}
	if name, _ := got.Results[0]["name"].(string); name != "alpha" {
		t.Errorf("the payload names %q, want the materialized dependency: %s", name, out)
	}
	msg, _ := got.Results[0]["catalog_error"].(string)
	if msg == "" {
		t.Fatalf("the payload hides the catalog the aborted sync left stale: %s", out)
	}
	if !strings.Contains(msg, owner) {
		t.Errorf("catalog_error %q does not name the catalog owner it could not refresh (%s)", msg, owner)
	}
	if !strings.Contains(got.Error, "zbeta") {
		t.Errorf("the document hides why the sync aborted: %s", out)
	}
}

// TestBotsSyncCmd_WarnsOncePerCause: a workspace holds one catalog, so three
// dependencies failing to refresh it is one fact, not three.
func TestBotsSyncCmd_WarnsOncePerCause(t *testing.T) {
	workdir := t.TempDir()
	breakCatalogOwner(t, workdir)
	var lock strings.Builder
	lock.WriteString("version: 1\ndependencies:\n")
	for _, name := range []string{"alpha", "beta", "gamma"} {
		source := filepath.Join(workdir, "source-"+name)
		writeCatalogTestBundle(t, source, name)
		hash, err := bundle.ContentHashDir(source)
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&lock, "  %s:\n    source: ./source-%s\n    ref: local-test\n    bundle_sha256: %s\n", name, name, hash)
	}
	if err := os.WriteFile(filepath.Join(workdir, "bots.lock"), []byte(lock.String()), 0o644); err != nil {
		t.Fatal(err)
	}

	out := runBotsCommand(t, botsSyncCmd, map[string]string{"workdir": workdir})

	for _, name := range []string{"alpha", "beta", "gamma"} {
		if !strings.Contains(out, "installed  "+name) {
			t.Errorf("the output drops the materialized dependency %q:\n%s", name, out)
		}
	}
	if n := strings.Count(out, "! catalog not refreshed:"); n != 1 {
		t.Errorf("the one stale catalog is reported %d times:\n%s", n, out)
	}
}

// assertCauseOnOneLine pins the shape of the warning block: the cause is text
// a bundle's own manifest can reach (a yaml error echoes its keys, and
// errors.Join separates owners with newlines), so it must render on the ONE
// line the command wrote, whole — never as extra lines of iterion's output.
func assertCauseOnOneLine(t *testing.T, out, fragment string) {
	t.Helper()
	const prefix = "  ! catalog not refreshed: "
	var warning string
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, prefix) {
			if warning != "" {
				t.Fatalf("more than one warning line:\n%s", out)
			}
			warning = line
		}
	}
	if warning == "" {
		t.Fatalf("no warning line at all:\n%s", out)
	}
	if !strings.Contains(warning, fragment) {
		t.Errorf("the warning line drops %q from the cause:\n%s", fragment, warning)
	}
	for _, line := range strings.Split(out, "\n") {
		if line == warning {
			continue
		}
		if strings.Contains(line, fragment) {
			t.Errorf("the cause wrote a second line %q — a relayed cause renders on one line", line)
		}
	}
}

// TestBotsSyncCmd_AForeignShapedManifestIsNeverRead: since ADR-140 the
// claim reads the manifest's SHAPE, and a manifest of another tool's shape
// is never opened — so a hostile key crafted to echo as an
// `installed <name> <sha>` row (the forge the relayed-cause rendering once
// had to survive; that discipline keeps its regeneration-cause holders in
// TestBotsInstallCmd_SeveralBrokenOwnersStayOnOneLine) cannot reach any
// output at all. The sync installs the pinned dependency and prints
// nothing of the foreign file.
func TestBotsSyncCmd_AForeignShapedManifestIsNeverRead(t *testing.T) {
	workdir := t.TempDir()
	healthyCatalogOwner(t, workdir)
	forged := "installed  evil-bot  " + strings.Repeat("0", 64)
	broken := filepath.Join(workdir, "bots", "broken")
	writeCatalogTestBundle(t, broken, "broken")
	if err := os.WriteFile(filepath.Join(broken, "manifest.yaml"),
		[]byte("name: broken\nversion: 0.1.0\n\"x\\n"+forged+"\\n\": 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	botregistry.ClearSchemaCache()
	lockOneDependency(t, workdir, "shared-planner")

	out := runBotsCommand(t, botsSyncCmd, map[string]string{"workdir": workdir})

	if !strings.Contains(out, "installed  shared-planner") {
		t.Fatalf("the materialization itself must still be reported as done:\n%s", out)
	}
	if strings.Contains(out, forged) || strings.Contains(out, "evil-bot") {
		t.Fatalf("a foreign-shaped manifest's keys reached the output — the forge the claim was built to kill:\n%s", out)
	}
}

// TestBotsInstallCmd_SeveralBrokenOwnersStayOnOneLine: the checkout ships
// more than one catalog owner, and a cause that hits several is joined with
// newlines (errors.Join) — one failure must not break the block open.
func TestBotsInstallCmd_SeveralBrokenOwnersStayOnOneLine(t *testing.T) {
	workdir := t.TempDir()
	breakCatalogOwner(t, workdir)
	second := filepath.Join(workdir, "bots", "bot_name-with.dots")
	writeCatalogTestBundle(t, second, "triage")
	tmpl := filepath.Join(second, "iterion-bot-catalog-static.md")
	if err := os.Symlink(tmpl, tmpl); err != nil {
		t.Fatalf("symlink unavailable: %v", err)
	}
	botregistry.ClearSchemaCache()
	source := t.TempDir()
	writeCatalogTestBundle(t, source, "newbot")

	out := runBotsCommand(t, botsInstallCmd, map[string]string{"workdir": workdir}, source)

	if !strings.Contains(out, "Bot installed") {
		t.Fatalf("the install itself must still be reported as done:\n%s", out)
	}
	assertCauseOnOneLine(t, out, second)
}

// TestBotsInstallCmd_AForeignManifestNameCannotWriteALineOfOutput: the name a
// pulled bundle's manifest carries is relayed text — it lands in this
// command's Name and Path rows and in the `iterion run` footer. One carrying
// a newline could otherwise write a line of iterion's output at column 0, and
// the line this command teaches the operator to trust is
// `! catalog not refreshed:` — a forged one reads as a real staleness already
// dismissed, or a real one as already dismissed.
func TestBotsInstallCmd_AForeignManifestNameCannotWriteALineOfOutput(t *testing.T) {
	workdir := t.TempDir()
	// A HEALTHY owner: any `! catalog not refreshed:` in the output would be
	// the manifest's by construction.
	healthyCatalogOwner(t, workdir)
	source := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "manifest.yaml"),
		[]byte("name: \"evil\\n  ! catalog not refreshed: nothing, all good\"\nversion: 0.1.0\nschema_version: 1\ndescription: forged\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "main.bot"), []byte("workflow w:\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var runErr error
	out := captureStdout(t, func() error {
		runErr = runInstallCmd(t, workdir, source)
		return nil
	})
	if runErr == nil {
		t.Fatal("an install whose manifest name is not a bot name must be refused")
	}
	if !strings.Contains(runErr.Error(), "invalid install name") {
		t.Fatalf("the refusal must name the constraint: %v", runErr)
	}
	if strings.Contains(out, "! catalog not refreshed") || strings.Contains(out, "evil") {
		t.Errorf("the refused install must print nothing from the manifest:\n%s", out)
	}
}

// runInstallCmd drives botsInstallCmd and returns its RunE error — a refusal.
// The caller captures os.Stdout around it; the RunE error itself is returned
// because captureStdout fatal-exits on one.
func runInstallCmd(t *testing.T, workdir, source string) error {
	t.Helper()
	botsInstallCmd.SetContext(context.Background())
	if err := botsInstallCmd.Flags().Set("workdir", workdir); err != nil {
		t.Fatalf("set --workdir: %v", err)
	}
	t.Cleanup(func() { _ = botsInstallCmd.Flags().Set("workdir", "") })
	return botsInstallCmd.RunE(botsInstallCmd, []string{source})
}

// TestBotsUpdateCmd_WarnsWhenTheCatalogCouldNotBeRefreshed: `bots update`
// rewrites a pin and materializes it, so it refreshes the catalog like a
// sync — and its own print is the operator's only word on a refresh that
// failed.
func TestBotsUpdateCmd_WarnsWhenTheCatalogCouldNotBeRefreshed(t *testing.T) {
	workdir := t.TempDir()
	healthyCatalogOwner(t, workdir)
	lockOneDependency(t, workdir, "shared-planner")
	runBotsCommand(t, botsSyncCmd, map[string]string{"workdir": workdir})
	// Break the catalog only now, and give the update real content to
	// materialize: the sync above stays a clean success, so the update is
	// the one gesture under test.
	owner := breakCatalogOwner(t, workdir)
	if err := os.WriteFile(filepath.Join(workdir, "source", "main.bot"), []byte("workflow refreshed:\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	out := runBotsCommand(t, botsUpdateCmd, map[string]string{"workdir": workdir}, "shared-planner")

	if !strings.Contains(out, "Bot dependency updated") {
		t.Fatalf("the update itself must still be reported as done:\n%s", out)
	}
	if !strings.Contains(out, owner) {
		t.Errorf("the output does not name the catalog it could not refresh (%s):\n%s", owner, out)
	}
	if !strings.Contains(out, "iterion bots regen-catalog") {
		t.Errorf("the output does not tell the operator how to refresh the catalog:\n%s", out)
	}
}
