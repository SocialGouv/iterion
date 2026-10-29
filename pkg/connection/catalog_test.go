package connection_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/connection"
	"github.com/SocialGouv/iterion/pkg/connector/spec"
	"github.com/SocialGouv/iterion/pkg/store"
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

	_, err := connection.LocalCatalogs(connection.LocalPaths{Project: project, Home: t.TempDir()}, trustedRun())
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
	}, trustedRun())
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
	if _, err := connection.LocalCatalogs(connection.LocalPaths{Project: project, Home: t.TempDir()}, trustedRun()); err != nil {
		t.Errorf("an ungranted project root must not decide anything: %v", err)
	}

	// And the THIRD, which is the same rule reached by the other condition
	// (#2769): a tier the GRANT opens but this run's trust closes is not
	// consulted either, so its readability is nobody's problem here too. A
	// fork-lane run would otherwise be failed outright by an unreadable
	// directory in the very checkout it is reviewing.
	t.Setenv(connection.ProjectCatalogEnv, "1")
	if _, err := connection.LocalCatalogs(connection.LocalPaths{Project: project, Home: t.TempDir()},
		connection.TrustOfRun(store.RunTrustFork)); err != nil {
		t.Errorf("a granted root this run's trust closes must not decide anything either: %v", err)
	}
}

// tierPackage is a one-operation `probe` package whose operation does
// whatever method+path it is told to, against whatever origin.
//
// The method+path is the whole variable: it is the one thing a connection PINS
// nothing about, so it is what distinguishes the operator's installed package
// from a repository-shipped one claiming the same id.
func tierPackage(base, method, path string) *spec.Package {
	return tierPackageAs("probe", base, method, path, spec.EffectCreate)
}

// tierPackageAs is tierPackage with the connector id and the declared EFFECT
// as variables too.
//
// Both are what the untrusted-tier case turns on (#2769): the effect is the
// lie the attack rests on — #1802's refusal reads `effect:` as the catalog
// served it, so a DELETE declaring `read` buys the whole mutation — and a
// second id present in ONE tier only is the only way a LISTING can be
// asserted at all (an id both tiers hold is listed either way).
func tierPackageAs(id, base, method, path string, effect spec.Effect) *spec.Package {
	return &spec.Package{
		Connector: spec.Connector{
			SchemaVersion: spec.SchemaVersion, ID: id, Version: "1.0.0",
			DisplayName: "Probe",
			BaseURL:     spec.BaseURL{Default: base},
			Auth: []spec.AuthScheme{{
				ID: "token", Kind: spec.AuthAPIKey, In: "header",
				Name: "Authorization", ValuePrefix: "token ",
			}},
			Maturity: spec.MaturityQualified,
		},
		Ops: []spec.OpsFile{{
			SchemaVersion: spec.SchemaVersion, Connector: id, Domain: "issue",
			Operations: []spec.Operation{{
				ID: id + ".issue.comment", Resource: "issue", Verb: "comment",
				HTTP:   spec.HTTPBinding{Method: method, Path: path},
				Effect: effect, Deterministic: true,
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
	writeTierPackageAs(t, root, "probe", method, path, spec.EffectCreate)
}

// writeTierPackageAs puts a package under its OWN id, which is also the
// directory name a `.bot` addresses (FSCatalog refuses the two disagreeing).
func writeTierPackageAs(t *testing.T, root, id, method, path string, effect spec.Effect) {
	t.Helper()
	dir := filepath.Join(root, id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := spec.Write(dir, tierPackageAs(id, "https://example.invalid", method, path, effect)); err != nil {
		t.Fatal(err)
	}
}

// trustedRun is the trust the tier tests that predate #2769 already meant: the
// workspace is the operator's own checkout, which is what every run that
// exists today is (store.RunTrust's zero value).
func trustedRun() connection.ActionTrust {
	return connection.TrustOfRun(store.RunTrustDefault)
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
		tiers, err := connection.LocalCatalogs(paths, trustedRun())
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

	tiers, err := connection.LocalCatalogs(connection.LocalPaths{Project: project, Home: t.TempDir()}, trustedRun())
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

// TestTheWorkspaceTierIsNotConsultedForAnUntrustedRun is #2769.
//
// The project tier outranks every other, and on a fork-lane run the workspace
// IS the outsider's checkout. #1802 refuses a MUTATING action on such a run,
// but its predicate is spec.Effect.Mutating() on the operation THE CATALOG
// RESOLVED — so a reviewed tree only had to ship `connectors/probe/` declaring
// `effect: read` on a DELETE and the guard read the attacker's own word for
// what the operation does. Nothing pins the tier onto the resolved package
// (spec.Package carries no provenance, NewLayeredCatalog hands back no tier),
// so the tier that must not win is not BUILT for that run — decided where the
// tiers are built, which is where the trust already is.
//
// The TRUSTED arm is what makes the untrusted ones mean something: the same
// workspace, the same grant, serving the DELETE. And the fix is "that tier is
// not consulted" rather than "no catalog" — an untrusted run still resolves
// the operator's OWN package, and a review lane still reads.
//
// Asserted through the RESOLUTION, which is what a run reaches: the operation
// handed to the executor is the oracle, and under the defect it was the
// outsider's DELETE carrying a read's licence.
func TestTheWorkspaceTierIsNotConsultedForAnUntrustedRun(t *testing.T) {
	// GRANTED throughout: a closed tier is not consulted for anybody, so it
	// could not prove anything about the trust. This is the deployment that
	// HAS the composition — the grant, plus a lane that serves forks.
	t.Setenv(connection.ProjectCatalogEnv, "1")
	project, home := t.TempDir(), t.TempDir()
	// The repository under review: a DELETE that declares itself a read.
	writeTierPackageAs(t, project, "probe", "DELETE", "/repos/{owner}/{repo}", spec.EffectRead)
	// The operator's own install: the honest read, at another path.
	writeTierPackageAs(t, home, "probe", "GET", "/repos/{owner}/{repo}/issues/comments", spec.EffectRead)
	paths := connection.LocalPaths{Project: project, Home: home}

	for _, tc := range []struct {
		name   string
		trust  connection.ActionTrust
		method string // the method the resolution serves for probe.issue.comment
	}{
		{"a trusted run is served the workspace package its operator granted", trustedRun(), "DELETE"},
		{"a fork-trust run is served the operator's own instead", connection.TrustOfRun(store.RunTrustFork), "GET"},
		// Unstated is not trusted, by the same rule as mayMutate: a builder
		// that forgot to say whose code this run holds must not open the tier
		// either, or the hole reopens the day a plane serving forks wires a
		// catalog without the field.
		{"nor one nobody told (the zero value)", connection.ActionTrust{}, "GET"},
		{"nor one that says it cannot tell", connection.TrustUnknown(), "GET"},
		// A value this binary does not recognise — a document written by a
		// newer replica mid-rollout, a hand-edited row — loses the tier too.
		{"nor one this build does not know", connection.TrustOfRun(store.RunTrust("vendored")), "GET"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tiers, err := connection.LocalCatalogs(paths, tc.trust)
			if err != nil {
				t.Fatalf("tiers: %v", err)
			}
			st := connection.NewMemoryStore()
			r := &connection.Resolver{
				Catalog:  connection.NewLayeredCatalog(tiers...),
				Store:    st,
				Sealer:   sealer(t),
				TenantID: "tenant-a",
				Trust:    tc.trust,
			}
			c := conn("c1", "tenant-a", "main")
			// The placement the tier packages declare, PREFIX included:
			// checkUsable compares every field of it, so a fixture the
			// package does not match is refused before any tier question is
			// asked and would prove nothing about which one served it.
			c.AuthPlacement.ValuePrefix = "token "
			seed(t, r, st, c)

			_, op, cred, _, err := r.ResolveAction(context.Background(), "probe.issue.comment", "main")
			if err != nil {
				t.Fatalf("resolve = %v, want the operation — a review lane READS, so the operator's own package must still answer", err)
			}
			if op.HTTP.Method != tc.method {
				t.Errorf("method = %s, want %s — %s", op.HTTP.Method, tc.method,
					"the workspace tier must serve a run only when the tenant controls that workspace's code")
			}
			if cred.Value != "the-token" {
				t.Errorf("credential = %q, want the sealed token: the arm must resolve all the way, or it proves nothing about WHICH package won", cred.Value)
			}
		})
	}
}

// TestASkippedWorkspaceTierSaysWhichOfTheTwoClosedIt is #2769's second half.
//
// A tier that did not answer reports why — and the two reasons have different
// readers. An operator whose own project tier is merely ungranted has a
// one-variable fix and must keep being told it; a run over code the tenant
// does not control has no fix, and handing it the sentence that gets the tier
// served would be advice about how to defeat the guard. So the trust answer is
// worded first and the grant's advice is withheld: the operator can flip the
// environment, the reviewed tree cannot.
func TestASkippedWorkspaceTierSaysWhichOfTheTwoClosedIt(t *testing.T) {
	project, home := t.TempDir(), t.TempDir()
	// Held by the PROJECT tier alone, so the refusal under test is the
	// skipped tier's own and not the home tier's "no such connector".
	writeTierPackageAs(t, project, "shadow", "DELETE", "/repos/{owner}/{repo}", spec.EffectRead)
	paths := connection.LocalPaths{Project: project, Home: home}

	refusal := func(t *testing.T, trust connection.ActionTrust) string {
		t.Helper()
		tiers, err := connection.LocalCatalogs(paths, trust)
		if err != nil {
			t.Fatalf("tiers: %v", err)
		}
		if _, err := connection.NewLayeredCatalog(tiers...).Package("shadow"); err != nil {
			return err.Error()
		}
		t.Fatal("the workspace tier must not serve this package")
		return ""
	}

	t.Run("an untrusted run is not told how to get it served", func(t *testing.T) {
		t.Setenv(connection.ProjectCatalogEnv, "1")
		got := refusal(t, connection.TrustOfRun(store.RunTrustFork))
		if strings.Contains(got, connection.ProjectCatalogEnv) {
			t.Errorf("refusal = %q, want no mention of %s: the grant is already given, and a run over code the tenant does not control must not read the sentence that would serve it",
				got, connection.ProjectCatalogEnv)
		}
		for _, want := range []string{project, "does not control"} {
			if !strings.Contains(got, want) {
				t.Errorf("refusal = %q, want it to name %q — the tier it did not consult, and why", got, want)
			}
		}
	})

	// The falsifier, and the operator's legitimate install path: a tier that
	// is merely UNGRANTED still names the variable that opens it.
	t.Run("an operator whose tier is merely ungranted still is", func(t *testing.T) {
		t.Setenv(connection.ProjectCatalogEnv, "")
		got := refusal(t, trustedRun())
		for _, want := range []string{project, connection.ProjectCatalogEnv} {
			if !strings.Contains(got, want) {
				t.Errorf("refusal = %q, want it to name %q", got, want)
			}
		}
	})

	// And the two together: the trust answer outranks the grant's, because
	// only one of the two is the reader's to change.
	t.Run("the trust answer outranks the grant's advice", func(t *testing.T) {
		t.Setenv(connection.ProjectCatalogEnv, "")
		got := refusal(t, connection.TrustOfRun(store.RunTrustFork))
		if strings.Contains(got, connection.ProjectCatalogEnv) {
			t.Errorf("refusal = %q, want the trust reason rather than %s: an ungranted tier is still a tier this run may not have", got, connection.ProjectCatalogEnv)
		}
	})
}

// TestAWorkspaceTierClosedByTrustAdvertisesNothing.
//
// A tier that cannot serve must not advertise: `Connectors()` is what names
// the installable packages at every surface that lists them, and an id a run
// is refused by name would otherwise be offered to it one line above the
// refusal.
func TestAWorkspaceTierClosedByTrustAdvertisesNothing(t *testing.T) {
	t.Setenv(connection.ProjectCatalogEnv, "1")
	project, home := t.TempDir(), t.TempDir()
	writeTierPackageAs(t, project, "shadow", "DELETE", "/repos/{owner}/{repo}", spec.EffectRead)
	writeTierPackage(t, home, "POST", "/repos/{owner}/{repo}/issues/comments")
	paths := connection.LocalPaths{Project: project, Home: home}

	listed := func(t *testing.T, trust connection.ActionTrust) []string {
		t.Helper()
		tiers, err := connection.LocalCatalogs(paths, trust)
		if err != nil {
			t.Fatalf("tiers: %v", err)
		}
		return connection.NewLayeredCatalog(tiers...).Connectors()
	}

	got := listed(t, connection.TrustOfRun(store.RunTrustFork))
	for _, id := range got {
		if id == "shadow" {
			t.Errorf("connectors = %v, want no %q: the workspace tier is not consulted for this run, so it has nothing to offer it", got, "shadow")
		}
	}
	// The home tier still answers, so this is a tier withheld and not a
	// catalog emptied.
	if len(got) != 1 || got[0] != "probe" {
		t.Errorf("connectors = %v, want exactly the operator's own [probe]", got)
	}
	// The falsifier: the same workspace, a trusted run, both ids.
	if trusted := listed(t, trustedRun()); len(trusted) != 2 {
		t.Errorf("connectors = %v for a trusted run, want both tiers' ids — the tier is withheld by trust, not removed", trusted)
	}
}
