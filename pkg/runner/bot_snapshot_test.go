package runner

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/bundle"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/store"
)

func TestSnapshotRunnerIgnoresDivergentCatalog(t *testing.T) {
	r, _ := subbotTestRunner(t)
	serverCatalog := t.TempDir()
	writeSubbotFixture(t, filepath.Join(serverCatalog, "parent"), "main.bot", subbotTestParent)
	writeSubbotFixture(t, filepath.Join(serverCatalog, "child"), "main.bot", subbotTestChild)
	writeSubbotFixture(t, filepath.Join(serverCatalog, "child/skills"), "contract.md", "SERVER SKILL")
	snapshot := &bundle.Snapshot{Root: "parent"}
	for _, name := range []string{"parent", "child"} {
		if err := snapshot.AddDir(name, filepath.Join(serverCatalog, name)); err != nil {
			t.Fatal(err)
		}
	}
	body, digest, err := snapshot.Encode()
	if err != nil {
		t.Fatal(err)
	}
	runnerCatalog := t.TempDir()
	writeSubbotFixture(t, filepath.Join(runnerCatalog, "child"), "main.bot", strings.ReplaceAll(subbotTestChild, "validated", "wrong_runner_version"))
	r.cfg.BotsPaths = []string{runnerCatalog}
	msg := &queue.RunMessage{RunID: "snapshot-parent", TenantID: "t1", OwnerID: "u1", BotID: "parent",
		BotBundle: &queue.BotBundleRef{Slug: "parent", Snapshot: body, SnapshotDigest: digest}}
	b, cleanup, err := r.materializeBotBundle(context.Background(), msg.BotBundle)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	run := r.subbotRunnerFor(msg, b.Dir, t.TempDir(), iterlog.Nop(), filepath.Dir(b.Dir))
	out, err := run(context.Background(), runtime.SubbotRequest{Source: "../child/main.bot", Vars: map[string]any{"ticket": "snapshot"}, ParentRunID: msg.RunID, NodeID: "child", ReattachKey: "child"})
	if err != nil {
		t.Fatal(err)
	}
	if out["validated"] != true || out["echoed"] != "snapshot" {
		t.Fatalf("wrong child executed: %v", out)
	}
	// Even with a matching runner catalog, missing snapshot content must fail.
	if err := os.Remove(filepath.Join(b.Dir, "../child/main.bot")); err != nil {
		t.Fatal(err)
	}
	_, err = run(context.Background(), runtime.SubbotRequest{Source: "../child/main.bot", ParentRunID: msg.RunID, NodeID: "missing", ReattachKey: "missing"})
	if err == nil || !strings.Contains(err.Error(), "snapshot") {
		t.Fatalf("fell back to runner catalog: %v", err)
	}
}

type snapshotBlobStore struct {
	store.RunStore
	*fakeIRBlobs
}

func TestSnapshotRunnerFetchesAndChecksStoredBytes(t *testing.T) {
	r, st := subbotTestRunner(t)
	snap := &bundle.Snapshot{Root: "parent", Files: map[string]bundle.SnapshotFile{"parent/main.bot": {Content: []byte(subbotTestChild)}}}
	body, digest, err := snap.Encode()
	if err != nil {
		t.Fatal(err)
	}
	blobs := &fakeIRBlobs{blobs: map[string][]byte{"ir/frozen.json": body}}
	r.cfg.Store = &snapshotBlobStore{RunStore: st, fakeIRBlobs: blobs}
	ref := &queue.BotBundleRef{SnapshotDigest: digest, SnapshotRef: &queue.IRRef{StorageKey: "ir/frozen.json", Backend: queue.IRBackendS3}}
	b, cleanup, err := r.materializeBotBundle(context.Background(), ref)
	if err != nil {
		t.Fatal(err)
	}
	cleanup()
	if b == nil {
		t.Fatal("stored snapshot not materialized")
	}
	blobs.blobs["ir/frozen.json"] = append(body, ' ')
	if _, _, err := r.materializeBotBundle(context.Background(), ref); err == nil || !strings.Contains(err.Error(), "digest") {
		t.Fatalf("corrupted snapshot accepted: %v", err)
	}
}
