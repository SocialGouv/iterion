package mongo

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/internal/mongotest"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/store/storetest"
	"go.mongodb.org/mongo-driver/v2/bson"
)

func nativeNamespaceStore(t *testing.T) *Store {
	t.Helper()
	uri := os.Getenv("ITERION_TEST_MONGO_URI")
	if uri == "" {
		if os.Getenv("ITERION_TEST_REQUIRED") == "1" {
			t.Fatal("required native Mongo tests need ITERION_TEST_MONGO_URI and a writable replica set")
		}
		t.Skip("ITERION_TEST_MONGO_URI not set")
	}
	ctx, cancel := mongotest.Ctx(t)
	defer cancel()
	scratch := t.TempDir()
	t.Cleanup(func() {
		if err := os.RemoveAll(scratch + "-ports-v1"); err != nil {
			t.Error(err)
		}
		if err := os.RemoveAll(scratch + "-ports-v1-publications"); err != nil {
			t.Error(err)
		}
	})
	s, err := New(ctx, Config{URI: uri, Database: "iterion_native_" + bsonNonce(t), Blob: newInMemoryBlob(), RunFilesScratchDir: scratch})
	if err != nil {
		t.Fatal(err)
	}
	var hello struct {
		Writable bool   `bson:"isWritablePrimary"`
		SetName  string `bson:"setName"`
	}
	if err := s.db.RunCommand(ctx, bson.D{{Key: "hello", Value: 1}}).Decode(&hello); err != nil || !hello.Writable || hello.SetName == "" {
		t.Fatalf("native tests require a writable replica set: %+v, %v", hello, err)
	}
	t.Cleanup(func() { ctx, cancel := mongotest.TeardownCtx(); defer cancel(); _ = s.db.Drop(ctx); _ = s.Close(ctx) })
	return s
}

// Legacy run IDs are not reserved words. A run called "ports-v1" can use
// every supported old scratch operation without reaching native scratch.
func TestNativeMongoScratchCannotBeSweptByLegacyRunName(t *testing.T) {
	s := nativeNamespaceStore(t)
	base, cancel := mongotest.Ctx(t)
	defer cancel()
	ctx := store.WithoutTenantFilter(base)
	const nativeID, legacyID = "pc1_scratch", "ports-v1"
	if _, err := s.CreateRun(store.WithRuntimeSemantics(ctx, store.RuntimeSemanticsPortsV1), nativeID, "fixture", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateRun(ctx, legacyID, "fixture", nil); err != nil {
		t.Fatal(err)
	}
	nativeDir, err := s.EnsureRunFilesDir(ctx, nativeID)
	if err != nil {
		t.Fatal(err)
	}
	legacyDir, err := s.EnsureRunFilesDir(ctx, legacyID)
	if err != nil {
		t.Fatal(err)
	}
	if strings.HasPrefix(nativeDir, legacyDir+string(os.PathSeparator)) {
		t.Fatalf("native scratch %q is inside a legacy run's scratch %q", nativeDir, legacyDir)
	}
	if err := os.WriteFile(filepath.Join(nativeDir, "native.txt"), []byte("native survives"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacyDir, "legacy.txt"), []byte("legacy"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UploadRunFiles(ctx, legacyID); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteRun(ctx, legacyID); err != nil {
		t.Fatal(err)
	}
	if content, err := os.ReadFile(filepath.Join(nativeDir, "native.txt")); err != nil || string(content) != "native survives" {
		t.Fatalf("legacy scratch operations changed native bytes: %q, %v", content, err)
	}
	if _, err := s.LoadRun(ctx, nativeID); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteRun(ctx, nativeID); err != nil {
		t.Fatal(err)
	}
}

func TestNativeExecutionStateMongo(t *testing.T) {
	storetest.RunPortExecutionState(t, func(t *testing.T) store.RunStore { return nativeNamespaceStore(t) })
}

func TestNativeFileCaptureMongo(t *testing.T) {
	storetest.RunPortFiles(t, func(t *testing.T) store.RunStore { return nativeNamespaceStore(t) })
}

func TestNativeMongoRefusesUnsupportedRecordBeforeMutation(t *testing.T) {
	s := nativeNamespaceStore(t)
	base, cancel := mongotest.Ctx(t)
	defer cancel()
	ctx := store.WithRuntimeSemantics(store.WithoutTenantFilter(base), store.RuntimeSemanticsPortsV1)
	const id = "pc1_future"
	if _, err := s.CreateRun(ctx, id, "test", nil); err != nil {
		t.Fatal(err)
	}
	coll := s.collectionForRun(id, s.runs)
	if _, err := coll.UpdateOne(ctx, bson.M{"_id": id}, bson.M{"$set": bson.M{"format_version": store.NativeRunFormatVersion + 1}}); err != nil {
		t.Fatal(err)
	}
	checks := []func() error{
		func() error { _, err := s.LoadRun(ctx, id); return err },
		func() error { _, err := s.EnsureRunFilesDir(ctx, id); return err },
		func() error { _, err := s.AppendEvent(ctx, id, store.Event{Type: store.EventNodeStarted}); return err },
		func() error { return s.WriteArtifact(ctx, &store.Artifact{RunID: id, NodeID: "node", Version: 1}) },
		func() error { return s.UpdateRunStatus(ctx, id, store.RunStatusFinished, "") },
		func() error { return s.DeleteRun(ctx, id) },
	}
	for i, check := range checks {
		if err := check(); !errors.Is(err, store.ErrRunSemantics) {
			t.Errorf("operation %d: %v", i, err)
		}
	}
	if n, err := s.collectionForRun(id, s.events).CountDocuments(ctx, bson.M{}); err != nil || n != 0 {
		t.Fatalf("unsupported event mutation: %d, %v", n, err)
	}
	if len(s.blob.(*inMemoryBlob).data) != 0 {
		t.Fatal("unsupported blob mutation")
	}
	var r store.Run
	if err := coll.FindOne(ctx, bson.M{"_id": id}).Decode(&r); err != nil || r.FormatVersion != store.NativeRunFormatVersion+1 || r.DeletedAt != nil || r.Status != store.RunStatusQueued {
		t.Fatalf("unsupported record changed: %+v, %v", r, err)
	}
}

func TestNativeMongoDeleteRemovesSequenceAndIRClosure(t *testing.T) {
	s := nativeNamespaceStore(t)
	base, cancel := mongotest.Ctx(t)
	defer cancel()
	ctx := store.WithRuntimeSemantics(store.WithIdentity(base, "tenant", "owner"), store.RuntimeSemanticsPortsV1)
	const id = "pc1_delete"
	if _, err := s.CreateRun(ctx, id, "test", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendEvent(ctx, id, store.Event{Type: store.EventNodeStarted}); err != nil {
		t.Fatal(err)
	}
	key, err := s.PutIRBlob(ctx, id, []byte(`{"workflow":"native"}`))
	if err != nil || !strings.HasPrefix(key, "ports-v1/") {
		t.Fatalf("native IR: %s, %v", key, err)
	}
	if err := s.DeleteRun(ctx, id); err != nil {
		t.Fatal(err)
	}
	if n, err := s.collectionForRun(id, s.runSeq).CountDocuments(ctx, bson.M{}); err != nil || n != 0 {
		t.Fatalf("sequence closure survived: %d, %v", n, err)
	}
	if len(s.blob.(*inMemoryBlob).data) != 0 {
		t.Fatalf("IR blob survived: %v", s.blob.(*inMemoryBlob).data)
	}
	if _, err := s.GetIRBlob(ctx, key); err == nil {
		t.Fatal("IR remains readable after deletion")
	}
}

func TestNativeNamespacesMongo(t *testing.T) {
	storetest.RunNativeNamespaces(t, func(t *testing.T) store.RunStore { return nativeNamespaceStore(t) })
}

func TestNativeMongoNeverFallsBackToLegacyShadow(t *testing.T) {
	s := nativeNamespaceStore(t)
	base, cancel := mongotest.Ctx(t)
	defer cancel()
	ctx := store.WithRuntimeSemantics(store.WithoutTenantFilter(base), store.RuntimeSemanticsPortsV1)
	const id = "pc1_shadow"
	if _, err := s.CreateRun(ctx, id, "native", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.runs.InsertOne(ctx, bson.M{"_id": id, "workflow_name": "legacy shadow", "format_version": 1}); err != nil {
		t.Fatal(err)
	}
	if ids, err := s.ListRuns(ctx); err != nil || !reflect.DeepEqual(ids, []string{id}) {
		t.Fatalf("shadow enumeration: %v, %v", ids, err)
	}
	if _, err := s.collectionForRun(id, s.runs).DeleteOne(ctx, bson.M{"_id": id}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.LoadRun(ctx, id); !errors.Is(err, store.ErrRunNotFound) {
		t.Fatalf("native absence fell back: %v", err)
	}
	if ids, err := s.ListRuns(ctx); err != nil || len(ids) != 0 {
		t.Fatalf("shadow became authoritative: %v, %v", ids, err)
	}
}
