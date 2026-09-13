package store_test

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/gittest"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/store/storetest"
)

func nativeFileDigests(t *testing.T, root string) map[string]string {
	t.Helper()
	files := map[string]string{}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		files[filepath.ToSlash(rel)] = fmt.Sprintf("%x", sha256.Sum256(data))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func TestNativeOldFilesystemExecutablesCannotMutateNativeClosure(t *testing.T) {
	probe := storetest.LegacyTool(t, "ITERION_TEST_LEGACY_PROBE")
	cli := storetest.LegacyTool(t, "ITERION_TEST_LEGACY_BINARY")
	workspace := gittest.SourceRepo(t)
	root := t.TempDir()
	s, err := store.New(root)
	if err != nil {
		t.Fatal(err)
	}
	ctx := store.WithRuntimeSemantics(context.Background(), store.RuntimeSemanticsPortsV1)
	const id = "pc1_old_tools"
	if _, err := s.CreateRun(ctx, id, "native-protected", map[string]any{"value": "native"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateChildRun(store.WithRuntimeSemantics(ctx, store.RuntimeSemanticsLegacyAdapterV1), id+"_child", "adapter", id, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendEvent(ctx, id, store.Event{Type: store.EventNodeStarted}); err != nil {
		t.Fatal(err)
	}
	if err := s.WriteArtifact(ctx, &store.Artifact{RunID: id, NodeID: "node", Version: 1, Data: map[string]any{"value": "native artifact"}}); err != nil {
		t.Fatal(err)
	}
	if err := store.AsBackendSessionStore(s).PutBackendSession(ctx, id, "session", []byte("native conversation")); err != nil {
		t.Fatal(err)
	}
	dir, err := store.AsRunFilesStore(s).EnsureRunFilesDir(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "product.txt"), []byte("native product"), 0600); err != nil {
		t.Fatal(err)
	}
	nativeRoot := filepath.Join(root, store.NativeRunsDirectory)
	before := nativeFileDigests(t, nativeRoot)
	assertUnchanged := func() {
		t.Helper()
		if got := nativeFileDigests(t, nativeRoot); !reflect.DeepEqual(got, before) {
			t.Fatalf("old operation changed native closure: before=%v after=%v", before, got)
		}
	}
	// Actual CLI read/restore/fork entry points must fail because the old
	// reader cannot resolve a native record through its legacy namespace.
	for _, args := range [][]string{
		{"inspect", "--run-id", id},
		{"fork", "--run-id", id, "--node", "node"},
		{"rewind", "--run-id", id, "--node", "node", "--restore-scope", "none"},
	} {
		out := storetest.RunLegacyTool(t, cli, workspace, false, append(args, "--store-dir", root)...)
		if !strings.Contains(out, id) || (!strings.Contains(out, "not found") && !strings.Contains(out, "no such file") && !strings.Contains(out, "cannot load run")) {
			t.Fatalf("old CLI did not reach the missing-run check: %v\n%s", args, out)
		}
		assertUnchanged()
	}
	list := storetest.RunLegacyTool(t, probe, workspace, true, "-root", root, "-action", "list")
	if strings.Contains(list, id) {
		t.Fatalf("old list exposed native identity: %s", list)
	}
	// Old writers are allowed to create a legacy shadow with the same text
	// ID. Every actual old API still stays outside the protected namespace.
	for _, action := range []string{"delete", "prune", "write", "repair", "delete", "prune"} {
		storetest.RunLegacyTool(t, probe, workspace, true, "-root", root, "-id", id, "-action", action)
		assertUnchanged()
	}
	storetest.RunLegacyTool(t, cli, workspace, true, "runs", "prune", "--store-dir", root, "--older-than", "0s", "--keep-last", "0")
	assertUnchanged()
	if r, err := s.LoadRun(ctx, id); err != nil || r.WorkflowName != "native-protected" {
		t.Fatalf("native identity changed: %+v, %v", r, err)
	}
}
