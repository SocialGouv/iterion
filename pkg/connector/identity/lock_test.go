package identity_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	yaml "go.yaml.in/yaml/v2"

	"github.com/SocialGouv/iterion/pkg/connector/gen"
	"github.com/SocialGouv/iterion/pkg/connector/identity"
	"github.com/SocialGouv/iterion/pkg/connector/overlay"
	"github.com/SocialGouv/iterion/pkg/connector/spec"
)

func generated(t *testing.T, paths map[string]string, auth map[string]any) *spec.Package {
	t.Helper()
	items := map[string]any{}
	for path, id := range paths {
		items[path] = map[string]any{"get": map[string]any{"tags": []string{"issue"}, "operationId": id, "responses": map[string]any{"200": map[string]any{"description": "ok"}}}}
	}
	doc := map[string]any{"swagger": "2.0", "info": map[string]string{"title": "Probe", "version": "1"}, "host": "probe.example", "schemes": []string{"https"}, "paths": items, "securityDefinitions": auth}
	body, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	pkg, _, err := gen.Generate(body, gen.Options{ConnectorID: "probe"})
	if err != nil {
		t.Fatal(err)
	}
	return pkg
}

func TestRetiredNamesAndVendorRenames(t *testing.T) {
	lock := identity.New("probe")
	first := generated(t, map[string]string{"/old": "issueList", "/stay": "issueGet"}, nil)
	if err := lock.Reconcile(first); err != nil {
		t.Fatal(err)
	}
	if err := lock.ObservePublic(first); err != nil {
		t.Fatal(err)
	}
	oldID, stayID := "probe.issue.list", "probe.issue.get"
	second := generated(t, map[string]string{"/new": "issueList", "/stay": "totallyRenamed"}, nil)
	if err := lock.Reconcile(second); err != nil {
		t.Fatal(err)
	}
	if _, ok := second.Operation(oldID); ok {
		t.Fatal("new endpoint reused a retired name")
	}
	if op, ok := second.Operation(stayID); !ok || op.HTTP.Path != "/stay" {
		t.Fatalf("vendor rename changed unchanged HTTP identity: %+v", op)
	}
	if err := lock.ObservePublic(second); err != nil {
		t.Fatal(err)
	}
	// An authored overlay can rename, but cannot transfer the retired name.
	second.Ops[0].Operations[0].ID = oldID
	if err := lock.ObservePublic(second); err == nil {
		t.Fatal("public overlay stole retired name")
	}
}

func TestAuthIdentitySurvivesVendorKeyRenameAndRefusesAmbiguousShapes(t *testing.T) {
	key := func(name string) map[string]any {
		return map[string]any{"type": "apiKey", "in": "header", "name": name}
	}
	lock := identity.New("probe")
	first := generated(t, map[string]string{"/a": "issueList"}, map[string]any{"Z": key("Authorization")})
	if err := lock.Reconcile(first); err != nil {
		t.Fatal(err)
	}
	if err := lock.ObservePublic(first); err != nil {
		t.Fatal(err)
	}
	second := generated(t, map[string]string{"/a": "issueList"}, map[string]any{"A": key("X-New"), "Renamed": key("Authorization")})
	// Exercise the internal per-operation references as well as declarations.
	second.Ops[0].Operations[0].Security = []spec.SecurityRequirement{{Terms: []spec.SecurityTerm{{SchemeID: "token_2"}}}}
	if err := lock.Reconcile(second); err != nil {
		t.Fatal(err)
	}
	if got := second.Ops[0].Operations[0].Security[0].Terms[0].SchemeID; got != "token" {
		t.Fatalf("security still names shifted id %q", got)
	}
	if auth, _ := second.Connector.AuthScheme("token"); auth.Name != "Authorization" {
		t.Fatalf("auth moved: %+v", auth)
	}
	if err := lock.ObservePublic(second); err != nil {
		t.Fatal(err)
	}
	second.Connector.Auth = []spec.AuthScheme{{ID: "token", Kind: spec.AuthAPIKey, In: "header", Name: "Exfiltrate"}}
	if err := lock.ObservePublic(second); err == nil {
		t.Fatal("overlay reassigned public auth placement")
	}
	ambiguous := generated(t, map[string]string{"/a": "issueList"}, map[string]any{"A": key("Authorization"), "B": key("Authorization")})
	if err := lock.Reconcile(ambiguous); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("ambiguous auth accepted: %v", err)
	}
}

func TestLockStrictVersionAndStableBytes(t *testing.T) {
	lock := identity.New("probe")
	pkg := generated(t, map[string]string{"/a": "issueList"}, nil)
	if err := lock.Reconcile(pkg); err != nil {
		t.Fatal(err)
	}
	if err := lock.ObservePublic(pkg); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := lock.Write(dir); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(dir, identity.File))
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := identity.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := loaded.Reconcile(pkg); err != nil {
		t.Fatal(err)
	}
	if err := loaded.ObservePublic(pkg); err != nil {
		t.Fatal(err)
	}
	if err := loaded.Write(dir); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(filepath.Join(dir, identity.File))
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("unchanged regeneration rewrote lock")
	}
	for _, data := range []string{string(before) + "typo: true\n", "schema_version: 99\nfuture: true\n", strings.Replace(string(before), "schema_version: 1", "schema_version: 0", 1)} {
		if _, err := identity.Parse([]byte(data)); err == nil {
			t.Fatalf("invalid lock accepted: %s", data)
		}
	}
	_, err = identity.Parse([]byte("schema_version: 99\nfuture: true\n"))
	if err == nil || !strings.Contains(err.Error(), "upgrade iterion") {
		t.Fatalf("strict parse masked future-version remedy: %v", err)
	}
}

func TestForgejoIdentityLockMatchesAllExistingOperations(t *testing.T) {
	dir := filepath.Join("..", "..", "..", "connectors", "forgejo")
	raw, err := spec.LoadGenerated(dir)
	if err != nil {
		t.Fatal(err)
	}
	public, err := overlay.LoadPackage(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw.Operations()) != 503 || len(public.Operations()) != 503 {
		t.Fatalf("Forgejo baseline changed: %d/%d", len(raw.Operations()), len(public.Operations()))
	}
	lock := identity.New("forgejo")
	before := raw.Operations()
	if err := lock.Reconcile(raw); err != nil {
		t.Fatal(err)
	}
	if err := lock.ObservePublic(public); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, raw.Operations()) {
		t.Fatal("seeding renamed existing Forgejo operations")
	}
	committed, err := identity.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if committed == nil {
		t.Fatal("Forgejo identity lock not seeded")
	}
	want, err := yaml.Marshal(lock)
	if err != nil {
		t.Fatal(err)
	}
	got, err := yaml.Marshal(committed)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatal("committed lock differs from existing Forgejo identities")
	}
}
