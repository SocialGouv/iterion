package ciguard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/gittest"
)

// The behaviour of scripts/ci/tap-entry-check.sh, driven the way the brand
// step drives it: a real git checkout, the step's environment, and a stand-in
// for the one API call it makes. Every refusal below is a way a code change
// could otherwise ride a Homebrew tap update's skipped checks into main.

const (
	tapCheckScript = "../../scripts/ci/tap-entry-check.sh"
	tapRepo        = "SocialGouv/fixture"
	tapToken       = "fixture-token"
)

type tapCheckResult struct {
	code int
	out  string
}

// scriptEnv is the environment the scripts under test run with: PATH and a
// HOME of their own, nothing else from the host — no token, no GITHUB_*, no
// proxy, no git config. Fixture git commands go through internal/gittest.
func scriptEnv(t *testing.T) []string {
	t.Helper()
	return []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + t.TempDir(),
		"GIT_CONFIG_NOSYSTEM=1",
		"LC_ALL=C",
	}
}

func writeFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for path, body := range files {
		full := filepath.Join(dir, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// commit writes files, removes paths, and commits; it returns the new HEAD.
func commit(t *testing.T, dir, msg string, files map[string]string, remove ...string) string {
	t.Helper()
	writeFiles(t, dir, files)
	for _, p := range remove {
		gittest.Run(t, dir, "rm", "-q", p)
	}
	gittest.Run(t, dir, "add", "-A")
	gittest.Run(t, dir, "commit", "-q", "-m", msg)
	return gittest.Run(t, dir, "rev-parse", "HEAD")
}

// tapCheckout is a repository holding the tap, a Go file and a note, with one
// commit: the base every entry below is built on.
func tapCheckout(t *testing.T) (dir, base string) {
	t.Helper()
	dir = t.TempDir()
	gittest.InitRepo(t, dir)
	base = commit(t, dir, "base", map[string]string{
		"Formula/iterion.rb":      "version \"1.0.0\"\n",
		"Cask/iterion-desktop.rb": "version \"1.0.0\"\n",
		"main.go":                 "package main\n",
		"notes.txt":               strings.Repeat("a note that is long enough to be paired as a rename\n", 8),
	})
	return dir, base
}

// rulesServer answers the rules of `main` with the given merge-queue grouping
// strategies — one merge_queue rule per entry, none for an empty list — and
// a 500 for status 500. It refuses a request without the step's token.
func rulesServer(t *testing.T, status int, strategies ...string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+tapToken {
			http.Error(w, "no token", http.StatusUnauthorized)
			return
		}
		if r.URL.Path != "/repos/"+tapRepo+"/rules/branches/main" {
			http.NotFound(w, r)
			return
		}
		if status != http.StatusOK {
			http.Error(w, "boom", status)
			return
		}
		rules := []any{map[string]any{"type": "required_status_checks"}}
		for _, s := range strategies {
			rules = append(rules, map[string]any{
				"type":       "merge_queue",
				"parameters": map[string]any{"grouping_strategy": s},
			})
		}
		_ = json.NewEncoder(w).Encode(rules)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func runTapCheck(t *testing.T, dir string, env map[string]string) tapCheckResult {
	t.Helper()
	script, err := filepath.Abs(tapCheckScript)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", script)
	cmd.Dir = dir
	cmd.Env = scriptEnv(t)
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	out, err := cmd.CombinedOutput()
	code := 0
	if err != nil {
		ee, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("run %s: %v", script, err)
		}
		code = ee.ExitCode()
	}
	return tapCheckResult{code: code, out: string(out)}
}

// queueEnv is the brand step's environment for a merge-queue entry whose head
// is checked out in dir.
func queueEnv(t *testing.T, dir, base, api string) map[string]string {
	t.Helper()
	return map[string]string{
		"TAP_ONLY":          "true",
		"EVENT_NAME":        "merge_group",
		"QUEUE_BASE_SHA":    base,
		"QUEUE_HEAD_SHA":    gittest.Run(t, dir, "rev-parse", "HEAD"),
		"QUEUE_BASE_REF":    "refs/heads/main",
		"GITHUB_API_URL":    api,
		"GITHUB_REPOSITORY": tapRepo,
		"GH_TOKEN":          tapToken,
	}
}

// pullRequestCheckout builds what actions/checkout hands the brand step on a
// pull request: the test merge of a head carrying files into the base.
func pullRequestCheckout(t *testing.T, files map[string]string) (dir, head string) {
	t.Helper()
	dir, base := tapCheckout(t)
	gittest.Run(t, dir, "checkout", "-q", "-b", "pr")
	head = commit(t, dir, "chore(brew): update tap to v1.1.0", files)
	gittest.Run(t, dir, "checkout", "-q", "--detach", base)
	gittest.Run(t, dir, "merge", "-q", "--no-ff", "-m", "test merge", "pr")
	return dir, head
}

func expectVerified(t *testing.T, r tapCheckResult) {
	t.Helper()
	if r.code != 0 {
		t.Fatalf("want exit 0 (verified), got %d:\n%s", r.code, r.out)
	}
}

func expectRefused(t *testing.T, r tapCheckResult, because string) {
	t.Helper()
	if r.code != 1 {
		t.Fatalf("want exit 1 (refused), got %d:\n%s", r.code, r.out)
	}
	if !strings.Contains(r.out, because) {
		t.Fatalf("refused, but the output does not say %q:\n%s", because, r.out)
	}
}

func TestTapCheckLetsAnythingElseThrough(t *testing.T) {
	// TAP_ONLY false: the heavy jobs ran, so there is nothing to verify, and
	// nothing is read — not even a repository.
	expectVerified(t, runTapCheck(t, t.TempDir(), map[string]string{"TAP_ONLY": "false"}))
}

func TestTapCheckVerifiesAQueueEntryTouchingOnlyTheTap(t *testing.T) {
	dir, base := tapCheckout(t)
	commit(t, dir, "chore(brew): update tap to v1.1.0 (#1)", map[string]string{
		"Formula/iterion.rb":      "version \"1.1.0\"\n",
		"Cask/iterion-desktop.rb": "version \"1.1.0\"\n",
	})
	r := runTapCheck(t, dir, queueEnv(t, dir, base, rulesServer(t, 200, "ALLGREEN")))
	expectVerified(t, r)
	if !strings.Contains(r.out, "2 path(s)") {
		t.Fatalf("the verdict does not count the two tap files:\n%s", r.out)
	}
}

func TestTapCheckRefusesAQueueEntryReachingPastTheTap(t *testing.T) {
	dir, base := tapCheckout(t)
	commit(t, dir, "chore(brew): update tap to v1.1.0 (#1)", map[string]string{
		"Formula/iterion.rb": "version \"1.1.0\"\n",
		"main.go":            "package main\n\nfunc init() {}\n",
	})
	expectRefused(t, runTapCheck(t, dir, queueEnv(t, dir, base, rulesServer(t, 200, "ALLGREEN"))), "main.go")
}

func TestTapCheckRefusesATapReshaped(t *testing.T) {
	// A tap update moves version and sha256 values; a line added to the
	// formula — or a comment on its version line — is not one.
	for name, formula := range map[string]string{
		"a line added":                 "version \"1.1.0\"\nrevision 1\n",
		"a comment on the version":     "version \"1.1.0\" # pinned by hand\n",
		"a version line moved apart":   "  version  \"1.1.0\"\n\n",
		"the version line re-indented": "  version \"1.1.0\"\n",
		"a blank line appended":        "version \"1.1.0\"\n\n",
	} {
		t.Run(name, func(t *testing.T) {
			dir, base := tapCheckout(t)
			commit(t, dir, "chore(brew): update tap to v1.1.0 (#1)", map[string]string{"Formula/iterion.rb": formula})
			expectRefused(t, runTapCheck(t, dir, queueEnv(t, dir, base, rulesServer(t, 200, "ALLGREEN"))), "more than the version and sha256 values")
		})
	}
}

func TestTapCheckRefusesValuesUpdateBrewTapNeverWrites(t *testing.T) {
	// Only a dotted numeric version and a 64-hex digest are tap values: a
	// Ruby interpolation runs on `brew install`, an empty digest checks
	// nothing.
	digest := strings.Repeat("a", 64)
	for name, change := range map[string][2]string{
		"an interpolated version": {"version \"#{%x(id)}1.1.0\"\n", "sha256 \"" + digest + "\"\n"},
		"an empty digest":         {"version \"1.1.0\"\n", "sha256 \"\"\n"},
	} {
		t.Run(name, func(t *testing.T) {
			dir, _ := tapCheckout(t)
			base := commit(t, dir, "a canonical tap", map[string]string{"Cask/iterion-desktop.rb": "version \"1.0.0\"\nsha256 \"" + digest + "\"\n"})
			commit(t, dir, "chore(brew): update tap to v1.1.0 (#1)", map[string]string{"Cask/iterion-desktop.rb": change[0] + change[1]})
			expectRefused(t, runTapCheck(t, dir, queueEnv(t, dir, base, rulesServer(t, 200, "ALLGREEN"))), "more than the version and sha256 values")
		})
	}
}

func TestTapCheckVerifiesWhatUpdateBrewTapWrites(t *testing.T) {
	// The writer and the verifier agree: update-brew-tap.sh's real output on
	// the repository's real formula and cask is a tap update.
	root, artefacts := tapTree(t, "1.0.0", "1.0.0")
	gittest.InitRepo(t, root)
	gittest.Run(t, root, "add", "-A")
	gittest.Run(t, root, "commit", "-q", "-m", "the tap at 1.0.0")
	base := gittest.Run(t, root, "rev-parse", "HEAD")
	updateTap(t, root, artefacts, "1.1.0")
	gittest.Run(t, root, "commit", "-q", "-am", "chore(brew): update tap to v1.1.0 (#1)")
	r := runTapCheck(t, root, queueEnv(t, root, base, rulesServer(t, 200, "ALLGREEN")))
	expectVerified(t, r)
	if !strings.Contains(r.out, "2 path(s)") {
		t.Fatalf("the real update moved %s, want both tap files:\n%s", "fewer", r.out)
	}
}

func TestTapCheckReadsTheRulesWithAnIsolatedPython(t *testing.T) {
	// The checkout is the queue head: a json.py an entry ahead planted at its
	// root must not answer for the rules API.
	dir, base := tapCheckout(t)
	commit(t, dir, "chore(brew): update tap to v1.1.0 (#1)", map[string]string{"Formula/iterion.rb": "version \"1.1.0\"\n"})
	writeFiles(t, dir, map[string]string{"json.py": "def load(_):\n    return [{\"type\": \"merge_queue\", \"parameters\": {\"grouping_strategy\": \"ALLGREEN\"}}]\n"})
	expectRefused(t, runTapCheck(t, dir, queueEnv(t, dir, base, rulesServer(t, 200, "HEADGREEN"))), "not ALLGREEN")
}

func TestTapCheckRefusesALineEndingRewrite(t *testing.T) {
	// One CR after a version bump: brew reads the line differently, and text
	// mode would have translated it away.
	dir, base := tapCheckout(t)
	commit(t, dir, "chore(brew): update tap to v1.1.0 (#1)", map[string]string{"Formula/iterion.rb": "version \"1.1.0\"\r\n"})
	expectRefused(t, runTapCheck(t, dir, queueEnv(t, dir, base, rulesServer(t, 200, "ALLGREEN"))), "more than the version and sha256 values")
}

func TestTapCheckRefusesAModeChange(t *testing.T) {
	dir, base := tapCheckout(t)
	writeFiles(t, dir, map[string]string{"Formula/iterion.rb": "version \"1.1.0\"\n"})
	if err := os.Chmod(filepath.Join(dir, "Formula", "iterion.rb"), 0o755); err != nil {
		t.Fatal(err)
	}
	gittest.Run(t, dir, "add", "-A")
	gittest.Run(t, dir, "commit", "-q", "-m", "chore(brew): update tap to v1.1.0 (#1)")
	expectRefused(t, runTapCheck(t, dir, queueEnv(t, dir, base, rulesServer(t, 200, "ALLGREEN"))), "more than the version and sha256 values")
}

func TestTapCheckIgnoresAPlantedPythonPath(t *testing.T) {
	// A re.py on PYTHONPATH that ends python with success would make every
	// tap file pass; the checks run python isolated (-I), so it is never
	// imported.
	dir, base := tapCheckout(t)
	commit(t, dir, "chore(brew): update tap to v1.1.0 (#1)", map[string]string{"Formula/iterion.rb": "version \"1.1.0\"\nrevision 1\n"})
	planted := t.TempDir()
	writeFiles(t, planted, map[string]string{"re.py": "raise SystemExit(0)\n"})
	env := queueEnv(t, dir, base, rulesServer(t, 200, "ALLGREEN"))
	env["PYTHONPATH"] = planted
	expectRefused(t, runTapCheck(t, dir, env), "more than the version and sha256 values")
}

func TestTapCheckMatchesTheTapPathsExactly(t *testing.T) {
	// A path that merely starts like a tap file is not one.
	dir, base := tapCheckout(t)
	commit(t, dir, "chore(brew): update tap to v1.1.0 (#1)", map[string]string{
		"Formula/iterion.rb.orig": "anything\n",
	})
	expectRefused(t, runTapCheck(t, dir, queueEnv(t, dir, base, rulesServer(t, 200, "ALLGREEN"))), "Formula/iterion.rb.orig")
}

func TestTapCheckSeesAFileMovedIntoTheTap(t *testing.T) {
	// With rename detection, a file moved onto a tap path lists as the tap
	// path alone, and the deletion it carries vanishes from the diff.
	dir, _ := tapCheckout(t)
	notes, err := os.ReadFile(filepath.Join(dir, "notes.txt"))
	if err != nil {
		t.Fatal(err)
	}
	base := commit(t, dir, "drop the formula", nil, "Formula/iterion.rb")
	commit(t, dir, "chore(brew): update tap to v1.1.0 (#1)", map[string]string{"Formula/iterion.rb": string(notes)}, "notes.txt")
	expectRefused(t, runTapCheck(t, dir, queueEnv(t, dir, base, rulesServer(t, 200, "ALLGREEN"))), "notes.txt")
}

func TestTapCheckRefusesAQueueThatDoesNotGroupAllGreen(t *testing.T) {
	cases := map[string][]string{
		"HEADGREEN":                     {"HEADGREEN"},
		"two queue rules that disagree": {"ALLGREEN", "HEADGREEN"},
		"no merge-queue rule":           nil,
	}
	for name, strategies := range cases {
		t.Run(name, func(t *testing.T) {
			dir, base := tapCheckout(t)
			commit(t, dir, "chore(brew): update tap to v1.1.0 (#1)", map[string]string{"Formula/iterion.rb": "version \"1.1.0\"\n"})
			expectRefused(t, runTapCheck(t, dir, queueEnv(t, dir, base, rulesServer(t, 200, strategies...))), "not ALLGREEN")
		})
	}
}

func TestTapCheckRefusesWhenTheRulesCannotBeRead(t *testing.T) {
	dir, base := tapCheckout(t)
	commit(t, dir, "chore(brew): update tap to v1.1.0 (#1)", map[string]string{"Formula/iterion.rb": "version \"1.1.0\"\n"})
	expectRefused(t, runTapCheck(t, dir, queueEnv(t, dir, base, rulesServer(t, 500))), "cannot read the rules")
}

func TestTapCheckRefusesAQueueEntryItDidNotCheckOut(t *testing.T) {
	dir, base := tapCheckout(t)
	commit(t, dir, "chore(brew): update tap to v1.1.0 (#1)", map[string]string{"Formula/iterion.rb": "version \"1.1.0\"\n"})
	env := queueEnv(t, dir, base, rulesServer(t, 200, "ALLGREEN"))
	env["QUEUE_HEAD_SHA"] = base
	expectRefused(t, runTapCheck(t, dir, env), "would judge another tree")
}

func TestTapCheckVerifiesAPullRequestTouchingOnlyTheTap(t *testing.T) {
	dir, head := pullRequestCheckout(t, map[string]string{"Cask/iterion-desktop.rb": "version \"1.1.0\"\n"})
	expectVerified(t, runTapCheck(t, dir, map[string]string{
		"TAP_ONLY": "true", "EVENT_NAME": "pull_request", "PR_HEAD_SHA": head,
	}))
}

func TestTapCheckRefusesAPullRequestReachingPastTheTap(t *testing.T) {
	dir, head := pullRequestCheckout(t, map[string]string{
		"Cask/iterion-desktop.rb": "version \"1.1.0\"\n",
		"main.go":                 "package main\n\nfunc init() {}\n",
	})
	expectRefused(t, runTapCheck(t, dir, map[string]string{
		"TAP_ONLY": "true", "EVENT_NAME": "pull_request", "PR_HEAD_SHA": head,
	}), "main.go")
}

func TestTapCheckRefusesAPullRequestCheckoutThatIsNotItsTestMerge(t *testing.T) {
	t.Run("a plain commit", func(t *testing.T) {
		dir, head := pullRequestCheckout(t, map[string]string{"Cask/iterion-desktop.rb": "version \"1.1.0\"\n"})
		gittest.Run(t, dir, "checkout", "-q", "--detach", head)
		expectRefused(t, runTapCheck(t, dir, map[string]string{
			"TAP_ONLY": "true", "EVENT_NAME": "pull_request", "PR_HEAD_SHA": head,
		}), "test merge")
	})
	t.Run("the merge of another head", func(t *testing.T) {
		dir, _ := pullRequestCheckout(t, map[string]string{"Cask/iterion-desktop.rb": "version \"1.1.0\"\n"})
		expectRefused(t, runTapCheck(t, dir, map[string]string{
			"TAP_ONLY": "true", "EVENT_NAME": "pull_request", "PR_HEAD_SHA": strings.Repeat("e", 40),
		}), "second parent")
	})
}

func TestTapCheckRefusesInputsItCannotRead(t *testing.T) {
	cases := map[string]struct {
		env     map[string]string
		because string
	}{
		"TAP_ONLY missing":         {map[string]string{}, "TAP_ONLY is ''"},
		"TAP_ONLY not a boolean":   {map[string]string{"TAP_ONLY": "True"}, "TAP_ONLY is 'True'"},
		"the skip taken on a push": {map[string]string{"TAP_ONLY": "true", "EVENT_NAME": "push"}, "'push' event"},
		"a queue entry without its event fields": {map[string]string{
			"TAP_ONLY": "true", "EVENT_NAME": "merge_group",
		}, "QUEUE_BASE_SHA is empty"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			dir, _ := tapCheckout(t)
			expectRefused(t, runTapCheck(t, dir, c.env), c.because)
		})
	}
}

func TestTapCheckRefusesAQueueEntryWithoutAToken(t *testing.T) {
	dir, base := tapCheckout(t)
	commit(t, dir, "chore(brew): update tap to v1.1.0 (#1)", map[string]string{"Formula/iterion.rb": "version \"1.1.0\"\n"})
	env := queueEnv(t, dir, base, rulesServer(t, 200, "ALLGREEN"))
	delete(env, "GH_TOKEN")
	expectRefused(t, runTapCheck(t, dir, env), "GH_TOKEN is empty")
}
