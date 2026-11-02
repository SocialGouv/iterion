package chart_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// runKindWait runs scripts/chart-kind-wait.sh on a values file and returns
// its stdout, its stderr and the command error, so a refusal is judged on
// its message, not merely on its exit status.
func runKindWait(t *testing.T, values string) (string, string, error) {
	t.Helper()
	script, err := filepath.Abs(filepath.Join("..", "..", "scripts", "chart-kind-wait.sh"))
	if err != nil {
		t.Fatal(err)
	}
	var stdout, stderr strings.Builder
	cmd := exec.Command("sh", script, values)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err = cmd.Run()
	return stdout.String(), stderr.String(), err
}

// TestChartKindWaitFollowsTheValuesDeadline pins the derivation both
// install sites share: the client wait is values-dev's own deadline
// (1800 s) plus the helper's 120 s margin, so the deadline — not pull
// time — decides the install's verdict. Removing the knob from
// values-dev.yaml, or breaking the helper's parse, reds here: the client
// watch no longer covers the deadline it installs.
func TestChartKindWaitFollowsTheValuesDeadline(t *testing.T) {
	stdout, stderr, err := runKindWait(t, filepath.Join("values-dev.yaml"))
	if err != nil {
		t.Fatalf("chart-kind-wait values-dev.yaml: %v — %s", err, strings.TrimSpace(stderr))
	}
	if got, want := strings.TrimSpace(stdout), "1920s"; got != want {
		t.Fatalf("chart-kind-wait values-dev.yaml = %q, want %q — 1800 s deadline + 120 s margin; the client watch must cover the deadline it installs", got, want)
	}
}

// TestChartKindWaitDerivesAndRefuses exercises the helper's contract on
// scratch values files: the margin arithmetic, the two block orders
// (runner's knob before or after the server's), the whitespace forms
// go-yaml tolerates, and the refusals — an unset knob, a zero, a
// non-integer, a missing file, and the runner's own deadline, which is
// not the server's.
func TestChartKindWaitDerivesAndRefuses(t *testing.T) {
	t.Run("deadline plus the 120s margin", func(t *testing.T) {
		stdout, stderr, err := runKindWait(t, valuesFile(t, "mode: cloud\nserver:\n  progressDeadlineSeconds: 600\n"))
		if err != nil {
			t.Fatalf("chart-kind-wait: %v — %s", err, strings.TrimSpace(stderr))
		}
		if got, want := strings.TrimSpace(stdout), "720s"; got != want {
			t.Fatalf("chart-kind-wait = %q, want %q — deadline + 120 s margin", got, want)
		}
	})

	t.Run("server block after the runner block", func(t *testing.T) {
		stdout, stderr, err := runKindWait(t, valuesFile(t, "runner:\n  progressDeadlineSeconds: 30000\nserver:\n  progressDeadlineSeconds: 600\n"))
		if err != nil {
			t.Fatalf("chart-kind-wait: %v — %s", err, strings.TrimSpace(stderr))
		}
		if got, want := strings.TrimSpace(stdout), "720s"; got != want {
			t.Fatalf("chart-kind-wait = %q, want %q — the runner's knob must not be read as the server's", got, want)
		}
	})

	t.Run("server block before the runner block", func(t *testing.T) {
		stdout, stderr, err := runKindWait(t, valuesFile(t, "server:\n  progressDeadlineSeconds: 600\nrunner:\n  progressDeadlineSeconds: 30000\n"))
		if err != nil {
			t.Fatalf("chart-kind-wait: %v — %s", err, strings.TrimSpace(stderr))
		}
		if got, want := strings.TrimSpace(stdout), "720s"; got != want {
			t.Fatalf("chart-kind-wait = %q, want %q — the server's knob must win whichever block comes first", got, want)
		}
	})

	t.Run("a column-0 key go-yaml accepts closes the server block", func(t *testing.T) {
		// go-yaml — helm's parser — accepts top-level keys starting with
		// `_` or a digit; a block nested under one must not keep the
		// previous top key, or the watch is sized from a knob that is not
		// the server's while the install runs on Kubernetes' default.
		stdout, stderr, err := runKindWait(t, valuesFile(t, "server:\n  replicas: 1\n_helpers:\n  progressDeadlineSeconds: 300\n"))
		if err == nil {
			t.Fatalf("chart-kind-wait = %q — it sized the watch from _helpers's knob while the server block has no knob of its own; stderr: %s", strings.TrimSpace(stdout), strings.TrimSpace(stderr))
		}
		if !strings.Contains(stderr, "server.progressDeadlineSeconds") {
			t.Fatalf("refusal does not name server.progressDeadlineSeconds — stderr: %s", strings.TrimSpace(stderr))
		}
		stdout, stderr, err = runKindWait(t, valuesFile(t, "server:\n  progressDeadlineSeconds: 600\n2nd:\n  progressDeadlineSeconds: 300\n"))
		if err != nil {
			t.Fatalf("chart-kind-wait refused a server knob that precedes a digit-keyed block — %s", strings.TrimSpace(stderr))
		}
		if got, want := strings.TrimSpace(stdout), "720s"; got != want {
			t.Fatalf("chart-kind-wait = %q, want %q — the server's own 600 s must win over the digit-keyed block that follows it", got, want)
		}
	})

	t.Run("whitespace go-yaml tolerates is not refused", func(t *testing.T) {
		// go-yaml — the parser helm reads values with — accepts trailing
		// spaces and tabs after the value, a tab between key and value,
		// a tab before a comment, and CRLF line endings; trimming them
		// keeps the refusal path for values helm would refuse too.
		for _, form := range []string{
			"server:\n  progressDeadlineSeconds: 600   \n",
			"server:\n  progressDeadlineSeconds: 600\t\n",
			"server:\n  progressDeadlineSeconds:\t600\n",
			"server:\n  progressDeadlineSeconds: 600\t# trailing comment\n",
			// CRLF, on the value line, on the top-key anchor, and on
			// both — CR is stripped where it lands, and a line ending
			// helm accepts never leaves $1 as `server:\r`.
			"server:\n  progressDeadlineSeconds: 600\r\n",
			"server:\r\n  progressDeadlineSeconds: 600\n",
			"server:\r\n  progressDeadlineSeconds: 600\r\n",
		} {
			stdout, stderr, err := runKindWait(t, valuesFile(t, form))
			if err != nil {
				t.Fatalf("chart-kind-wait refused %q — go-yaml accepts it, so helm installs it; stderr: %s", form, strings.TrimSpace(stderr))
			}
			if got, want := strings.TrimSpace(stdout), "720s"; got != want {
				t.Fatalf("chart-kind-wait = %q, want %q — deadline + 120 s margin on %q", got, want, form)
			}
		}
	})

	t.Run("integer forms go-yaml resolves are not refused", func(t *testing.T) {
		// go-yaml — helm's values parser — resolves each form to the
		// pinned value (verified live through helm template on the chart):
		// refusing one would stop an install helm runs. 0600 is the trap —
		// go-yaml reads a leading-0 form as octal — so the witness pins the
		// converted value, not the decimal misread.
		for _, tc := range []struct{ form, want string }{
			{"+600", "720s"},
			{"1_800", "1920s"},
			{"0x2580", "9720s"},
			{"0o754", "612s"},
			{"0b101", "125s"},
			{"0600", "504s"},
			{"08", "128s"},
			{"600.0", "720s"},
			{"6e2", "720s"},
		} {
			stdout, stderr, err := runKindWait(t, valuesFile(t, "server:\n  progressDeadlineSeconds: "+tc.form+"\n"))
			if err != nil {
				t.Fatalf("chart-kind-wait refused %q — go-yaml resolves it and helm installs it; stderr: %s", tc.form, strings.TrimSpace(stderr))
			}
			if got, want := strings.TrimSpace(stdout), tc.want; got != want {
				t.Fatalf("chart-kind-wait %q = %q, want %q — the watch must follow the value go-yaml hands the install", tc.form, got, want)
			}
		}
	})

	t.Run("negative deadline is refused", func(t *testing.T) {
		_, stderr, err := runKindWait(t, valuesFile(t, "server:\n  progressDeadlineSeconds: -600\n"))
		if err == nil {
			t.Fatalf("chart-kind-wait accepted a negative deadline — a negative deadline cannot budget the install watch")
		}
		if !strings.Contains(stderr, "negative") {
			t.Fatalf("refusal does not name the negative deadline — stderr: %s", strings.TrimSpace(stderr))
		}
	})

	t.Run("a deadline beyond the exact integer range is refused", func(t *testing.T) {
		_, stderr, err := runKindWait(t, valuesFile(t, "server:\n  progressDeadlineSeconds: 123456789012345678901234567890\n"))
		if err == nil {
			t.Fatalf("chart-kind-wait accepted a 30-digit deadline — the watch is printed through awk doubles, which round past 2^53, so the printed wait would not be the deadline the file sets")
		}
		if !strings.Contains(stderr, "exact integer range") {
			t.Fatalf("refusal does not name the range — stderr: %s", strings.TrimSpace(stderr))
		}
	})

	t.Run("unset knob is refused", func(t *testing.T) {
		path := valuesFile(t, "server:\n  replicas: 1\n")
		_, stderr, err := runKindWait(t, path)
		if err == nil {
			t.Fatalf("chart-kind-wait accepted %q — an unset knob means Kubernetes' 600 s default applies and the watch shrinks silently", path)
		}
		for _, want := range []string{"server.progressDeadlineSeconds", path} {
			if !strings.Contains(stderr, want) {
				t.Fatalf("refusal names neither %q nor the file — stderr: %s", want, strings.TrimSpace(stderr))
			}
		}
	})

	t.Run("zero is refused", func(t *testing.T) {
		_, stderr, err := runKindWait(t, valuesFile(t, "server:\n  progressDeadlineSeconds: 0\n"))
		if err == nil {
			t.Fatalf("chart-kind-wait accepted a zero — the templates render the key through a `with` that skips it, Kubernetes' 600 s default applies")
		}
		if !strings.Contains(stderr, "600 s default") {
			t.Fatalf("refusal does not name the 600 s default — stderr: %s", strings.TrimSpace(stderr))
		}
	})

	t.Run("non-integer is refused", func(t *testing.T) {
		// 600.5 and 1:30 — base-60, which go-yaml hands over as a string —
		// render verbatim and the API refuses the Deployment: same verdict
		// here as at the install.
		for _, form := range []string{"many", "600.5", "1:30"} {
			_, stderr, err := runKindWait(t, valuesFile(t, "server:\n  progressDeadlineSeconds: "+form+"\n"))
			if err == nil {
				t.Fatalf("chart-kind-wait accepted %q as a deadline — helm renders it verbatim and the API refuses it", form)
			}
			if !strings.Contains(stderr, "not an integer") {
				t.Fatalf("refusal of %q does not name the bad value — stderr: %s", form, strings.TrimSpace(stderr))
			}
		}
	})

	t.Run("missing file is refused", func(t *testing.T) {
		_, _, err := runKindWait(t, filepath.Join(t.TempDir(), "absent.yaml"))
		if err == nil {
			t.Fatalf("chart-kind-wait accepted a values file that does not exist")
		}
	})

	t.Run("runner knob alone is not the server knob", func(t *testing.T) {
		path := valuesFile(t, "runner:\n  progressDeadlineSeconds: 30000\nserver:\n  replicas: 1\n")
		_, stderr, err := runKindWait(t, path)
		if err == nil {
			t.Fatalf("chart-kind-wait read the runner's deadline out of %q — the server's knob is the one that budgets the server Deployment", path)
		}
		if !strings.Contains(stderr, "server.progressDeadlineSeconds") {
			t.Fatalf("refusal does not name server.progressDeadlineSeconds — stderr: %s", strings.TrimSpace(stderr))
		}
	})
}

// taskfileTasks mirrors the shape TestTheInlinedTasksMatchTheirTaskfileEntry
// reads: task command entries are scalar nodes, block or plain.
type taskfileTasks struct {
	Tasks map[string]struct {
		Cmds []yaml.Node `yaml:"cmds"`
	} `yaml:"tasks"`
}

// TestChartKindInstallDerivesItsWaitFromTheValuesFile is the wiring witness:
// the smoke's install passes the helper's output to --timeout. Reverting the
// task to a literal cap reds here on its own assertion — a fixed cap smaller
// than values-dev's deadline makes helm give up while the server pod is
// still Progressing.
func TestChartKindInstallDerivesItsWaitFromTheValuesFile(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("..", "..", "Taskfile.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var tf taskfileTasks
	if err := yaml.Unmarshal(src, &tf); err != nil {
		t.Fatalf("parse Taskfile.yml: %v", err)
	}
	task, ok := tf.Tasks["chart:kind"]
	if !ok {
		t.Fatalf("Taskfile.yml has no chart:kind task")
	}
	var install string
	for _, c := range task.Cmds {
		if strings.Contains(c.Value, "helm upgrade --install") {
			install = c.Value
		}
	}
	if install == "" {
		t.Fatalf("chart:kind has no `helm upgrade --install` command anymore")
	}
	if !strings.Contains(install, "scripts/chart-kind-wait.sh") {
		t.Fatalf("chart:kind's install no longer calls scripts/chart-kind-wait.sh — the client wait drifts from the values deadline again; cmd: %s", install)
	}
	if !strings.Contains(install, `--timeout "$KIND_WAIT"`) {
		t.Fatalf("chart:kind's install must pass the helper's output to --timeout; cmd: %s", install)
	}
}

// workflowJobs reads the pieces of tests.yml the cloud-e2e witness needs.
type workflowJobs struct {
	Jobs map[string]struct {
		TimeoutMinutes int `yaml:"timeout-minutes"`
		Steps          []struct {
			Name string `yaml:"name"`
			Run  string `yaml:"run"`
		} `yaml:"steps"`
	} `yaml:"jobs"`
}

// TestChartCloudE2EInstallDerivesItsWaitFromTheValuesFile witnesses the CI
// install the same way: cloud-e2e derives its --timeout from values-dev's
// deadline through the same helper, and its job cap outlives what the
// verdict needs — the helm cap derived from values-dev.yaml on top of the
// job's preamble — so helm's timeout, not the runner kill, reports a slow
// install.
func TestChartCloudE2EInstallDerivesItsWaitFromTheValuesFile(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", "tests.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var wf workflowJobs
	if err := yaml.Unmarshal(src, &wf); err != nil {
		t.Fatalf("parse tests.yml: %v", err)
	}
	job, ok := wf.Jobs["cloud-e2e"]
	if !ok {
		t.Fatalf("tests.yml has no cloud-e2e job")
	}
	var install string
	for _, st := range job.Steps {
		if st.Name == "Helm install (values-dev.yaml)" {
			install = st.Run
		}
	}
	if install == "" {
		t.Fatalf("cloud-e2e has no step named \"Helm install (values-dev.yaml)\"")
	}
	if !strings.Contains(install, "scripts/chart-kind-wait.sh") {
		t.Fatalf("cloud-e2e's install no longer calls scripts/chart-kind-wait.sh — a fixed client cap decides the install again; run: %s", install)
	}
	if !strings.Contains(install, `--timeout "${KIND_WAIT}"`) {
		t.Fatalf("cloud-e2e's install must pass the helper's output to --timeout; run: %s", install)
	}
	// The runner kill must not preempt helm's verdict: the derived cap is
	// what the helper prints for values-dev.yaml — derived HERE by running
	// the same helper the install runs, so a deadline bump in values-dev
	// flows into this floor instead of leaving a stale constant behind —
	// and the cap only fires after the job's preamble — checkout, image
	// build, kind up, image load, chart deps — measured at 4.0-4.7 m on
	// recent success runs. The floor sums both budgets, the cap rounded up
	// to the whole minutes the field counts; tests.yml runs the job at
	// 40 m, two above it.
	stdout, stderr, err := runKindWait(t, filepath.Join("values-dev.yaml"))
	if err != nil {
		t.Fatalf("chart-kind-wait values-dev.yaml: %v — %s", err, strings.TrimSpace(stderr))
	}
	helmSeconds, err := strconv.Atoi(strings.TrimSuffix(strings.TrimSpace(stdout), "s"))
	if err != nil {
		t.Fatalf("chart-kind-wait values-dev.yaml printed %q — not a seconds count: %v", strings.TrimSpace(stdout), err)
	}
	helmCapMinutes := (helmSeconds + 59) / 60 // round up: the field counts whole minutes
	const preambleBudgetMinutes = 6
	if job.TimeoutMinutes < helmCapMinutes+preambleBudgetMinutes {
		t.Fatalf("cloud-e2e timeout-minutes = %d — below the %d m a verdict needs (%d m helm cap for values-dev.yaml + %d m preamble budget); a lower cap lets the runner kill replace helm's timeout with a silence", job.TimeoutMinutes, helmCapMinutes+preambleBudgetMinutes, helmCapMinutes, preambleBudgetMinutes)
	}
}
