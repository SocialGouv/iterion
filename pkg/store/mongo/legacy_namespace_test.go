package mongo

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/internal/mongotest"
	"github.com/SocialGouv/iterion/pkg/internal/s3test"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/store/blob"
	"github.com/SocialGouv/iterion/pkg/store/storetest"
	"go.mongodb.org/mongo-driver/v2/bson"
)

func nativeMongoRecords(t *testing.T, ctx context.Context, s *Store) map[string][]string {
	t.Helper()
	names, err := s.db.ListCollectionNames(ctx, bson.M{})
	if err != nil {
		t.Fatal(err)
	}
	out := map[string][]string{}
	for _, name := range names {
		if !strings.HasSuffix(name, "_ports_v1") {
			continue
		}
		cursor, err := s.db.Collection(name).Find(ctx, bson.M{})
		if err != nil {
			t.Fatal(err)
		}
		var records []bson.Raw
		if err := cursor.All(ctx, &records); err != nil {
			t.Fatal(err)
		}
		values := make([]string, 0, len(records))
		for _, record := range records {
			values = append(values, fmt.Sprintf("%x", []byte(record)))
		}
		sort.Strings(values)
		out[name] = values
	}
	return out
}

func TestNativeOldMongoAndS3ExecutablesCannotMutateNativeClosure(t *testing.T) {
	probe := storetest.LegacyTool(t, "ITERION_TEST_LEGACY_PROBE")
	s := nativeNamespaceStore(t)
	base, cancel := mongotest.Ctx(t)
	defer cancel()
	ctx := store.WithRuntimeSemantics(store.WithoutTenantFilter(base), store.RuntimeSemanticsPortsV1)
	gateway, server := s3test.New(t, "native-compatibility")
	objects, err := blob.NewS3(ctx, blob.Config{Endpoint: server.URL, Region: "us-east-1", Bucket: "native-compatibility", UsePathStyle: true, AccessKeyID: "test-key", SecretAccessKey: "test-secret"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = objects.Close() })
	s.blob = objects
	const id = "pc1_old_cloud_tools"
	if _, err := s.CreateRun(ctx, id, "native-protected", map[string]any{"value": "native"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateChildRun(store.WithRuntimeSemantics(ctx, store.RuntimeSemanticsLegacyAdapterV1), id+"_child", "adapter", id, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.WriteArtifact(ctx, &store.Artifact{RunID: id, NodeID: "node", Version: 1, Data: map[string]any{"value": "native"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.WriteInteraction(ctx, &store.Interaction{ID: "question", RunID: id, NodeID: "node"}); err != nil {
		t.Fatal(err)
	}
	if err := s.WriteAttachment(ctx, id, store.AttachmentRecord{Name: "input", OriginalFilename: "input.txt"}, bytes.NewReader([]byte("native attachment"))); err != nil {
		t.Fatal(err)
	}
	if _, err := s.WriteToolBlob(ctx, id, "call", "output", []byte("native tool")); err != nil {
		t.Fatal(err)
	}
	if err := s.PutBackendSession(ctx, id, "session", []byte("native session")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutIRBlob(ctx, id, []byte("native IR")); err != nil {
		t.Fatal(err)
	}
	if err := objects.PutRunFile(ctx, id, "product.txt", "text/plain", bytes.NewReader([]byte("native product")), 14); err != nil {
		t.Fatal(err)
	}
	recordsBefore := nativeMongoRecords(t, ctx, s)
	protectedObjects := func() map[string][]byte {
		out := map[string][]byte{}
		for key, value := range gateway.Snapshot() {
			if strings.HasPrefix(key, "ports-v1/") {
				out[key] = value
			}
		}
		return out
	}
	objectsBefore := protectedObjects()
	if len(objectsBefore) != 6 {
		t.Fatalf("fixture did not seed all native blob families: %v", gateway.Keys())
	}
	args := []string{"-root", t.TempDir(), "-mongo", os.Getenv("ITERION_TEST_MONGO_URI"), "-database", s.db.Name(), "-s3", server.URL, "-bucket", "native-compatibility", "-id", id}
	workspace := t.TempDir()
	read := storetest.RunLegacyTool(t, probe, workspace, false, append(args, "-action", "read")...)
	if !strings.Contains(read, "not found") {
		t.Fatalf("old reader did not reach native-record lookup: %s", read)
	}
	list := storetest.RunLegacyTool(t, probe, workspace, true, append(args, "-action", "list")...)
	if strings.Contains(list, id) {
		t.Fatalf("old list exposed native ID: %s", list)
	}
	for _, action := range []string{"delete", "prune", "write", "repair", "delete", "prune"} {
		storetest.RunLegacyTool(t, probe, workspace, true, append(args, "-action", action)...)
		if got := nativeMongoRecords(t, ctx, s); !reflect.DeepEqual(got, recordsBefore) {
			t.Fatalf("old %s changed native Mongo records", action)
		}
		if got := protectedObjects(); !reflect.DeepEqual(got, objectsBefore) {
			t.Fatalf("old %s changed native S3 bodies", action)
		}
	}
	if r, err := s.LoadRun(ctx, id); err != nil || r.WorkflowName != "native-protected" {
		t.Fatalf("native identity changed: %+v, %v", r, err)
	}
}
