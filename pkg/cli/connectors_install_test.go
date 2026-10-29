package cli_test

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/gittest"
	"github.com/SocialGouv/iterion/internal/testfs"
	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/cli"
	"github.com/SocialGouv/iterion/pkg/connection"
	"github.com/SocialGouv/iterion/pkg/connector/spec"
	"github.com/SocialGouv/iterion/pkg/store"
)

// pinnedPackageSource is the package a fixture repository ships, under the id
// a `.bot` addresses. The HTTP binding is the variable: it is what an
// operation DOES, the one thing no connection pins, so it is the honest way to
// ask which commit's package is serving.
func pinnedPackageSource(method, path string) *spec.Package {
	return &spec.Package{
		Connector: spec.Connector{
			SchemaVersion: spec.SchemaVersion, ID: "probe", Version: "1.0.0",
			DisplayName: "Probe",
			BaseURL:     spec.BaseURL{Default: "https://probe.example"},
			Auth: []spec.AuthScheme{{
				ID: "token", Kind: spec.AuthAPIKey, In: "header",
				Name: "Authorization", ValuePrefix: "token ",
			}},
			Maturity: spec.MaturityQualified,
		},
		Ops: []spec.OpsFile{{
			SchemaVersion: spec.SchemaVersion, Connector: "probe", Domain: "issue",
			Operations: []spec.Operation{{
				ID: "probe.issue.comment", Resource: "issue", Verb: "comment",
				HTTP:   spec.HTTPBinding{Method: method, Path: path},
				Effect: spec.EffectCreate, Deterministic: true,
				Params: []spec.Param{
					{Key: "owner", Name: "owner", In: spec.InPath, Type: "string", Required: true},
					{Key: "repo", Name: "repo", In: spec.InPath, Type: "string", Required: true},
				},
				Results: []spec.ResultCase{{Status: 201}},
			}},
		}},
	}
}

// pinnedSourceRepo is a repository shipping a connector package under
// connectors/probe, as a team or a vendor would.
func pinnedSourceRepo(t *testing.T) string {
	t.Helper()
	if _, err := os.Stat("/dev/null"); err != nil {
		t.Skipf("no usable filesystem: %v", err)
	}
	repo := t.TempDir()
	gittest.InitRepo(t, repo)
	return repo
}

// pinnedSourceCommit rewrites the package in the repository and commits it,
// returning the commit an operator would read and pass to --ref.
func pinnedSourceCommit(t *testing.T, repo, method, path string) string {
	t.Helper()
	dir := filepath.Join(repo, "connectors", "probe")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := spec.Write(dir, pinnedPackageSource(method, path)); err != nil {
		t.Fatalf("write the fixture package: %v", err)
	}
	gittest.Run(t, repo, "add", "connectors")
	gittest.Run(t, repo, "commit", "-q", "-m", method+" "+path)
	return gittest.Run(t, repo, "rev-parse", "HEAD")
}

// pinnedFutureSchemaCommit ships a package this build cannot read: a
// schema_version from a newer iterion.
//
// Written as TEXT because spec.Write refuses to produce one — which is the
// honest fixture anyway: such a package reaches a catalog from a source that
// was built by a newer iterion, never from this process.
func pinnedFutureSchemaCommit(t *testing.T, repo string) string {
	t.Helper()
	dir := filepath.Join(repo, "connectors", "probe")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := spec.Write(dir, pinnedPackageSource("DELETE", "/repos/{owner}/{repo}")); err != nil {
		t.Fatalf("write the fixture package: %v", err)
	}
	path := filepath.Join(dir, spec.ConnectorFile)
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	declared := fmt.Sprintf("schema_version: %d", spec.SchemaVersion)
	patched := strings.Replace(string(body), declared, "schema_version: 99", 1)
	if patched == string(body) {
		t.Fatalf("the fixture declares no %q, so it was not made unreadable and the refusal below would prove nothing:\n%s", declared, body)
	}
	if err := os.WriteFile(path, []byte(patched), 0o644); err != nil {
		t.Fatal(err)
	}
	gittest.Run(t, repo, "add", "connectors")
	gittest.Run(t, repo, "commit", "-q", "-m", "a package from a newer iterion")
	return gittest.Run(t, repo, "rev-parse", "HEAD")
}

// pinnedHomeTier sets up an iterion home as the only tier that can answer,
// and returns its catalog root — the directory `connectors install` must write
// into for anything it does to matter.
func pinnedHomeTier(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("ITERION_HOME", home)
	t.Setenv(connection.ProjectCatalogEnv, "")
	// A workspace with no connectors/ of its own: the project tier must not be
	// what answers these assertions.
	t.Chdir(t.TempDir())
	return filepath.Join(home, "connectors")
}

// pinnedServedOperation resolves `probe` through the tier composition a run
// uses and returns the operation's binding and the tier that served it.
//
// A FRESH composition every call, deliberately: FSCatalog caches a package
// after the first load, and a cached answer cannot say whether the bytes on
// disk changed. The question here is always "what would a run started NOW
// resolve".
func pinnedServedOperation(t *testing.T) (method, path, tier string) {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	paths := connection.LocalCatalogPaths(wd, store.GlobalIterionDataDir())
	tiers, err := connection.LocalCatalogs(paths, connection.TrustOfRun(store.RunTrustDefault))
	if err != nil {
		t.Fatalf("compose the tiers: %v", err)
	}
	pkg, served, err := connection.NewLayeredCatalog(tiers...).Package("probe")
	if err != nil {
		t.Fatalf("resolve probe: %v", err)
	}
	for _, op := range pkg.Operations() {
		if op.ID == "probe.issue.comment" {
			return op.HTTP.Method, op.HTTP.Path, served.String()
		}
	}
	t.Fatalf("the served package has no probe.issue.comment: %v", pkg.Operations())
	return "", "", ""
}

func pinnedInstall(t *testing.T, repo, ref string, force bool) (string, error) {
	t.Helper()
	out := &bytes.Buffer{}
	err := cli.ConnectorsInstall(context.Background(), cli.ConnectorsInstallOptions{
		Source: repo, Ref: ref, Path: filepath.Join("connectors", "probe"), Force: force,
	}, out)
	return out.String(), err
}

// TestAGitSourcedPackageIsPinnedAndVerifiedBeforeItServes is the witness of
// #2757's git-source unit.
//
// The property: the COMMIT decides which operations a run resolves, and only
// an install changes that. A source that could decide it later would decide,
// at run time, where a connection's credential goes on the wire and what the
// operation it goes with does — from a host nobody re-reviewed.
//
// Asserted in both directions and with the branch deliberately AHEAD of the
// pin: a lane that silently followed the branch tip would satisfy a test whose
// pin is also the tip, which is every test written on a fresh fixture.
func TestAGitSourcedPackageIsPinnedAndVerifiedBeforeItServes(t *testing.T) {
	root := pinnedHomeTier(t)
	repo := pinnedSourceRepo(t)
	first := pinnedSourceCommit(t, repo, "POST", "/repos/{owner}/{repo}/issues/comments")

	printed, err := pinnedInstall(t, repo, first, false)
	if err != nil {
		t.Fatalf("install at %s: %v", first, err)
	}
	if !strings.Contains(printed, first) {
		t.Errorf("the install printed %q, want the pin %s in it", printed, first)
	}
	if method, path, tier := pinnedServedOperation(t); method != "POST" || path != "/repos/{owner}/{repo}/issues/comments" {
		t.Fatalf("resolved %s %s from the %s, want the pinned POST", method, path, tier)
	} else if !strings.Contains(tier, "home") {
		t.Errorf("the installed package served from the %s, want the operator's home tier — an install the resolver does not read is the one failure this command cannot report", tier)
	}

	// The install RECORD: what was installed, from where, at which commit,
	// and the hash of the bytes that passed validation.
	pin, err := cli.ReadConnectorPin(root, "probe")
	if err != nil {
		t.Fatalf("read the install record: %v", err)
	}
	if pin.Ref != first {
		t.Errorf("the record says ref %s, want %s", pin.Ref, first)
	}
	installedHash, err := bundle.ContentHashDir(filepath.Join(root, "probe"))
	if err != nil {
		t.Fatal(err)
	}
	if pin.ContentSHA256 != installedHash {
		t.Errorf("the record certifies %s, the installed package hashes to %s", pin.ContentSHA256, installedHash)
	}

	// The branch moves to a package whose operation is a DELETE under another
	// path — the shape of the threat: same connector id, same scheme, same
	// placement, different thing done.
	second := pinnedSourceCommit(t, repo, "DELETE", "/repos/{owner}/{repo}")
	if second == first {
		t.Fatalf("the fixture made one commit, not two")
	}
	if method, path, _ := pinnedServedOperation(t); method != "POST" || path != "/repos/{owner}/{repo}/issues/comments" {
		t.Errorf("the branch moved and a run would now resolve %s %s: nothing but an install may change what serves", method, path)
	}

	// Re-installing the SAME pin while the branch is ahead of it: the pin is
	// what is fetched, not the tip.
	if _, err := pinnedInstall(t, repo, first, true); err != nil {
		t.Fatalf("re-install at %s: %v", first, err)
	}
	if method, path, _ := pinnedServedOperation(t); method != "POST" || path != "/repos/{owner}/{repo}/issues/comments" {
		t.Errorf("re-installing pin %s served %s %s, want what that commit holds — the branch tip %s is not the pin", first, method, path, second)
	}

	// And an install IS what changes it.
	if _, err := pinnedInstall(t, repo, second, true); err != nil {
		t.Fatalf("install at %s: %v", second, err)
	}
	if method, path, _ := pinnedServedOperation(t); method != "DELETE" || path != "/repos/{owner}/{repo}" {
		t.Errorf("after installing pin %s a run resolves %s %s, want the DELETE that commit holds", second, method, path)
	}
	if pin, err := cli.ReadConnectorPin(root, "probe"); err != nil || pin.Ref != second {
		t.Errorf("the record after the second install = %v (err %v), want ref %s", pin, err, second)
	}

	// Once installed, the source may vanish: a run resolves bytes on disk and
	// never dials. The repository is the test's own scratch.
	testfs.RemoveAll(t, repo, filepath.Join(repo, ".git"))
	testfs.RemoveAll(t, repo, filepath.Join(repo, "connectors"))
	if method, path, _ := pinnedServedOperation(t); method != "DELETE" || path != "/repos/{owner}/{repo}" {
		t.Errorf("with the source gone a run resolves %s %s, want the installed DELETE", method, path)
	}
}

// TestAnUnpinnedConnectorSourceIsRefused keeps a moving ref out of the one
// lane that decides what a run resolves.
//
// A branch is not a pin by convention: the same command run twice installs
// different operations under the same connector id, and the only word on which
// ones a run got is the minute it ran. A tag is the same object with a
// publisher who can move it.
func TestAnUnpinnedConnectorSourceIsRefused(t *testing.T) {
	root := pinnedHomeTier(t)
	repo := pinnedSourceRepo(t)
	head := pinnedSourceCommit(t, repo, "POST", "/repos/{owner}/{repo}/issues/comments")
	gittest.Run(t, repo, "tag", "v1.0.0")

	for _, ref := range []string{"main", "v1.0.0", "HEAD", "", head[:12], strings.ToUpper(head)} {
		_, err := pinnedInstall(t, repo, ref, false)
		if err == nil {
			t.Errorf("install at %q = nil, want a refusal", ref)
			continue
		}
		if !strings.Contains(err.Error(), "not a commit pin") {
			t.Errorf("install at %q = %v, want it refused as not a commit pin", ref, err)
		}
		if entries, readErr := os.ReadDir(root); readErr == nil && len(entries) > 0 {
			t.Errorf("install at %q left %v in the catalog root, want nothing installed", ref, entries)
		}
	}

	// The pin itself installs, so the table refuses shapes rather than
	// refusing everything.
	if _, err := pinnedInstall(t, repo, head, false); err != nil {
		t.Fatalf("install at the pin %s: %v", head, err)
	}
	if method, _, _ := pinnedServedOperation(t); method != "POST" {
		t.Errorf("resolved %s, want the installed POST", method)
	}
}

// TestAPackageThatDoesNotLoadIsRefusedAndTheInstalledOneStaysServing is the
// install-time gate: a fetched package is validated with the loader a run uses
// BEFORE it can serve, and a refusal leaves the previous install alone.
//
// Replacing first and validating second would hand a run a package nobody
// checked — and, on a refusal, a tier emptied of a connector the operator
// never asked to remove.
func TestAPackageThatDoesNotLoadIsRefusedAndTheInstalledOneStaysServing(t *testing.T) {
	root := pinnedHomeTier(t)
	repo := pinnedSourceRepo(t)
	good := pinnedSourceCommit(t, repo, "POST", "/repos/{owner}/{repo}/issues/comments")
	if _, err := pinnedInstall(t, repo, good, false); err != nil {
		t.Fatalf("install at %s: %v", good, err)
	}

	// A package from a future iterion: the loader refuses it, which is
	// exactly what the layered catalog counts on to never serve a namesake
	// from another tier instead.
	broken := pinnedFutureSchemaCommit(t, repo)
	out, err := pinnedInstall(t, repo, broken, true)
	if err == nil {
		t.Fatalf("install of a package that does not load = nil, want a refusal (printed %q)", out)
	}
	if !strings.Contains(err.Error(), "does not load, so it is not installed") {
		t.Errorf("install of a broken package = %v, want it to say the package was not installed", err)
	}
	if !strings.Contains(err.Error(), broken) {
		t.Errorf("the refusal %v does not name the pin %s it refused", err, broken)
	}

	if method, path, _ := pinnedServedOperation(t); method != "POST" || path != "/repos/{owner}/{repo}/issues/comments" {
		t.Errorf("after the refused install a run resolves %s %s, want the previously installed POST /repos/{owner}/{repo}/issues/comments", method, path)
	}
	if pin, pinErr := cli.ReadConnectorPin(root, "probe"); pinErr != nil || pin.Ref != good {
		t.Errorf("the record after the refusal = %v (err %v), want it still at %s", pin, pinErr, good)
	}
	// Nothing half-written: a staging directory left behind is a tier the
	// next install cannot reason about.
	entries, readErr := os.ReadDir(root)
	if readErr != nil {
		t.Fatal(readErr)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), "staging") {
			t.Errorf("the refused install left %s in the catalog root", e.Name())
		}
	}
}

// TestAFetchedPackageCarryingASymlinkIsNeverInstalled keeps an entry no
// package may carry out of the catalog.
//
// The content hash is the refusal: bundle.ContentHashDir rejects a symlink,
// and the lane takes it three times — on the fetched tree before a byte is
// copied, on the staged copy, and on what landed. Without it CopyFS would
// install the bytes of whatever the link points at, read on the machine that
// ran the install, from a tree the source controls; the mutation that ignores
// all three is what this test is red on.
func TestAFetchedPackageCarryingASymlinkIsNeverInstalled(t *testing.T) {
	root := pinnedHomeTier(t)
	repo := pinnedSourceRepo(t)
	pinnedSourceCommit(t, repo, "POST", "/repos/{owner}/{repo}/issues/comments")
	if err := os.Symlink("/etc/hostname", filepath.Join(repo, "connectors", "probe", "linked.yaml")); err != nil {
		t.Skipf("this filesystem refuses a symlink: %v", err)
	}
	gittest.Run(t, repo, "add", "connectors")
	gittest.Run(t, repo, "commit", "-q", "-m", "a package carrying a link")
	head := gittest.Run(t, repo, "rev-parse", "HEAD")

	_, err := pinnedInstall(t, repo, head, false)
	if err == nil {
		t.Fatalf("install of a package carrying a symlink = nil, want a refusal")
	}
	if !strings.Contains(err.Error(), "symlinks not allowed") {
		t.Fatalf("install of a package carrying a symlink = %v, want it refused for the link", err)
	}
	if _, statErr := os.Stat(filepath.Join(root, "probe")); !os.IsNotExist(statErr) {
		t.Errorf("stat the catalog entry after the refusal = %v, want nothing installed", statErr)
	}
}

// TestTheInstallRecordSitsBesideThePackageNotInsideIt pins where the
// provenance lives.
//
// Inside the package it would change the very content hash it certifies, and
// `FSCatalog.Connectors` — which lists what a catalog can serve — would read
// its directory as a connector id.
func TestTheInstallRecordSitsBesideThePackageNotInsideIt(t *testing.T) {
	root := pinnedHomeTier(t)
	repo := pinnedSourceRepo(t)
	head := pinnedSourceCommit(t, repo, "POST", "/repos/{owner}/{repo}/issues/comments")
	if _, err := pinnedInstall(t, repo, head, false); err != nil {
		t.Fatalf("install at %s: %v", head, err)
	}

	pkgDir := filepath.Join(root, "probe")
	// The committed layout, asserted as a path rather than through the
	// reader: a record the reader finds wherever it put it says nothing
	// about where it put it.
	if _, err := os.Stat(filepath.Join(root, ".pins", "probe.json")); err != nil {
		t.Errorf("stat the install record beside the packages = %v, want it at <root>/.pins/probe.json", err)
	}

	if got := connection.NewFSCatalog(root).Connectors(); len(got) != 1 || got[0] != "probe" {
		t.Errorf("the catalog lists %v, want exactly [probe] — the record's directory is not a connector", got)
	}

	// The record certifies the bytes a run loads: the hash is the installed
	// package's, with the record outside it.
	pin, err := cli.ReadConnectorPin(root, "probe")
	if err != nil {
		t.Fatalf("read the install record: %v", err)
	}
	hash, err := bundle.ContentHashDir(pkgDir)
	if err != nil {
		t.Fatal(err)
	}
	if pin.ContentSHA256 != hash {
		t.Errorf("the record certifies %s, the installed package hashes to %s", pin.ContentSHA256, hash)
	}
	if pin.Version != "1.0.0" || pin.Connector != "probe" {
		t.Errorf("the record names %s v%s, want probe v1.0.0", pin.Connector, pin.Version)
	}
}

// TestAPathThatResolvesOutsideTheFetchedRepositoryIsRefused keeps `--path`
// and the fetched tree from naming bytes the pin does not cover.
//
// A repository may ship `connectors/x` as a symlink: os.Stat follows it, the
// content hash does not (WalkDir refuses a symlinked root), and CopyFS does —
// so the install would have staged a directory of the machine's own while the
// hash certified an empty tree.
func TestAPathThatResolvesOutsideTheFetchedRepositoryIsRefused(t *testing.T) {
	root := pinnedHomeTier(t)
	repo := pinnedSourceRepo(t)
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.yaml"), []byte("not yours\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(repo, "connectors"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(repo, "connectors", "probe")); err != nil {
		t.Skipf("this filesystem refuses a symlink: %v", err)
	}
	gittest.Run(t, repo, "add", "connectors")
	gittest.Run(t, repo, "commit", "-q", "-m", "a package directory that is a link")
	head := gittest.Run(t, repo, "rev-parse", "HEAD")

	_, err := pinnedInstall(t, repo, head, false)
	if err == nil {
		t.Fatalf("install of a --path that leaves the repository = nil, want a refusal")
	}
	if !strings.Contains(err.Error(), "outside the fetched repository") {
		t.Fatalf("install of a --path that leaves the repository = %v, want it refused for leaving it", err)
	}
	if entries, readErr := os.ReadDir(root); readErr == nil && len(entries) > 0 {
		t.Errorf("the refused install left %v in the catalog root, want nothing", entries)
	}
}

// TestTheInstallRecordCarriesNoCredentialMaterial repeats a refusal this
// repository already paid for: ADR-098 records a fetch URL written verbatim
// into a package's provenance, and the only credential a git URL carries is
// in its userinfo or its query.
//
// The record lives in the operator's home, readable by whoever can read the
// catalog, so it keeps what identifies the source — host and path — and
// nothing else.
func TestTheInstallRecordCarriesNoCredentialMaterial(t *testing.T) {
	root := pinnedHomeTier(t)
	// A source whose path carries a token the way a fetch URL does. A real
	// one is `https://host/org/repo?private_token=…`; this is the shape a
	// test can fetch from.
	repo := filepath.Join(t.TempDir(), "packages?private_token=s3cr3t-do-not-record")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	gittest.InitRepo(t, repo)
	head := pinnedSourceCommit(t, repo, "POST", "/repos/{owner}/{repo}/issues/comments")

	printed, err := pinnedInstall(t, repo, head, false)
	if err != nil {
		t.Fatalf("install from a tokened source: %v", err)
	}
	pin, err := cli.ReadConnectorPin(root, "probe")
	if err != nil {
		t.Fatalf("read the install record: %v", err)
	}
	if strings.Contains(pin.Source, "s3cr3t") {
		t.Errorf("the install record names the source as %q, want the credential material gone", pin.Source)
	}
	if strings.Contains(printed, "s3cr3t") {
		t.Errorf("the command printed %q, want the credential material gone", printed)
	}
	// Still recognisable as the source it was: a record naming nothing sends
	// the reader nowhere.
	if !strings.Contains(pin.Source, "packages") {
		t.Errorf("the install record names the source as %q, want the path still in it", pin.Source)
	}
}
