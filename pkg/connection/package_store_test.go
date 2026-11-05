package connection_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/SocialGouv/iterion/pkg/connection"
	"github.com/SocialGouv/iterion/pkg/connector/spec"
	"github.com/SocialGouv/iterion/pkg/internal/mongotest"
)

// newTestMongoPackageStore builds the Mongo twin over a fresh database, gated
// on ITERION_TEST_MONGO_URI like every mongo suite in this tree (the
// mongo-conformance CI job's package list names ./pkg/connection/...).
func newTestMongoPackageStore(t *testing.T) *connection.MongoPackageStore {
	t.Helper()
	uri := os.Getenv("ITERION_TEST_MONGO_URI")
	if uri == "" {
		t.Skip("ITERION_TEST_MONGO_URI not set")
	}
	client, err := mongo.Connect(options.Client().ApplyURI(uri))
	if err != nil {
		t.Fatalf("mongo connect: %v", err)
	}
	nonce := make([]byte, 4)
	_, _ = rand.Read(nonce)
	db := client.Database("iterion_conn_pkgs_" + hex.EncodeToString(nonce))
	t.Cleanup(func() {
		drop, dropCancel := mongotest.TeardownCtx()
		defer dropCancel()
		_ = db.Drop(drop)
		_ = client.Disconnect(drop)
	})
	return connection.NewMongoPackageStore(db)
}

// publishedFiles builds a VALID package as a file map through the format's
// own writer, so the fixture is what a publisher would write rather than a
// hand-spelled YAML that drifts from the loader. id parameterises the
// connector id so a test can publish the same package under two names.
func publishedFiles(t *testing.T, id string) map[string]string {
	t.Helper()
	return packageFiles(t, publishedPackage(id))
}

// publishedPackage is the valid package publishedFiles writes, for a test
// that edits it before writing.
func publishedPackage(id string) *spec.Package {
	return &spec.Package{
		Connector: spec.Connector{
			SchemaVersion: spec.SchemaVersion, ID: id, Version: "1.0.0",
			DisplayName: "Probe", Description: "the test connector",
			BaseURL: spec.BaseURL{Default: "https://probe.example"},
			Auth: []spec.AuthScheme{
				{ID: "token", Kind: spec.AuthAPIKey, In: "header", Name: "Authorization"},
			},
			Maturity: spec.MaturityExperimental,
		},
		Ops: []spec.OpsFile{{
			SchemaVersion: spec.SchemaVersion, Connector: id, Domain: "issue",
			Operations: []spec.Operation{{
				ID: id + ".issue.get", Resource: "issue", Verb: "get",
				HTTP:   spec.HTTPBinding{Method: "GET", Path: "/issues/{id}"},
				Effect: spec.EffectRead, Deterministic: true,
				Params:      []spec.Param{{Key: "id", Name: "id", In: spec.InPath, Type: "integer", Required: true}},
				Results:     []spec.ResultCase{{Status: 200}},
				Security:    []spec.SecurityRequirement{{Terms: []spec.SecurityTerm{{SchemeID: "token"}}}},
				Description: "get one issue",
			}},
		}},
	}
}

// packageFiles writes pkg through the format's own writer and reads it back
// as the file map a store row holds.
func packageFiles(t *testing.T, pkg *spec.Package) map[string]string {
	t.Helper()
	dir := t.TempDir()
	if err := spec.Write(dir, pkg); err != nil {
		t.Fatalf("spec.Write: %v", err)
	}
	var files map[string]string
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		body, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if files == nil {
			files = map[string]string{}
		}
		files[filepath.ToSlash(rel)] = string(body)
		return nil
	})
	if err != nil || len(files) == 0 {
		t.Fatalf("fixture copied nothing (err=%v) — the test would prove nothing", err)
	}
	return files
}

func publish(t *testing.T, st connection.PackageStore, tenant, id string) {
	t.Helper()
	if err := st.Publish(context.Background(), connection.StoredPackage{
		TenantID: tenant, ConnectorID: id, Files: publishedFiles(t, id), CreatedBy: "test",
	}); err != nil {
		t.Fatalf("publish %s/%s: %v", tenant, id, err)
	}
}

// The store contract, held against both twins (#2757): publish validates
// before it serves, reads are tenant-scoped in the signature, and the
// durable tiers answer the same way in memory and in Mongo.
func TestPackageStore_PublishGetRetractAcrossTwins(t *testing.T) {
	run := func(t *testing.T, st connection.PackageStore) {
		ctx := context.Background()
		publish(t, st, "team-a", "probe")

		got, err := st.Get(ctx, "team-a", "probe")
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if got.ContentHash == "" || got.CreatedAt.IsZero() {
			t.Errorf("the publish must record the hash and the timestamps: %+v", got)
		}
		// Tenant-scoped in the signature: another tenant's id reads not-found.
		if _, err := st.Get(ctx, "team-b", "probe"); err == nil {
			t.Fatal("a package read across tenants must miss")
		}

		// Republish replaces: one row per (tenant, connector).
		publish(t, st, "team-a", "probe")
		rows, err := st.ListByTenant(ctx, "team-a")
		if err != nil || len(rows) != 1 {
			t.Fatalf("list after republish: %v rows=%d", err, len(rows))
		}

		if err := st.Retract(ctx, "team-a", "probe"); err != nil {
			t.Fatalf("retract: %v", err)
		}
		if _, err := st.Get(ctx, "team-a", "probe"); err == nil {
			t.Fatal("a retracted package must not serve")
		}
		if err := st.Retract(ctx, "team-a", "probe"); err == nil {
			t.Fatal("retracting an absent package must answer not-found")
		}
	}
	t.Run("memory", func(t *testing.T) { run(t, connection.NewMemoryPackageStore()) })
	t.Run("mongo", func(t *testing.T) {
		st := newTestMongoPackageStore(t)
		run(t, st)
	})
}

// A republish keeps the creation's when and by whom — the first publish's —
// in both twins: one rule (stampPublish), or a twin answers differently.
func TestPackageStore_ARepublishKeepsTheCreation(t *testing.T) {
	run := func(t *testing.T, st connection.PackageStore) {
		ctx := context.Background()
		for _, tc := range []struct{ first, second, want string }{
			{"alice", "bob", "alice"},
			// An unknown creator stays unknown: the republisher did not
			// create the row.
			{"", "bob", ""},
		} {
			for _, by := range []string{tc.first, tc.second} {
				if err := st.Publish(ctx, connection.StoredPackage{
					TenantID: "team-a", ConnectorID: "probe", Files: publishedFiles(t, "probe"), CreatedBy: by,
				}); err != nil {
					t.Fatalf("publish as %q: %v", by, err)
				}
			}
			got, err := st.Get(ctx, "team-a", "probe")
			if err != nil {
				t.Fatal(err)
			}
			if got.CreatedBy != tc.want {
				t.Errorf("created_by after %q then %q = %q, want %q", tc.first, tc.second, got.CreatedBy, tc.want)
			}
			if err := st.Retract(ctx, "team-a", "probe"); err != nil {
				t.Fatal(err)
			}
		}
	}
	t.Run("memory", func(t *testing.T) { run(t, connection.NewMemoryPackageStore()) })
	t.Run("mongo", func(t *testing.T) { run(t, newTestMongoPackageStore(t)) })
}

// The publish gate: what must NOT reach a tier — held against BOTH twins,
// because the gate is the two implementations' one shared function and a
// twin that skipped it would serve what the other refused.
func TestPackageStore_PublishRefusesWhatCannotServe(t *testing.T) {
	for _, twin := range []struct {
		name string
		st   func(t *testing.T) connection.PackageStore
	}{
		{"memory", func(t *testing.T) connection.PackageStore { return connection.NewMemoryPackageStore() }},
		{"mongo", func(t *testing.T) connection.PackageStore { return newTestMongoPackageStore(t) }},
	} {
		t.Run(twin.name, func(t *testing.T) {
			st := twin.st(t)
			ctx := context.Background()

			for _, tc := range []struct {
				name    string
				mutate  func(*connection.StoredPackage)
				wantMsg string
			}{
				{"no files", func(p *connection.StoredPackage) { p.Files = nil }, "no files"},
				{"a path that escapes", func(p *connection.StoredPackage) {
					p.Files["../evil.yaml"] = "id: evil"
				}, "plain relative name"},
				{"a path with a backslash", func(p *connection.StoredPackage) {
					p.Files[`ops\..\evil.yaml`] = "id: evil"
				}, "backslash"},
				{"the id the content declares differs", func(p *connection.StoredPackage) {
					p.ConnectorID = "other"
				}, "declares id"},
				{"a malformed document", func(p *connection.StoredPackage) {
					p.Files["connector.yaml"] = "schema_version: 1\nid: probe\nversion: 1.0.0\nauth: ["
				}, "does not load"},
				// The hash is recorded AT PUBLISH, never trusted from the
				// caller: a supplied one must not survive.
				{"a caller-supplied content hash", func(p *connection.StoredPackage) {
					p.ContentHash = "deadbeef"
				}, ""},
			} {
				t.Run(tc.name, func(t *testing.T) {
					pkg := connection.StoredPackage{TenantID: "team-a", ConnectorID: "probe", Files: publishedFiles(t, "probe")}
					tc.mutate(&pkg)
					err := st.Publish(ctx, pkg)
					if tc.wantMsg == "" {
						if err != nil {
							t.Fatalf("the publish itself is valid: %v", err)
						}
						got, gerr := st.Get(ctx, "team-a", "probe")
						if gerr != nil {
							t.Fatal(gerr)
						}
						if got.ContentHash == "" || got.ContentHash == "deadbeef" {
							t.Fatalf("the stored hash must be the computed one, got %q", got.ContentHash)
						}
						_ = st.Retract(ctx, "team-a", "probe")
						return
					}
					if err == nil {
						t.Fatalf("want a refusal mentioning %q", tc.wantMsg)
					}
					if !strings.Contains(err.Error(), tc.wantMsg) {
						t.Errorf("refusal = %v, want it to mention %q", err, tc.wantMsg)
					}
					// And the refusal left nothing serving.
					if _, gerr := st.Get(ctx, "team-a", "probe"); gerr == nil {
						t.Fatal("a refused publish must leave no row serving")
					}
				})
			}
		})
	}
}

// The publish gate builds the package's OWN facade: two operations mapping
// to one MCP tool name load cleanly and can never be served to an agent, so
// the write refuses them — held against both twins, like every refusal.
func TestPackageStore_PublishRefusesToolsItsFacadeCannotServe(t *testing.T) {
	pkg := publishedPackage("probe")
	base := pkg.Ops[0].Operations[0]
	for _, c := range []struct{ id, resource, verb string }{
		{"probe.issue_x.get", "issue_x", "get"},
		{"probe.issue.x_get", "issue", "x_get"},
	} {
		op := base
		op.ID, op.Resource, op.Verb, op.MCP = c.id, c.resource, c.verb, true
		pkg.Ops[0].Operations = append(pkg.Ops[0].Operations, op)
	}
	files := packageFiles(t, pkg)
	for _, twin := range []struct {
		name string
		st   func(t *testing.T) connection.PackageStore
	}{
		{"memory", func(t *testing.T) connection.PackageStore { return connection.NewMemoryPackageStore() }},
		{"mongo", func(t *testing.T) connection.PackageStore { return newTestMongoPackageStore(t) }},
	} {
		t.Run(twin.name, func(t *testing.T) {
			st := twin.st(t)
			err := st.Publish(context.Background(), connection.StoredPackage{TenantID: "team-a", ConnectorID: "probe", Files: files})
			if err == nil || !strings.Contains(err.Error(), "probe_issue_x_get") {
				t.Fatalf("err = %v, want the publish refused naming the colliding tool", err)
			}
			if _, gerr := st.Get(context.Background(), "team-a", "probe"); gerr == nil {
				t.Fatal("a refused publish must leave no row serving")
			}
		})
	}
}

// The precedence, witnessed in both directions (#2757's done-when): team over
// platform, platform over baked, and the tier that SERVED is named in the
// answer each time.
func TestCloudTiers_ThePrecedenceHoldsInBothDirections(t *testing.T) {
	store := connection.NewMemoryPackageStore()
	publish(t, store, connection.PlatformTenantID, "probe")
	cat := connection.NewLayeredCatalog(connection.CloudCatalogs(store, "team-a")...)

	// Only the platform holds it: the platform tier serves and says so.
	_, tier, err := cat.Package("probe")
	if err != nil || tier.Kind != connection.TierPlatform {
		t.Fatalf("platform-only: tier=%v err=%v", tier.Kind, err)
	}

	// The team publishes the same id: the team's package wins, NAMED.
	publish(t, store, "team-a", "probe")
	_, tier, err = cat.Package("probe")
	if err != nil || tier.Kind != connection.TierTeam {
		t.Fatalf("team shadowing the platform: tier=%v err=%v — the shadow must be visible, not silent", tier.Kind, err)
	}

	// The floor serves what neither tier holds.
	_, tier, err = cat.Package("forgejo")
	if err != nil || tier.Kind != connection.TierBaked {
		t.Fatalf("the baked floor: tier=%v err=%v", tier.Kind, err)
	}

	// Another tenant has no team row: the platform tier serves it.
	catB := connection.NewLayeredCatalog(connection.CloudCatalogs(store, "team-b")...)
	if _, tier, err := catB.Package("probe"); err != nil || tier.Kind != connection.TierPlatform {
		t.Fatalf("team-b reads the platform's package: tier=%v err=%v", tier.Kind, err)
	}
}

// A tier that HOLDS the package and cannot serve it stops the search: the
// team's broken row must not fall through to the platform's sound one — that
// fall-through is validation becoming selection, the defect the layered
// catalog's error discipline exists against.
func TestCloudTiers_ABrokenTeamRowStopsTheSearch(t *testing.T) {
	store := connection.NewMemoryPackageStore()
	publish(t, store, connection.PlatformTenantID, "probe")
	broken := &brokenPackageStore{PackageStore: store, tenant: "team-a"}
	cat := connection.NewLayeredCatalog(
		connection.TeamCatalog(broken, "team-a"),
		connection.PlatformCatalog(store),
		connection.BakedCatalog(),
	)
	_, _, err := cat.Package("probe")
	if err == nil || !strings.Contains(err.Error(), "does not load") {
		t.Fatalf("err = %v — the team's broken row must stop the search, not fall through to the platform's", err)
	}
	// The refusal names the ROW (tenant + id): the repair is a retract or a
	// republish of exactly it.
	if !strings.Contains(err.Error(), "team-a") {
		t.Errorf("err = %v, want it to name the tenant whose row is broken", err)
	}
}

// brokenPackageStore answers one tenant's Get with a row that cannot load —
// the state a hand edit or a migration leaves behind.
type brokenPackageStore struct {
	connection.PackageStore
	tenant string
}

func (s *brokenPackageStore) Get(ctx context.Context, tenantID, connectorID string) (connection.StoredPackage, error) {
	if tenantID == s.tenant {
		return connection.StoredPackage{TenantID: tenantID, ConnectorID: connectorID,
			Files: map[string]string{"connector.yaml": "schema_version: 1\nid: " + connectorID + "\nversion: ["}}, nil
	}
	return s.PackageStore.Get(ctx, tenantID, connectorID)
}

// listFailingStore is a store whose listing fails — a network blip, a
// timeout — while its reads still answer.
type listFailingStore struct {
	connection.PackageStore
}

var errListBlip = errors.New("list blip")

func (listFailingStore) ListByTenant(context.Context, string) ([]connection.StoredPackage, error) {
	return nil, errListBlip
}

// A tier that cannot LIST says so, through the composition: the facade built
// over a silently empty team tier would serve the floor's tools alone with no
// sign the team's were lost.
func TestCloudTiers_AListingFailureIsTheListingsError(t *testing.T) {
	store := connection.NewMemoryPackageStore()
	publish(t, store, "team-a", "probe")
	failing := listFailingStore{PackageStore: store}

	ids, err := connection.TeamCatalog(failing, "team-a").Connectors()
	if !errors.Is(err, errListBlip) || ids != nil {
		t.Fatalf("team tier listing = %v, %v — want the store's error, not an empty listing", ids, err)
	}
	if !strings.Contains(err.Error(), "team tier (tenant team-a)") {
		t.Errorf("err = %v, want it to name the tier and tenant", err)
	}
	ids, err = connection.NewLayeredCatalog(connection.CloudCatalogs(failing, "team-a")...).Connectors()
	if !errors.Is(err, errListBlip) || ids != nil {
		t.Fatalf("the composition's listing = %v, %v — a failing tier must fail the union, not drop out of it", ids, err)
	}
	// The healthy store lists through the same composition.
	ids, err = connection.NewLayeredCatalog(connection.CloudCatalogs(store, "team-a")...).Connectors()
	if err != nil || strings.Join(ids, ",") != "forgejo,probe" {
		t.Fatalf("healthy listing = %v, %v", ids, err)
	}
}

// An FS tier: an absent root holds nothing; a root that cannot be read as a
// directory is the listing's error.
func TestFSCatalog_AnUnreadableRootIsTheListingsError(t *testing.T) {
	dir := t.TempDir()
	if ids, err := connection.NewFSCatalog(filepath.Join(dir, "absent")).Connectors(); err != nil || len(ids) != 0 {
		t.Fatalf("absent root = %v, %v — want an empty listing", ids, err)
	}
	file := filepath.Join(dir, "not-a-dir")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	ids, err := connection.NewFSCatalog(file).Connectors()
	if err == nil || ids != nil || !strings.Contains(err.Error(), "cannot be listed") {
		t.Fatalf("a root that is a file = %v, %v — want the read failure", ids, err)
	}
}
