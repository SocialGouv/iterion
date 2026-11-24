package server

import (
	"context"
	"errors"
	"github.com/SocialGouv/iterion/pkg/audit"
	"github.com/SocialGouv/iterion/pkg/identity"
	"github.com/SocialGouv/iterion/pkg/store"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

type cutoverRuns struct {
	store.RunStore
	ids    []string
	err    error
	calls  int
	tenant string
}

func (s *cutoverRuns) ListAliveRunsForTeam(ctx context.Context, team string) ([]string, error) {
	s.calls++
	s.tenant, _ = store.TenantFromContext(ctx)
	if team != "t1" {
		return nil, errors.New("wrong target team")
	}
	return s.ids, s.err
}

func TestAdminTeamCutover(t *testing.T) {
	for _, tc := range []struct{ name, previous, next string }{{"map", "", "alpha"}, {"unmap", "alpha", ""}, {"repoint", "alpha", "beta"}} {
		for _, force := range []bool{false, true} {
			t.Run(tc.name+map[bool]string{false: "/backstop", true: "/forced"}[force], func(t *testing.T) {
				s, _, done := newPoolMappingTestServer(t)
				defer done()
				if _, err := s.authStore().PatchTeam(context.Background(), "t1", identity.TeamPatch{RunnerPool: &tc.previous}); err != nil {
					t.Fatal(err)
				}
				s.auditStore = audit.NewMemoryStore()
				runs := &cutoverRuns{RunStore: s.cfg.Store, ids: []string{"queued-a", "running-b"}}
				s.cfg.Store = runs
				url := "/api/admin/teams/t1/runner-pool"
				if force {
					url += "?force=1"
				}
				req := orgReq(superAdminCtx(), http.MethodPut, url, `{"runner_pool":"`+tc.next+`"}`, "t1")
				w := httptest.NewRecorder()
				s.handleAdminSetTeamRunnerPool(w, req)
				wantStatus := http.StatusConflict
				wantPool := tc.previous
				if force {
					wantStatus = http.StatusOK
					wantPool = tc.next
				}
				if w.Code != wantStatus {
					t.Fatalf("cutover status=%d body=%s", w.Code, w.Body.String())
				}
				if !force {
					for _, want := range []string{"2", "queued-a", "running-b", "draining", "resumes", "force=1"} {
						if !strings.Contains(w.Body.String(), want) {
							t.Errorf("refusal omits %q: %s", want, w.Body.String())
						}
					}
				}
				if runs.calls != 1 || runs.tenant != "t1" {
					t.Fatalf("alive read calls=%d tenant=%q", runs.calls, runs.tenant)
				}
				team, err := s.authStore().GetTeam(context.Background(), "t1")
				if err != nil || team.RunnerPool != wantPool {
					t.Fatalf("mapping: %+v %v", team, err)
				}
				if force {
					assertCutoverAudit(t, s, tc.previous, tc.next, true, runs.ids)
				}
			})
		}
	}
}

func assertCutoverAudit(t *testing.T, s *Server, previous, next string, forced bool, alive []string) {
	t.Helper()
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		ready := true
		for _, scope := range []string{"t1", "o1"} {
			rows, err := s.auditStore.ListByTenant(context.Background(), scope, audit.Page{Action: "team.runner_pool_set"})
			if err != nil {
				t.Fatal(err)
			}
			if len(rows) == 0 {
				ready = false
				continue
			}
			if len(rows) != 1 {
				t.Fatalf("audit %s has %d rows", scope, len(rows))
			}
			m := rows[0].Meta
			if m["previous"] != previous || m["runner_pool"] != next || m["forced"] != forced || !reflect.DeepEqual(m["alive_runs"], alive) {
				t.Fatalf("audit %s lost cutover evidence: %+v", scope, m)
			}
		}
		if ready {
			return
		}
		select {
		case <-tick.C:
		case <-deadline.C:
			t.Fatal("cutover audit never reached both scopes")
		}
	}
}

func TestAdminTeamCutoverUnknownAndNoop(t *testing.T) {
	for _, tc := range []struct {
		name                 string
		missing, force, noop bool
		err                  error
		query                string
		want                 int
	}{
		{name: "missing", missing: true, want: 503}, {name: "missing-force", missing: true, force: true, want: 503},
		{name: "unreadable", err: errors.New("unreadable"), want: 503}, {name: "unreadable-force", err: errors.New("unreadable"), force: true, want: 503},
		{name: "noop", missing: true, noop: true, want: 200}, {name: "drained", want: 200}, {name: "invalid-force", query: "?force=true", want: 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _, done := newPoolMappingTestServer(t)
			defer done()
			s.auditStore = audit.NewMemoryStore()
			runs := &cutoverRuns{RunStore: s.cfg.Store, ids: []string{}, err: tc.err}
			s.cfg.Store = runs
			if tc.missing {
				s.cfg.Store = nil
			}
			next := "alpha"
			if tc.noop {
				next = ""
			}
			query := tc.query
			if tc.force {
				query = "?force=1"
			}
			req := orgReq(superAdminCtx(), http.MethodPut, "/api/admin/teams/t1/runner-pool"+query, `{"runner_pool":"`+next+`"}`, "t1")
			w := httptest.NewRecorder()
			s.handleAdminSetTeamRunnerPool(w, req)
			if w.Code != tc.want {
				t.Fatalf("status=%d want%d: %s", w.Code, tc.want, w.Body.String())
			}
			team, err := s.authStore().GetTeam(context.Background(), "t1")
			if err != nil {
				t.Fatal(err)
			}
			if tc.want != 200 && team.RunnerPool != "" {
				t.Fatalf("refused write changed mapping: %q", team.RunnerPool)
			}
			if tc.noop && runs.calls != 0 {
				t.Fatal("no-op queried live runs")
			}
			if tc.want == 200 {
				assertCutoverAudit(t, s, "", next, false, []string{})
			}
		})
	}
}

func TestAdminTeamCutoverUnknownDecodableMetadataRefuses(t *testing.T) {
	for _, force := range []bool{false, true} {
		t.Run(map[bool]string{false: "normal", true: "forced"}[force], func(t *testing.T) {
			s, _, done := newPoolMappingTestServer(t)
			defer done()
			fs, e := store.New(t.TempDir())
			if e != nil {
				t.Fatal(e)
			}
			ctx := store.WithTenant(context.Background(), "t1")
			r, e := fs.CreateRun(ctx, "unknown", "workflow", nil)
			if e != nil {
				t.Fatal(e)
			}
			r.TenantID = "t1"
			r.Status = "runing"
			if e = fs.SaveRun(ctx, r); e != nil {
				t.Fatal(e)
			}
			s.cfg.Store = fs
			path := "/api/admin/teams/t1/runner-pool"
			if force {
				path += "?force=1"
			}
			w := httptest.NewRecorder()
			s.handleAdminSetTeamRunnerPool(w, orgReq(superAdminCtx(), http.MethodPut, path, `{"runner_pool":"alpha"}`, "t1"))
			team, e := s.authStore().GetTeam(context.Background(), "t1")
			if e != nil {
				t.Fatal(e)
			}
			t.Logf("force=%t status=%d mapping=%q body=%s", force, w.Code, team.RunnerPool, w.Body)
			if w.Code != 503 {
				t.Fatalf("unknown persistent status allowed cutover: status%d mapping=%q", w.Code, team.RunnerPool)
			}
		})
	}
}
