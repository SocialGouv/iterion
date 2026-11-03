package chart_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// The kind-wait witnesses: scripts/chart-kind-wait.sh sizes the helm
// --timeout of `task chart:kind` and the cloud-e2e job from the
// progressDeadlineSeconds the install's own render gives the server
// Deployment — `helm template` on the install's arguments, read with yq —
// plus a 120 s margin. The helper runs need helm and yq like the render
// tests need helm: `task chart:check` and the helm-lint CI job require
// them (ITERION_CHART_RENDER_REQUIRE=1), a plain `go test ./...` without
// them skips and says so. The wiring witnesses read Taskfile.yml and
// tests.yml and run everywhere.

// yqBinary is helmBinary for yq: the helper reads the render with
// mikefarah/yq, so a machine without it skips the helper runs — and reds
// them where the chart gate requires its tools.
func yqBinary(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("yq"); err != nil {
		if os.Getenv("ITERION_CHART_RENDER_REQUIRE") == "1" {
			t.Fatalf("yq is required here (ITERION_CHART_RENDER_REQUIRE=1) and not in PATH: %v", err)
		}
		t.Skipf("yq not in PATH (%v) — the kind-wait helper runs under `task chart:check` and the helm-lint CI job", err)
	}
}

func kindWaitScript(t *testing.T) string {
	t.Helper()
	script, err := filepath.Abs(filepath.Join("..", "..", "scripts", "chart-kind-wait.sh"))
	if err != nil {
		t.Fatal(err)
	}
	return script
}

// runKindWait runs scripts/chart-kind-wait.sh with the helm arguments given
// — release, chart, then the -f/--set the install passes — and returns its
// stdout, its stderr and the command error, so a refusal is judged on its
// message, not merely on its exit status. Paths are relative to this
// package's directory, where the tests run: the chart is ".".
func runKindWait(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	helmBinary(t)
	yqBinary(t)
	var stdout, stderr strings.Builder
	cmd := exec.Command("sh", append([]string{kindWaitScript(t)}, args...)...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return stdout.String(), stderr.String(), err
}

// mustWait runs the helper and returns what it printed, failing on a
// refusal.
func mustWait(t *testing.T, args ...string) string {
	t.Helper()
	stdout, stderr, err := runKindWait(t, args...)
	if err != nil {
		t.Fatalf("chart-kind-wait %s: %v — %s", strings.Join(args, " "), err, strings.TrimSpace(stderr))
	}
	return strings.TrimSpace(stdout)
}

// mustRefuseWait runs the helper, requires a refusal and returns its
// message, which the caller judges: a refusal must name its cause.
func mustRefuseWait(t *testing.T, args ...string) string {
	t.Helper()
	stdout, stderr, err := runKindWait(t, args...)
	if err == nil {
		t.Fatalf("chart-kind-wait %s printed %q — a refusal was owed, and the install would watch a wait sized from nothing", strings.Join(args, " "), strings.TrimSpace(stdout))
	}
	if strings.TrimSpace(stdout) != "" {
		t.Fatalf("chart-kind-wait %s refused and still printed %q — a caller reading stdout alone would install with it", strings.Join(args, " "), strings.TrimSpace(stdout))
	}
	return stderr
}

// scratchChart writes a chart whose single template is the manifests given,
// verbatim, so the helper's reading of a render is exercised on shapes the
// iterion chart never renders: two server Deployments, a rendered 0 or a
// negative. It returns the chart's path.
func scratchChart(t *testing.T, manifests string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "Chart.yaml"), []byte("apiVersion: v2\nname: scratch\nversion: 0.1.0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "templates"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "templates", "all.yaml"), []byte(manifests), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// serverDeployment is a Deployment the helper selects — labelled
// component=server — with the deadline spelled as given.
func serverDeployment(name, deadline string) string {
	return "apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: " + name + "\n  labels:\n    app.kubernetes.io/component: server\nspec:\n  progressDeadlineSeconds: " + deadline + "\n"
}

// TestChartKindWaitFollowsTheRenderedDeadline pins the derivation both
// install sites share: the client wait is the deadline values-dev.yaml
// renders into the server Deployment (1800 s) plus the helper's 120 s
// margin, so the deadline — not pull time — decides the install's verdict.
// Removing the knob from values-dev.yaml, or sizing the wait from anything
// but the render, reds here: the client watch no longer covers the deadline
// it installs.
func TestChartKindWaitFollowsTheRenderedDeadline(t *testing.T) {
	if got, want := mustWait(t, "iterion", ".", "-f", "values-dev.yaml"), "1920s"; got != want {
		t.Fatalf("chart-kind-wait iterion . -f values-dev.yaml = %q, want %q — the 1800 s deadline values-dev renders plus the 120 s margin; the client watch must cover the deadline it installs", got, want)
	}
}

// TestChartKindWaitDerivesAndRefuses exercises the helper's contract: the
// margin arithmetic on the render, a --set override the file never held,
// the values forms a line reader of the file could not pair with their
// key, and the refusals — a deadline the render leaves out (unset or 0), a
// string or a float, a release with no server Deployment or several, a
// render helm refuses, a 0, a negative or an int32 overflow in the render.
func TestChartKindWaitDerivesAndRefuses(t *testing.T) {
	t.Run("deadline plus the 120s margin", func(t *testing.T) {
		if got, want := mustWait(t, "iterion", ".", "-f", valuesFile(t, "mode: cloud\nserver:\n  progressDeadlineSeconds: 600\n")), "720s"; got != want {
			t.Fatalf("chart-kind-wait = %q, want %q — deadline + 120 s margin", got, want)
		}
	})
	t.Run("a --set override is read from the render, not the file", func(t *testing.T) {
		values := valuesFile(t, "mode: cloud\nserver:\n  progressDeadlineSeconds: 600\n")
		if got, want := mustWait(t, "iterion", ".", "-f", values, "--set", "server.progressDeadlineSeconds=900"), "1020s"; got != want {
			t.Fatalf("chart-kind-wait = %q, want %q — the install renders 900, so the wait follows 900; a reader of the values file would size 720 s for a deadline the install never runs with", got, want)
		}
	})
	t.Run("the forms a line reader could not pair with their key render their deadline", func(t *testing.T) {
		// A %YAML 1.1 directive and the explicit document start, the server
		// block as a flow mapping that outlives its opening line, a literal
		// block scalar entry holding a brace, a quoted key holding a colon:
		// go-yaml reads them all, helm renders 1800, and the render is what
		// the helper reads — no form of the file is its business.
		values := valuesFile(t, "%YAML 1.1\n---\nmode: cloud\nserver: {\n  replicas: 1,\n  progressDeadlineSeconds: 1800 }\nrunner:\n  args:\n    - |\n      {not: a flow}\n    - \"x:y\"\n  \"quoted:key\": 1\n")
		if got, want := mustWait(t, "iterion", ".", "-f", values), "1920s"; got != want {
			t.Fatalf("chart-kind-wait = %q, want %q — helm renders this file with 1800, and the wait follows the render", got, want)
		}
	})
	t.Run("a file helm refuses is refused with helm's cause", func(t *testing.T) {
		// go-yaml reads no %YAML 1.2 document ("found incompatible YAML
		// document"): the install never starts, and the helper says so
		// instead of sizing a watch for it.
		stderr := mustRefuseWait(t, "iterion", ".", "-f", valuesFile(t, "%YAML 1.2\n---\nmode: cloud\nserver:\n  progressDeadlineSeconds: 600\n"))
		for _, want := range []string{"incompatible YAML document", "helm template", "failed"} {
			if !strings.Contains(stderr, want) {
				t.Fatalf("the refusal does not carry %q — helm's own cause and the helper's reading of it are both owed:\n%s", want, stderr)
			}
		}
	})
	t.Run("an unset deadline is refused and named", func(t *testing.T) {
		stderr := mustRefuseWait(t, "iterion", ".", "-f", valuesFile(t, "mode: cloud\n"))
		if !strings.Contains(stderr, "renders no spec.progressDeadlineSeconds") || !strings.Contains(stderr, "600 s default") {
			t.Fatalf("the refusal must name the rendered Deployment without the field and the 600 s default the install would watch:\n%s", stderr)
		}
	})
	t.Run("a zero is skipped by the template and refused as unset", func(t *testing.T) {
		stderr := mustRefuseWait(t, "iterion", ".", "-f", valuesFile(t, "mode: cloud\nserver:\n  progressDeadlineSeconds: 0\n"))
		if !strings.Contains(stderr, "renders no spec.progressDeadlineSeconds") {
			t.Fatalf("the template renders a 0 through a `with` that skips it, so the refusal is the unset one:\n%s", stderr)
		}
	})
	t.Run("the runner's deadline is not the server's", func(t *testing.T) {
		// values.yaml gives the runner Deployment a 30000 s deadline by
		// default, and this file gives it 900: a selection reading any
		// Deployment with the field would print 1020 s or 30120 s.
		stderr := mustRefuseWait(t, "iterion", ".", "-f", valuesFile(t, "mode: cloud\nrunner:\n  progressDeadlineSeconds: 900\n"))
		if !strings.Contains(stderr, "Deployment/iterion renders no spec.progressDeadlineSeconds") {
			t.Fatalf("the refusal must name the server Deployment, not read the runner's:\n%s", stderr)
		}
	})
	t.Run("a string deadline is refused as not an integer", func(t *testing.T) {
		stderr := mustRefuseWait(t, "iterion", ".", "-f", valuesFile(t, "mode: cloud\nserver:\n  progressDeadlineSeconds: \"abc\"\n"))
		if !strings.Contains(stderr, "progressDeadlineSeconds: abc (!!str)") || !strings.Contains(stderr, "not an integer") {
			t.Fatalf("the refusal must name the rendered value, its tag and the cause:\n%s", stderr)
		}
	})
	t.Run("a float deadline is refused as not an integer", func(t *testing.T) {
		stderr := mustRefuseWait(t, "iterion", ".", "-f", valuesFile(t, "mode: cloud\nserver:\n  progressDeadlineSeconds: 600.5\n"))
		if !strings.Contains(stderr, "(!!float)") || !strings.Contains(stderr, "not an integer") {
			t.Fatalf("the refusal must name the float and the cause:\n%s", stderr)
		}
	})
	t.Run("mode local renders no server Deployment", func(t *testing.T) {
		stderr := mustRefuseWait(t, "iterion", ".", "-f", valuesFile(t, "mode: local\n"))
		if !strings.Contains(stderr, "carries no Deployment labelled app.kubernetes.io/component=server") || !strings.Contains(stderr, "StatefulSet/iterion") {
			t.Fatalf("the refusal must say no server Deployment rendered and name what carries the label instead:\n%s", stderr)
		}
	})
	t.Run("several server Deployments are refused, never the first", func(t *testing.T) {
		for _, order := range [][2]string{{"600", "900"}, {"900", "600"}} {
			chart := scratchChart(t, serverDeployment("a", order[0])+"---\n"+serverDeployment("b", order[1]))
			stderr := mustRefuseWait(t, "scratch", chart)
			if !strings.Contains(stderr, "carries 2 Deployments labelled app.kubernetes.io/component=server (a, b)") {
				t.Fatalf("the refusal must count and name the candidates (order %v):\n%s", order, stderr)
			}
		}
	})
	t.Run("a render with no server label at all is refused", func(t *testing.T) {
		stderr := mustRefuseWait(t, "scratch", scratchChart(t, "kind: ConfigMap\nmetadata:\n  name: lone\n"))
		if !strings.Contains(stderr, "carries no Deployment labelled app.kubernetes.io/component=server (the label is on: nothing)") {
			t.Fatalf("the refusal must say nothing in the render carries the label:\n%s", stderr)
		}
	})
	t.Run("a rendered 0 is refused", func(t *testing.T) {
		stderr := mustRefuseWait(t, "scratch", scratchChart(t, serverDeployment("a", "0")))
		if !strings.Contains(stderr, "progressDeadlineSeconds: 0") || !strings.Contains(stderr, "budgets no watch") {
			t.Fatalf("the refusal must name the rendered 0:\n%s", stderr)
		}
	})
	t.Run("a negative deadline is refused", func(t *testing.T) {
		stderr := mustRefuseWait(t, "scratch", scratchChart(t, serverDeployment("a", "-5")))
		if !strings.Contains(stderr, "progressDeadlineSeconds: -5") || !strings.Contains(stderr, "not a positive decimal integer") {
			t.Fatalf("the refusal must name the negative value:\n%s", stderr)
		}
	})
	t.Run("a deadline beyond int32 is refused", func(t *testing.T) {
		for _, v := range []string{"2147483648", "99999999999"} {
			stderr := mustRefuseWait(t, "scratch", scratchChart(t, serverDeployment("a", v)))
			if !strings.Contains(stderr, "progressDeadlineSeconds: "+v) || !strings.Contains(stderr, "beyond the int32") {
				t.Fatalf("the refusal must name %s and the int32 bound:\n%s", v, stderr)
			}
		}
		if got, want := mustWait(t, "scratch", scratchChart(t, serverDeployment("a", "2147483647"))), "2147483767s"; got != want {
			t.Fatalf("chart-kind-wait = %q, want %q — the largest int32 the field holds is a deadline, and its margin stays exact", got, want)
		}
	})
	t.Run("a chart helm cannot read is refused with helm's cause", func(t *testing.T) {
		stderr := mustRefuseWait(t, "iterion", filepath.Join(t.TempDir(), "nochart"))
		if !strings.Contains(stderr, "not found") || !strings.Contains(stderr, "helm template") {
			t.Fatalf("the refusal must carry helm's message and name the render that failed:\n%s", stderr)
		}
	})
	t.Run("the usage is refused without a release and a chart", func(t *testing.T) {
		stderr := mustRefuseWait(t, "iterion")
		if !strings.Contains(stderr, "usage: chart-kind-wait.sh <release> <chart>") {
			t.Fatalf("the usage must say what the helper reads:\n%s", stderr)
		}
	})
}

// uniqueMatch returns the index of the only candidate containing want, and
// fails with an error when the candidates hold none or several: which
// command a witness reads must never depend on the order commands were
// added.
func uniqueMatch(candidates []string, want, what string) (int, error) {
	var hits []int
	for i, c := range candidates {
		if strings.Contains(c, want) {
			hits = append(hits, i)
		}
	}
	switch len(hits) {
	case 1:
		return hits[0], nil
	case 0:
		return 0, fmt.Errorf("%s has no command containing %q", what, want)
	default:
		return 0, fmt.Errorf("%s has %d commands containing %q — the witness cannot tell which one to read; keep one so what wins does not depend on the order commands were added", what, len(hits), want)
	}
}

// timeoutFlag matches a --timeout flag and, when present, its value: a
// quoted shell word or a bare word. The value stays empty for a flag that
// carries none.
var timeoutFlag = regexp.MustCompile(`--timeout(?:[ =]("[^"]*"|\S+))?`)

// soleTimeout inspects every --timeout flag on an install command and
// errors when any of them carries something other than the derived
// variable, or when the command carries none. helm accepts a repeated
// flag and keeps the LAST occurrence (pflag sets the value once per
// occurrence), so a literal --timeout 5m beside the variable sets the
// real cap while a substring pin on the variable stays green — the
// fixed-cap regression #2780 closed. A literal reds wherever it sits in
// the order; the caller passes the expansion its shell quotes, `$VAR` or
// `${VAR}`.
func soleTimeout(cmd, variable string) error {
	matches := timeoutFlag.FindAllStringSubmatch(cmd, -1)
	if len(matches) == 0 {
		return fmt.Errorf("no --timeout flag on %q — the derived cap is not wired at all", cmd)
	}
	for _, m := range matches {
		got := m[1]
		if len(got) > 1 && got[0] == '"' && got[len(got)-1] == '"' {
			got = got[1 : len(got)-1]
		}
		if got != variable {
			return fmt.Errorf("--timeout carries %q, not the derived %q — helm keeps the last repeated flag, so this literal sets the real cap while the wiring reads intact", got, variable)
		}
	}
	return nil
}

// scriptLines splits a shell script into its trimmed lines, joining the
// backslash continuations first so a command spread over several lines is
// read as the one command the shell runs.
func scriptLines(script string) []string {
	joined := strings.ReplaceAll(script, "\\\n", " ")
	var lines []string
	for _, l := range strings.Split(joined, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			lines = append(lines, l)
		}
	}
	return lines
}

// installWiring is what a wiring witness reads off an install script: the
// one argument list both commands expand, the helper call's arguments and
// the install command's arguments after its verb.
type installWiring struct {
	list    string // the list's text, as declared
	helper  string // what follows scripts/chart-kind-wait.sh, up to the closing )
	install string // what follows the install verb
}

// sameArguments judges a wiring: the helper is called on the list's
// expansion and nothing else, the install passes that same expansion first
// and then only the wait flags, so the helper never reads a render the
// install does not apply and the install never applies a -f or --set the
// helper did not see. The list itself must carry the chart and values-dev.
func sameArguments(t *testing.T, what string, w installWiring, expansion, timeoutVar string) {
	t.Helper()
	if !strings.Contains(w.list, "./charts/iterion") || !regexp.MustCompile(`(-f|--values) (\./)?charts/iterion/values-dev\.yaml`).MatchString(w.list) {
		t.Fatalf("%s's shared argument list must carry the chart and values-dev.yaml, got: %s", what, w.list)
	}
	if strings.TrimSpace(w.helper) != expansion {
		t.Fatalf("%s calls scripts/chart-kind-wait.sh on %q, want the list's expansion %q alone — the helper must render the install's own arguments, no more and no fewer", what, strings.TrimSpace(w.helper), expansion)
	}
	words := strings.Fields(w.install)
	if len(words) == 0 || words[0] != expansion {
		t.Fatalf("%s's install does not start with the list's expansion %q: %s", what, expansion, w.install)
	}
	allowed := map[string]bool{"--wait": true, "--timeout": true, `"` + timeoutVar + `"`: true, timeoutVar: true}
	for _, word := range words[1:] {
		if !allowed[word] {
			t.Fatalf("%s's install carries %q outside the shared list — an argument the helper never saw changes the render the wait was sized from; add it to the list: %s", what, word, w.install)
		}
	}
	if err := soleTimeout(w.install, timeoutVar); err != nil {
		t.Fatalf("%s's install must pass the helper's output to --timeout and nothing else: %v — cmd: %s", what, err, w.install)
	}
}

// helperArguments extracts what a line passes to scripts/chart-kind-wait.sh:
// the text after the script path up to the closing ) of the substitution.
func helperArguments(t *testing.T, what, line string) string {
	t.Helper()
	m := regexp.MustCompile(`^KIND_WAIT=\$\(sh scripts/chart-kind-wait\.sh (.*)\)`).FindStringSubmatch(line)
	if m == nil {
		t.Fatalf("%s must capture the helper's output as KIND_WAIT=$(sh scripts/chart-kind-wait.sh …), got: %s", what, line)
	}
	return m[1]
}

// taskfileTasks reads the piece of Taskfile.yml the chart:kind witness
// needs: the commands of each task.
type taskfileTasks struct {
	Tasks map[string]struct {
		Cmds []yaml.Node `yaml:"cmds"`
	} `yaml:"tasks"`
}

// TestChartKindInstallSharesItsArgumentsWithTheHelper is the wiring
// witness of the smoke: chart:kind declares its helm arguments once
// (`set -- …`), runs the helper on "$@" and installs with "$@" plus the
// derived --timeout — so the wait is sized from the render the install
// applies. The list is then run through the helper here: values-dev
// renders 1800, the task's install watches 1920 s. A literal cap, a second
// -f on either command, or a helper call on a values file reds on its own
// assertion.
func TestChartKindInstallSharesItsArgumentsWithTheHelper(t *testing.T) {
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
	cmds := make([]string, 0, len(task.Cmds))
	for _, c := range task.Cmds {
		cmds = append(cmds, c.Value)
	}
	idx, err := uniqueMatch(cmds, "helm upgrade --install", "chart:kind")
	if err != nil {
		t.Fatal(err)
	}
	lines := scriptLines(cmds[idx])
	var w installWiring
	var setLine string
	for _, line := range lines {
		switch {
		case strings.HasPrefix(line, "set -- "):
			if setLine != "" {
				t.Fatalf("chart:kind declares its argument list twice — which one the install reads depends on order: %q then %q", setLine, line)
			}
			setLine = line
			w.list = strings.TrimPrefix(line, "set -- ")
		case strings.Contains(line, "scripts/chart-kind-wait.sh"):
			if w.helper != "" {
				t.Fatalf("chart:kind calls scripts/chart-kind-wait.sh twice: %s", cmds[idx])
			}
			if setLine == "" {
				t.Fatalf("chart:kind calls the helper before declaring the list it expands: %s", cmds[idx])
			}
			w.helper = helperArguments(t, "chart:kind", line)
		case strings.HasPrefix(line, "helm upgrade --install "):
			if w.helper == "" {
				t.Fatalf("chart:kind installs before the helper sized the wait: %s", cmds[idx])
			}
			w.install = strings.TrimPrefix(line, "helm upgrade --install ")
		default:
			if strings.Contains(line, "values-dev") || strings.Contains(line, "--set") {
				t.Fatalf("chart:kind carries helm arguments outside the shared list: %s", line)
			}
		}
	}
	if setLine == "" || w.helper == "" || w.install == "" {
		t.Fatalf("chart:kind must declare the list (set -- …), call the helper on it and install with it; cmd: %s", cmds[idx])
	}
	sameArguments(t, "chart:kind", w, `"$@"`, `$KIND_WAIT`)

	// The list, run through the helper the task runs, from the repository
	// root the task runs in: the wait the smoke installs with.
	helmBinary(t)
	yqBinary(t)
	var stdout, stderr strings.Builder
	cmd := exec.Command("sh", "-c", setLine+"\nexec sh \"$KIND_WAIT_SCRIPT\" \"$@\"")
	cmd.Dir = filepath.Join("..", "..")
	cmd.Env = append(os.Environ(), "KIND_WAIT_SCRIPT="+kindWaitScript(t))
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("the helper refused chart:kind's own list (%s): %v — %s", w.list, err, strings.TrimSpace(stderr.String()))
	}
	if got, want := strings.TrimSpace(stdout.String()), "1920s"; got != want {
		t.Fatalf("chart:kind's list sizes the wait at %q, want %q — values-dev renders 1800 and the margin is 120 s", got, want)
	}
}

// TestWitnessSelectorsRequireAUniqueMatch pins the selection rule both
// wiring witnesses share: a selector that matches none or several commands
// is an error, not a silent first/last win that drifts with the order
// commands were added.
func TestWitnessSelectorsRequireAUniqueMatch(t *testing.T) {
	for _, tc := range []struct {
		name       string
		candidates []string
		want       string
		hit        int
		errPart    string
	}{
		{"the single match is returned", []string{"kind up", "helm upgrade --install iterion ./charts/iterion", "kind down"}, "helm upgrade --install", 1, ""},
		{"no match is an error", []string{"kind up", "kind down"}, "helm upgrade --install", 0, "no command containing"},
		{"two matches are an error", []string{"helm upgrade --install a", "kind up", "helm upgrade --install b"}, "helm upgrade --install", 0, "2 commands containing"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hit, err := uniqueMatch(tc.candidates, tc.want, "chart:kind")
			if tc.errPart == "" {
				if err != nil {
					t.Fatalf("uniqueMatch refused the single match: %v", err)
				}
				if hit != tc.hit {
					t.Fatalf("uniqueMatch = %d, want %d", hit, tc.hit)
				}
				return
			}
			if err == nil {
				t.Fatalf("uniqueMatch accepted a selector matching several commands — a witness must refuse the ambiguity instead of reading whichever command comes last")
			}
			if !strings.Contains(err.Error(), tc.errPart) {
				t.Fatalf("error %q does not name %q", err, tc.errPart)
			}
		})
	}
}

// TestSoleTimeoutRefusesAnyLiteralTimeout pins the rule both wiring
// witnesses share through soleTimeout: every --timeout occurrence on the
// install command must carry the derived variable. helm accepts a
// repeated flag and keeps its last value, so the literal reds in either
// position — a guard keyed on the order commands happen to list their
// flags would let the trailing mutant through.
func TestSoleTimeoutRefusesAnyLiteralTimeout(t *testing.T) {
	const variable = `$KIND_WAIT`
	for _, tc := range []struct {
		name    string
		cmd     string
		valid   bool
		errPart string
	}{
		{"the quoted variable passes", `--wait --timeout "$KIND_WAIT"`, true, ""},
		{"the = form of the variable passes", `--timeout="$KIND_WAIT"`, true, ""},
		{"a trailing literal reds", `--wait --timeout "$KIND_WAIT" --timeout 5m`, false, "--timeout carries \"5m\""},
		{"a leading literal reds too", `--timeout 5m --wait --timeout "$KIND_WAIT"`, false, "--timeout carries \"5m\""},
		{"a flag without value reds", `--wait --timeout "$KIND_WAIT" --timeout`, false, "--timeout carries \"\""},
		{"no flag at all reds", `helm upgrade --install iterion ./charts/iterion --wait`, false, "no --timeout flag"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := soleTimeout(tc.cmd, variable)
			if tc.valid {
				if err != nil {
					t.Fatalf("soleTimeout refused a correctly derived cap: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("soleTimeout accepted %q — a literal beside the derived variable must red, whichever side it sits on; cmd: %s", variable, tc.cmd)
			}
			if !strings.Contains(err.Error(), tc.errPart) {
				t.Fatalf("error %q does not name %q", err, tc.errPart)
			}
		})
	}
}

// workflowJobs reads the pieces of tests.yml the cloud-e2e witness needs.
type workflowJobs struct {
	Jobs map[string]struct {
		TimeoutMinutes int               `yaml:"timeout-minutes"`
		Env            map[string]string `yaml:"env"`
		Steps          []struct {
			Name string `yaml:"name"`
			Run  string `yaml:"run"`
		} `yaml:"steps"`
	} `yaml:"jobs"`
}

// TestChartCloudE2EInstallSharesItsArgumentsWithTheHelper witnesses the CI
// install the same way: cloud-e2e declares its helm arguments once (a bash
// array), runs the helper on "${install[@]}" and installs with it plus the
// derived --timeout. The job's own list is then run through the helper
// here, with the job's env, so the cap the floor below reads is the one
// the job derives — a deadline bump in values-dev, or a --set on the list,
// flows into the floor instead of leaving a stale constant behind. The job
// cap must outlive that helm cap on top of the preamble, so helm's timeout,
// not the runner kill, reports a slow install.
func TestChartCloudE2EInstallSharesItsArgumentsWithTheHelper(t *testing.T) {
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
	names := make([]string, 0, len(job.Steps))
	for _, st := range job.Steps {
		names = append(names, st.Name)
	}
	idx, err := uniqueMatch(names, "Helm install (values-dev.yaml)", "cloud-e2e")
	if err != nil {
		t.Fatal(err)
	}
	run := job.Steps[idx].Run
	lines := scriptLines(run)
	var w installWiring
	var listLines []string
	inList := false
	for _, line := range lines {
		switch {
		case line == "install=(":
			if listLines != nil {
				t.Fatalf("cloud-e2e declares its argument list twice: %s", run)
			}
			inList = true
			listLines = []string{line}
		case inList:
			listLines = append(listLines, line)
			if line == ")" {
				inList = false
				w.list = strings.Join(listLines[1:len(listLines)-1], " ")
			}
		case strings.Contains(line, "scripts/chart-kind-wait.sh"):
			if w.helper != "" {
				t.Fatalf("cloud-e2e calls scripts/chart-kind-wait.sh twice: %s", run)
			}
			if w.list == "" {
				t.Fatalf("cloud-e2e calls the helper before declaring the list it expands: %s", run)
			}
			w.helper = helperArguments(t, "cloud-e2e", line)
		case strings.HasPrefix(line, "helm install "):
			if w.helper == "" {
				t.Fatalf("cloud-e2e installs before the helper sized the wait: %s", run)
			}
			w.install = strings.TrimPrefix(line, "helm install ")
		default:
			if strings.Contains(line, "values-dev") || strings.Contains(line, "--set") {
				t.Fatalf("cloud-e2e carries helm arguments outside the shared list: %s", line)
			}
		}
	}
	if w.list == "" || w.helper == "" || w.install == "" {
		t.Fatalf("cloud-e2e must declare the list (install=( … )), call the helper on it and install with it; run: %s", run)
	}
	sameArguments(t, "cloud-e2e", w, `"${install[@]}"`, `${KIND_WAIT}`)

	// The runner kill must not preempt helm's verdict: the derived cap is
	// what the helper prints for the job's own list, run here with the
	// job's env from the repository root the job runs in, and the cap only
	// fires after the job's preamble — checkout, image build, kind up,
	// image load, chart deps — measured at 4.0-4.7 m on recent success
	// runs. The floor sums both budgets, the cap rounded up to the whole
	// minutes the field counts; tests.yml runs the job at 40 m, two above.
	helmBinary(t)
	yqBinary(t)
	env := os.Environ()
	for _, key := range []string{"IMG", "NS", "RELEASE"} {
		value, ok := job.Env[key]
		if !ok {
			t.Fatalf("cloud-e2e's env no longer sets %s, which its argument list expands", key)
		}
		env = append(env, key+"="+value)
	}
	var stdout, stderr strings.Builder
	cmd := exec.Command("bash", "-c", "set -euo pipefail\n"+strings.Join(listLines, "\n")+"\nexec sh \"$KIND_WAIT_SCRIPT\" \"${install[@]}\"")
	cmd.Dir = filepath.Join("..", "..")
	cmd.Env = append(env, "KIND_WAIT_SCRIPT="+kindWaitScript(t))
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("the helper refused cloud-e2e's own list (%s): %v — %s", w.list, err, strings.TrimSpace(stderr.String()))
	}
	helmSeconds, err := strconv.Atoi(strings.TrimSuffix(strings.TrimSpace(stdout.String()), "s"))
	if err != nil {
		t.Fatalf("chart-kind-wait printed %q for cloud-e2e's list — not a seconds count: %v", strings.TrimSpace(stdout.String()), err)
	}
	if got, want := helmSeconds, 1920; got != want {
		t.Fatalf("cloud-e2e's list sizes the wait at %d s, want %d — values-dev renders 1800 and the margin is 120 s; a --set on the list that moves the deadline moves this witness too", got, want)
	}
	helmCapMinutes := (helmSeconds + 59) / 60 // round up: the field counts whole minutes
	const preambleBudgetMinutes = 6
	if job.TimeoutMinutes < helmCapMinutes+preambleBudgetMinutes {
		t.Fatalf("cloud-e2e timeout-minutes = %d — below the %d m a verdict needs (%d m helm cap for the job's list + %d m preamble budget); a lower cap lets the runner kill replace helm's timeout with a silence", job.TimeoutMinutes, helmCapMinutes+preambleBudgetMinutes, helmCapMinutes, preambleBudgetMinutes)
	}
}
