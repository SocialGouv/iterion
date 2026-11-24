package ciguard

import (
	"os/exec"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// The ARC canary's verdict, run the way the watchdog runs it: the shipped
// script over the facts the Actions API answers with.
//
// Why it is worth a harness (#983): this verdict is the whole detection layer
// for the scale set going deaf — the 2026-09-29 outage passed unseen for eight
// minutes, and the nights of 2026-10-14..18 waited 5-8 hours for a runner. It
// lived as inline workflow bash, where a threshold, an exemption or a missing
// date could change and nothing in the tree could redden.
const (
	canaryVerdictScript  = "../../scripts/ci/arc-canary-verdict.sh"
	canaryWorkflowPath   = "../../.github/workflows/arc-canary.yml"
	canaryWatchdogJob    = "watchdog"
	canaryProbeCreated   = "2026-10-29T03:00:00Z"
	canaryProbeStarted   = "2026-10-29T03:00:12Z" // a healthy scale set: 12 s
	canaryProbeLate      = "2026-10-29T03:30:00Z" // 1800 s after creation
	canaryProbeAtCeiling = "2026-10-29T03:25:00Z" // exactly 1500 s: the threshold is `>`
)

// canaryVerdict runs the shipped script with the given facts and returns the
// verdict, the detail, the log it wrote to stderr and its exit code.
func canaryVerdict(t *testing.T, facts map[string]string) (verdict, detail, log string, code int) {
	t.Helper()
	cmd := exec.Command("bash", scriptUnderTest(t, canaryVerdictScript))
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
			t.Fatalf("run %s: %v\nstderr: %s", canaryVerdictScript, err, stderr.String())
		}
		code = exit.ExitCode()
	}
	for _, line := range strings.Split(stdout.String(), "\n") {
		switch {
		case strings.HasPrefix(line, "verdict="):
			verdict = strings.TrimPrefix(line, "verdict=")
		case strings.HasPrefix(line, "detail="):
			detail = strings.TrimPrefix(line, "detail=")
		case strings.TrimSpace(line) != "":
			t.Errorf("the script wrote %q to stdout — only verdict= and detail= belong there, the caller parses it", line)
		}
	}
	return verdict, detail, stderr.String(), code
}

// TestTheCanaryVerdictPagesOnAStallAndOnNothingElse holds each rule of the
// verdict to a probe, in both directions: a stall that does not page is an
// outage nobody sees (measured: eight minutes on 2026-09-29, and 5-8 hour
// waits on 2026-10-14..18), and a page on a healthy scale set is what makes an
// alert ignorable.
//
// Mutations seen red, each on its own case (one line of
// scripts/ci/arc-canary-verdict.sh per mutation):
//
//   - STALL_SECONDS raised to 999999: "nothing picked up the probe for 1800 s"
//     and "a runner took it after 1800 s" both go green;
//   - the `cancelled` exemption dropped (the branch folded into the red one):
//     "a human cancelled the probe" pages;
//   - the threshold comparison widened from `-gt` to `-ge`: "exactly at the
//     threshold" pages;
//   - epoch_of answering 0 instead of refusing: "a success whose job never
//     started" reports a nonsensical wait as a verdict instead of stopping.
func TestTheCanaryVerdictPagesOnAStallAndOnNothingElse(t *testing.T) {
	for _, tc := range []struct {
		name        string
		facts       map[string]string
		wantVerdict string
		wantDetail  string // substring; "" means the detail must be empty
		wantCode    int
	}{
		{
			name: "nothing picked up the probe for 1800 s",
			facts: map[string]string{
				"PREV_STATUS": "queued", "PREV_CREATED": canaryProbeCreated, "NOW": canaryProbeLate,
			},
			wantVerdict: "red",
			wantDetail:  "still 'queued' 1800s after creation",
		},
		{
			name: "a runner took the probe 1800 s after it was dispatched",
			facts: map[string]string{
				"PREV_STATUS": "completed", "PREV_CREATED": canaryProbeCreated,
				"JOB_CONCLUSION": "success", "JOB_CREATED": canaryProbeCreated, "JOB_STARTED": canaryProbeLate,
			},
			wantVerdict: "red",
			wantDetail:  "queued 1800s before a runner took it",
		},
		{
			name: "a probe that ran and failed",
			facts: map[string]string{
				"PREV_STATUS": "completed", "PREV_CREATED": canaryProbeCreated,
				"JOB_CONCLUSION": "failure", "JOB_CREATED": canaryProbeCreated, "JOB_STARTED": canaryProbeStarted,
			},
			wantVerdict: "red",
			wantDetail:  "ran and reported 'failure' after 12s in queue",
		},
		{
			// A ghost registration can leave a job that never starts and is
			// reported `skipped` or `timed_out`: the wait is unknown, and the
			// verdict must survive that instead of killing the watchdog.
			name: "a probe reported timed_out that no runner ever started",
			facts: map[string]string{
				"PREV_STATUS": "completed", "PREV_CREATED": canaryProbeCreated,
				"JOB_CONCLUSION": "timed_out", "JOB_CREATED": canaryProbeCreated, "JOB_STARTED": "null",
			},
			wantVerdict: "red",
			wantDetail:  "ran and reported 'timed_out' after an unknown time in queue",
		},
		{
			name: "a healthy scale set starts the probe in seconds",
			facts: map[string]string{
				"PREV_STATUS": "completed", "PREV_CREATED": canaryProbeCreated,
				"JOB_CONCLUSION": "success", "JOB_CREATED": canaryProbeCreated, "JOB_STARTED": canaryProbeStarted,
			},
			wantVerdict: "green",
		},
		{
			// A human cancelling a probe — a doubled cron, an incident worked
			// by hand — is not a stall. Its job has no started_at at all,
			// which is why the exemption is read before any date.
			name: "a cancelled probe is not a stall signal",
			facts: map[string]string{
				"PREV_STATUS": "completed", "PREV_CREATED": canaryProbeCreated,
				"JOB_CONCLUSION": "cancelled", "JOB_CREATED": canaryProbeCreated, "JOB_STARTED": "null",
			},
			wantVerdict: "green",
		},
		{
			name: "a wait exactly at the threshold is ordinary contention",
			facts: map[string]string{
				"PREV_STATUS": "completed", "PREV_CREATED": canaryProbeCreated,
				"JOB_CONCLUSION": "success", "JOB_CREATED": canaryProbeCreated, "JOB_STARTED": canaryProbeAtCeiling,
			},
			wantVerdict: "green",
		},
		{
			name: "a probe still running inside the threshold is not judged yet",
			facts: map[string]string{
				"PREV_STATUS": "in_progress", "PREV_CREATED": canaryProbeCreated, "NOW": canaryProbeAtCeiling,
			},
			wantVerdict: "green",
		},
		{
			name:        "day one has no previous probe",
			facts:       map[string]string{"PREV_STATUS": ""},
			wantVerdict: "skip",
			wantDetail:  "no previous probe",
		},
		{
			name: "a run that carries no canary job is nothing to judge",
			facts: map[string]string{
				"PREV_STATUS": "completed", "PREV_CREATED": canaryProbeCreated, "JOB_CONCLUSION": "",
			},
			wantVerdict: "skip",
			wantDetail:  "no canary job",
		},
		{
			// The expensive silence: a watchdog that reads a missing date as
			// epoch 0 computes a wait of 56 years and pages, or a stall of
			// zero and resolves. Both are verdicts on a guess.
			name: "a success whose job never started is refused, not guessed",
			facts: map[string]string{
				"PREV_STATUS": "completed", "PREV_CREATED": canaryProbeCreated,
				"JOB_CONCLUSION": "success", "JOB_CREATED": canaryProbeCreated, "JOB_STARTED": "",
			},
			wantVerdict: "",
			wantCode:    2,
		},
		{
			name: "a date the clock cannot read is refused",
			facts: map[string]string{
				"PREV_STATUS": "queued", "PREV_CREATED": "the night of the fourteenth",
			},
			wantVerdict: "",
			wantCode:    2,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			verdict, detail, log, code := canaryVerdict(t, tc.facts)
			if code != tc.wantCode {
				t.Fatalf("exit %d, want %d (verdict %q, detail %q)\nlog: %s", code, tc.wantCode, verdict, detail, log)
			}
			if verdict != tc.wantVerdict {
				t.Errorf("verdict %q, want %q (detail %q)\nlog: %s", verdict, tc.wantVerdict, detail, log)
			}
			switch {
			case tc.wantDetail == "" && detail != "":
				t.Errorf("verdict %q carries detail %q — a green or refused verdict says nothing about a stall", verdict, detail)
			case tc.wantDetail != "" && !strings.Contains(detail, tc.wantDetail):
				t.Errorf("detail %q does not contain %q — the issue body is this sentence", detail, tc.wantDetail)
			}
			if tc.wantCode == 2 && !strings.Contains(log, "arc-canary-verdict:") {
				t.Errorf("the refusal does not name itself on stderr: %q", log)
			}
		})
	}
}

// TestTheCanaryWatchdogRunsTheVerdictThisHarnessRuns pins the pair. A harness
// over a script the workflow does not run proves nothing, and this watchdog is
// the only thing that pages on the scale set.
//
// It also holds the two properties the extraction created: the job reads the
// repository (its permissions block REPLACES the workflow default, so a
// missing `contents: read` would fail every run at checkout), and it refuses
// any answer that is neither green, red nor skip rather than falling through
// to one of the branches.
func TestTheCanaryWatchdogRunsTheVerdictThisHarnessRuns(t *testing.T) {
	var wf struct {
		Jobs map[string]struct {
			Permissions map[string]string `yaml:"permissions"`
			Steps       []struct {
				Name string `yaml:"name"`
				Uses string `yaml:"uses"`
				Run  string `yaml:"run"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(mustRead(t, canaryWorkflowPath), &wf); err != nil {
		t.Fatalf("parse %s: %v", canaryWorkflowPath, err)
	}
	job, ok := wf.Jobs[canaryWatchdogJob]
	if !ok {
		t.Fatalf("%s defines no job %q — the watchdog these guards read is gone", canaryWorkflowPath, canaryWatchdogJob)
	}
	var body string
	checkout := false
	for _, st := range job.Steps {
		body += st.Run
		if strings.HasPrefix(st.Uses, "actions/checkout@") {
			checkout = true
		}
	}
	if !strings.Contains(body, "./scripts/ci/arc-canary-verdict.sh") {
		t.Errorf("job %q does not run %s — the verdict would be inline again, and this harness would exercise a file nothing calls", canaryWatchdogJob, canaryVerdictScript)
	}
	if !checkout {
		t.Errorf("job %q checks nothing out but runs a script from the tree", canaryWatchdogJob)
	}
	if job.Permissions["contents"] != "read" {
		t.Errorf("job %q carries permissions %v — the job-level block replaces the workflow default, so without `contents: read` the checkout fails on every period", canaryWatchdogJob, job.Permissions)
	}
	if job.Permissions["issues"] != "write" {
		t.Errorf("job %q cannot file or close the issue it exists to raise (permissions %v)", canaryWatchdogJob, job.Permissions)
	}
	for _, needle := range []string{"verdict=//p", "detail=//p", "skip) exit 0"} {
		if !strings.Contains(body, needle) {
			t.Errorf("job %q does not read %q out of the verdict — a caller that ignores a key acts on a decision nobody made", canaryWatchdogJob, needle)
		}
	}
	// A verdict the script could answer and the caller cannot place must stop
	// the job, not fall into the red or the green branch.
	if !strings.Contains(body, "refusing to decide between paging and resolving") {
		t.Errorf("job %q does not refuse an unknown verdict — a typo in the script would read as one of the two branches", canaryWatchdogJob)
	}
	if !strings.Contains(string(mustRead(t, canaryVerdictScript)), "STALL_SECONDS=1500") {
		t.Errorf("%s no longer carries the 25-minute threshold as one named value — the two stalls it judges would drift apart", canaryVerdictScript)
	}
}
