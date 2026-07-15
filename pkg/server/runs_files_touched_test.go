package server

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/store"
)

// seedTouchedEvents appends the given events to an existing run through a
// fresh store handle (same pattern as seedRun).
func seedTouchedEvents(t *testing.T, srv *Server, runID string, events []store.Event) {
	t.Helper()
	st, err := store.New(srv.cfg.StoreDir)
	if err != nil {
		t.Fatalf("open seed store: %v", err)
	}
	for i, evt := range events {
		if _, err := st.AppendEvent(context.Background(), runID, evt); err != nil {
			t.Fatalf("seed event %d: %v", i, err)
		}
	}
}

func getTouched(t *testing.T, hs string, runID string) runTouchedFilesResponse {
	t.Helper()
	resp, err := http.Get(hs + "/api/runs/" + runID + "/files/touched")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var out runTouchedFilesResponse
	decodeJSONResp(t, resp, &out)
	return out
}

// TestRunTouchedFiles_Aggregation covers the core extraction: both tool
// namespaces, path normalization (absolute-under-workdir → relative),
// dedup with node attribution + write counts, and the exclusion of
// read-only / shell tools.
func TestRunTouchedFiles_Aggregation(t *testing.T) {
	srv, hs := newTestServer(t)
	workDir := t.TempDir()
	seedRunWithWorkDir(t, srv, "touched", workDir, false)
	seedTouchedEvents(t, srv, "touched", []store.Event{
		// claude_code Write, absolute path under workdir → relative.
		{Type: store.EventToolStarted, RunID: "touched", NodeID: "implement",
			Data: map[string]any{"tool": "Write", "input": `{"file_path": "` + workDir + `/pkg/a.go", "content": "x"}`}},
		// Same file edited again by another node → one row, two nodes, 2 writes.
		{Type: store.EventToolStarted, RunID: "touched", NodeID: "fix",
			Data: map[string]any{"tool": "Edit", "input": `{"file_path": "` + workDir + `/pkg/a.go", "old_string": "x", "new_string": "y"}`}},
		// claw write_file with a workdir-relative path.
		{Type: store.EventToolStarted, RunID: "touched", NodeID: "implement",
			Data: map[string]any{"tool": "write_file", "input": `{"path": "docs/readme.md", "content": "hi"}`}},
		// Path outside the workdir stays absolute.
		{Type: store.EventToolStarted, RunID: "touched", NodeID: "report",
			Data: map[string]any{"tool": "Write", "input": `{"file_path": "/tmp/elsewhere/report.md", "content": "r"}`}},
		// Read-only + shell tools are ignored even with a path/command.
		{Type: store.EventToolStarted, RunID: "touched", NodeID: "implement",
			Data: map[string]any{"tool": "Read", "input": `{"file_path": "` + workDir + `/pkg/a.go"}`}},
		{Type: store.EventToolStarted, RunID: "touched", NodeID: "implement",
			Data: map[string]any{"tool": "Bash", "input": `{"command": "touch ` + workDir + `/pkg/b.go"}`}},
		// tool_called (post-execution) events never carry input — ignored.
		{Type: store.EventToolCalled, RunID: "touched", NodeID: "implement",
			Data: map[string]any{"tool": "Write", "input_size": 12}},
		// Degenerate paths (a tool_started fires before the tool errors):
		// whitespace-only normalizes to "", the workdir itself to "." —
		// neither may become a row.
		{Type: store.EventToolStarted, RunID: "touched", NodeID: "implement",
			Data: map[string]any{"tool": "Write", "input": `{"file_path": "   ", "content": "x"}`}},
		{Type: store.EventToolStarted, RunID: "touched", NodeID: "implement",
			Data: map[string]any{"tool": "Write", "input": `{"file_path": "` + workDir + `", "content": "x"}`}},
	})

	out := getTouched(t, hs.URL, "touched")
	if out.WorkDir != workDir {
		t.Errorf("WorkDir = %q, want %q", out.WorkDir, workDir)
	}
	if len(out.Files) != 3 {
		t.Fatalf("files = %+v, want 3 entries", out.Files)
	}
	// Sorted by path: /tmp/... < docs/... < pkg/...
	if out.Files[0].Path != "/tmp/elsewhere/report.md" {
		t.Errorf("files[0].Path = %q", out.Files[0].Path)
	}
	if out.Files[1].Path != "docs/readme.md" {
		t.Errorf("files[1].Path = %q", out.Files[1].Path)
	}
	f := out.Files[2]
	if f.Path != "pkg/a.go" {
		t.Errorf("files[2].Path = %q, want pkg/a.go", f.Path)
	}
	if f.Writes != 2 {
		t.Errorf("pkg/a.go writes = %d, want 2", f.Writes)
	}
	if len(f.NodeIDs) != 2 || f.NodeIDs[0] != "implement" || f.NodeIDs[1] != "fix" {
		t.Errorf("pkg/a.go node_ids = %v, want [implement fix]", f.NodeIDs)
	}
	if f.LastSeq <= 0 {
		t.Errorf("pkg/a.go last_seq = %d, want > 0", f.LastSeq)
	}
}

// TestRunTouchedFiles_TruncatedPreviewAndBlob covers the sidecar shapes: a
// 4 KB preview cut mid-content still yields the path (lenient token walk),
// and a preview that lost the key falls back to the sidecar blob head.
func TestRunTouchedFiles_TruncatedPreviewAndBlob(t *testing.T) {
	srv, hs := newTestServer(t)
	workDir := t.TempDir()
	seedRunWithWorkDir(t, srv, "trunc", workDir, false)

	// Preview: valid prefix of a Write input, cut inside the content value.
	preview := `{"file_path": "big/file.txt", "content": "` + strings.Repeat("a", 100)
	// Blob-backed input whose preview carried no key at all; the full blob does.
	blobInput := `{"content": "` + strings.Repeat("b", 200) + `", "file_path": "from/blob.txt"}`
	st, err := store.New(srv.cfg.StoreDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.WriteToolBlob(context.Background(), "trunc", "tu-blob", "input", []byte(blobInput)); err != nil {
		t.Fatalf("WriteToolBlob: %v", err)
	}

	seedTouchedEvents(t, srv, "trunc", []store.Event{
		{Type: store.EventToolStarted, RunID: "trunc", NodeID: "n1",
			Data: map[string]any{"tool": "Write", "input_preview": preview, "input_ref": "tu-x", "input_size": 9000}},
		{Type: store.EventToolStarted, RunID: "trunc", NodeID: "n2",
			Data: map[string]any{"tool": "Write", "input_preview": `{"content": "` + strings.Repeat("b", 40), "input_ref": "tu-blob", "input_size": len(blobInput)}},
	})

	out := getTouched(t, hs.URL, "trunc")
	if len(out.Files) != 2 {
		t.Fatalf("files = %+v, want 2 entries", out.Files)
	}
	if out.Files[0].Path != "big/file.txt" || out.Files[0].NodeIDs[0] != "n1" {
		t.Errorf("files[0] = %+v, want big/file.txt from n1", out.Files[0])
	}
	if out.Files[1].Path != "from/blob.txt" || out.Files[1].NodeIDs[0] != "n2" {
		t.Errorf("files[1] = %+v, want from/blob.txt from n2", out.Files[1])
	}
}

// TestRunTouchedFiles_EmptyAndMissing: a run with no write events returns
// an empty list; an unknown run 404s.
func TestRunTouchedFiles_EmptyAndMissing(t *testing.T) {
	srv, hs := newTestServer(t)
	seedRun(t, srv, "quiet", "wf", store.RunStatusFinished)

	out := getTouched(t, hs.URL, "quiet")
	if len(out.Files) != 0 {
		t.Errorf("files = %+v, want empty", out.Files)
	}

	resp, err := http.Get(hs.URL + "/api/runs/nope/files/touched")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

// TestPathFromJSONObject exercises the lenient parser directly on the
// shapes persistToolPayload produces.
func TestPathFromJSONObject(t *testing.T) {
	keys := []string{"path", "file_path"}
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"complete", `{"path": "a/b.txt", "content": "x"}`, "a/b.txt"},
		{"complete second key", `{"file_path": "c.txt"}`, "c.txt"},
		{"key priority", `{"file_path": "second.txt", "path": "first.txt"}`, "first.txt"},
		{"truncated inside string", `{"path": "a/b.txt", "content": "aaaa`, "a/b.txt"},
		{"truncated inside nested", `{"path": "a/b.txt", "edits": [{"old": "x", "new`, "a/b.txt"},
		{"truncated before key", `{"content": "aaaa`, ""},
		{"key after nested value", `{"meta": {"k": 1}, "path": "late.txt"}`, "late.txt"},
		{"not an object", `["path"]`, ""},
		{"empty", ``, ""},
		{"non-string value", `{"path": 42, "file_path": "real.txt"}`, "real.txt"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := pathFromJSONObject(tc.raw, keys); got != tc.want {
				t.Errorf("pathFromJSONObject(%q) = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}
}

// TestNormalizeTouchedPath covers the workdir-relativization rules.
func TestNormalizeTouchedPath(t *testing.T) {
	cases := []struct {
		workDir, in, want string
	}{
		{"/w", "/w/a/b.txt", "a/b.txt"},
		{"/w", "a/b.txt", "a/b.txt"},
		{"/w", "./a/b.txt", "a/b.txt"},
		{"/w", "/elsewhere/x.txt", "/elsewhere/x.txt"},
		{"/w", "/w", "."},
		{"", "/abs/x.txt", "/abs/x.txt"},
		{"/w", "  /w/spaced.txt ", "spaced.txt"},
		{"/w", "", ""},
	}
	for _, tc := range cases {
		if got := normalizeTouchedPath(tc.workDir, tc.in); got != tc.want {
			t.Errorf("normalizeTouchedPath(%q, %q) = %q, want %q", tc.workDir, tc.in, got, tc.want)
		}
	}
}
