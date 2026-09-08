package runview

import (
	"context"
	"errors"
	"os/exec"
	"reflect"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/store"
)

func TestWorkspaceCheckpointReducerKeepsLatestSuccessAcrossRunRefresh(t *testing.T) {
	run := &store.Run{ID: "orphaned", Status: store.RunStatusFailedResumable}
	b := NewSnapshotBuilder(run)
	sha := strings.Repeat("a", 40)
	b.Apply(evt(1, store.EventRunWorkspaceCheckpoint, "", "", map[string]any{"ref": "saved/first", "commit": sha}))
	first := b.Snapshot()
	b.Apply(evt(2, store.EventRunWorkspaceCheckpoint, "", "", map[string]any{"ref": "saved/latest", "commit": strings.Repeat("b", 64)}))
	want := *b.Snapshot().Run.WorkspaceCheckpoint
	for i, data := range []map[string]any{
		{"ref": "failed/push", "error": "offline"},
		{"ref": "ambiguous/push", "commit": sha, "error": "offline"},
		{"ref": "missing/sha"},
		{"ref": "invalid/sha", "commit": "not-a-commit"},
		{"ref": "invalid:ref", "commit": sha},
	} {
		b.Apply(evt(int64(i+3), store.EventRunWorkspaceCheckpoint, "", "", data))
	}
	// A stale replay and a refreshed Run document cannot erase event-derived state.
	b.Apply(evt(1, store.EventRunWorkspaceCheckpoint, "", "", map[string]any{"ref": "stale", "commit": sha}))
	b.SetRun(run)
	got := b.Snapshot()
	if !reflect.DeepEqual(got.Run.WorkspaceCheckpoint, &want) {
		t.Fatalf("latest success lost: %+v", got.Run.WorkspaceCheckpoint)
	}
	if got.Run.FinalBranch != "" || got.Run.FinalCommit != "" || got.Run.MergeStatus != "" {
		t.Fatalf("checkpoint made merge eligible: %+v", got.Run)
	}
	if first.Run.WorkspaceCheckpoint.Ref != "saved/first" {
		t.Fatal("later event mutated previous snapshot")
	}
	got.Run.WorkspaceCheckpoint.Ref = "caller mutation"
	if b.Snapshot().Run.WorkspaceCheckpoint.Ref != want.Ref {
		t.Fatal("snapshot aliases reducer state")
	}
}

// Legal Git ref characters must remain one literal shell argument. Execute
// only argument parsing, not fetch: the command substitution is a harmless
// canary that would change the recorded name if quoting were dropped.
func TestWorkspaceCheckpointFetchQuotesRecordedRef(t *testing.T) {
	for _, ref := range []string{"refs/heads/ready", "saved/$(printf-injected)'semi;colon"} {
		cp := workspaceCheckpointFromEvent(evt(1, store.EventRunWorkspaceCheckpoint, "", "", map[string]any{"ref": ref, "commit": strings.Repeat("a", 40)}))
		if cp == nil {
			t.Fatalf("valid ref rejected: %q", ref)
		}
		cmd := exec.Command("sh", "-c", "set -- "+cp.FetchCommand+"; printf '%s\n' \"$@\"") // #nosec G204 -- parse the generated recovery command without executing fetch
		out, err := cmd.CombinedOutput()
		wantRef := ref
		if !strings.HasPrefix(ref, "refs/") {
			wantRef = "refs/heads/" + ref
		}
		if want := "git\nfetch\norigin\n" + wantRef + "\n"; err != nil || string(out) != want {
			t.Fatalf("shell arguments = %q, %v; want %q", out, err, want)
		}
	}
}

type checkpointReadStore struct {
	store.RunStore
	loadErr, scanErr error
	loaded, scanned  bool
	t                *testing.T
}

func (s *checkpointReadStore) LoadRun(ctx context.Context, id string) (*store.Run, error) {
	s.loaded = true
	if tid, _ := store.TenantFromContext(ctx); tid != "tenant-a" || id != "r" {
		s.t.Fatal("load lost identity or run id")
	}
	return &store.Run{ID: id}, s.loadErr
}

func (s *checkpointReadStore) ScanEvents(ctx context.Context, id string, visit func(*store.Event) bool) error {
	s.scanned = true
	if tid, _ := store.TenantFromContext(ctx); tid != "tenant-a" || id != "r" {
		s.t.Fatal("scan lost identity or run id")
	}
	visit(evt(2, store.EventRunWorkspaceCheckpoint, "", "", map[string]any{"ref": "saved/new", "commit": strings.Repeat("b", 40)}))
	visit(evt(1, store.EventRunWorkspaceCheckpoint, "", "", map[string]any{"ref": "saved/old", "commit": strings.Repeat("a", 40)}))
	return s.scanErr
}

func TestLoadWorkspaceCheckpointIdentityAndReadFailures(t *testing.T) {
	denied := errors.New("run not visible")
	unreadable := errors.New("event store unavailable")
	for _, tc := range []struct {
		name             string
		loadErr, scanErr error
	}{
		{name: "latest sequence"}, {name: "denied", loadErr: denied}, {name: "scan failed", scanErr: unreadable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := &checkpointReadStore{t: t, loadErr: tc.loadErr, scanErr: tc.scanErr}
			cp, err := LoadWorkspaceCheckpoint(store.WithTenant(context.Background(), "tenant-a"), st, "r")
			if !st.loaded || st.scanned != (tc.loadErr == nil) {
				t.Fatal("ownership was not checked before scanning")
			}
			wantErr := tc.loadErr
			if wantErr == nil {
				wantErr = tc.scanErr
			}
			if !errors.Is(err, wantErr) {
				t.Fatalf("error = %v, want %v", err, wantErr)
			}
			if wantErr != nil {
				if cp != nil {
					t.Fatalf("partial scan masquerades as latest checkpoint: %+v", cp)
				}
			} else if cp == nil || cp.Ref != "saved/new" {
				t.Fatalf("latest sequence lost: %+v", cp)
			}
		})
	}
}
