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

	t.Run("the last duplicate declaration wins", func(t *testing.T) {
		// go-yaml — helm's values parser — merges duplicate keys and
		// multiple documents last-wins (proven on helm 4.1.1: a repeated
		// server block and a second --- document both render the last
		// value through helm template), so the watch must follow the last
		// declaration whichever way the file spells it — a repeated block
		// left at first-wins sizes the watch for a deadline the install
		// does not have, the #2780 failure with every gate green.
		for _, tc := range []struct{ name, content, want string }{
			{"repeated block, 600 then 1200", "server:\n  progressDeadlineSeconds: 600\nserver:\n  progressDeadlineSeconds: 1200\n", "1320s"},
			{"repeated block, 1200 then 600", "server:\n  progressDeadlineSeconds: 1200\nserver:\n  progressDeadlineSeconds: 600\n", "720s"},
			{"second document, 600 then 1200", "server:\n  progressDeadlineSeconds: 600\n---\nserver:\n  progressDeadlineSeconds: 1200\n", "1320s"},
			{"second document, 1200 then 600", "server:\n  progressDeadlineSeconds: 1200\n---\nserver:\n  progressDeadlineSeconds: 600\n", "720s"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				stdout, stderr, err := runKindWait(t, valuesFile(t, tc.content))
				if err != nil {
					t.Fatalf("chart-kind-wait refused a duplicate form helm merges — %s", strings.TrimSpace(stderr))
				}
				if got, want := strings.TrimSpace(stdout), tc.want; got != want {
					t.Fatalf("chart-kind-wait = %q, want %q — the watch must follow the declaration go-yaml leaves last", got, want)
				}
			})
		}
		t.Run("a last zero hides the earlier deadline", func(t *testing.T) {
			_, stderr, err := runKindWait(t, valuesFile(t, "server:\n  progressDeadlineSeconds: 600\nserver:\n  progressDeadlineSeconds: 0\n"))
			if err == nil {
				t.Fatalf("chart-kind-wait sized the watch from the earlier 600 — go-yaml leaves the 0 last and the templates skip it")
			}
			if !strings.Contains(stderr, "600 s default") {
				t.Fatalf("refusal does not name the 600 s default — stderr: %s", strings.TrimSpace(stderr))
			}
		})
	})

	t.Run("the block's own indent is read", func(t *testing.T) {
		// YAML fixes no indent width: go-yaml accepts the server block at
		// one, three, four or eight spaces, and an indented comment fixes
		// nothing. A deeper key belongs to a nested block — its knob is a
		// different key helm does not read as the deadline (both orders:
		// which one comes first must not decide the verdict).
		for _, form := range []string{
			"server:\n progressDeadlineSeconds: 600\n",
			"server:\n   progressDeadlineSeconds: 600\n",
			"server:\n    progressDeadlineSeconds: 600\n",
			"server:\n        progressDeadlineSeconds: 600\n",
			"server:\n  # a comment at a foreign indent fixes nothing\n    progressDeadlineSeconds: 600\n",
		} {
			stdout, stderr, err := runKindWait(t, valuesFile(t, form))
			if err != nil {
				t.Fatalf("chart-kind-wait refused %q — go-yaml accepts the block at its own indent; stderr: %s", form, strings.TrimSpace(stderr))
			}
			if got, want := strings.TrimSpace(stdout), "720s"; got != want {
				t.Fatalf("chart-kind-wait = %q, want %q on %q", got, want, form)
			}
		}
		for _, form := range []string{
			"server:\n  hpa:\n    progressDeadlineSeconds: 900\n  progressDeadlineSeconds: 600\n",
			"server:\n  progressDeadlineSeconds: 600\n  hpa:\n    progressDeadlineSeconds: 900\n",
		} {
			stdout, stderr, err := runKindWait(t, valuesFile(t, form))
			if err != nil {
				t.Fatalf("chart-kind-wait refused %q — the nested block's knob is not the server's deadline; stderr: %s", form, strings.TrimSpace(stderr))
			}
			if got, want := strings.TrimSpace(stdout), "720s"; got != want {
				t.Fatalf("chart-kind-wait = %q, want %q on %q — the deeper 900 belongs to hpa, not to the deadline", got, want, form)
			}
		}
	})

	t.Run("quoted keys and a space before the colon are not refused", func(t *testing.T) {
		// YAML spells keys bare or quoted and allows space before the
		// colon; go-yaml accepts each form and helm installs it (proven on
		// helm 4.1.1: every form renders the 600 it spells).
		for _, form := range []string{
			"\"server\":\n  progressDeadlineSeconds: 600\n",
			"'server':\n  progressDeadlineSeconds: 600\n",
			"\"server\" :\n  progressDeadlineSeconds: 600\n",
			"server :\n  progressDeadlineSeconds: 600\n",
			"server:\n  \"progressDeadlineSeconds\": 600\n",
			"server:\n  'progressDeadlineSeconds' : 600\n",
		} {
			stdout, stderr, err := runKindWait(t, valuesFile(t, form))
			if err != nil {
				t.Fatalf("chart-kind-wait refused %q — go-yaml accepts the key spelled that way; stderr: %s", form, strings.TrimSpace(stderr))
			}
			if got, want := strings.TrimSpace(stdout), "720s"; got != want {
				t.Fatalf("chart-kind-wait = %q, want %q on %q", got, want, form)
			}
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

	t.Run("a quoted decimal installs like the bare integer", func(t *testing.T) {
		// go-yaml hands "600" to the templates as the string "600", but
		// the templates print the scalar verbatim, so the applied manifest
		// holds the bare integer and installs like the unquoted form —
		// helm 4.1.1 renders "600" byte-identical to 600. Refusing it
		// would stop an install helm runs.
		for _, form := range []string{`"600"`, `'600'`} {
			stdout, stderr, err := runKindWait(t, valuesFile(t, "server:\n  progressDeadlineSeconds: "+form+"\n"))
			if err != nil {
				t.Fatalf("chart-kind-wait refused %q — the manifest helm applies reads it as the integer it spells; stderr: %s", form, strings.TrimSpace(stderr))
			}
			if got, want := strings.TrimSpace(stdout), "720s"; got != want {
				t.Fatalf("chart-kind-wait %q = %q, want %q", form, got, want)
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

	t.Run("a scalar on the line after the key is refused", func(t *testing.T) {
		// go-yaml accepts the scalar on its own, deeper line and renders
		// it; a line reader cannot pair it with its key, so the guard
		// refuses loudly — naming the one form it reads — instead of
		// sizing the watch from a value it never saw.
		stdout, stderr, err := runKindWait(t, valuesFile(t, "server:\n  progressDeadlineSeconds:\n    600\n"))
		if err == nil {
			t.Fatalf("chart-kind-wait printed %q — the scalar on the following line is not visible to a line reader", strings.TrimSpace(stdout))
		}
		for _, want := range []string{"server.progressDeadlineSeconds", "the only form this guard reads"} {
			if !strings.Contains(stderr, want) {
				t.Fatalf("refusal does not name %q — stderr: %s", want, strings.TrimSpace(stderr))
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
	cmds := make([]string, 0, len(task.Cmds))
	for _, c := range task.Cmds {
		cmds = append(cmds, c.Value)
	}
	idx, err := uniqueMatch(cmds, "helm upgrade --install", "chart:kind")
	if err != nil {
		t.Fatal(err)
	}
	install := cmds[idx]
	if !strings.Contains(install, "scripts/chart-kind-wait.sh") {
		t.Fatalf("chart:kind's install no longer calls scripts/chart-kind-wait.sh — the client wait drifts from the values deadline again; cmd: %s", install)
	}
	if err := soleTimeout(install, `$KIND_WAIT`); err != nil {
		t.Fatalf("chart:kind's install must pass the helper's output to --timeout and nothing else: %v — cmd: %s", err, install)
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
	names := make([]string, 0, len(job.Steps))
	for _, st := range job.Steps {
		names = append(names, st.Name)
	}
	idx, err := uniqueMatch(names, "Helm install (values-dev.yaml)", "cloud-e2e")
	if err != nil {
		t.Fatal(err)
	}
	install := job.Steps[idx].Run
	if !strings.Contains(install, "scripts/chart-kind-wait.sh") {
		t.Fatalf("cloud-e2e's install no longer calls scripts/chart-kind-wait.sh — a fixed client cap decides the install again; run: %s", install)
	}
	if err := soleTimeout(install, `${KIND_WAIT}`); err != nil {
		t.Fatalf("cloud-e2e's install must pass the helper's output to --timeout and nothing else: %v — run: %s", err, install)
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
