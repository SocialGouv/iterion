package server

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/botsource"
	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/store"
)

func snapshotParent(child string) string {
	return "subbot child:\n  source: \"../" + child + "/main.bot\"\n\nworkflow parent:\n  entry: child\n  child -> done\n"
}

type snapshotFailChildStore struct{ botsource.Store }

func (s snapshotFailChildStore) GetBySlug(ctx context.Context, tenant, slug string) (botsource.BotSource, error) {
	if slug == "child" {
		return botsource.BotSource{}, errors.New("temporary store outage")
	}
	return s.Store.GetBySlug(ctx, tenant, slug)
}

func TestSnapshotResumeRetriesChildAuthorityOutage(t *testing.T) {
	s, root := snapshotFixture(t)
	s.botSources = snapshotFailChildStore{Store: s.botSources}
	_, err := s.resolveResumeBot(context.Background(), "", filepath.Join(root, "parent/main.bot"))
	if !errors.Is(err, errResumeResolveTransient) {
		t.Fatalf("child store outage abandoned the resume: %v", err)
	}
}

func TestSnapshotCapturesTransitiveChildren(t *testing.T) {
	s, root := snapshotFixture(t)
	if err := os.WriteFile(filepath.Join(root, "child/main.bot"), []byte(snapshotParent("newchild")), 0o644); err != nil {
		t.Fatal(err)
	}
	lb, err := s.resolveBotSource(context.Background(), "", "parent")
	if err != nil {
		t.Fatal(err)
	}
	defer lb.Cleanup()
	snap, err := bundle.DecodeSnapshot(lb.Ref.Snapshot, lb.Ref.SnapshotDigest)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := snap.Files["newchild/main.bot"]; !ok {
		t.Fatal("transitive child was not captured")
	}
}

func snapshotFixture(t *testing.T) (*Server, string) {
	t.Helper()
	s, _, _ := newBotSourceTestServer(t)
	s.cfg.Mode = "cloud"
	root := filepath.Join(t.TempDir(), "bots")
	s.cfg.Bots.Paths = []string{root}
	for name, source := range map[string]string{"parent": snapshotParent("child"), "child": testBotMain, "newchild": testBotMain} {
		dir := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Join(dir, "skills"), 0o755); err != nil {
			t.Fatal(err)
		}
		for file, body := range map[string]string{"main.bot": source, "manifest.yaml": "name: " + name + "\n", "devbox.json": "{}", "skills/read.md": name + "-v1"} {
			if err := os.WriteFile(filepath.Join(dir, file), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	return s, root
}

func TestSnapshotCatalogLaunchAndResume(t *testing.T) {
	s, root := snapshotFixture(t)
	lb, err := s.resolveBotSource(context.Background(), "", "parent")
	if err != nil {
		t.Fatal(err)
	}
	defer lb.Cleanup()
	if lb.Ref == nil || lb.Ref.SnapshotDigest == "" || lb.Ref.TenantID != "" {
		t.Fatalf("catalog ref = %+v", lb.Ref)
	}
	snap, err := bundle.DecodeSnapshot(lb.Ref.Snapshot, lb.Ref.SnapshotDigest)
	if err != nil {
		t.Fatal(err)
	}
	if string(snap.Files["child/skills/read.md"].Content) != "child-v1" {
		t.Fatal("child skill not frozen")
	}
	if err := os.WriteFile(filepath.Join(root, "child/skills/read.md"), []byte("child-v2"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(lb.BundleDir, "../child/skills/read.md"))
	if err != nil || string(got) != "child-v1" {
		t.Fatalf("launch drifted: %q %v", got, err)
	}
	resumed, err := s.resolveResumeBot(context.Background(), "", lb.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer resumed.Cleanup()
	if resumed == nil || resumed.Ref == nil {
		t.Fatal("resume did not resolve the catalog bundle")
	}
	if resumed.Ref.SnapshotDigest == lb.Ref.SnapshotDigest {
		t.Fatal("new resume did not freeze changed resources")
	}
	// Inline force-resume must freeze ITS new child graph, not the catalog graph.
	overridden, err := s.resolveResumeBot(context.Background(), "", lb.Path, snapshotParent("newchild"))
	if err != nil {
		t.Fatal(err)
	}
	defer overridden.Cleanup()
	override, err := bundle.DecodeSnapshot(overridden.Ref.Snapshot, overridden.Ref.SnapshotDigest)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := override.Files["newchild/main.bot"]; !ok {
		t.Fatal("inline resume child missing")
	}
	if _, ok := override.Files["child/main.bot"]; ok {
		t.Fatal("inline resume kept old child graph")
	}
	collection := filepath.Dir(lb.BundleDir)
	lb.Cleanup()
	if _, err := os.Stat(collection); !os.IsNotExist(err) {
		t.Fatalf("collection not cleaned: %v", err)
	}
}

func TestSnapshotChildUsesServerAuthorityAndRefusesMissingChild(t *testing.T) {
	s, root := snapshotFixture(t)
	_, err := s.botSources.Create(store.WithTenant(context.Background(), botsource.PlatformTenantID), botsource.BotSource{
		TenantID: botsource.PlatformTenantID, Slug: "child", Files: map[string]string{"main.bot": testBotMain, "skills/read.md": "platform-child"},
	})
	if err != nil {
		t.Fatal(err)
	}
	lb, err := s.resolveBotSource(context.Background(), "", "parent")
	if err != nil {
		t.Fatal(err)
	}
	defer lb.Cleanup()
	snap, err := bundle.DecodeSnapshot(lb.Ref.Snapshot, lb.Ref.SnapshotDigest)
	if err != nil {
		t.Fatal(err)
	}
	if string(snap.Files["child/skills/read.md"].Content) != "platform-child" {
		t.Fatal("server override lost for child")
	}
	if err := os.WriteFile(filepath.Join(root, "parent/main.bot"), []byte(snapshotParent("missing")), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.resolveBotSource(context.Background(), "", "parent"); err == nil || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("missing child = %v", err)
	}
}
