package server

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/orgusage"
)

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

	s := newOrgTestServer(t)
	s.cfg.WorkDir = workDir
	s.orgUsage = orgusage.NewMemoryCounter()
	ctx := seedGate(t, s, gateSpec{id: "t1"})
	// A publisher puts the compile on the launch path (compileForLaunch,
	// cloud or not): the refusal below is its error, answered verbatim.
	s.runs = freshRuns(t, &landingPublishStub{})

	r := httptest.NewRequest(http.MethodPost, "/api/runs", strings.NewReader(`{"file_path":"broken/main.bot"}`)).WithContext(ctx)
	w := httptest.NewRecorder()
	s.handleLaunchRun(w, r)
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
