package ciguard

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// quarantineList is the committed flake quarantine every gating `go test`
// skips (#3663).
const quarantineList = "scripts/ci/test-quarantine.tsv"

// quarantineExempt are the gated-leg steps that run `go test` over a FIXED
// test selection rather than a package set, so a quarantined flake cannot be
// among what they run: the freshness gate names its two tests and the live
// compile runs none (`-run '^$'`).
var quarantineExempt = map[string]bool{
	"Generated artefacts are fresh (fail fast)": true,
	"Live e2e compile check":                    true,
}

// Every gating step that runs a package set skips the quarantine, and the
// value it skips is the one scripts/ci/testquarantine printed from the
// committed list: each step is EXECUTED with a fake Go, so what is observed is
// what the shell passes to `go test` — including that a refused list stops
// the step before any test runs, rather than degrading into an empty `-skip` and
// putting every flake back into the gate.
func TestEveryGatingGoTestSkipsTheQuarantine(t *testing.T) {
	var wf struct {
		Jobs map[string]struct {
			Steps []struct {
				Name string `yaml:"name"`
				Run  string `yaml:"run"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(mustRead(t, workflowPaths[0]), &wf); err != nil {
		t.Fatalf("parse %s: %v", workflowPaths[0], err)
	}
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Fatal(err)
	}
	const skipped = "^TestQuarantined$"
	const fakeGo = `#!/bin/sh
case "$1" in
  run)
    case "$*" in
      *./scripts/ci/testquarantine*)
        printf '%s\n' "$@" > "$QUARANTINE_ARGS"
        printf '%s\n' '` + skipped + `'
        exit "$QUARANTINE_EXIT"
        ;;
      *" -tests "*) printf '%s\n' '^(TestChosen)$' ;;
      *) printf '%s\n' 'example.com/chosen/a example.com/chosen/b' ;;
    esac
    ;;
  test)
    { printf '%s\n' "$@"; printf '%s\n' '--end--'; } >> "$TEST_ARGS"
    ;;
  *) exit 93 ;;
esac
`
	checked := 0
	for job := range gatedJobs {
		for _, step := range wf.Jobs[job].Steps {
			if !strings.Contains(step.Run, "go test ") || quarantineExempt[step.Name] {
				continue
			}
			checked++
			for _, exit := range []string{"0", "7"} {
				t.Run(job+"/"+step.Name+"/quarantine-exit-"+exit, func(t *testing.T) {
					dir := t.TempDir()
					if err := os.WriteFile(filepath.Join(dir, "go"), []byte(fakeGo), 0o755); err != nil {
						t.Fatal(err)
					}
					testArgs, quarantineArgs := filepath.Join(dir, "test"), filepath.Join(dir, "quarantine")
					cmd := exec.Command(bash, "--noprofile", "--norc", "-e", "-c", strings.ReplaceAll(step.Run, "${{ matrix.shard }}", "0"))
					cmd.Dir = dir
					cmd.Env = []string{
						"PATH=" + dir + string(os.PathListSeparator) + "/usr/bin:/bin",
						"HOME=" + dir,
						"TEST_ARGS=" + testArgs,
						"QUARANTINE_ARGS=" + quarantineArgs,
						"QUARANTINE_EXIT=" + exit,
					}
					output, runErr := cmd.CombinedOutput()
					if exit != "0" {
						if runErr == nil {
							t.Fatalf("the step succeeded after the quarantine list was refused: %s", output)
						}
						if _, err := os.Stat(testArgs); !os.IsNotExist(err) {
							t.Fatal("go test ran after the quarantine list was refused: every flake would be back in the gate")
						}
						return
					}
					if runErr != nil {
						t.Fatalf("step: %v\n%s", runErr, output)
					}
					raw, err := os.ReadFile(quarantineArgs)
					if err != nil {
						t.Fatalf("the step never asked scripts/ci/testquarantine for its skip: %v", err)
					}
					qargs := strings.Split(strings.TrimSpace(string(raw)), "\n")
					at := slices.Index(qargs, "-list")
					if at < 0 || at+1 == len(qargs) || qargs[at+1] != quarantineList || !slices.Contains(qargs, "-skip") {
						t.Fatalf("the quarantine is not read from %s in -skip mode: %v", quarantineList, qargs)
					}
					rawTests, err := os.ReadFile(testArgs)
					if err != nil {
						t.Fatalf("the step ran no go test: %v", err)
					}
					invocations := strings.Split(strings.TrimSuffix(strings.TrimSpace(string(rawTests)), "--end--"), "--end--\n")
					for _, inv := range invocations {
						args := strings.Split(strings.TrimSpace(inv), "\n")
						at := slices.Index(args, "-skip")
						if at < 0 || at+1 == len(args) || args[at+1] != skipped {
							t.Errorf("go test %v does not skip the quarantine (want -skip %q)", args, skipped)
						}
					}
				})
			}
		}
	}
	if checked < 5 {
		t.Fatalf("checked %d gating go test step(s), want at least 5 (test-unit, test-e2e, race-shard and test-native-exec's two): the harvest no longer matches the workflow", checked)
	}
}

// The nightly `tickets` step accepts only an OPEN ISSUE as a row's owner: the
// issues API also answers for a pull request, so the step is executed with a
// stand-in gh that applies the step's own --jq to canned API documents.
func TestTheNightlyTicketCheckRefusesAPullRequest(t *testing.T) {
	jq, err := exec.LookPath("jq")
	if err != nil {
		t.Fatalf("jq is required by this witness's stand-in gh; it is declared in devbox.json, so run through `devbox run --`: %v", err)
	}
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Fatal(err)
	}
	var wf struct {
		Jobs map[string]struct {
			Steps []struct {
				Name string `yaml:"name"`
				Run  string `yaml:"run"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	const nightly = "../../.github/workflows/test-quarantine.yml"
	if err := yaml.Unmarshal(mustRead(t, nightly), &wf); err != nil {
		t.Fatalf("parse %s: %v", nightly, err)
	}
	var step string
	for _, s := range wf.Jobs["tickets"].Steps {
		if strings.Contains(s.Run, "gh api ") {
			step = s.Run
		}
	}
	if step == "" {
		t.Fatalf("%s: the tickets job reads no ticket", nightly)
	}
	const fakeGo = "#!/bin/sh\nprintf '%s\\n' $TICKETS\n"
	const fakeGh = `#!/bin/sh
[ "$1" = api ] || exit 94
doc="$FIXTURES/${2##*/}.json"
[ "$3" = --jq ] || exit 95
[ -f "$doc" ] || { echo "HTTP 404: Not Found" >&2; exit 1; }
exec jq -r "$4" "$doc"
`
	for _, tc := range []struct {
		name, tickets string
		wantRed       bool
		wantError     string
	}{
		{"open issue", "3624", false, ""},
		{"open pull request", "3624 3621", true, "::error::ticket #3621"},
		{"closed issue", "3512", true, "::error::ticket #3512"},
		{"missing", "9", true, "::error::ticket #9"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, fixtures := t.TempDir(), t.TempDir()
			for name, body := range map[string]string{
				"go": fakeGo, "gh": fakeGh,
			} {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Symlink(jq, filepath.Join(dir, "jq")); err != nil {
				t.Fatal(err)
			}
			for n, doc := range map[string]string{
				"3624": `{"number":3624,"state":"open"}`,
				"3621": `{"number":3621,"state":"open","pull_request":{"url":"https://api.github.com/repos/o/r/pulls/3621"}}`,
				"3512": `{"number":3512,"state":"closed"}`,
			} {
				if err := os.WriteFile(filepath.Join(fixtures, n+".json"), []byte(doc), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			cmd := exec.Command(bash, "--noprofile", "--norc", "-c", step)
			cmd.Dir = dir
			cmd.Env = []string{
				"PATH=" + dir + string(os.PathListSeparator) + "/usr/bin:/bin",
				"HOME=" + dir,
				"GH_REPO=o/r",
				"FIXTURES=" + fixtures,
				"TICKETS=" + tc.tickets,
			}
			out, runErr := cmd.CombinedOutput()
			if !tc.wantRed {
				if runErr != nil {
					t.Fatalf("an open issue reddened the step: %v\n%s", runErr, out)
				}
				return
			}
			if runErr == nil {
				t.Fatalf("tickets %s passed as open issues:\n%s", tc.tickets, out)
			}
			if !strings.Contains(string(out), tc.wantError) {
				t.Fatalf("the step failed without naming the ticket (%q):\n%s", tc.wantError, out)
			}
		})
	}
}
