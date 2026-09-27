package bots

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/gittest"
)

// initAssessedRepo turns a stackWorkspace into the repository under
// assessment, commits the given files and returns the pinned commit. The
// skills directories beside them stay untracked, as the engine leaves them.
func initAssessedRepo(t *testing.T, ws string, files map[string]string) string {
	t.Helper()
	gittest.Run(t, ws, "init", "-q", "-b", "main")
	gittest.Run(t, ws, "config", "user.email", "t@example.com")
	gittest.Run(t, ws, "config", "user.name", "t")
	gittest.Run(t, ws, "config", "commit.gpgsign", "false")
	names := make([]string, 0, len(files))
	for rel, body := range files {
		full := filepath.Join(ws, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		names = append(names, rel)
	}
	gittest.Run(t, ws, append([]string{"add", "--"}, names...)...)
	gittest.Run(t, ws, "commit", "-qm", "the commit under assessment")
	return strings.TrimSpace(gittest.Run(t, ws, "rev-parse", "HEAD"))
}

// A PATH IS READ AS ITSELF by the extractors too. git C-quotes a name carrying
// a byte >= 0x80 (`"cmd/caf\303\251/main.go"`), and the quoted spelling ends
// in `.go"` and matches no suffix: the file drops out of the list with no
// error and no coverage void. A repository whose commands or routes all sit
// under accented paths would measure zero on two of the four axes and publish
// NOT APPLICABLE instead of its letter.
//
// Proven on the scripts this bundle SHIPS, not on a stand-in: the runner sets
// the configuration once, for every script a skill carries now or later.
func TestAssessmentShippedExtractorsReadAccentedPathsAsThemselves(t *testing.T) {
	requireAssessmentTools(t)
	body, err := os.ReadFile(filepath.Join("assessment", "skills", "stack-go.md"))
	if err != nil {
		t.Fatal(err)
	}
	ws := stackWorkspace(t, map[string]string{"stack-go.md": string(body)})
	sha := initAssessedRepo(t, ws, map[string]string{
		"go.mod": "module example.test/app\n\ngo 1.21\n",
		"cmd/café/main.go": "package main\n\nimport \"net/http\"\n\nfunc main() {\n" +
			"\thttp.HandleFunc(\"/a\", nil)\n\thttp.HandleFunc(\"/b\", nil)\n}\n",
	})

	scratch := t.TempDir()
	out := runExtractorsAt(t, ws, scratch, []map[string]any{
		{"id": "go", "evidence": "go.mod", "supported": true}}, sha)
	if !assessmentBool(t, out, "ok") {
		t.Fatalf("the runner refused: %s", assessmentString(t, out, "reason"))
	}
	for name, want := range map[string]map[string]float64{
		"go-commands.json":    {"deployables": 1},
		"go-entrypoints.json": {"entrypoints": 2},
	} {
		raw, err := os.ReadFile(filepath.Join(scratch, name))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		var document struct {
			Facts map[string]float64 `json:"facts"`
		}
		if err := json.Unmarshal(raw, &document); err != nil {
			t.Fatalf("%s is not the extractor's JSON: %v\n%s", name, err, raw)
		}
		for metric, value := range want {
			if document.Facts[metric] != value {
				t.Errorf("%s measured %s = %v, want %v: the file under an accented path was "+
					"listed C-quoted and dropped out of the count in silence\n%s",
					name, metric, document.Facts[metric], value, raw)
			}
		}
	}
}

// THE EXTRACTORS READ UNDER THE SAME GIT ENVIRONMENT AS EVERY OTHER NODE. An
// ambient GIT_DIR points `git -C <workspace>` at another object store, so the
// pinned commit is not found there and every extractor errors — or, worse,
// finds a commit of the same name. And the engine passes its own git entries
// (safe.directory in the sandbox) through GIT_CONFIG_COUNT: the runner appends
// to that channel, and a runner that wrote slot 0 would erase the entry that
// lets git open the workspace at all.
func TestAssessmentExtractorsReadUnderTheNeutralisedGitEnvironment(t *testing.T) {
	requireAssessmentTools(t)
	probeSkill := "---\nname: stack-probe\ndescription: synthetic fixture\n---\n\n" +
		"<!-- iterion:extractors\n" +
		`[{"id":"env","output":"probe-env.json","emits":[],"interpreter":"python3"}]` +
		"\n-->\n\n" +
		"<!-- iterion:script env -->\n\n" +
		"```python\n" +
		"import json, os, subprocess\n" +
		"ws, sha = os.environ[\"WORKSPACE_DIR\"], os.environ[\"BASE_SHA\"]\n" +
		"def config(key):\n" +
		"    return subprocess.run([\"git\", \"-C\", ws, \"config\", \"--get\", key],\n" +
		"                          capture_output=True, text=True).stdout.strip()\n" +
		"listed = subprocess.run([\"git\", \"-C\", ws, \"ls-tree\", \"-r\", \"--name-only\", sha],\n" +
		"                        capture_output=True, text=True)\n" +
		"print(json.dumps({\"stack\": \"probe\", \"extractor\": \"env\", \"facts\": {},\n" +
		"    \"canary\": config(\"iterion.canary\"), \"quotepath\": config(\"core.quotepath\"),\n" +
		"    \"listed\": listed.returncode, \"files\": listed.stdout.split()}))\n" +
		"```\n"
	ws := stackWorkspace(t, map[string]string{"stack-probe.md": probeSkill})
	sha := initAssessedRepo(t, ws, map[string]string{"the-assessed-file.txt": "assessed\n"})
	elsewhere, _ := synthRepo(t, map[string]string{"another-repository.txt": "not this one\n"})

	// The hostile ambient environment, set only once both repositories exist:
	// every git call of the test itself would read it too.
	t.Setenv("GIT_DIR", filepath.Join(elsewhere, ".git"))
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "iterion.canary")
	t.Setenv("GIT_CONFIG_VALUE_0", "the-engine-entry")

	scratch := t.TempDir()
	out := runExtractorsAt(t, ws, scratch, []map[string]any{
		{"id": "probe", "evidence": "the-assessed-file.txt", "supported": true}}, sha)
	if !assessmentBool(t, out, "ok") {
		t.Fatalf("the runner refused: %s", assessmentString(t, out, "reason"))
	}
	raw, err := os.ReadFile(filepath.Join(scratch, "probe-env.json"))
	if err != nil {
		t.Fatalf("the probe extractor wrote nothing (errors: %v): %v", out["errors"], err)
	}
	var seen struct {
		Canary    string   `json:"canary"`
		QuotePath string   `json:"quotepath"`
		Listed    int      `json:"listed"`
		Files     []string `json:"files"`
	}
	if err := json.Unmarshal(raw, &seen); err != nil {
		t.Fatalf("probe output: %v\n%s", err, raw)
	}
	if seen.Listed != 0 || len(seen.Files) != 1 || seen.Files[0] != "the-assessed-file.txt" {
		t.Errorf("the extractor did not read the pinned commit of the WORKSPACE (exit %d, files %v): "+
			"an ambient GIT_DIR pointed it at another repository", seen.Listed, seen.Files)
	}
	if seen.Canary != "the-engine-entry" {
		t.Errorf("the engine's own git entry did not survive (%q): the runner wrote over a slot "+
			"somebody else declared instead of appending", seen.Canary)
	}
	if seen.QuotePath != "false" {
		t.Errorf("core.quotePath = %q in the extractor's environment, want false", seen.QuotePath)
	}
}

// A COUNT THAT IS NOT A COUNT IS A REFUSAL. The runner appends its entry after
// the ones the engine declared; with no number to append after, the only
// alternatives are overwriting one of them or guessing, and both are silent.
func TestAssessmentExtractorsRefuseAMalformedGitConfigCount(t *testing.T) {
	requireAssessmentTools(t)
	ws := stackWorkspace(t, map[string]string{"stack-synth.md": aStackSkill})
	t.Setenv("GIT_CONFIG_COUNT", "one")
	out := runExtractors(t, ws, t.TempDir(), []map[string]any{
		{"id": "synth", "evidence": "a", "supported": true}})
	if assessmentBool(t, out, "ok") {
		t.Fatal("the runner appended to a GIT_CONFIG_COUNT that is not a number")
	}
	if !strings.Contains(assessmentString(t, out, "reason"), "GIT_CONFIG_COUNT") {
		t.Errorf("the refusal does not name the variable: %s", assessmentString(t, out, "reason"))
	}
}
