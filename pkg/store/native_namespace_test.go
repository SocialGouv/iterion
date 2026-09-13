package store_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/store/storetest"
)

func TestNativeNamespacesFilesystem(t *testing.T) {
	storetest.RunNativeNamespaces(t, func(t *testing.T) store.RunStore {
		s, err := store.New(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		return s
	})
}

func TestNativeExecutionStateFilesystem(t *testing.T) {
	storetest.RunPortExecutionState(t, func(t *testing.T) store.RunStore {
		s, err := store.New(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		return s
	})
}

func TestNativeFileCaptureFilesystem(t *testing.T) {
	storetest.RunPortFiles(t, func(t *testing.T) store.RunStore {
		s, err := store.New(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		return s
	})
}

func TestNativeFilesystemRefusesUnsupportedRecordBeforeMutation(t *testing.T) {
	root := t.TempDir()
	s, err := store.New(root)
	if err != nil {
		t.Fatal(err)
	}
	ctx := store.WithRuntimeSemantics(context.Background(), store.RuntimeSemanticsPortsV1)
	const id = "pc1_future"
	r, err := s.CreateRun(ctx, id, "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	r.FormatVersion++
	data, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, store.NativeRunsDirectory, id, "run.ports-v1.json")
	if err := os.WriteFile(path, data, 0600); err != nil {
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
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("unsupported record mutated: %v", err)
	}
	for _, name := range []string{"events.jsonl", "artifacts", ".deleted"} {
		if _, err := os.Stat(filepath.Join(filepath.Dir(path), name)); !os.IsNotExist(err) {
			t.Errorf("operation created %s: %v", name, err)
		}
	}
}

func TestNativeFilesystemNeverFallsBackToLegacyShadow(t *testing.T) {
	root := t.TempDir()
	s, err := store.New(root)
	if err != nil {
		t.Fatal(err)
	}
	const id = "pc1_shadow"
	ctx := store.WithRuntimeSemantics(context.Background(), store.RuntimeSemanticsPortsV1)
	if _, err := s.CreateRun(ctx, id, "native", nil); err != nil {
		t.Fatal(err)
	}
	legacyDir := filepath.Join(root, "runs", id)
	if err := os.MkdirAll(legacyDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacyDir, "run.json"), []byte(`{"id":"pc1_shadow","workflow_name":"shadow","format_version":1}`), 0600); err != nil {
		t.Fatal(err)
	}
	if ids, err := s.ListRuns(ctx); err != nil || !reflect.DeepEqual(ids, []string{id}) {
		t.Fatalf("shadow enumeration: %v, %v", ids, err)
	}
	if err := os.Remove(filepath.Join(root, store.NativeRunsDirectory, id, "run.ports-v1.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.LoadRun(ctx, id); !errors.Is(err, store.ErrRunNotFound) {
		t.Fatalf("native absence fell back: %v", err)
	}
	if ids, err := s.ListRuns(ctx); err != nil || len(ids) != 0 {
		t.Fatalf("shadow became authoritative: %v, %v", ids, err)
	}
}
