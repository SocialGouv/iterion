package bots

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/gittest"
)

// requireAssessmentTools skips an assessment bot-level test when a tool it
// needs is missing on a developer's host — and FAILS under CI, where a skipped
// guard is a green no-op: the exact class these guards exist to refuse.
func requireAssessmentTools(t *testing.T, tools ...string) {
	t.Helper()
	if len(tools) == 0 {
		tools = []string{"python3", "git"}
	}
	for _, tool := range tools {
		if _, err := exec.LookPath(tool); err != nil {
			if os.Getenv("CI") != "" {
				t.Fatalf("%s not on PATH under CI — the assessment guard tests would skip into a green no-op; install it in the workflow", tool)
			}
			t.Skipf("%s not on PATH", tool)
		}
	}
}

// assessmentRun executes the REAL script of an assessment tool node and
// returns its parsed JSON output. quoted values are substituted as Python
// string literals (what the engine does for a string ref); raw values are
// substituted verbatim (what it does for a json ref).
//
// Nothing here re-implements a node: the body under test is the one the
// bundle ships, read out of the compiled workflow.
func assessmentRun(t *testing.T, node string, quoted, raw map[string]string) (map[string]any, int, string) {
	t.Helper()
	body := toolScript(t, "assessment/main.bot", node)
	for ref, value := range quoted {
		body = strings.ReplaceAll(body, ref, strconv.Quote(value))
	}
	for ref, value := range raw {
		body = strings.ReplaceAll(body, ref, value)
	}
	if i := strings.Index(body, "{{"); i >= 0 {
		end := i + 60
		if end > len(body) {
			end = len(body)
		}
		t.Fatalf("unresolved template ref in %s near %q", node, body[i:end])
	}
	path := filepath.Join(t.TempDir(), node+".py")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("python3", path)
	stdout, err := cmd.Output()
	exit := 0
	if err != nil {
		ee, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("%s failed to execute: %v (out %q)", node, err, stdout)
		}
		exit = ee.ExitCode()
		if exit != 0 {
			return nil, exit, string(ee.Stderr)
		}
	}
	var out map[string]any
	if uerr := json.Unmarshal(stdout, &out); uerr != nil {
		t.Fatalf("%s output is not JSON: %v (out %q)", node, uerr, stdout)
	}
	return out, exit, ""
}

// assessmentBool reads a boolean field out of a node's output, failing rather
// than defaulting: a missing field read as false is how a guard passes for the
// wrong reason.
func assessmentBool(t *testing.T, out map[string]any, field string) bool {
	t.Helper()
	value, ok := out[field]
	if !ok {
		t.Fatalf("output carries no %q field (got %v)", field, keysOf(out))
	}
	b, ok := value.(bool)
	if !ok {
		t.Fatalf("field %q is %T, want bool", field, value)
	}
	return b
}

func assessmentString(t *testing.T, out map[string]any, field string) string {
	t.Helper()
	value, ok := out[field]
	if !ok {
		t.Fatalf("output carries no %q field (got %v)", field, keysOf(out))
	}
	s, ok := value.(string)
	if !ok {
		t.Fatalf("field %q is %T, want string", field, value)
	}
	return s
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// synthRepo builds a throwaway git repository from a path->content map and
// commits it. Everything an assessment test measures is SYNTHETIC by
// construction: the bundle ships to a public catalog, and a fixture carrying a
// real tree would carry whatever that tree is about.
func synthRepo(t *testing.T, files map[string]string) (dir, sha string) {
	t.Helper()
	dir = t.TempDir()
	gittest.Run(t, dir, "init", "-q", "-b", "main")
	gittest.Run(t, dir, "config", "user.email", "t@example.com")
	gittest.Run(t, dir, "config", "user.name", "t")
	gittest.Run(t, dir, "config", "commit.gpgsign", "false")
	for path, content := range files {
		full := filepath.Join(dir, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gittest.Run(t, dir, "add", "-A")
	gittest.Run(t, dir, "commit", "-qm", "fixture")
	return dir, strings.TrimSpace(gittest.Run(t, dir, "rev-parse", "HEAD"))
}

// writeSurvey materialises a survey document the way survey_write does, so a
// lint test exercises the file the lint actually reads.
func writeSurvey(t *testing.T, dir, baseSHA string, stacks, declarations []map[string]any) string {
	t.Helper()
	path := filepath.Join(dir, "survey.json")
	body, err := json.Marshal(map[string]any{
		"version": 1, "base_sha": baseSHA, "stacks": stacks,
		"declarations": declarations, "notes": "",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}
