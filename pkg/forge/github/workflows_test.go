package github

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/SocialGouv/iterion/pkg/forge"
)

// dispatchAPI is GitHub's dispatch surface as strict as the real one: the
// default branch from GET /repos/{owner}/{repo}, 204 on a dispatch, 404 on an
// unknown workflow, 422 on an unknown ref or a workflow without a
// workflow_dispatch trigger. A branch is accepted short or fully qualified,
// as GitHub accepts it. edge.yml stands for an edge failing after the write:
// 502.
type dispatchAPI struct {
	mu       sync.Mutex
	requests []string
	got      []map[string]any
}

func newDispatchAPI(t *testing.T) (*dispatchAPI, *httptest.Server) {
	t.Helper()
	api := &dispatchAPI{}
	reply := func(w http.ResponseWriter, code int, msg string) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(map[string]string{"message": msg})
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v3/repos/acme/widgets", func(w http.ResponseWriter, r *http.Request) {
		api.mu.Lock()
		api.requests = append(api.requests, "GET repo")
		api.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"full_name": "acme/widgets", "default_branch": "trunk"})
	})
	mux.HandleFunc("POST /api/v3/repos/acme/widgets/actions/workflows/{file}/dispatches", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		api.mu.Lock()
		api.requests = append(api.requests, "POST "+r.PathValue("file"))
		api.mu.Unlock()
		ref, _ := body["ref"].(string)
		switch {
		case r.PathValue("file") == "edge.yml":
			reply(w, http.StatusBadGateway, "Server Error")
		case r.PathValue("file") == "nope.yml":
			reply(w, http.StatusNotFound, "Not Found")
		case strings.TrimPrefix(ref, "refs/heads/") != "trunk" && strings.TrimPrefix(ref, "refs/heads/") != "release":
			reply(w, http.StatusUnprocessableEntity, "No ref found for: "+ref)
		case r.PathValue("file") == "ci.yml":
			reply(w, http.StatusUnprocessableEntity, "Workflow does not have 'workflow_dispatch' trigger")
		default:
			api.mu.Lock()
			api.got = append(api.got, body)
			api.mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return api, srv
}

func TestDispatchWorkflow(t *testing.T) {
	ctx := context.Background()
	// The default branch goes on the wire fully qualified: a short name is
	// ambiguous with a tag of the same name.
	//
	// Mutation witnessed: the default branch sent short (wireRef = ref) →
	// the body reads {ref: trunk} → red.
	t.Run("an empty ref is the default branch, read from the forge", func(t *testing.T) {
		api, srv := newDispatchAPI(t)
		res, err := New(srv.Client(), srv.URL, "tok").DispatchWorkflow(ctx, "acme/widgets", "renovate.yml", "")
		if err != nil || res.Ref != "trunk" {
			t.Fatalf("DispatchWorkflow = (%+v, %v), want dispatched on trunk", res, err)
		}
		if len(api.got) != 1 || !maps.Equal(api.got[0], map[string]any{"ref": "refs/heads/trunk"}) {
			t.Fatalf("dispatch bodies = %v, want exactly {ref: refs/heads/trunk} — the branch, never a same-named tag, and no inputs", api.got)
		}
	})
	t.Run("a named ref is dispatched as named", func(t *testing.T) {
		api, srv := newDispatchAPI(t)
		res, err := New(srv.Client(), srv.URL, "tok").DispatchWorkflow(ctx, "acme/widgets", "renovate.yml", "release")
		if err != nil || res.Ref != "release" || len(api.requests) != 1 {
			t.Fatalf("DispatchWorkflow = (%+v, %v) after %v, want one dispatch on release and no default-branch read", res, err, api.requests)
		}
	})
	for _, tc := range []struct{ name, workflow, ref string }{
		{"an unknown workflow (404)", "nope.yml", ""},
		{"an unknown ref (422)", "renovate.yml", "gone"},
		{"no workflow_dispatch trigger (422)", "ci.yml", ""},
	} {
		t.Run(tc.name+" is an answered refusal", func(t *testing.T) {
			_, srv := newDispatchAPI(t)
			_, err := New(srv.Client(), srv.URL, "tok").DispatchWorkflow(ctx, "acme/widgets", tc.workflow, tc.ref)
			if !errors.Is(err, forge.ErrWorkflowNotDispatched) {
				t.Fatalf("DispatchWorkflow = %v, want an error wrapping ErrWorkflowNotDispatched", err)
			}
		})
	}
	// The client's own guard, apart from the server's: a name that is not a
	// plain workflow file never reaches the wire. Red if the guard goes —
	// "dir/x.yml" would then address another path.
	t.Run("a name that is not a plain workflow file is never sent", func(t *testing.T) {
		api, srv := newDispatchAPI(t)
		_, err := New(srv.Client(), srv.URL, "tok").DispatchWorkflow(ctx, "acme/widgets", "dir/x.yml", "trunk")
		if !errors.Is(err, forge.ErrWorkflowNotDispatched) || len(api.requests) != 0 {
			t.Fatalf("DispatchWorkflow(dir/x.yml) = %v after %v, want a refusal and no request", err, api.requests)
		}
	})
	// A 5xx is an answer but not a verdict: the edge may fail after the
	// backend queued the run, so it never claims nothing was queued.
	//
	// Mutation witnessed: the 5xx branch removed → the 502 wraps
	// ErrWorkflowNotDispatched → red.
	t.Run("a forge 5xx leaves the outcome unknown", func(t *testing.T) {
		_, srv := newDispatchAPI(t)
		_, err := New(srv.Client(), srv.URL, "tok").DispatchWorkflow(ctx, "acme/widgets", "edge.yml", "")
		if err == nil || errors.Is(err, forge.ErrWorkflowNotDispatched) {
			t.Fatalf("DispatchWorkflow answered 502 = %v, want an error that does NOT claim nothing was queued", err)
		}
	})
	// No answer is not a refusal: the forge may have queued the run.
	t.Run("a forge that does not answer leaves the outcome unknown", func(t *testing.T) {
		_, srv := newDispatchAPI(t)
		srv.Close()
		_, err := New(srv.Client(), srv.URL, "tok").DispatchWorkflow(ctx, "acme/widgets", "renovate.yml", "trunk")
		if err == nil || errors.Is(err, forge.ErrWorkflowNotDispatched) {
			t.Fatalf("DispatchWorkflow on a dead forge = %v, want an error that does NOT claim nothing was queued", err)
		}
	})
}

// The dispatch token's permission set is exactly what the endpoint needs:
// actions write (the dispatch) and metadata read (the default-branch read).
func TestWorkflowDispatchInstallationPermissionsIsExact(t *testing.T) {
	want := map[string]string{"actions": "write", "metadata": "read"}
	if got := WorkflowDispatchInstallationPermissions(); !maps.Equal(got, want) {
		t.Fatalf("WorkflowDispatchInstallationPermissions = %v, want exactly %v", got, want)
	}
	for _, set := range []map[string]string{RuntimeInstallationPermissions(), DeliveryInstallationPermissions(), RunCIReadPermissions()} {
		if _, ok := set[PermissionActions]; ok {
			t.Fatalf("%v carries %s — no run-token set may (the dispatch is the per-bot opt-in)", set, PermissionActions)
		}
	}
}
