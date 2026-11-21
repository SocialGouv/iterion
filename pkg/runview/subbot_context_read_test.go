package runview

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/store"
)

// The reattach read succeeds; only the later context-inheritance read fails.
type parentContextReadStore struct {
	store.RunStore
	parentID    string
	parentReads int
	injected    bool
	readErr     error
}

func (s *parentContextReadStore) Unwrap() store.RunStore { return s.RunStore }
func (s *parentContextReadStore) LoadRun(ctx context.Context, id string) (*store.Run, error) {
	if id == s.parentID {
		s.parentReads++
		if s.parentReads == 2 {
			s.injected = true
			return nil, s.readErr
		}
	}
	return s.RunStore.LoadRun(ctx, id)
}

func TestSubbotParentContextReadFailure(t *testing.T) {
	for _, tc := range []struct {
		name          string
		readErr       error
		legacyMissing bool
	}{
		{name: "transient", readErr: errors.New("fixture parent read unavailable")},
		{name: "cancelled", readErr: context.Canceled},
		{name: "missing", readErr: store.ErrRunNotFound, legacyMissing: true},
		{name: "wrapped-missing", readErr: fmt.Errorf("fixture: %w", store.ErrRunNotFound), legacyMissing: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("ITERION_RELIABILITY_MODE", "legacy")
			t.Chdir(t.TempDir())
			svc, childPath := contextLaunchService(t, false)
			dir := filepath.Dir(childPath)
			base := svc.store
			parent, err := base.CreateRun(t.Context(), "parent-context-read", "parent", nil)
			if err != nil {
				t.Fatal(err)
			}
			parent.ExecutionContext = &store.ExecutionContext{
				Policy:         store.ContextPolicyEnforce,
				BusinessStores: []store.ContextRef{{ID: "ledger", Kind: "postgres", Namespace: "team", Required: true}},
			}
			if err := base.SaveRun(t.Context(), parent); err != nil {
				t.Fatal(err)
			}
			wrapped := &parentContextReadStore{RunStore: base, parentID: parent.ID, readErr: tc.readErr}
			svc.store = wrapped
			run := svc.subbotRunnerFor(filepath.Join(dir, "parent.bot"), iterlog.Nop())
			_, runErr := run(boundLegacySubbotFixture(t, t.Context(), parent.ID), runtime.SubbotRequest{Source: "child.bot", ParentRunID: parent.ID, NodeID: "child", ReattachKey: "child", WorkDir: dir})
			if !wrapped.injected {
				t.Fatalf("PARENT_READ_ORACLE_NOT_REACHED: %v", runErr)
			}
			ids, err := base.ListRuns(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if tc.legacyMissing {
				if runErr != nil || len(ids) != 2 {
					t.Fatalf("legacy missing parent refused: err=%v runs=%v", runErr, ids)
				}
				return
			}
			if !errors.Is(runErr, tc.readErr) || len(ids) != 1 {
				t.Fatalf("parent context read failure must preserve cause and refuse before child: err=%v runs=%v", runErr, ids)
			}
			reloaded, err := base.LoadRun(t.Context(), parent.ID)
			if err != nil {
				t.Fatal(err)
			}
			if len(reloaded.SubbotChildren) != 0 {
				t.Fatalf("failed parent read left child bindings: %v", reloaded.SubbotChildren)
			}
			if active := svc.manager.ActiveRuns(); len(active) != 0 {
				t.Fatalf("failed parent read registered child: %v", active)
			}
		})
	}
}
