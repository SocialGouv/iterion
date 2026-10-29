package connection_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/connection"
	"github.com/SocialGouv/iterion/pkg/connector/spec"
)

// TestTheServingTierIsNamedInTheAnswer — #2757.
//
// Two tiers can hold the same connector id, the same version, the same scheme
// id and the same placement; what differs is WHAT THE OPERATION DOES. So an
// operator debugging an operation they did not expect cannot derive which copy
// answered from the package, and cannot derive it from the search either — a
// layered lookup asks its tiers in order and only the one that answered knows
// it did. The tier therefore travels with the answer.
//
// BOTH DIRECTIONS, because a one-direction assertion passes on a reversed list
// and on a hardcoded name: each arm expects a DIFFERENT tier to win, and each
// checks the named tier against the package actually served (the method only
// that copy declares). A tier name that did not follow the win fails one arm or
// the other.
func TestTheServingTierIsNamedInTheAnswer(t *testing.T) {
	project, home := t.TempDir(), t.TempDir()
	writeTierPackage(t, project, "DELETE", "/repos/{owner}/{repo}")
	writeTierPackage(t, home, "POST", "/repos/{owner}/{repo}/issues/comments")
	paths := connection.LocalPaths{Project: project, Home: home}

	for _, tc := range []struct {
		name    string
		granted string
		kind    connection.TierKind
		source  string
		method  string
	}{
		{"the granted project tier wins, and the answer says so", "1", connection.TierProject, project, "DELETE"},
		// The same two roots with the grant closed: the home tier wins, so a
		// name copied from the first tier of the list — or from the tier an
		// implementation expected to win — is red here.
		{"with the grant closed the home tier wins, and the answer says so", "", connection.TierHome, home, "POST"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(connection.ProjectCatalogEnv, tc.granted)
			tiers, err := connection.LocalCatalogs(paths, trustedRun())
			if err != nil {
				t.Fatalf("tiers: %v", err)
			}
			pkg, tier, err := connection.NewLayeredCatalog(tiers...).Package("probe")
			if err != nil {
				t.Fatalf("package: %v", err)
			}
			if tier.Kind != tc.kind || tier.Source != tc.source {
				t.Errorf("tier = %+v, want kind %q at %s", tier, tc.kind, tc.source)
			}
			// The tier named must be the tier that SERVED, not a label: the
			// method is the one thing a connection pins nothing about, so it
			// identifies the copy.
			op, ok := pkg.Operation("probe.issue.comment")
			if !ok {
				t.Fatal("probe.issue.comment is absent")
			}
			if op.HTTP.Method != tc.method {
				t.Errorf("method = %s, want %s — the tier named must be the one whose package was served", op.HTTP.Method, tc.method)
			}
			// And the phrase a surface prints carries both halves: an operator
			// told only "project" still has to find which directory that is.
			printed := tier.String()
			for _, want := range []string{string(tc.kind), tc.source} {
				if !strings.Contains(printed, want) {
					t.Errorf("tier.String() = %q, want it to name %q", printed, want)
				}
			}
		})
	}
}

// unnamedTier is a Catalog that serves a package without saying which tier it
// is — the shape a new tier implementation falls into by forgetting a return
// value, and the one that would make every printed provenance a guess.
type unnamedTier struct {
	pkg  *spec.Package
	tier connection.Tier
}

func (c unnamedTier) Package(string) (*spec.Package, connection.Tier, error) {
	return c.pkg, c.tier, nil
}

func (c unnamedTier) Connectors() []string { return []string{"probe"} }

// TestATierThatServesWithoutNamingItselfIsRefused.
//
// The alternative is worse than an error: the layered lookup would have to
// invent the provenance — the index of the loop, the first tier, the last
// reason — and every surface that prints it would read as a fact. A guess
// about which tier spent a credential is the one thing this answer exists to
// prevent, so an unnamed answer is refused BY NAME.
func TestATierThatServesWithoutNamingItselfIsRefused(t *testing.T) {
	pkg := tierPackage("https://example.invalid", "POST", "/issues")

	_, _, err := connection.NewLayeredCatalog(unnamedTier{pkg: pkg}).Package("probe")
	if err == nil {
		t.Fatal("a tier that serves without naming itself must be refused: nothing could say which tier won")
	}
	for _, want := range []string{"probe", "did not name its tier"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %v, want it to name %q", err, want)
		}
	}

	// The falsifier: the same catalog that DOES name itself serves normally,
	// so the refusal is about the missing name and not about the tier being a
	// stranger to this package.
	named := connection.Tier{Kind: connection.TierMemory, Source: "a stored package"}
	got, tier, err := connection.NewLayeredCatalog(unnamedTier{pkg: pkg, tier: named}).Package("probe")
	if err != nil {
		t.Fatalf("a tier that names itself must serve: %v", err)
	}
	if got != pkg || tier != named {
		t.Errorf("served %v from %+v, want the package from %+v", got != nil, tier, named)
	}
}

// movedCredentialTier writes a package that keeps the connector id, the
// version and the scheme ID while moving the credential OUT of the
// Authorization header and into a query string — where the vendor's logs, its
// proxies and its referrers keep it. It is the shadowing package the project
// tier's own doc describes (pkg/connection/local.go), written by the only
// party that can: whoever controls the checkout under review.
func movedCredentialTier(t *testing.T, root string) {
	t.Helper()
	pkg := tierPackage("https://probe.example", "POST", "/repos/{owner}/{repo}/issues/comments")
	pkg.Connector.Auth = []spec.AuthScheme{{
		ID: "token", Kind: spec.AuthAPIKey, In: "query", Name: "access_token",
	}}
	dir := filepath.Join(root, "probe")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := spec.Write(dir, pkg); err != nil {
		t.Fatal(err)
	}
}

// TestAMovedCredentialIsRefusedNamingTheTierThatMovedIt — #2757, through the
// PUBLIC path (ResolveAction), which is the one an attacker reaches.
//
// checkUsable already refused a package that moves a pinned credential. What
// it could not say was WHICH package: the id, the version and the scheme id
// are identical across tiers by construction here, so "the package now asks
// for a query string" named nothing an operator could go and look at. The
// repair is one directory away from the resolution, and which directory is
// exactly the missing fact.
func TestAMovedCredentialIsRefusedNamingTheTierThatMovedIt(t *testing.T) {
	project, home := t.TempDir(), t.TempDir()
	// The repository under review: the same scheme id, the credential moved.
	movedCredentialTier(t, project)
	// The operator's own install: the honest header placement the connection
	// below was entrusted under.
	writeTierPackage(t, home, "POST", "/repos/{owner}/{repo}/issues/comments")
	paths := connection.LocalPaths{Project: project, Home: home}

	resolve := func(t *testing.T) error {
		t.Helper()
		tiers, err := connection.LocalCatalogs(paths, trustedRun())
		if err != nil {
			t.Fatalf("tiers: %v", err)
		}
		st := connection.NewMemoryStore()
		r := &connection.Resolver{
			Catalog:  connection.NewLayeredCatalog(tiers...),
			Store:    st,
			Sealer:   sealer(t),
			TenantID: "tenant-a",
			Trust:    trustedRun(),
		}
		c := conn("c1", "tenant-a", "main")
		// The placement as `connections add` pinned it from the honest
		// package, prefix included: checkUsable compares every field, so a
		// fixture the home tier does not match would be refused for the wrong
		// reason and would prove nothing about the shadowing tier.
		c.AuthPlacement.ValuePrefix = "token "
		seed(t, r, st, c)
		_, _, _, _, err = r.ResolveAction(context.Background(), "probe.issue.comment", "main")
		return err
	}

	t.Run("the granted shadowing tier is refused, and it is named", func(t *testing.T) {
		t.Setenv(connection.ProjectCatalogEnv, "1")
		err := resolve(t)
		if err == nil {
			t.Fatal("a package that moves a pinned credential must be refused rather than served")
		}
		for _, want := range []string{"was entrusted for", string(connection.TierProject), project} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("refusal = %v, want it to name %q — the repair is in that directory", err, want)
			}
		}
		// And it must not misname the tier that did NOT serve: an operator
		// sent to their own install would delete the honest package.
		if strings.Contains(err.Error(), home) {
			t.Errorf("refusal = %v, want no mention of the home tier %s: it is not the one that moved the credential", err, home)
		}
	})

	// The falsifier: with the grant closed the home tier serves the same
	// operation and the resolution SUCCEEDS, so the arm above is about the
	// moved credential and not about this fixture being unusable.
	t.Run("the honest tier still resolves", func(t *testing.T) {
		t.Setenv(connection.ProjectCatalogEnv, "")
		if err := resolve(t); err != nil {
			t.Fatalf("resolve = %v, want the operation: the operator's own package places the credential where it was entrusted", err)
		}
	})
}

// TestAMissingOperationNamesTheTierThatWasAsked.
//
// "connector forgejo (v1.2.0) has no operation forgejo.issue.comment" sends a
// reader to the package they EXPECTED to answer. When two tiers hold the id at
// the same version — a pinned project copy over an installed one — that is the
// wrong directory, and the near-miss list it prints comes from the other
// package entirely.
func TestAMissingOperationNamesTheTierThatWasAsked(t *testing.T) {
	home := t.TempDir()
	writeTierPackage(t, home, "POST", "/repos/{owner}/{repo}/issues/comments")
	tiers, err := connection.LocalCatalogs(connection.LocalPaths{Home: home}, trustedRun())
	if err != nil {
		t.Fatalf("tiers: %v", err)
	}
	_, _, _, err = connection.ResolveOperation(connection.NewLayeredCatalog(tiers...), "probe.issue.nope")
	if err == nil {
		t.Fatal("an operation the package does not declare must be refused")
	}
	for _, want := range []string{"probe", string(connection.TierHome), home} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %v, want it to name %q — which package was asked is half the repair", err, want)
		}
	}
}

// TestTheTierPhraseAlwaysSaysSomething.
//
// The phrase is interpolated into refusals ("the %s now asks for"), so an
// empty one does not degrade the message: it makes it unreadable, and the zero
// value is reachable from any implementer that forgets a return. A tier that
// is not a path says what kind it is and nothing it cannot back up.
func TestTheTierPhraseAlwaysSaysSomething(t *testing.T) {
	if got := (connection.Tier{}).String(); got == "" {
		t.Error("the zero tier must still print something: it is interpolated into refusals")
	}
	if got := (connection.Tier{Kind: connection.TierMemory}).String(); got != "memory catalog" {
		t.Errorf("tier without a source = %q, want %q", got, "memory catalog")
	}
	// The UNLABELLED door — the one an e2e fixture and an operator's own tree
	// come through — says what it is rather than nothing, which is what keeps
	// a refusal from reading "the no tier now asks for".
	root := t.TempDir()
	// The path bears the operation's own `{owner}`/`{repo}` parameters: the
	// FS tier loads through the package's own validation, so a shorter path
	// is refused before any tier question is asked.
	writeTierPackage(t, root, "POST", "/repos/{owner}/{repo}/issues/comments")
	_, dirTier, err := connection.NewFSCatalog(root).Package("probe")
	if err != nil {
		t.Fatalf("package: %v", err)
	}
	if dirTier.Kind != connection.TierDirectory || dirTier.Source != root {
		t.Errorf("tier = %+v, want kind %q at %s", dirTier, connection.TierDirectory, root)
	}

	// The in-memory tier names itself like every other, so a resolver built
	// over stored content reports a tier rather than nothing.
	_, tier, err := connection.NewMemoryCatalog(tierPackage("https://probe.example", "POST", "/issues")).Package("probe")
	if err != nil {
		t.Fatalf("package: %v", err)
	}
	if tier.Kind != connection.TierMemory {
		t.Errorf("tier = %+v, want kind %q", tier, connection.TierMemory)
	}
}
