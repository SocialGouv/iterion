package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

const importScriptJS = `export const meta = { name: 'hello-flow' }
const r = await agent('Say hello and stop.', { label: 'greeter' })
return { r }
`

func doImport(t *testing.T, srv *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/bots/import", strings.NewReader(body))
	rec := httptest.NewRecorder()
	srv.handler.ServeHTTP(rec, req)
	return rec
}

func importBody(t *testing.T, req botImportRequest) string {
	t.Helper()
	b, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestBotImport_DryRunAndWrite(t *testing.T) {
	workdir := t.TempDir()
	srv := newBotServer(t, workdir)

	// Dry-run: draft + report, nothing on disk.
	rec := doImport(t, srv, importBody(t, botImportRequest{Source: importScriptJS, DryRun: true}))
	if rec.Code != http.StatusOK {
		t.Fatalf("dry-run status = %d body=%s", rec.Code, rec.Body.String())
	}
	var resp botImportResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.WorkflowName != "hello_flow" || !strings.Contains(resp.BotSource, "## IMPORT REPORT") {
		t.Fatalf("resp = %+v", resp)
	}
	if _, err := os.Stat(filepath.Join(workdir, "bots", "hello_flow.bot")); !os.IsNotExist(err) {
		t.Fatal("dry-run must not write")
	}

	// Write mode lands the draft in bots/.
	rec = doImport(t, srv, importBody(t, botImportRequest{Source: importScriptJS}))
	if rec.Code != http.StatusOK {
		t.Fatalf("write status = %d body=%s", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Path != filepath.Join("bots", "hello_flow.bot") {
		t.Fatalf("path = %q", resp.Path)
	}
	data, err := os.ReadFile(filepath.Join(workdir, resp.Path))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "workflow hello_flow:") {
		t.Fatal("draft content missing workflow")
	}

	// Second write refuses to overwrite.
	rec = doImport(t, srv, importBody(t, botImportRequest{Source: importScriptJS}))
	if rec.Code != http.StatusConflict {
		t.Fatalf("overwrite status = %d, want 409", rec.Code)
	}
}

func TestBotImport_InputGates(t *testing.T) {
	srv := newBotServer(t, t.TempDir())

	rec := doImport(t, srv, `{"dry_run":true}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("missing source status = %d, want 400", rec.Code)
	}

	rec = doImport(t, srv, importBody(t, botImportRequest{Source: "const x = {{{", DryRun: true}))
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("unparsable status = %d, want 422", rec.Code)
	}

	rec = doImport(t, srv, importBody(t, botImportRequest{Source: "log('no agents here')", DryRun: true}))
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("no-agents status = %d, want 422", rec.Code)
	}
}

func TestBotImport_CloudForbidden(t *testing.T) {
	srv := New(Config{DisableAuth: true, Mode: "cloud"}, iterlog.New(iterlog.LevelError, nil))
	srv.handler = srv.mux
	rec := doImport(t, srv, importBody(t, botImportRequest{Source: importScriptJS, DryRun: true}))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cloud status = %d, want 403", rec.Code)
	}
}
