package cloudpublisher

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/queue"
)

func TestSnapshotOffloadIsImmutableAcrossResume(t *testing.T) {
	st := &fakeIRStore{}
	p := testPublisher(st, 4096)
	var refs []string
	for _, revision := range []string{"old", "new"} {
		snap := &bundle.Snapshot{Root: "parent", Files: map[string]bundle.SnapshotFile{"parent/main.bot": {Content: []byte(strings.Repeat(revision, 2000))}}}
		body, digest, err := snap.Encode()
		if err != nil {
			t.Fatal(err)
		}
		msg := &queue.RunMessage{V: queue.SchemaVersion, RunID: "same-run", WorkflowName: "parent", IRCompiled: json.RawMessage(`{"workflows":[]}`), BotBundle: &queue.BotBundleRef{Snapshot: body, SnapshotDigest: digest}}
		if err := p.offloadBundleSnapshot(context.Background(), msg); err != nil {
			t.Fatal(err)
		}
		if msg.BotBundle.SnapshotRef == nil || len(msg.BotBundle.Snapshot) > 0 {
			t.Fatal("large snapshot not offloaded")
		}
		if err := msg.Validate(); err != nil {
			t.Fatal(err)
		}
		encoded, err := json.Marshal(msg)
		if err != nil || len(encoded) > 4096 {
			t.Fatalf("message still exceeds payload: %d %v", len(encoded), err)
		}
		key := msg.BotBundle.SnapshotRef.StorageKey
		got, err := st.GetIRBlob(context.Background(), key)
		if err != nil || !bytes.Equal(got, body) {
			t.Fatal("snapshot changed on transport")
		}
		refs = append(refs, key)
	}
	if refs[0] == refs[1] || len(st.blobs) != 2 {
		t.Fatal("resume overwrote earlier snapshot")
	}
}

func TestSnapshotPromptsTravelInCompiledPayload(t *testing.T) {
	dir := t.TempDir()
	source := "workflow main:\n  entry: done\n"
	if err := os.MkdirAll(filepath.Join(dir, "prompts"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.bot"), []byte(source), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "prompts/contract.md"), []byte("snapshotted prompt"), 0o644); err != nil {
		t.Fatal(err)
	}
	body, err := marshalIRFromSpec("bots/probe/main.bot", source, dir)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(body, []byte("snapshotted prompt")) {
		t.Fatal("bundle prompt missing from serialized AST")
	}
}
