package mongo

import (
	"context"
	"errors"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/mongo/address"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"go.mongodb.org/mongo-driver/v2/x/mongo/driver/dns"

	"github.com/SocialGouv/iterion/pkg/internal/mongotest"
	"github.com/SocialGouv/iterion/pkg/store"
)

func configuredIdentityStore(t *testing.T, uri, database string) *Store {
	t.Helper()
	opts := options.Client().ApplyURI(uri)
	if err := opts.Validate(); err != nil {
		t.Fatal(err)
	}
	ns, err := configuredContextNamespace(opts, database)
	if err != nil {
		t.Fatal(err)
	}
	return &Store{contextNamespace: ns}
}

func TestMongoContextIdentitySeparatesDestinationsAndIgnoresCredentials(t *testing.T) {
	base := "mongodb://alpha.example:27017,beta.example/"
	ctx := store.WithIdentity(context.Background(), "team-a", "owner-a")
	want, err := configuredIdentityStore(t, base, "runs").ContextIdentity(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, uri, db, tenant, owner string
		same                         bool
	}{
		{name: "seed-order", uri: "mongodb://beta.example,ALPHA.EXAMPLE:27017/", db: "runs", tenant: "team-a", same: true},
		{name: "credentials", uri: "mongodb://FAKE_USER:FAKE_PASSWORD@alpha.example,beta.example/ignored?authSource=admin&maxPoolSize=3", db: "runs", tenant: "team-a", same: true},
		{name: "owner", uri: base, db: "runs", tenant: "team-a", owner: "owner-b", same: true},
		{name: "database", uri: base, db: "other", tenant: "team-a"},
		{name: "hosts", uri: "mongodb://gamma.example/", db: "runs", tenant: "team-a"},
		{name: "tenant", uri: base, db: "runs", tenant: "team-b"},
		{name: "unix", uri: "mongodb://FAKE_USER:FAKE_PASSWORD@%2Ftmp%2FMongo.sock/", db: "runs", tenant: "team-a"},
		{name: "ipv6", uri: "mongodb://FAKE_USER:FAKE_PASSWORD@[::1]:27017,[::2]:27018/", db: "runs", tenant: "team-a"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := configuredIdentityStore(t, tc.uri, tc.db)
			got, err := s.ContextIdentity(store.WithIdentity(context.Background(), tc.tenant, tc.owner))
			if err != nil || (got.ID == want.ID) != tc.same {
				t.Fatalf("identity=%+v err=%v same=%v, base=%+v", got, err, tc.same, want)
			}
			if strings.Contains(got.Namespace, "FAKE_") || strings.Contains(got.Namespace, "authSource") || strings.Contains(got.Namespace, "maxPoolSize") {
				t.Fatalf("identity persists credentials or connection options: %+v", got)
			}
		})
	}
	if _, err := configuredIdentityStore(t, base, "runs").ContextIdentity(context.Background()); err == nil {
		t.Fatal("missing tenant falsely certifies a Mongo scope")
	}
	unscoped, err := configuredIdentityStore(t, base, "runs").ContextIdentity(store.WithoutTenantFilter(context.Background()))
	if err != nil || unscoped.ID == want.ID {
		t.Fatalf("explicit all-tenant scope=%+v err=%v", unscoped, err)
	}
}

func TestMongoContextIdentityKeepsConfiguredSRVServiceWithoutAnotherDNSLookup(t *testing.T) {
	previous := dns.DefaultResolver
	t.Cleanup(func() { dns.DefaultResolver = previous })
	var calls int
	dns.DefaultResolver = &dns.Resolver{
		LookupTXT: func(string) ([]string, error) { return nil, nil },
		LookupSRV: func(service, _, _ string) (string, []*net.SRV, error) {
			calls++
			return "", []*net.SRV{{Target: service + ".example.test.", Port: 27017}}, nil
		},
	}
	ctx := store.WithTenant(context.Background(), "team")
	uri := "mongodb+srv://FAKE_USER:FAKE_PASSWORD@cluster.example.test/?srvMaxHosts=1"
	opts := options.Client().ApplyURI(uri)
	if err := opts.Validate(); err != nil {
		t.Fatal(err)
	}
	nativeCalls := calls
	dns.DefaultResolver.LookupSRV = func(string, string, string) (string, []*net.SRV, error) {
		calls++
		return "", nil, errors.New("DNS unavailable after client configuration")
	}
	ns, err := configuredContextNamespace(opts, "runs")
	if err != nil || calls != nativeCalls {
		t.Fatalf("identity reparses SRV: err=%v DNS calls=%d want=%d", err, calls, nativeCalls)
	}
	first, err := (&Store{contextNamespace: ns}).ContextIdentity(ctx)
	if err != nil || !strings.Contains(first.Namespace, "cluster.example.test") || strings.Contains(first.Namespace, "mongodb.example.test") {
		t.Fatalf("identity follows resolved hosts: %+v err=%v", first, err)
	}
	// Changing only a resolved answer must not change this declaration.
	opts.Hosts = []string{"another.example.test:27017"}
	again, err := configuredContextNamespace(opts, "runs")
	if err != nil {
		t.Fatal(err)
	}
	second, err := (&Store{contextNamespace: again}).ContextIdentity(ctx)
	if err != nil || second != first {
		t.Fatalf("resolved SRV answer changes identity: %+v err=%v want=%+v", second, err, first)
	}
	dns.DefaultResolver.LookupSRV = func(service, _, _ string) (string, []*net.SRV, error) {
		return "", []*net.SRV{{Target: service + ".example.test.", Port: 27017}}, nil
	}
	opts = options.Client().ApplyURI(uri + "&srvServiceName=analytics")
	if err := opts.Validate(); err != nil {
		t.Fatal(err)
	}
	other, err := configuredContextNamespace(opts, "runs")
	if err != nil {
		t.Fatal(err)
	}
	different, err := (&Store{contextNamespace: other}).ContextIdentity(ctx)
	if err != nil || different.ID == first.ID {
		t.Fatalf("distinct SRV service collapses: %+v err=%v", different, err)
	}
}

func TestMongoContextIdentityUsesTheConstructedDatabase(t *testing.T) {
	uri := os.Getenv("ITERION_TEST_MONGO_URI")
	if uri == "" {
		t.Skip("ITERION_TEST_MONGO_URI not set; needs the real Mongo constructor")
	}
	var refs []store.ContextRef
	for range 2 {
		ctx, cancel := mongotest.Ctx(t)
		s, err := New(ctx, Config{URI: uri, Database: "iterion_identity_" + bsonNonce(t), Blob: newInMemoryBlob()})
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			ctx, cancel := mongotest.TeardownCtx()
			defer cancel()
			if err := s.db.Drop(ctx); err != nil {
				t.Error(err)
			}
			if err := s.Close(ctx); err != nil {
				t.Error(err)
			}
		})
		ref, err := store.ResolveStoreContextRef(store.WithTenant(t.Context(), "team"), s)
		if err != nil {
			t.Fatalf("constructed Mongo store has no identity: %v", err)
		}
		refs = append(refs, ref)
	}
	if refs[0].ID == refs[1].ID || !store.HasStoreContextIdentity(refs[0]) || !store.HasStoreContextIdentity(refs[1]) {
		t.Fatalf("different Mongo databases collapse to the same or legacy identity: %+v", refs)
	}
}

func TestMongoContextIdentityDiscardsNativeUserinfoBeforeDelimiters(t *testing.T) {
	ctx := store.WithTenant(t.Context(), "team")
	want, err := configuredIdentityStore(t, "mongodb://real-db.example/", "runs").ContextIdentity(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, uri := range []string{
		"mongodb://PRIVATE_USER:12345?SECRET_SUFFIX@real-db.example/",
		"mongodb://PRIVATE_USER:PASSWORD?SUFFIX@real-db.example/",
		"mongodb://PRIVATE_USER?SUFFIX:PASSWORD@real-db.example/",
		"mongodb://PRIVATE_USER:PASSWORD@real-db.example/?appName=other@option",
		"mongodb://PRIVATE_USER:PASSWORD%3FSUFFIX@real-db.example/",
	} {
		got, err := configuredIdentityStore(t, uri, "runs").ContextIdentity(ctx)
		if err != nil || got != want {
			t.Errorf("credentials/options change the native destination identity: got=%+v err=%v want=%+v", got, err, want)
		}
	}
}

func TestMongoContextIdentityKeepsNativeTransportDestinationsDistinct(t *testing.T) {
	t.Chdir(t.TempDir())
	ctx := store.WithTenant(t.Context(), "team")
	for _, pair := range [][2]string{
		{"mongo", "mongo."},
		{"Mongo.sock", "mongo.sock"},
		{"%2Ftmp%2FMongo.sock", "%2Ftmp%2Fmongo.sock"},
		{"%FF.sock", "%FE.sock"},
	} {
		var refs []store.ContextRef
		for _, host := range pair {
			opts := options.Client().ApplyURI("mongodb://" + host + "/")
			if err := opts.Validate(); err != nil {
				t.Fatal(err)
			}
			native := address.Address(opts.Hosts[0])
			ns, err := configuredContextNamespace(opts, "runs")
			if err != nil {
				t.Fatal(err)
			}
			want := native.String()
			if native.Network() == "unix" {
				want, err = filepath.Abs(want)
				if err != nil {
					t.Fatal(err)
				}
				want = "unix:" + url.PathEscape(want)
			}
			if len(ns.Hosts) != 1 || ns.Hosts[0] != want {
				t.Errorf("identity changes native %s destination %q to %v", native.Network(), want, ns.Hosts)
			}
			ref, err := (&Store{contextNamespace: ns}).ContextIdentity(ctx)
			if err != nil {
				t.Fatal(err)
			}
			refs = append(refs, ref)
		}
		if refs[0].ID == refs[1].ID {
			t.Errorf("distinct native transport destinations collapse: %v", pair)
		}
	}
}

func TestMongoContextIdentityRootsRelativeUnixSockets(t *testing.T) {
	ctx := store.WithTenant(t.Context(), "team")
	var refs []store.ContextRef
	for range 2 {
		t.Chdir(t.TempDir())
		ref, err := configuredIdentityStore(t, "mongodb://Mongo.sock/", "runs").ContextIdentity(ctx)
		if err != nil {
			t.Fatal(err)
		}
		refs = append(refs, ref)
	}
	if refs[0].ID == refs[1].ID {
		t.Fatal("relative Unix socket in different directories has the same identity")
	}
}
