package cli

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

const remoteUnitMain = "import \"lib/nodes.bot\"\n\nworkflow w:\n  entry: worker\n  worker -> done\n"

// Pinned model and backend: the compile refuses C018 on a credential-less
// host, and these tests measure the upload.
const remoteUnitNodes = "schema out:\n  ok: bool\n\nprompt mission:\n  {{include \"part.md\"}}\n\nagent worker:\n  backend: \"claude_code\"\n  model: \"anthropic/claude-opus-4-8\"\n  system: mission\n  output: out\n"

const remoteSingleBot = "schema out:\n  ok: bool\n\nagent worker:\n  backend: \"claude_code\"\n  model: \"anthropic/claude-opus-4-8\"\n  output: out\n\nworkflow w:\n  entry: worker\n  worker -> done\n"

func writeRemoteUnit(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, src := range files {
		full := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// TestPrepareUnitWritesTheProgramOutFlat: a bot in several files is
// uploaded as one — no import line, every include resolved beside the file
// that carries it — and that text is a program a pod with none of the
// files compiles. It is the run's identity: stable for the same unit,
// another for an edited fragment or include. A bot in one file with no
// include travels as written, byte for byte.
func TestPrepareUnitWritesTheProgramOutFlat(t *testing.T) {
	root := t.TempDir()
	writeRemoteUnit(t, root, map[string]string{"main.bot": remoteUnitMain, "lib/nodes.bot": remoteUnitNodes, "lib/part.md": "PART-FROM-THE-FRAGMENT"})
	mainBot := filepath.Join(root, "main.bot")

	flat, err := prepareUnit(mainBot)
	if err != nil {
		t.Fatalf("prepareUnit: %v", err)
	}
	if strings.Contains(flat, "import ") || strings.Contains(flat, "{{include") {
		t.Fatalf("an import or include marker travels:\n%s", flat)
	}
	if !strings.Contains(flat, "PART-FROM-THE-FRAGMENT") || !strings.Contains(flat, "agent worker") {
		t.Fatalf("the fragment's node or its include is missing:\n%s", flat)
	}
	pr := parser.Parse("<inline>", flat)
	for _, d := range pr.Diagnostics {
		if d.Severity == parser.SeverityError {
			t.Fatalf("the flat text does not parse: %s", d.Error())
		}
	}
	cr := ir.Compile(pr.File)
	if cr.HasErrors() {
		t.Fatalf("the flat text does not compile: %v", cr.Diagnostics)
	}
	if _, ok := cr.Workflow.Nodes["worker"]; !ok {
		t.Fatal("the fragment's node did not travel")
	}

	again, err := prepareUnit(mainBot)
	if err != nil || again != flat {
		t.Fatalf("the same unit flattened to another text (err=%v)", err)
	}
	writeRemoteUnit(t, root, map[string]string{"lib/part.md": "PART, EDITED"})
	edited, err := prepareUnit(mainBot)
	if err != nil || edited == flat {
		t.Fatalf("an edited include left the flat text unchanged (err=%v)", err)
	}

	single := filepath.Join(root, "single.bot")
	if err := os.WriteFile(single, []byte(remoteSingleBot), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, err := prepareUnit(single); err != nil || got != remoteSingleBot {
		t.Fatalf("a bot in one file no longer travels as written (err=%v):\n%s", err, got)
	}

	writeRemoteUnit(t, root, map[string]string{"lib/nodes.bot": "agent worker:\n  bogus: 1\n"})
	if _, err := prepareUnit(mainBot); err == nil || !strings.Contains(err.Error(), "nodes.bot") {
		t.Fatalf("a broken fragment was not named: %v", err)
	}
}

// TestRemoteSurfacesUploadTheUnitFlat: launch, resume and the cost preview
// send the same flattened text — one preparation, so the identity a
// resume presents is the one the launch recorded.
func TestRemoteSurfacesUploadTheUnitFlat(t *testing.T) {
	root := t.TempDir()
	writeRemoteUnit(t, root, map[string]string{"main.bot": remoteUnitMain, "lib/nodes.bot": remoteUnitNodes, "lib/part.md": "PART-FROM-THE-FRAGMENT"})
	mainBot := filepath.Join(root, "main.bot")
	want, err := prepareUnit(mainBot)
	if err != nil {
		t.Fatal(err)
	}
	sources := map[string]string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req map[string]any
		_ = json.Unmarshal(body, &req)
		src, _ := req["source"].(string)
		sources[r.Method+" "+r.URL.Path] = src
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"run_id":"r1","id":"r1","status":"queued"}`))
	}))
	defer srv.Close()
	c := NewRemoteClientFor(RemoteConfig{BaseURL: srv.URL, Token: "t"})
	p := &Printer{W: io.Discard, Format: OutputJSON}
	ctx := context.Background()
	if err := RemoteRunsLaunch(ctx, c, p, RemoteRunsLaunchOptions{FilePath: mainBot}); err != nil {
		t.Fatalf("launch: %v", err)
	}
	if err := RemoteRunsResume(ctx, c, p, "r1", RemoteRunsResumeOptions{FilePath: mainBot}); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if err := RemoteRunsPreviewCost(ctx, c, p, mainBot, nil); err != nil {
		t.Fatalf("preview: %v", err)
	}
	for _, key := range []string{"POST /api/runs", "POST /api/runs/r1/resume", "POST /api/runs/preview-cost"} {
		got, ok := sources[key]
		if !ok {
			t.Fatalf("%s was not called; calls: %v", key, keysOf(sources))
		}
		if got != want {
			t.Fatalf("%s uploaded:\n%s\nwant the flattened unit:\n%s", key, got, want)
		}
	}
}

func keysOf(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
