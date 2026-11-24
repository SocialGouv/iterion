package store_test

import (
	"context"
	"github.com/SocialGouv/iterion/internal/testfs"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/store/storetest"
	"os"
	"path/filepath"
	"testing"
)

func TestTeamAliveRunsFilesystem(t *testing.T) {
	s, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	storetest.TeamAliveRuns(t, s)
}

type aliveWrappedStore struct{ store.RunStore }

func (s aliveWrappedStore) Unwrap() store.RunStore { return s.RunStore }

func TestTeamAliveRunsFilesystemUnknownIsNotDrained(t *testing.T) {
	for _, kind := range []string{"missing", "corrupt", "tombstoned", "removed"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			s, err := store.New(root)
			if err != nil {
				t.Fatal(err)
			}
			ctx := store.WithTenant(context.Background(), "team")
			run, err := s.CreateRun(ctx, "unknown", "cutover", nil)
			if err != nil {
				t.Fatal(err)
			}
			run.TenantID = "team"
			if err = s.SaveRun(ctx, run); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "missing":
				err = os.Remove(filepath.Join(root, "runs", "unknown", "run.json"))
			case "corrupt":
				err = os.WriteFile(filepath.Join(root, "runs", "unknown", "run.json"), []byte("{"), 0600)
			case "tombstoned":
				err = s.DeleteRun(ctx, "unknown")
			case "removed":
				testfs.RemoveAll(t, root, filepath.Join(root, "runs", "unknown"))
			}
			if err != nil {
				t.Fatal(err)
			}
			reader := store.AsTeamAliveRunsReader(aliveWrappedStore{s})
			if reader == nil {
				t.Fatal("capability lost through decorator")
			}
			got, err := reader.ListAliveRunsForTeam(ctx, "team")
			if kind == "missing" || kind == "corrupt" {
				if err == nil {
					t.Fatalf("unreadable run became drained: %v", got)
				}
			} else if err != nil || len(got) != 0 {
				t.Fatalf("removed run = %v,%v", got, err)
			}
		})
	}
}

func TestTeamAliveRunsFilesystemMalformedMetadataIsUnknown(t *testing.T) {
	for _, tc := range []struct {
		name, raw string
		unknown   bool
	}{
		{"empty-object", `{}`, true}, {"null", `null`, true}, {"missing-status", `{"id":"r","tenant_id":"team"}`, true}, {"unknown-status", `{"id":"r","tenant_id":"team","status":"runing"}`, true},
		{"wrong-id", `{"id":"different","tenant_id":"team","status":"running"}`, true},
		{"array-status", `{"id":"r","tenant_id":"team","status":["running","finished"]}`, true},
		{"known-running", `{"id":"r","tenant_id":"team","status":"running"}`, false}, {"known-finished", `{"id":"r","tenant_id":"team","status":"finished"}`, false}, {"known-local", `{"id":"r","status":"running"}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			s, e := store.New(root)
			if e != nil {
				t.Fatal(e)
			}
			ctx := store.WithTenant(context.Background(), "team")
			if _, e = s.CreateRun(ctx, "r", "test", nil); e != nil {
				t.Fatal(e)
			}
			if e = os.WriteFile(filepath.Join(root, "runs", "r", "run.json"), []byte(tc.raw), 0600); e != nil {
				t.Fatal(e)
			}
			ids, e := s.ListAliveRunsForTeam(ctx, "team")
			t.Logf("raw=%s ids=%q err=%v", tc.raw, ids, e)
			if tc.unknown && e == nil {
				t.Errorf("unknown metadata reported as drained: %q", ids)
			}
			if !tc.unknown && e != nil {
				t.Fatal(e)
			}
		})
	}
}
