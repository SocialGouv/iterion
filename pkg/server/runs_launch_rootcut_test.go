package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/orgusage"
)

// newLaunchRootcutServer is a gated server on workDir whose run service
// carries a publisher: the compile then happens on the launch path
// (compileForLaunch), and its refusal is answered verbatim as the 400
// body `launch: %v`.
func newLaunchRootcutServer(t *testing.T, workDir string) (*Server, context.Context) {
	t.Helper()
	s := newOrgTestServer(t)
	s.cfg.WorkDir = workDir
	s.cfg.StoreDir = filepath.Join(workDir, ".iterion")
	s.orgUsage = orgusage.NewMemoryCounter()
	ctx := seedGate(t, s, gateSpec{id: "t1"})
	s.runs = freshRuns(t, &landingPublishStub{})
	return s, ctx
}

func postLaunch(t *testing.T, s *Server, ctx context.Context, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/api/runs", strings.NewReader(body)).WithContext(ctx)
	w := httptest.NewRecorder()
	s.handleLaunchRun(w, r)
	return w
}

// The 400 of a launch whose bot does not compile — `launch: %v` — cites the
// fragment at fault by its unit-relative path, never by the absolute one the
// server's loader parsed it under: the workdir/snapshot layout is the
// server's, not the client's (#1934's root cut, on the launch path).
func TestLaunchNamesABrokenFragmentWithoutTheHostRoot(t *testing.T) {
	workDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(workDir, "broken", "lib"), 0o755); err != nil {
		t.Fatal(err)
	}
	main := "import \"lib/nodes.bot\"\n\nworkflow x:\n  entry: done\n"
	if err := os.WriteFile(filepath.Join(workDir, "broken", "main.bot"), []byte(main), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workDir, "broken", "lib", "nodes.bot"), []byte("prompt p:\n  hi\n\nagent \n  model\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	s, ctx := newLaunchRootcutServer(t, workDir)
	w := postLaunch(t, s, ctx, `{"file_path":"broken/main.bot"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want the 400 of a bot that does not compile: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "lib/nodes.bot:") {
		t.Errorf("the 400 does not cite the fragment by its relative path: %s", w.Body.String())
	}
	if strings.Contains(w.Body.String(), workDir) {
		t.Errorf("the 400 discloses the server's directory layout: %s", w.Body.String())
	}
}

// The 400 of a launch whose file_path names nothing — `cannot read file:
// open <abs>` — names the file, never the workdir the server resolved it
// against.
func TestLaunchWithAFilePathTypoNamesNoHostPath(t *testing.T) {
	workDir := t.TempDir()
	s, ctx := newLaunchRootcutServer(t, workDir)
	w := postLaunch(t, s, ctx, `{"file_path":"ghost/missing.bot"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want the 400 of a file that is not there: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "missing.bot") {
		t.Errorf("the 400 does not name the file: %s", w.Body.String())
	}
	if strings.Contains(w.Body.String(), workDir) {
		t.Errorf("the 400 discloses the server's directory layout: %s", w.Body.String())
	}
}

// The 422 of a launch whose file_path is a bundle's entrypoint and the
// bundle's manifest does not decode names the manifest, never the
// workdir: the bundle directory rode the refusal three times (the
// entrypoint subject, the bundle, the manifest's parse error).
func TestLaunchOfABundleWhoseManifestDoesNotOpenNamesNoHostPath(t *testing.T) {
	workDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(workDir, "bund"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workDir, "bund", "main.bot"), []byte("workflow x:\n  entry: done\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workDir, "bund", "manifest.yaml"), []byte("schema_version: 99\nname: [broken\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s, ctx := newLaunchRootcutServer(t, workDir)
	w := postLaunch(t, s, ctx, `{"file_path":"bund/main.bot"}`)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want the 422 of a bundle that does not open: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "manifest") {
		t.Errorf("the 422 does not name the manifest: %s", w.Body.String())
	}
	if strings.Contains(w.Body.String(), workDir) {
		t.Errorf("the 422 discloses the server's directory layout: %s", w.Body.String())
	}
}

// The 400 of an inline launch whose source carries an import names the
// refusal, never the materialized copy's absolute path under the server's
// store (`<store>/inline-sources/<hash>-main.bot`).
func TestLaunchOfInlineSourceWithAnImportNamesNoHostPath(t *testing.T) {
	workDir := t.TempDir()
	s, ctx := newLaunchRootcutServer(t, workDir)
	w := postLaunch(t, s, ctx, `{"source":"import \"lib/x.bot\"\n\nworkflow x:\n  entry: done\n"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want the 400 of an inline source that imports: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "import") {
		t.Errorf("the 400 does not name the refusal: %s", w.Body.String())
	}
	if strings.Contains(w.Body.String(), workDir) {
		t.Errorf("the 400 discloses the server's directory layout: %s", w.Body.String())
	}
}
