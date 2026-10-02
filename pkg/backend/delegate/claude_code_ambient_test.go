package delegate

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/ambient"
	"github.com/SocialGouv/iterion/pkg/backend/delegate/claudesdk"
)

// unsetSettingSourcesEnv makes ITERION_CLAUDE_CODE_SETTING_SOURCES absent for
// the test (t.Setenv restores the original value afterwards): an EMPTY value
// is not absent, it is the legacy "load nothing".
func unsetSettingSourcesEnv(t *testing.T) {
	t.Helper()
	t.Setenv(settingSourcesEnv, "")
	if err := os.Unsetenv(settingSourcesEnv); err != nil {
		t.Fatal(err)
	}
}

// ambientTree lays out the measured shape of a laptop: a home, a directory
// above the repository, the repository and a sub-directory as the work dir.
func ambientTree(t *testing.T) (home, lab, repo, work string) {
	t.Helper()
	tmp, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	home = filepath.Join(tmp, "home")
	lab = filepath.Join(home, "lab")
	repo = filepath.Join(lab, "repo")
	work = filepath.Join(repo, "sub")
	for _, d := range []string{filepath.Join(home, ".claude"), filepath.Join(repo, ".git"), work} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return home, lab, repo, work
}

func TestClaudeAmbientTranslatesEveryPolicy(t *testing.T) {
	unsetSettingSourcesEnv(t)
	home, lab, repo, work := ambientTree(t)
	user := []claudesdk.SettingSource{claudesdk.SettingSourceUser, claudesdk.SettingSourceProject}
	project := []claudesdk.SettingSource{claudesdk.SettingSourceProject}

	all := claudeAmbient(Task{WorkDir: work, AmbientContext: ambient.All})
	if !reflect.DeepEqual(all.sources, user) || all.excludes != nil {
		t.Errorf("all: sources=%v excludes=%v, want user,project and nothing excluded", all.sources, all.excludes)
	}

	ws := claudeAmbient(Task{WorkDir: work, AmbientContext: ambient.Workspace})
	if !reflect.DeepEqual(ws.sources, project) {
		t.Errorf("workspace: sources=%v, want project only (the operator's user scope stays out)", ws.sources)
	}
	for _, want := range []string{
		filepath.Join(home, ".claude", "CLAUDE.md"),   // the operator's memory, reached by the walk-up
		filepath.Join(home, ".claude", "rules", "**"), // and the operator's rules
		filepath.Join(lab, "CLAUDE.md"),               // a directory above the repository
		filepath.Join(home, "CLAUDE.md"),
		filepath.Join(string(filepath.Separator), "CLAUDE.md"),
	} {
		if !slices.Contains(ws.excludes, want) {
			t.Errorf("workspace: excludes lack %s: %v", want, ws.excludes)
		}
	}
	for _, e := range ws.excludes {
		if strings.HasPrefix(e, repo+string(filepath.Separator)) || e == "**" {
			t.Errorf("workspace: exclude %q reaches the repository's own files", e)
		}
	}

	op := claudeAmbient(Task{WorkDir: work, AmbientContext: ambient.Operator})
	if !reflect.DeepEqual(op.sources, user) || !reflect.DeepEqual(op.excludes, []string{filepath.Join(repo, "**")}) {
		t.Errorf("operator: sources=%v excludes=%v, want user,project minus %s/**", op.sources, op.excludes, repo)
	}

	none := claudeAmbient(Task{WorkDir: work, AmbientContext: ambient.None})
	if !reflect.DeepEqual(none.sources, project) || !reflect.DeepEqual(none.excludes, []string{"**"}) {
		t.Errorf("none: sources=%v excludes=%v, want project (skills and plugins) minus every memory file", none.sources, none.excludes)
	}

	for _, p := range []ambient.Policy{ambient.All, ambient.Workspace, ambient.Operator, ambient.None} {
		if a := claudeAmbient(Task{WorkDir: work, AmbientContext: p}); a.legacy || a.sources == nil {
			t.Errorf("%v: legacy=%v sources=%v — the policy must decide, and always emit the flag", p, a.legacy, a.sources)
		}
	}
}

func TestClaudeAmbientLegacyVariableOverridesThePolicy(t *testing.T) {
	_, _, _, work := ambientTree(t)

	t.Setenv(settingSourcesEnv, "user,project")
	got := claudeAmbient(Task{WorkDir: work, AmbientContext: ambient.None})
	if !got.legacy || got.excludes != nil || !reflect.DeepEqual(got.sources, []claudesdk.SettingSource{claudesdk.SettingSourceUser, claudesdk.SettingSourceProject}) {
		t.Errorf("legacy user,project: %+v, want the scopes verbatim and no exclusion", got)
	}

	for _, none := range []string{"none", "", " NONE "} {
		t.Setenv(settingSourcesEnv, none)
		got := claudeAmbient(Task{WorkDir: work})
		if !got.legacy || got.sources == nil || len(got.sources) != 0 {
			t.Errorf("legacy %q: sources=%#v, want an empty, non-nil list (`--setting-sources \"\"`)", none, got.sources)
		}
	}

	t.Setenv(settingSourcesEnv, "user,usr")
	got = claudeAmbient(Task{WorkDir: work})
	if !reflect.DeepEqual(got.unknown, []string{"usr"}) {
		t.Errorf("unknown tokens = %v, want [usr]", got.unknown)
	}
}

// TestClaudeSpawnsCarryTheAmbientPolicy drives the REAL Execute against the
// stand-in CLI: both the session and the structured-output pass must load
// the policy's scopes, and carry its exclusions in the flag settings file.
func TestClaudeSpawnsCarryTheAmbientPolicy(t *testing.T) {
	unsetSettingSourcesEnv(t)
	cases := []struct {
		policy      ambient.Policy
		sources     string
		wantExclude bool
		allExcluded bool
	}{
		{ambient.All, "user,project", false, false},
		{ambient.Workspace, "project", true, false},
		{ambient.Operator, "user,project", true, false},
		{ambient.None, "project", true, true},
	}
	for _, c := range cases {
		t.Run(c.policy.String(), func(t *testing.T) {
			rec := spawnRecorded(t, Task{
				NodeID:         "n",
				AmbientContext: c.policy,
				OutputSchema:   []byte(`{"type":"object","properties":{"ok":{"type":"boolean"}}}`),
			})
			if len(rec.argv) < 2 {
				t.Fatalf("expected the session and the formatting pass, got %d spawn(s): %v", len(rec.argv), rec.argv)
			}
			for i, argv := range rec.argv {
				if got := argAfter(argv, "--setting-sources"); got != c.sources {
					t.Errorf("spawn #%d: --setting-sources %q, want %q (argv: %s)", i+1, got, c.sources, argv)
				}
				var settings map[string]any
				if err := json.Unmarshal([]byte(rec.settings[i]), &settings); err != nil {
					t.Fatalf("spawn #%d: the flag settings are not JSON: %v (%s)", i+1, err, rec.settings[i])
				}
				excludes, has := settings["claudeMdExcludes"].([]any)
				if has != c.wantExclude {
					t.Errorf("spawn #%d: claudeMdExcludes present=%v, want %v (%s)", i+1, has, c.wantExclude, rec.settings[i])
				}
				if c.allExcluded && (len(excludes) != 1 || excludes[0] != "**") {
					t.Errorf("spawn #%d: claudeMdExcludes=%v, want [\"**\"]", i+1, excludes)
				}
			}
		})
	}
}

// argAfter returns the word following flag in a space-joined argv.
func argAfter(argv, flag string) string {
	words := strings.Fields(argv)
	for i, w := range words {
		if w == flag && i+1 < len(words) {
			return words[i+1]
		}
	}
	return "<absent>"
}

// excludeMatches is the CLI's reading of our two exclusion shapes: a path
// ending in "/**" matches every file under it, anything else one exact file.
func excludeMatches(exclude, file string) bool {
	if exclude == "**" {
		return true
	}
	if prefix, ok := strings.CutSuffix(exclude, "**"); ok {
		return strings.HasPrefix(file, prefix)
	}
	return exclude == file
}

// TestClaudeAmbientKeepsANestedWorktreesOwnFiles covers a session or run
// worktree nested in its main checkout's .claude/worktrees/: the ancestors'
// exclusions must not reach the worktree's own memory files, and `operator`
// must keep the main checkout's files out too, since they are the same
// repository's.
func TestClaudeAmbientKeepsANestedWorktreesOwnFiles(t *testing.T) {
	unsetSettingSourcesEnv(t)
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not on PATH")
	}
	tmp, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	main := filepath.Join(tmp, "main")
	wt := filepath.Join(main, ".claude", "worktrees", "w")
	for _, args := range [][]string{
		{"init", "-q", "-b", "main", main},
		{"-C", main, "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-q", "--allow-empty", "-m", "init"},
		{"-C", main, "worktree", "add", "-q", "--detach", wt},
	} {
		if out, err := exec.Command(git, args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	own := []string{filepath.Join(wt, "CLAUDE.md"), filepath.Join(wt, ".claude", "CLAUDE.md"), filepath.Join(wt, ".claude", "rules", "r.md")}

	ws := claudeAmbient(Task{WorkDir: wt, AmbientContext: ambient.Workspace})
	for _, f := range own {
		for _, e := range ws.excludes {
			if excludeMatches(e, f) {
				t.Errorf("workspace: exclude %q hides the worktree's own %s", e, f)
			}
		}
	}
	mainFile := filepath.Join(main, "CLAUDE.md")
	if !slices.ContainsFunc(ws.excludes, func(e string) bool { return excludeMatches(e, mainFile) }) {
		t.Errorf("workspace: the main checkout's CLAUDE.md, an ancestor, is not excluded: %v", ws.excludes)
	}

	op := claudeAmbient(Task{WorkDir: wt, AmbientContext: ambient.Operator})
	for _, f := range append(own, mainFile, filepath.Join(main, ".claude", "rules", "m.md")) {
		if !slices.ContainsFunc(op.excludes, func(e string) bool { return excludeMatches(e, f) }) {
			t.Errorf("operator: %s is repository content and must be excluded: %v", f, op.excludes)
		}
	}
}
