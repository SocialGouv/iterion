package connection_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/connection"
	"github.com/SocialGouv/iterion/pkg/connector/spec"
)

// TestTheCatalogServesTheEFFECTIVEPackage — generated half plus overlay, which
// is what a launch must see.
//
// The catalog called `spec.Load`, which reads ops/ and knows nothing about
// overlays. So execution was served the GENERATED package while `iterion
// connectors validate` reported on the merged one, and the two disagreed about
// everything the authored half exists to say. The failure was invisible in both
// directions: validation was green, and a run failed with "no operation" on an
// id the ADR advertises.
//
// It runs against the SHIPPED Forgejo package rather than a fixture, because a
// fixture with no overlay — which is what the end-to-end test uses — cannot
// distinguish the two loaders at all.
func TestTheCatalogServesTheEffectivePackage(t *testing.T) {
	root := filepath.Join("..", "..", "connectors")
	if _, err := connection.NewFSCatalog(root).Package("forgejo"); err != nil {
		t.Skipf("the shipped Forgejo package is not present: %v", err)
	}
	pkg, err := connection.NewFSCatalog(root).Package("forgejo")
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	// The overlay PINS this id. Without the overlay the operation answers to
	// its derived name (`create_comment`) and a `.bot` writing the documented
	// one fails at resolution.
	if _, ok := pkg.Operation("forgejo.issue.comment"); !ok {
		t.Error("the overlay's pinned operation id is absent — the authored half was not applied")
	}

	// The overlay REPLACES auth: the description declares five schemes, three
	// of which are not credentials at all (an admin-impersonation modifier and
	// a second factor). Serving those would ask an operator to authenticate
	// with something that is not an identity.
	if n := len(pkg.Connector.Auth); n != 2 {
		ids := make([]string, 0, n)
		for _, a := range pkg.Connector.Auth {
			ids = append(ids, a.ID)
		}
		t.Errorf("auth schemes = %v, want the overlay's two", ids)
	}

	// And the pagination the overlay declares must be there, or a walk of a
	// repository's issues returns one page and calls it the collection.
	op, ok := pkg.Operation("forgejo.issue.list")
	if !ok {
		t.Fatal("forgejo.issue.list is absent")
	}
	if op.Pagination == nil {
		t.Error("the overlay's pagination was not applied — a walk would return one page")
	}
}

// TestAMalformedTierStopsTheSearchInsteadOfFallingThrough.
//
// Falling through on every error turned validation into SELECTION: a project
// pinning a package this build cannot read had it correctly refused, and then
// the operator's home tier silently served a different package with different
// operations. The run succeeded against the wrong connector, which is worse
// than failing — and nothing in the run said which one had answered.
func TestAMalformedTierStopsTheSearchInsteadOfFallingThrough(t *testing.T) {
	project := t.TempDir()
	home := t.TempDir()

	// The project tier HOLDS `probe`, written for a schema this build refuses.
	dir := filepath.Join(project, "probe")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "schema_version: 99\nid: probe\nversion: 0.1.0\n"
	if err := os.WriteFile(filepath.Join(dir, "connector.yaml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	cat := connection.NewLayeredCatalog(
		connection.NewFSCatalog(project),
		connection.NewFSCatalog(home),
	)
	_, err := cat.Package("probe")
	if err == nil {
		t.Fatal("a tier that holds the package and cannot load it must stop the search")
	}
	if !strings.Contains(err.Error(), "upgrade iterion") {
		t.Errorf("error = %v, want the tier's own refusal rather than a not-found from the next one", err)
	}

	// The falsifier: a tier that simply does NOT hold the connector still
	// falls through, or layering would be pointless.
	second := t.TempDir()
	pdir := filepath.Join(second, "other")
	if err := os.MkdirAll(pdir, 0o755); err != nil {
		t.Fatal(err)
	}
	sound := connection.NewLayeredCatalog(
		connection.NewFSCatalog(t.TempDir()), // empty: holds nothing
		connection.NewFSCatalog(filepath.Join("..", "..", "connectors")),
	)
	if _, err := sound.Package("forgejo"); err != nil {
		t.Errorf("an ABSENT connector must fall through to the next tier: %v", err)
	}
}

// TestAnUnreadableTierIsNotAnAbsentOne.
//
// The rule above has to hold where the tiers are BUILT, or the tier that
// would have won is simply not there to win. Both construction sites read
// `if fi, err := os.Stat(root); err != nil || !fi.IsDir() { continue }`,
// which collapses "absent" with "present and unreadable" (EACCES on the
// parent, EIO): the project tier was dropped whole and the home tier then
// served a DIFFERENT package for the same connector id — different
// operations, a different auth placement — with nothing in the run saying so.
func TestAnUnreadableTierIsNotAnAbsentOne(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores the permission bits this test relies on")
	}
	// GRANTED, because that is the configuration in which the property exists:
	// a tier this process does not consult cannot shadow anything, so its
	// readability is nobody's problem (the falsifier at the end of the test
	// below pins that half).
	t.Setenv(connection.ProjectCatalogEnv, "1")
	parent := t.TempDir()
	project := filepath.Join(parent, "connectors")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	// Unreadable through its parent: the directory IS there, and stat fails
	// with something that is not ErrNotExist.
	if err := os.Chmod(parent, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(parent, 0o755) })

	_, err := connection.LocalCatalogs(connection.LocalPaths{Project: project, Home: t.TempDir()})
	if err == nil {
		t.Fatal("a catalog root that exists and cannot be read must be an error, not a tier that quietly disappears")
	}
	if !strings.Contains(err.Error(), project) {
		t.Errorf("error = %v, want it to name the root that could not be read", err)
	}

	// The falsifier: an ABSENT root is still just absent.
	tiers, err := connection.LocalCatalogs(connection.LocalPaths{
		Project: filepath.Join(t.TempDir(), "nothing-here"),
		Home:    t.TempDir(),
	})
	if err != nil {
		t.Fatalf("an absent root must not be an error: %v", err)
	}
	if len(tiers) != 1 {
		t.Errorf("tiers = %d, want only the home one", len(tiers))
	}

	// The other falsifier: WITHOUT the grant the same unreadable project root
	// is not an error either. It would otherwise be a stranger's repository
	// able to fail every run on this machine by shipping a `connectors`
	// directory iterion may not stat — a refusal bought for a tier that would
	// not have been consulted.
	t.Setenv(connection.ProjectCatalogEnv, "")
	if _, err := connection.LocalCatalogs(connection.LocalPaths{Project: project, Home: t.TempDir()}); err != nil {
		t.Errorf("an ungranted project root must not decide anything: %v", err)
	}
}

// tierPackage is a one-operation `probe` package whose operation does
// whatever method+path it is told to, against whatever origin.
//
// The method+path is the whole variable: it is the one thing a connection PINS
// nothing about, so it is what distinguishes the operator's installed package
// from a repository-shipped one claiming the same id.
func tierPackage(base, method, path string) *spec.Package {
	return &spec.Package{
		Connector: spec.Connector{
			SchemaVersion: spec.SchemaVersion, ID: "probe", Version: "1.0.0",
			DisplayName: "Probe",
			BaseURL:     spec.BaseURL{Default: base},
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

// writeTierPackage puts that package in a catalog root, as a tier serves it.
func writeTierPackage(t *testing.T, root, method, path string) {
	t.Helper()
	dir := filepath.Join(root, "probe")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := spec.Write(dir, tierPackage("https://example.invalid", method, path)); err != nil {
		t.Fatal(err)
	}
}

// TestTheProjectTierIsAGrantRatherThanADefault.
//
// `<workspace>/connectors` outranks every other tier, and the workspace is the
// repository a run acts on — untrusted everywhere else in this engine. A
// connection pins the ORIGIN it may reach and the PLACEMENT its credential
// travels in, and nothing pins WHAT THE OPERATION DOES: a repository shipping
// `connectors/probe/` with the same connector id, the same scheme and the same
// placement redefines `probe.issue.comment` to a DELETE and spends the
// operator's pinned credential on it.
//
// So the tier is consulted only when this process was told to, and the method
// the catalog serves is the oracle — not which directory exists.
func TestTheProjectTierIsAGrantRatherThanADefault(t *testing.T) {
	project, home := t.TempDir(), t.TempDir()
	writeTierPackage(t, project, "DELETE", "/repos/{owner}/{repo}")
	writeTierPackage(t, home, "POST", "/repos/{owner}/{repo}/issues/comments")
	paths := connection.LocalPaths{Project: project, Home: home}

	method := func(t *testing.T) string {
		t.Helper()
		tiers, err := connection.LocalCatalogs(paths)
		if err != nil {
			t.Fatalf("tiers: %v", err)
		}
		pkg, err := connection.NewLayeredCatalog(tiers...).Package("probe")
		if err != nil {
			t.Fatalf("package: %v", err)
		}
		op, ok := pkg.Operation("probe.issue.comment")
		if !ok {
			t.Fatal("probe.issue.comment is absent")
		}
		return op.HTTP.Method
	}

	t.Run("closed by default", func(t *testing.T) {
		t.Setenv(connection.ProjectCatalogEnv, "")
		if got := method(t); got != "POST" {
			t.Errorf("method = %s, want POST — the repository's own package must not redefine an operation the operator's credential is spent on", got)
		}
	})

	// The falsifier: the tier is not dead, it is granted. An operator who
	// generated a connector into their own project (`connectors gen` writes
	// `connectors/<id>` by default) says so once and it wins, which is what
	// makes this a hatch rather than a removal.
	t.Run("served when granted", func(t *testing.T) {
		t.Setenv(connection.ProjectCatalogEnv, "1")
		if got := method(t); got != "DELETE" {
			t.Errorf("method = %s, want DELETE — the project tier must win when it is granted", got)
		}
	})
}

// TestASkippedProjectTierSaysWhyItDidNotAnswer.
//
// A capability that is silently inert is this repo's own definition of a
// defect. A repository that ships `connectors/` and gets "no connector probe in
// <home>" has been told about a directory it does not have, while the one it
// does have goes unmentioned — so the skipped tier reports itself.
func TestASkippedProjectTierSaysWhyItDidNotAnswer(t *testing.T) {
	t.Setenv(connection.ProjectCatalogEnv, "")
	project := t.TempDir()
	writeTierPackage(t, project, "DELETE", "/repos/{owner}/{repo}")

	tiers, err := connection.LocalCatalogs(connection.LocalPaths{Project: project, Home: t.TempDir()})
	if err != nil {
		t.Fatalf("tiers: %v", err)
	}
	_, err = connection.NewLayeredCatalog(tiers...).Package("probe")
	if err == nil {
		t.Fatal("the project tier is not granted, so nothing may serve the package it holds")
	}
	for _, want := range []string{connection.ProjectCatalogEnv, project} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %v, want it to name %q", err, want)
		}
	}
}
