package chart_test

import (
	"os"
	"os/exec"
	"path/filepath"
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
// (runner's knob before or after the server's), and the refusals — an
// unset knob, a zero, a non-integer, a missing file, and the runner's own
// deadline, which is not the server's.
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
		_, stderr, err := runKindWait(t, valuesFile(t, "server:\n  progressDeadlineSeconds: many\n"))
		if err == nil {
			t.Fatalf("chart-kind-wait accepted a non-integer deadline")
		}
		if !strings.Contains(stderr, "not an integer") {
			t.Fatalf("refusal does not name the bad value — stderr: %s", strings.TrimSpace(stderr))
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
// deadline through the same helper, and its job cap outlives the derived
// helm cap (32 m) so helm's timeout — not the runner kill — reports a slow
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
	// 1800 s deadline + 120 s margin = 32 m: the job must outlive the cap it
	// installs, or its runner kill replaces helm's verdict with a silence.
	if job.TimeoutMinutes < 33 {
		t.Fatalf("cloud-e2e timeout-minutes = %d — below helm's derived 32 m cap; keep the job above the cap so helm's timeout reports the slow install", job.TimeoutMinutes)
	}
}
