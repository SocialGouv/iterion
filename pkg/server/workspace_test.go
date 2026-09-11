package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/server/projects"
)

func TestWorkspaceHostScopesProjectsWithoutRestartingThem(t *testing.T) {
	rootA, rootB := t.TempDir(), t.TempDir()
	storeA, storeB := filepath.Join(t.TempDir(), "a"), filepath.Join(t.TempDir(), "b")
	for _, dir := range []string{storeA, storeB} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	registry := &projects.Config{
		Version: 1,
		RecentProjects: []projects.Project{
			{ID: "a", Name: "A", Dir: rootA, StoreDir: storeA},
			{ID: "b", Name: "B", Dir: rootB, StoreDir: storeB},
		},
		CurrentProjectID: "a",
	}
	built := map[string]int{}
	host, err := NewWorkspaceHost(registry, func(project projects.Project) (*Server, error) {
		built[project.ID]++
		return New(Config{DisableAuth: true, SkipProjectRegistration: true}, iterlog.Nop()), nil
	}, iterlog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = host.Shutdown(ctx)
	})

	for _, id := range []string{"a", "b"} {
		req := httptest.NewRequest(http.MethodGet, "/x/"+id+"/api/project/identity", nil)
		rec := httptest.NewRecorder()
		host.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("identity %s = %d: %s", id, rec.Code, rec.Body.String())
		}
		var got WorkspaceRuntimeStatus
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if got.ID != id || got.State != "ready" || !got.RuntimeReady || got.Generation == "" {
			t.Fatalf("identity %s = %+v", id, got)
		}
	}
	// This registry is deliberately in-memory. Mutate its current pointer
	// directly: routing through the HTTP switch handler would call Save(), and
	// a Config literal has no isolated persistence path.
	if !host.registry.SetCurrent("b") {
		t.Fatal("could not make b current")
	}
	if host.runtimes["a"].server.CurrentProjectID() != "a" ||
		host.runtimes["b"].server.CurrentProjectID() != "b" {
		t.Fatalf(
			"embedded server scopes changed after switch: a=%q b=%q",
			host.runtimes["a"].server.CurrentProjectID(),
			host.runtimes["b"].server.CurrentProjectID(),
		)
	}
	if built["a"] != 1 || built["b"] != 1 {
		t.Fatalf("runtime builds = %#v, want one per project", built)
	}
	readyReq := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	readyRec := httptest.NewRecorder()
	host.ServeHTTP(readyRec, readyReq)
	if readyRec.Code != http.StatusOK || !strings.Contains(readyRec.Body.String(), `"ready":2`) {
		t.Fatalf("ready = %d: %s", readyRec.Code, readyRec.Body.String())
	}
}

func TestWorkspaceHostInjectsBrowserWorkspaceAndScopedSPA(t *testing.T) {
	root, storeDir := t.TempDir(), t.TempDir()
	registry := &projects.Config{
		Version:          1,
		RecentProjects:   []projects.Project{{ID: "town", Name: "Town", Dir: root, StoreDir: storeDir}},
		CurrentProjectID: "town",
	}
	host, err := NewWorkspaceHost(registry, func(project projects.Project) (*Server, error) {
		return New(Config{DisableAuth: true, SkipProjectRegistration: true}, iterlog.Nop()), nil
	}, iterlog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = host.Shutdown(ctx)
	})

	rootReq := httptest.NewRequest(http.MethodGet, "/", nil)
	rootRec := httptest.NewRecorder()
	host.ServeHTTP(rootRec, rootReq)
	if !strings.Contains(rootRec.Body.String(), "window.__ITERION_WORKSPACE__=true") {
		t.Fatal("root index does not identify the browser workspace")
	}
	scopeReq := httptest.NewRequest(http.MethodGet, "/x/town/runs/demo", nil)
	scopeRec := httptest.NewRecorder()
	host.ServeHTTP(scopeRec, scopeReq)
	if !strings.Contains(scopeRec.Body.String(), "window.__ITERION_SCOPE__=\"/x/town\"") {
		t.Fatal("scoped index does not contain the stable project prefix")
	}

	wsReq := httptest.NewRequest(http.MethodGet, "/x/town/_ws/info", nil)
	wsReq.Host = "127.0.0.1:4891"
	wsRec := httptest.NewRecorder()
	host.ServeHTTP(wsRec, wsReq)
	if !strings.Contains(wsRec.Body.String(), `"ws_base":"ws://127.0.0.1:4891/x/town"`) ||
		!strings.Contains(wsRec.Body.String(), `"needs_ticket":false`) {
		t.Fatalf("unexpected ws info: %s", wsRec.Body.String())
	}
}

func TestWorkspaceHandoffIsScopedAndSingleUse(t *testing.T) {
	rootA, rootB := t.TempDir(), t.TempDir()
	storeA, storeB := t.TempDir(), t.TempDir()
	registry := &projects.Config{
		Version: 1,
		RecentProjects: []projects.Project{
			{ID: "a", Name: "Town", Dir: rootA, StoreDir: storeA},
			{ID: "b", Name: "Tabarria", Dir: rootB, StoreDir: storeB},
		},
		CurrentProjectID: "a",
	}
	host, err := NewWorkspaceHost(registry, func(project projects.Project) (*Server, error) {
		return New(Config{DisableAuth: true, SkipProjectRegistration: true}, iterlog.Nop()), nil
	}, iterlog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = host.Shutdown(ctx)
	})

	create := httptest.NewRequest(
		http.MethodPost,
		"/x/a/api/workspace/handoffs",
		bytes.NewBufferString(`{"destination_project":"Tabarria","summary":"goal and next step"}`),
	)
	create.Header.Set("Origin", "http://iterion.test")
	create.Host = "iterion.test"
	created := httptest.NewRecorder()
	host.ServeHTTP(created, create)
	if created.Code != http.StatusOK {
		t.Fatalf("create = %d: %s", created.Code, created.Body.String())
	}
	var token struct {
		Ticket               string `json:"ticket"`
		DestinationProjectID string `json:"destination_project_id"`
	}
	if err := json.Unmarshal(created.Body.Bytes(), &token); err != nil {
		t.Fatal(err)
	}
	if len(token.Ticket) != 64 || token.DestinationProjectID != "b" {
		t.Fatalf("unexpected handoff: %+v", token)
	}

	redeemBody := []byte(`{"ticket":"` + token.Ticket + `"}`)
	redeem := httptest.NewRequest(
		http.MethodPost,
		"/x/b/api/workspace/handoffs/redeem",
		bytes.NewReader(redeemBody),
	)
	redeemed := httptest.NewRecorder()
	host.ServeHTTP(redeemed, redeem)
	if redeemed.Code != http.StatusOK ||
		!strings.Contains(redeemed.Body.String(), `"summary":"goal and next step"`) ||
		!strings.Contains(redeemed.Body.String(), `"bot_id":"copilot"`) {
		t.Fatalf("redeem = %d: %s", redeemed.Code, redeemed.Body.String())
	}

	replay := httptest.NewRequest(
		http.MethodPost,
		"/x/b/api/workspace/handoffs/redeem",
		bytes.NewReader(redeemBody),
	)
	replayed := httptest.NewRecorder()
	host.ServeHTTP(replayed, replay)
	if replayed.Code != http.StatusNotFound {
		t.Fatalf("replay = %d: %s", replayed.Code, replayed.Body.String())
	}

	createAgain := httptest.NewRequest(
		http.MethodPost,
		"/x/a/api/workspace/handoffs",
		bytes.NewBufferString(`{"destination_project":"b","summary":"generation check"}`),
	)
	createdAgain := httptest.NewRecorder()
	host.ServeHTTP(createdAgain, createAgain)
	if createdAgain.Code != http.StatusOK {
		t.Fatalf("create again = %d: %s", createdAgain.Code, createdAgain.Body.String())
	}
	if err := json.Unmarshal(createdAgain.Body.Bytes(), &token); err != nil {
		t.Fatal(err)
	}
	host.mu.Lock()
	host.runtimes["b"].generation = workspaceGeneration()
	host.mu.Unlock()
	generationRedeem := httptest.NewRequest(
		http.MethodPost,
		"/x/b/api/workspace/handoffs/redeem",
		bytes.NewBufferString(`{"ticket":"`+token.Ticket+`"}`),
	)
	generationResult := httptest.NewRecorder()
	host.ServeHTTP(generationResult, generationRedeem)
	if generationResult.Code != http.StatusConflict {
		t.Fatalf("generation redeem = %d: %s", generationResult.Code, generationResult.Body.String())
	}
}
