package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
)

// An orphaned pod can leave no final bank or baseline but a successful
// checkpoint push on its timeline. Both normal inspection surfaces must name it.
func TestRunWorkspaceCheckpointRecovery(t *testing.T) {
	for _, known := range []bool{false, true} {
		t.Run(map[bool]string{false: "no_success", true: "persisted_checkpoint"}[known], func(t *testing.T) {
			srv, hs := newTestServer(t)
			st := srv.runs.RunStore()
			ctx := context.Background()
			run, err := st.CreateRun(ctx, "orphaned", "wf", nil)
			if err != nil {
				t.Fatal(err)
			}
			run.Status = store.RunStatusFailedResumable
			run.FailureCode = store.FailureProcessOrphaned
			run.ContinuationState = store.ContinuationFinal
			run.WorkDir = "/missing/runner-pod/workspace"
			if err := st.SaveRun(ctx, run); err != nil {
				t.Fatal(err)
			}
			var success *store.Event
			sha := strings.Repeat("b", 40)
			// A deliberately non-conventional name proves we read the event.
			ref := "recovery/operator-selected-checkpoint"
			if known {
				if _, err := st.AppendEvent(ctx, run.ID, store.Event{Type: store.EventRunWorkspaceCheckpoint,
					Data: map[string]any{"ref": "recovery/older", "commit": strings.Repeat("a", 40)}}); err != nil {
					t.Fatal(err)
				}
				success, err = st.AppendEvent(ctx, run.ID, store.Event{Type: store.EventRunWorkspaceCheckpoint,
					Data: map[string]any{"ref": ref, "commit": sha}})
				if err != nil {
					t.Fatal(err)
				}
			}
			// A failed later push is not evidence that its ref holds work.
			if _, err := st.AppendEvent(ctx, run.ID, store.Event{Type: store.EventRunWorkspaceCheckpoint,
				Data: map[string]any{"ref": "recovery/failed", "error": "push failed"}}); err != nil {
				t.Fatal(err)
			}
			var recovery map[string]any
			for _, suffix := range []string{"", "/commits"} {
				resp, err := http.Get(hs.URL + "/api/runs/" + run.ID + suffix)
				if err != nil {
					t.Fatal(err)
				}
				if resp.StatusCode != http.StatusOK {
					t.Fatalf("%s status = %d", suffix, resp.StatusCode)
				}
				var out map[string]any
				decodeJSONResp(t, resp, &out)
				if suffix == "" {
					out = out["run"].(map[string]any)
					if out["final_branch"] != nil || out["final_commit"] != nil {
						t.Fatalf("checkpoint became final bank: %v", out)
					}
					if out["status"] != "failed_resumable" {
						t.Fatalf("status changed: %v", out)
					}
				} else {
					if out["available"] != false || out["reason"] != "no_baseline" || len(out["commits"].([]any)) != 0 {
						t.Fatalf("no-baseline contract changed: %v", out)
					}
				}
				raw := out["workspace_checkpoint"]
				if !known {
					if raw != nil {
						t.Fatalf("invented checkpoint: %v", raw)
					}
					continue
				}
				cp, ok := raw.(map[string]any)
				if !ok {
					t.Fatalf("%s: persisted checkpoint missing: %v", suffix, out)
				}
				if cp["ref"] != ref || cp["commit"] != sha || cp["source"] != "run_workspace_checkpoint" || cp["event_seq"] != float64(success.Seq) {
					t.Fatalf("wrong provenance: %v", cp)
				}
				var at string
				encoded, _ := json.Marshal(success.Timestamp)
				if err := json.Unmarshal(encoded, &at); err != nil {
					t.Fatal(err)
				}
				if cp["recorded_at"] != at || cp["fetch_command"] != "git fetch origin refs/heads/"+ref || !strings.Contains(cp["warning"].(string), "validate before merging") {
					t.Fatalf("missing recovery instructions: %v", cp)
				}
				if recovery != nil && !reflect.DeepEqual(recovery, cp) {
					t.Fatalf("inspection/commits disagree: %v != %v", recovery, cp)
				}
				recovery = cp
			}
		})
	}
}

type recoveryReadStore struct {
	store.RunStore
	scanErr error
	scans   int
}

func (s *recoveryReadStore) LoadRun(ctx context.Context, id string) (*store.Run, error) {
	if tid, _ := store.TenantFromContext(ctx); tid != "tenant-a" {
		return nil, store.ErrRunNotFound
	}
	return s.RunStore.LoadRun(ctx, id)
}

func (s *recoveryReadStore) ScanEvents(ctx context.Context, id string, visit func(*store.Event) bool) error {
	s.scans++
	if tid, _ := store.TenantFromContext(ctx); tid != "tenant-a" {
		return store.ErrRunNotFound
	}
	if s.scanErr != nil {
		return s.scanErr
	}
	return s.RunStore.ScanEvents(ctx, id, visit)
}

func TestRunCommitsRecoveryReadBoundary(t *testing.T) {
	srv, _ := newTestServer(t)
	original := srv.runs
	t.Cleanup(func() { srv.runs = original })
	st := original.RunStore()
	run, err := st.CreateRun(context.Background(), "saved", "wf", nil)
	if err != nil {
		t.Fatal(err)
	}
	run.WorkDir = "/missing/pod"
	if err := st.SaveRun(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AppendEvent(context.Background(), run.ID, store.Event{Type: store.EventRunWorkspaceCheckpoint,
		Data: map[string]any{"ref": "saved/work", "commit": strings.Repeat("a", 40)}}); err != nil {
		t.Fatal(err)
	}
	guarded := &recoveryReadStore{RunStore: st}
	srv.runs = newTestRunviewService(t, srv.cfg.StoreDir, runview.WithStore(guarded))
	for _, tc := range []struct {
		name, tenant string
		scanErr      error
		status       int
		wantScan     bool
	}{
		{name: "owner sees saved work", tenant: "tenant-a", status: 200, wantScan: true},
		{name: "another tenant cannot discover it", tenant: "tenant-b", status: 404},
		{name: "unreadable is not absent", tenant: "tenant-a", scanErr: errors.New("event store offline"), status: 500, wantScan: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			guarded.scanErr = tc.scanErr
			guarded.scans = 0
			req := httptest.NewRequest(http.MethodGet, "/api/runs/saved/commits", nil)
			req.SetPathValue("id", "saved")
			req = req.WithContext(store.WithTenant(req.Context(), tc.tenant))
			rec := httptest.NewRecorder()
			srv.handleListRunCommits(rec, req)
			if rec.Code != tc.status {
				t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
			}
			if (guarded.scans > 0) != tc.wantScan {
				t.Fatalf("event scans = %d, want scan %t", guarded.scans, tc.wantScan)
			}
			if tc.status == http.StatusOK {
				if !strings.Contains(rec.Body.String(), `"ref":"saved/work"`) {
					t.Fatalf("successful control missing recovery: %s", rec.Body.String())
				}
			} else if strings.Contains(rec.Body.String(), "saved/work") {
				t.Fatalf("recovery leaked through failed read: %s", rec.Body.String())
			}
		})
	}
}
