package chart_test

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"text/template/parse"

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
// margin, so the server's deadline — not pull time — decides the server
// rollout's verdict. Removing the knob from values-dev.yaml, or sizing the
// wait from anything but the render, reds here: the client watch no longer
// covers the deadline it installs. values-dev runs one server pod, the
// rollout that deadline bounds whole, so the helper prints no warning.
func TestChartKindWaitFollowsTheRenderedDeadline(t *testing.T) {
	stdout, stderr, err := runKindWait(t, "iterion", ".", "-f", "values-dev.yaml")
	if err != nil {
		t.Fatalf("chart-kind-wait iterion . -f values-dev.yaml: %v — %s", err, strings.TrimSpace(stderr))
	}
	if got, want := strings.TrimSpace(stdout), "1920s"; got != want {
		t.Fatalf("chart-kind-wait iterion . -f values-dev.yaml = %q, want %q — the 1800 s deadline values-dev renders plus the 120 s margin; the client watch must cover the deadline it installs", got, want)
	}
	if warning := strings.TrimSpace(stderr); warning != "" {
		t.Fatalf("chart-kind-wait warns on values-dev.yaml, whose one server pod the deadline bounds whole: %s", warning)
	}
}

// helperLines keeps the lines the helper itself writes to stderr, the ones
// it prefixes with its name; helm's and yq's messages are theirs.
func helperLines(stderr string) string {
	var own []string
	for _, l := range strings.Split(stderr, "\n") {
		if strings.HasPrefix(l, "chart-kind-wait: ") {
			own = append(own, l)
		}
	}
	return strings.Join(own, "\n")
}

// TestChartKindWaitDerivesAndRefuses exercises the helper's contract: the
// margin arithmetic on the render, the server's deadline alone beside a
// runner's that outlives it (the limit the helper's header states), the
// warning on a server of several pods or of an autoscaled count (the
// one-pod limit it states too), messages that print the install without
// the arguments a credential can ride in, a --set override the file never
// held, the values forms a line reader of the file could not pair with
// their key, and the refusals — a deadline the render
// leaves out (unset or 0), a string or a float, a release with no server
// Deployment or several, a render helm refuses, a 0, a negative, a leading
// zero, a sign, base prefix or underscore, or an int32 overflow in the
// render: each spelling other than plain decimal digits, whatever its
// readers make of it, and each refusal judged on the cause it names.
func TestChartKindWaitDerivesAndRefuses(t *testing.T) {
	t.Run("deadline plus the 120s margin", func(t *testing.T) {
		if got, want := mustWait(t, "iterion", ".", "-f", valuesFile(t, "mode: cloud\nserver:\n  progressDeadlineSeconds: 600\n")), "720s"; got != want {
			t.Fatalf("chart-kind-wait = %q, want %q — deadline + 120 s margin", got, want)
		}
	})
	t.Run("the wait follows the server alone, not a runner whose deadline outlives it", func(t *testing.T) {
		// helm --wait watches the runner too, and its drain budget is not
		// the wait's: the helper's header and docs/cloud-deployment.md
		// state that limit. The render must carry that runner, or the 720 s
		// proves nothing about following the server alone.
		values := valuesFile(t, "mode: cloud\nserver:\n  progressDeadlineSeconds: 600\nrunner:\n  enabled: true\n  progressDeadlineSeconds: 30000\n")
		runner := must(t, manifests(t, mustRender(t, "-f", values)), "Deployment", "iterion-runner")
		if got := dig(runner, "spec", "progressDeadlineSeconds"); got != 30000 {
			t.Fatalf("the runner Deployment renders progressDeadlineSeconds %v, want 30000 — the witness needs a runner whose deadline outlives the wait", got)
		}
		if got, want := mustWait(t, "iterion", ".", "-f", values), "720s"; got != want {
			t.Fatalf("chart-kind-wait = %q, want %q — the wait follows the server Deployment's 600 s alone; the runner's 30000 s is the limit the header states, not a deadline the wait is sized from", got, want)
		}
	})
	t.Run("a server of several pods gets the wait and a warning naming the count", func(t *testing.T) {
		// Each pod that turns Ready restarts the deadline, so a wait of one
		// deadline plus the margin can end while the rollout is still
		// Progressing: the wait is printed, and the warning says so.
		stdout, stderr, err := runKindWait(t, "iterion", ".", "-f", valuesFile(t, "mode: cloud\nserver:\n  replicas: 3\n  hpa:\n    enabled: false\n  progressDeadlineSeconds: 600\n"))
		if err != nil {
			t.Fatalf("chart-kind-wait refused a three-pod server: %v — %s", err, strings.TrimSpace(stderr))
		}
		if got, want := strings.TrimSpace(stdout), "720s"; got != want {
			t.Fatalf("chart-kind-wait = %q, want %q — the deadline still sizes the wait", got, want)
		}
		if !strings.Contains(stderr, "warning: Deployment/iterion renders spec.replicas: 3") || !strings.Contains(stderr, "restarts the deadline") {
			t.Fatalf("the warning must name the count and why the rollout can outlast the wait:\n%s", stderr)
		}
	})
	t.Run("an autoscaled server gets the wait and a warning naming the autoscaler", func(t *testing.T) {
		// values.yaml enables the server HPA, and the template then leaves
		// spec.replicas to it.
		stdout, stderr, err := runKindWait(t, "iterion", ".", "-f", valuesFile(t, "mode: cloud\nserver:\n  progressDeadlineSeconds: 600\n"))
		if err != nil {
			t.Fatalf("chart-kind-wait refused an autoscaled server: %v — %s", err, strings.TrimSpace(stderr))
		}
		if got, want := strings.TrimSpace(stdout), "720s"; got != want {
			t.Fatalf("chart-kind-wait = %q, want %q — the deadline still sizes the wait", got, want)
		}
		if !strings.Contains(stderr, "warning: Deployment/iterion renders no spec.replicas") || !strings.Contains(stderr, "server HPA") {
			t.Fatalf("the warning must name the autoscaler the count is left to:\n%s", stderr)
		}
	})
	t.Run("the messages print the install without its other arguments", func(t *testing.T) {
		// A --set can carry a credential and CI logs keep stderr: a message
		// prints the release, the chart and the values files alone. Each case
		// reaches a different message of the helper; helm's own message on a
		// values path it cannot open carries that path, so the URL cases
		// judge the helper's lines alone. helm splits a values argument on
		// commas and reads it glued to its -f, so one argument can carry
		// several userinfos: each values form below carries two, and the
		// helper prints neither. A flag the walk does not know stops it:
		// pflag hands such a flag the next argument whatever it starts with,
		// so a password spelled like a values flag (-f…, --values=…, -f) is
		// that flag's value, and a flag the walk knows takes its value the
		// same way. Words no flag takes are the release and the chart,
		// wherever they sit. This subtest's name holds no comma: the values
		// files sit in a temp dir named after it.
		const secret = "kind-wait-sentinel-credential"
		local := valuesFile(t, "mode: local\n")
		unset := valuesFile(t, "mode: cloud\n")
		chart := filepath.Join(t.TempDir(), "nochart")
		for _, tc := range []struct {
			name     string
			args     []string
			shown    string // the install as the message must print it
			cause    string
			helmEcho bool // helm's own message carries the secret
		}{
			{"a render helm refuses", []string{"iterion", chart, "-f", local, "--set", "secrets.llm.anthropicApiKey=" + secret}, "iterion " + chart + " -f " + local + " [2 more arguments not shown]", "failed (helm's message above)", false},
			{"no server Deployment", []string{"iterion", ".", "-f", local, "--namespace", "ns", "--set", "secrets.auth.mongoURI=mongodb://u:" + secret + "@db", "--set-string=secrets.auth.jwtSecret=" + secret}, "iterion . -f " + local + " [5 more arguments not shown]", "carries no Deployment", false},
			{"no deadline", []string{"iterion", ".", "--values=" + unset, "--set", "secrets.auth.jwtSecret=" + secret}, "iterion . --values=" + unset + " [2 more arguments not shown]", "renders no spec.progressDeadlineSeconds", false},
			{"a flag before the release", []string{"--set", "secrets.auth.jwtSecret=" + secret, "iterion", ".", "-f", unset}, "iterion . -f " + unset + " [2 more arguments not shown]", "renders no spec.progressDeadlineSeconds", false},
			{"a password glued like a values file", []string{"iterion", ".", "-f", unset, "--username", "bot", "--password", "-f" + secret}, "iterion . -f " + unset + " [4 more arguments not shown]", "renders no spec.progressDeadlineSeconds", false},
			{"a password spelled --values=", []string{"iterion", ".", "-f", unset, "--password", "--values=" + secret}, "iterion . -f " + unset + " [2 more arguments not shown]", "renders no spec.progressDeadlineSeconds", false},
			{"a password spelled -f", []string{"iterion", ".", "-f", unset, "--password", "-f", secret}, "iterion . -f " + unset + " [3 more arguments not shown]", "failed (helm's message above)", true},
			{"a known flag's value spelled like a values file", []string{"iterion", ".", "--namespace", "-f" + secret, "-f", unset}, "iterion . -f " + unset + " [2 more arguments not shown]", "renders no spec.progressDeadlineSeconds", false},
			{"a values URL's userinfo", []string{"iterion", ".", "-f", "file://kind-wait:" + secret + "@/nonexistent/values.yaml"}, "iterion . -f file://***@/nonexistent/values.yaml", "failed (helm's message above)", true},
			{"every userinfo of a comma-split values list", []string{"iterion", ".", "-f", "file://kind-wait:" + secret + "-a@/nonexistent/a.yaml,file://kind-wait:" + secret + "-b@/nonexistent/b.yaml"}, "iterion . -f file://***@/nonexistent/a.yaml,file://***@/nonexistent/b.yaml", "failed (helm's message above)", true},
			{"every userinfo of a values list glued to its -f", []string{"iterion", ".", "-ffile://kind-wait:" + secret + "-a@/nonexistent/a.yaml,file://kind-wait:" + secret + "-b@/nonexistent/b.yaml"}, "iterion . -ffile://***@/nonexistent/a.yaml,file://***@/nonexistent/b.yaml", "failed (helm's message above)", true},
			{"every userinfo of a --values= list", []string{"iterion", ".", "--values=file://kind-wait:" + secret + "-a@/nonexistent/a.yaml,file://kind-wait:" + secret + "-b@/nonexistent/b.yaml"}, "iterion . --values=file://***@/nonexistent/a.yaml,file://***@/nonexistent/b.yaml", "failed (helm's message above)", true},
			{"a chart URL's userinfo", []string{"iterion", "file://kind-wait:" + secret + "@/nonexistent/chart"}, "iterion file://***@/nonexistent/chart", "failed (helm's message above)", true},
			{"a chart URL's query", []string{"iterion", "file:///nonexistent/chart?token=" + secret}, "iterion file:///nonexistent/chart?***", "failed (helm's message above)", true},
		} {
			t.Run(tc.name, func(t *testing.T) {
				stderr := mustRefuseWait(t, tc.args...)
				own := helperLines(stderr)
				judged := stderr
				if tc.helmEcho {
					judged = own
				}
				if strings.Contains(judged, secret) {
					t.Fatalf("the refusal prints an argument the install may carry a credential in:\n%s", judged)
				}
				if !strings.Contains(own, "`helm template "+tc.shown+"`") || !strings.Contains(own, tc.cause) {
					t.Fatalf("the refusal must print the install as `helm template %s` and name its cause %q:\n%s", tc.shown, tc.cause, stderr)
				}
			})
		}
	})
	t.Run("a values URL is printed without its query or fragment", func(t *testing.T) {
		// A token (?token=, private_token=) or a presigned signature rides in
		// a URL's query. helm fetches each file below from this server,
		// renders, and prints nothing: the refusal is the helper's alone, so
		// all of stderr is judged. The first URL's query and fragment are
		// masked to the end of the argument, the files a comma lists after
		// them included.
		const secret = "kind-wait-sentinel-query"
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte("mode: cloud\n"))
		}))
		t.Cleanup(srv.Close)
		u := srv.URL + "/unset.yaml"
		host := strings.TrimPrefix(srv.URL, "http://")
		for _, tc := range []struct {
			name  string
			args  []string
			shown string // the install as the message must print it
		}{
			{"a token in the query", []string{"iterion", ".", "-f", u + "?token=" + secret}, "iterion . -f " + u + "?***"},
			{"a query after a comma-split file", []string{"iterion", ".", "-f", u + "," + u + "?X-Amz-Signature=" + secret}, "iterion . -f " + u + "," + u + "?***"},
			{"the files listed after a query", []string{"iterion", ".", "-f", u + "?token=" + secret + "-a," + u + "?token=" + secret + "-b"}, "iterion . -f " + u + "?***"},
			{"a fragment glued to its -f", []string{"iterion", ".", "-f" + u + "#" + secret}, "iterion . -f" + u + "#***"},
			{"a userinfo and a query given as --values=", []string{"iterion", ".", "--values=http://kind-wait:" + secret + "-a@" + host + "/unset.yaml?private_token=" + secret + "-b"}, "iterion . --values=http://***@" + host + "/unset.yaml?***"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				stderr := mustRefuseWait(t, tc.args...)
				if strings.Contains(stderr, secret) {
					t.Fatalf("the refusal prints a credential a values URL's query carries:\n%s", stderr)
				}
				if own := helperLines(stderr); !strings.Contains(own, "`helm template "+tc.shown+"`") || !strings.Contains(own, "renders no spec.progressDeadlineSeconds") {
					t.Fatalf("the refusal must print the install as `helm template %s` and name the deadline the fetched values leave unset:\n%s", tc.shown, stderr)
				}
			})
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
	t.Run("a leading zero is refused whatever its readers make of it", func(t *testing.T) {
		// helm's YAML 1.1 decoding, the form the API receives, reads 010 as
		// octal 8 and 00 as 0; yq reads 010 as 10 and tags 08 a float. Shell
		// arithmetic on the text would print 128s for 010 and 120s for 00 —
		// a wait sized by accident — so the spelling is refused by name.
		for _, v := range []string{"00", "000", "010", "0600", "08"} {
			stderr := mustRefuseWait(t, "scratch", scratchChart(t, serverDeployment("a", v)))
			if !strings.Contains(stderr, "progressDeadlineSeconds: "+v+" for") || !strings.Contains(stderr, "a leading zero") || !strings.Contains(stderr, "not a positive decimal integer") {
				t.Fatalf("the refusal of %s must name the value, its leading zero and the plain-digits contract:\n%s", v, stderr)
			}
		}
		// The chart renders one itself: helm keeps a --set value with a
		// leading zero as the string it was given, and the template prints
		// it bare.
		for _, v := range []string{"010", "00"} {
			stderr := mustRefuseWait(t, "iterion", ".", "--set", "mode=cloud", "--set", "server.progressDeadlineSeconds="+v)
			if !strings.Contains(stderr, "progressDeadlineSeconds: "+v+" for") || !strings.Contains(stderr, "a leading zero") {
				t.Fatalf("the chart renders --set server.progressDeadlineSeconds=%s bare, and the refusal must name it:\n%s", v, stderr)
			}
		}
	})
	t.Run("a sign, a base prefix or an underscore is refused", func(t *testing.T) {
		// helm's YAML 1.1 decoding reads each as the integer it denotes
		// (+600 is 600, 0x10 is 16, 0o10 is 8, 0b11 is 3, 1_000 is 1000);
		// the helper does not reimplement that reading.
		for _, v := range []string{"+600", "0x10", "0o10", "0b11", "1_000"} {
			stderr := mustRefuseWait(t, "scratch", scratchChart(t, serverDeployment("a", v)))
			if !strings.Contains(stderr, "progressDeadlineSeconds: "+v+" for") || !strings.Contains(stderr, "not a positive decimal integer") {
				t.Fatalf("the refusal of %s must name the value and the plain-digits contract:\n%s", v, stderr)
			}
		}
	})
	t.Run("an integral float is refused as a spelling, not as a deadline the API refuses", func(t *testing.T) {
		// helm prints a values number of a million or more in exponent
		// form, which helm's YAML 1.1 decoding reads as the integer it
		// denotes: the API would run 1000000. The refusal says so and names
		// the remedy — the quoted number renders its digits, and the wait
		// follows them.
		stderr := mustRefuseWait(t, "iterion", ".", "-f", valuesFile(t, "mode: cloud\nserver:\n  progressDeadlineSeconds: 1000000\n"))
		if !strings.Contains(stderr, "progressDeadlineSeconds: 1e+06 (!!float)") || !strings.Contains(stderr, "reads an integral float as its integer") || !strings.Contains(stderr, "quote that number") {
			t.Fatalf("the refusal must name the exponent form helm printed, what the API reads of it and the quoting remedy:\n%s", stderr)
		}
		if strings.Contains(stderr, "the API refuses the Deployment") {
			t.Fatalf("the API reads 1e+06 as 1000000 and runs it; the refusal must not claim the API refuses it:\n%s", stderr)
		}
		if got, want := mustWait(t, "iterion", ".", "-f", valuesFile(t, "mode: cloud\nserver:\n  progressDeadlineSeconds: \"1000000\"\n")), "1000120s"; got != want {
			t.Fatalf("chart-kind-wait = %q, want %q — the quoted values number renders its digits, the remedy the refusal names", got, want)
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

// installSite is the shape one install script must have: its argument list
// declared once, the helper called on the list's expansion verbatim, one
// install on that expansion plus the derived wait, and beside them only the
// inert lines named here. The shape is an allowlist: a line the witness
// does not know — a fallback cap, a second assignment of the wait, an edit
// of the list between the helper and the install — reds for being unknown,
// whatever its spelling, where a denylist passes every form it does not
// name.
type installSite struct {
	what        string   // the site, in messages
	listPrefix  string   // a one-line declaration: this prefix, then the list
	listOpen    string   // a block declaration: this line, the list's parts, then a lone )
	expansion   string   // the list's expansion, as both commands pass it
	helperLine  string   // the helper call, verbatim
	installVerb string   // the install command, up to the list's expansion
	timeoutVar  string   // the wait, as the install expands it
	inert       []string // whole lines that touch neither the list nor the wait
}

// The two install sites. Task runs a cmd without errexit, so chart:kind's
// helper call stops the cmd itself on a refusal; the cloud-e2e step runs
// under bash -e and its own set -e, where a failed substitution stops it.
var (
	chartKindSite = installSite{
		what:        "chart:kind",
		listPrefix:  "set -- ",
		expansion:   `"$@"`,
		helperLine:  `KIND_WAIT=$(sh scripts/chart-kind-wait.sh "$@") || exit 1`,
		installVerb: "helm upgrade --install ",
		timeoutVar:  `$KIND_WAIT`,
	}
	cloudE2ESite = installSite{
		what:        "cloud-e2e",
		listOpen:    "install=(",
		expansion:   `"${install[@]}"`,
		helperLine:  `KIND_WAIT=$(sh scripts/chart-kind-wait.sh "${install[@]}")`,
		installVerb: "helm install ",
		timeoutVar:  `${KIND_WAIT}`,
		inert:       []string{"set -euo pipefail", `kubectl create namespace "${NS}"`},
	}
)

// installWiring is what readInstallScript reads off an install script.
type installWiring struct {
	list     string   // the list's text, its parts joined by a space
	listDecl []string // the declaration's lines, as the shell runs them
	install  string   // what follows the install verb
}

// readInstallScript reads an install script against its site's shape and
// returns the wiring, or an error naming the first line that breaks it:
// the list declared once, then the helper line verbatim, then one install,
// the site's inert lines anywhere, and no other line.
func readInstallScript(site installSite, script string) (installWiring, error) {
	var w installWiring
	helper := false
	lines := scriptLines(script)
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		switch {
		case site.listPrefix != "" && strings.HasPrefix(line, site.listPrefix),
			site.listOpen != "" && line == site.listOpen:
			if w.listDecl != nil {
				return w, fmt.Errorf("%s declares its argument list twice — which one the install reads depends on the order: %q", site.what, line)
			}
			if site.listPrefix != "" {
				w.listDecl, w.list = []string{line}, strings.TrimPrefix(line, site.listPrefix)
				continue
			}
			end := slices.Index(lines[i+1:], ")")
			if end < 0 {
				return w, fmt.Errorf("%s opens its argument list with %q and never closes it on a lone )", site.what, line)
			}
			w.listDecl = lines[i : i+end+2]
			w.list = strings.Join(lines[i+1:i+1+end], " ")
			i += end + 1
		case strings.Contains(line, "chart-kind-wait"):
			if line != site.helperLine {
				return w, fmt.Errorf("%s calls the helper as %q, want exactly %q — the substitution's output must be the wait and nothing else: text after it (a fallback `|| KIND_WAIT=10m`, a second assignment) replaces the derived cap whenever it runs, and other arguments render a release the install does not apply", site.what, line, site.helperLine)
			}
			if w.listDecl == nil {
				return w, fmt.Errorf("%s calls the helper before declaring the list it expands", site.what)
			}
			if helper {
				return w, fmt.Errorf("%s calls the helper twice", site.what)
			}
			helper = true
		case strings.HasPrefix(line, site.installVerb):
			if !helper {
				return w, fmt.Errorf("%s installs before the helper sized the wait: %q", site.what, line)
			}
			if w.install != "" {
				return w, fmt.Errorf("%s installs twice — the wiring of one install says nothing of the other's cap: %q", site.what, line)
			}
			w.install = strings.TrimPrefix(line, site.installVerb)
		case slices.Contains(site.inert, line):
		default:
			return w, fmt.Errorf("%s carries a line the wiring witness does not know: %q — beside the list, the helper call and the install, the script may hold only the site's inert lines %q: an unknown line can reassign KIND_WAIT or edit the list between the helper and the install (a fallback cap, an appended -f), and the install would no longer watch the wait its render owes; a line that touches neither joins the site's inert lines in this witness", site.what, line, site.inert)
		}
	}
	if w.listDecl == nil || !helper || w.install == "" {
		return w, fmt.Errorf("%s must declare the list, call the helper on it and install with it; script:\n%s", site.what, script)
	}
	return w, judgeWiring(site, w)
}

// valuesDev matches values-dev.yaml handed to a -f/--values flag.
var valuesDev = regexp.MustCompile(`(-f|--values) (\./)?charts/iterion/values-dev\.yaml`)

// judgeWiring judges what the shape leaves free: the list carries the
// chart and values-dev, and the install passes the list's expansion first
// and then only the wait flags, so it never applies a -f or --set the
// helper did not render.
func judgeWiring(site installSite, w installWiring) error {
	if !strings.Contains(w.list, "./charts/iterion") || !valuesDev.MatchString(w.list) {
		return fmt.Errorf("%s's shared argument list must carry the chart and values-dev.yaml, got: %s", site.what, w.list)
	}
	words := strings.Fields(w.install)
	if len(words) == 0 || words[0] != site.expansion {
		return fmt.Errorf("%s's install does not start with the list's expansion %q: %s", site.what, site.expansion, w.install)
	}
	allowed := map[string]bool{"--wait": true, "--timeout": true, `"` + site.timeoutVar + `"`: true, site.timeoutVar: true}
	for _, word := range words[1:] {
		if !allowed[word] {
			return fmt.Errorf("%s's install carries %q outside the shared list — an argument the helper never saw changes the render the wait was sized from; add it to the list: %s", site.what, word, w.install)
		}
	}
	if err := soleTimeout(w.install, site.timeoutVar); err != nil {
		return fmt.Errorf("%s's install must pass the helper's output to --timeout and nothing else: %w — cmd: %s", site.what, err, w.install)
	}
	return nil
}

// helmInstalls matches a helm command that installs or upgrades a release.
var helmInstalls = regexp.MustCompile(`\bhelm\s+(?:install|upgrade)\b`)

// otherInstalls returns the commands besides the one at idx that install
// with helm: an install the wiring witness does not read escapes the
// derived wait whatever cap it carries.
func otherInstalls(cmds []string, idx int) []string {
	var others []string
	for i, c := range cmds {
		if i != idx && helmInstalls.MatchString(c) {
			others = append(others, c)
		}
	}
	return others
}

// taskfileTasks reads the piece of Taskfile.yml the chart:kind witness
// needs: the commands of each task.
type taskfileTasks struct {
	Tasks map[string]struct {
		Cmds []yaml.Node `yaml:"cmds"`
	} `yaml:"tasks"`
}

// cmdText is the shell text of one Taskfile cmd: the scalar, or the cmd of
// the mapping form.
func cmdText(n yaml.Node) string {
	if n.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(n.Content); i += 2 {
			if n.Content[i].Value == "cmd" {
				return n.Content[i+1].Value
			}
		}
	}
	return n.Value
}

// chartKindCmds returns chart:kind's commands and the index of its install,
// refusing a second command that installs with helm.
func chartKindCmds(t *testing.T) ([]string, int) {
	t.Helper()
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
		cmds = append(cmds, cmdText(c))
	}
	idx, err := uniqueMatch(cmds, "helm upgrade --install", "chart:kind")
	if err != nil {
		t.Fatal(err)
	}
	if others := otherInstalls(cmds, idx); len(others) > 0 {
		t.Fatalf("chart:kind installs with helm outside the command the wiring witness reads — that install escapes the derived wait: %q", others)
	}
	return cmds, idx
}

// TestChartKindInstallSharesItsArgumentsWithTheHelper is the wiring
// witness of the smoke: chart:kind declares its helm arguments once
// (`set -- …`), runs the helper on "$@" and installs with "$@" plus the
// derived --timeout, and holds no other line — so the wait is sized from
// the render the install applies, and nothing between the two replaces it.
// The list is then run through the helper here: values-dev renders 1800,
// the task's install watches 1920 s. TestInstallScriptsHoldNothingButTheirShape
// pins what the shape refuses.
func TestChartKindInstallSharesItsArgumentsWithTheHelper(t *testing.T) {
	cmds, idx := chartKindCmds(t)
	w, err := readInstallScript(chartKindSite, cmds[idx])
	if err != nil {
		t.Fatal(err)
	}

	// The list, run through the helper the task runs, from the repository
	// root the task runs in: the wait the smoke installs with.
	helmBinary(t)
	yqBinary(t)
	var stdout, stderr strings.Builder
	cmd := exec.Command("sh", "-c", strings.Join(w.listDecl, "\n")+"\nexec sh \"$KIND_WAIT_SCRIPT\" \"$@\"")
	cmd.Dir = filepath.Join("..", "..")
	cmd.Env = append(os.Environ(), "KIND_WAIT_SCRIPT="+kindWaitScript(t))
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("the helper refused chart:kind's own list (%s): %v — %s", w.list, err, strings.TrimSpace(stderr.String()))
	}
	if got, want := strings.TrimSpace(stdout.String()), "1920s"; got != want {
		t.Fatalf("chart:kind's list sizes the wait at %q, want %q — values-dev renders 1800 and the margin is 120 s", got, want)
	}
	if warning := strings.TrimSpace(stderr.String()); warning != "" {
		t.Fatalf("the helper warns on chart:kind's own list, so the smoke's wait no longer covers its server rollout: %s", warning)
	}
}

// TestInstallScriptsHoldNothingButTheirShape mutates each site's real
// install script one edit at a time — the edits that keep a substring or
// denylist reading green: a cap assigned between the helper and the
// install, a fallback or a second assignment on the helper line, the list
// extended or trimmed after the helper rendered it, a second install — and
// requires readInstallScript to refuse each on its own cause. The
// unmutated script must pass first, so each refusal is the edit's.
func TestInstallScriptsHoldNothingButTheirShape(t *testing.T) {
	cmds, idx := chartKindCmds(t)
	job, step := cloudE2EJob(t)
	type mutant struct {
		name     string
		from, to string // the edit: from must occur in the real script exactly once
		errPart  string
	}
	// edit inserts a line right after the helper call, before the install.
	edit := func(site installSite, name, line, errPart string) mutant {
		return mutant{name, site.helperLine + "\n", site.helperLine + "\n" + line + "\n", errPart}
	}
	for _, s := range []struct {
		site    installSite
		script  string
		mutants []mutant
	}{
		{chartKindSite, cmds[idx], []mutant{
			edit(chartKindSite, "a literal cap between the helper and the install", "KIND_WAIT=5m", "does not know"),
			edit(chartKindSite, "the list extended after the helper rendered it", `set "$@" -f charts/iterion/values-prod.yaml`, "does not know"),
			edit(chartKindSite, "a second install beside the wired one", "helm upgrade --install iterion ./charts/iterion -f charts/iterion/values-prod.yaml --wait --timeout 5m", "installs twice"),
			{"a fallback cap in place of the exit", chartKindSite.helperLine + "\n", `KIND_WAIT=$(sh scripts/chart-kind-wait.sh "$@") || KIND_WAIT=5m` + "\n", "calls the helper as"},
			{"a second assignment glued to the helper line", chartKindSite.helperLine + "\n", chartKindSite.helperLine + "; KIND_WAIT=5m\n", "calls the helper as"},
			{"the helper called on a values file", chartKindSite.helperLine + "\n", `KIND_WAIT=$(sh scripts/chart-kind-wait.sh iterion ./charts/iterion -f charts/iterion/values-dev.yaml) || exit 1` + "\n", "calls the helper as"},
		}},
		{cloudE2ESite, job.Steps[step].Run, []mutant{
			edit(cloudE2ESite, "the list extended after the helper rendered it", "install+=(-f ./charts/iterion/values-prod.yaml)", "does not know"),
			edit(cloudE2ESite, "a list part dropped after the helper rendered it", "unset 'install[3]'", "does not know"),
			edit(cloudE2ESite, "an exported literal cap", "export KIND_WAIT=10m", "does not know"),
			edit(cloudE2ESite, "a cap read from a file", "read -r KIND_WAIT < /tmp/cap", "does not know"),
			{"a fallback cap on the helper line", cloudE2ESite.helperLine + "\n", cloudE2ESite.helperLine + " || KIND_WAIT=10m\n", "calls the helper as"},
		}},
	} {
		if _, err := readInstallScript(s.site, s.script); err != nil {
			t.Fatalf("%s's real script must pass before its mutants mean anything: %v", s.site.what, err)
		}
		for _, m := range s.mutants {
			t.Run(s.site.what+"/"+m.name, func(t *testing.T) {
				if n := strings.Count(s.script, m.from); n != 1 {
					t.Fatalf("the edit's anchor %q occurs %d times in %s's script, want once — the mutant would not be the edit it names", m.from, n, s.site.what)
				}
				_, err := readInstallScript(s.site, strings.Replace(s.script, m.from, m.to, 1))
				if err == nil {
					t.Fatalf("readInstallScript accepted %s's script with %s — the install would run a wait other than the derived one while the wiring reads intact", s.site.what, m.name)
				}
				if !strings.Contains(err.Error(), m.errPart) {
					t.Fatalf("the refusal does not name %q: %v", m.errPart, err)
				}
			})
		}
	}
}

// TestOtherInstallsSeeEveryHelmInstall pins the guard both witnesses run
// over the commands they do not read: any helm install or upgrade beside
// the wired one is named — a Taskfile cmd in its mapping form too — and the
// other helm verbs are not.
func TestOtherInstallsSeeEveryHelmInstall(t *testing.T) {
	var tf taskfileTasks
	src := "tasks:\n  smoke:\n    cmds:\n      - kind create cluster\n      - |\n        set -- iterion ./charts/iterion\n        helm upgrade --install \"$@\" --wait\n      - helm test iterion\n      - cmd: helm install shadow ./charts/iterion --wait --timeout 5m\n      - helm  upgrade shadow ./charts/iterion\n      - helm dependency update ./charts/iterion\n"
	if err := yaml.Unmarshal([]byte(src), &tf); err != nil {
		t.Fatal(err)
	}
	var cmds []string
	for _, c := range tf.Tasks["smoke"].Cmds {
		cmds = append(cmds, cmdText(c))
	}
	got := otherInstalls(cmds, 1)
	want := []string{"helm install shadow ./charts/iterion --wait --timeout 5m", "helm  upgrade shadow ./charts/iterion"}
	if !slices.Equal(got, want) {
		t.Fatalf("otherInstalls = %q, want %q — a second install, in either cmd form, escapes the wiring the witness reads", got, want)
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

// workflowStep, workflowJob and workflowJobs read the pieces of tests.yml
// the cloud-e2e witness needs.
type workflowStep struct {
	Name           string `yaml:"name"`
	Run            string `yaml:"run"`
	TimeoutMinutes int    `yaml:"timeout-minutes"`
}

type workflowJob struct {
	TimeoutMinutes int               `yaml:"timeout-minutes"`
	Env            map[string]string `yaml:"env"`
	Steps          []workflowStep    `yaml:"steps"`
}

type workflowJobs struct {
	Jobs map[string]workflowJob `yaml:"jobs"`
}

// cloudE2EJob returns the cloud-e2e job and the index of its install step,
// refusing another step that installs with helm.
func cloudE2EJob(t *testing.T) (workflowJob, int) {
	t.Helper()
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
	runs := make([]string, 0, len(job.Steps))
	for _, st := range job.Steps {
		names = append(names, st.Name)
		runs = append(runs, st.Run)
	}
	idx, err := uniqueMatch(names, "Helm install (values-dev.yaml)", "cloud-e2e")
	if err != nil {
		t.Fatal(err)
	}
	if others := otherInstalls(runs, idx); len(others) > 0 {
		t.Fatalf("cloud-e2e installs with helm outside the step the wiring witness reads — that install escapes the derived wait: %q", others)
	}
	return job, idx
}

// TestChartCloudE2EInstallSharesItsArgumentsWithTheHelper witnesses the CI
// install the same way: cloud-e2e declares its helm arguments once (a bash
// array), runs the helper on "${install[@]}" and installs with it plus the
// derived --timeout, and holds no other line but the step's set -e and its
// namespace. The job's own list is then run through the helper here, with
// the job's env, so the cap the floor below reads is the one the job
// derives — a deadline bump in values-dev, or a --set on the list, flows
// into the floor instead of leaving a stale constant behind. The job cap
// must outlive the sum of the preamble's budget, that helm cap and the
// steps after the install, each capped by its own timeout-minutes, so a
// slow run whose preamble fits its budget ends on a message, not the
// runner kill; the preamble has no cap of its own to sum.
func TestChartCloudE2EInstallSharesItsArgumentsWithTheHelper(t *testing.T) {
	job, step := cloudE2EJob(t)
	w, err := readInstallScript(cloudE2ESite, job.Steps[step].Run)
	if err != nil {
		t.Fatal(err)
	}

	// The derived cap is what the helper prints for the job's own list, run
	// here with the job's env from the repository root the job runs in.
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
	cmd := exec.Command("bash", "-c", "set -euo pipefail\n"+strings.Join(w.listDecl, "\n")+"\nexec sh \"$KIND_WAIT_SCRIPT\" \"${install[@]}\"")
	cmd.Dir = filepath.Join("..", "..")
	cmd.Env = append(env, "KIND_WAIT_SCRIPT="+kindWaitScript(t))
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("the helper refused cloud-e2e's own list (%s): %v — %s", w.list, err, strings.TrimSpace(stderr.String()))
	}
	if warning := strings.TrimSpace(stderr.String()); warning != "" {
		t.Fatalf("the helper warns on cloud-e2e's own list, so the job's wait no longer covers its server rollout: %s", warning)
	}
	helmSeconds, err := strconv.Atoi(strings.TrimSuffix(strings.TrimSpace(stdout.String()), "s"))
	if err != nil {
		t.Fatalf("chart-kind-wait printed %q for cloud-e2e's list — not a seconds count: %v", strings.TrimSpace(stdout.String()), err)
	}
	if got, want := helmSeconds, 1920; got != want {
		t.Fatalf("cloud-e2e's list sizes the wait at %d s, want %d — values-dev renders 1800 and the margin is 120 s; a --set on the list that moves the deadline moves this witness too", got, want)
	}

	// The floor sums the job's bounds: a budget for the preamble before the
	// install — checkout, image build, kind up, image load, chart deps —
	// measured at 4.0-4.7 m on recent success runs and capped by nothing of
	// its own; the derived helm cap,
	// rounded up to the whole minutes the field counts; and every step
	// after the install, which a slow install that succeeds still runs —
	// the readiness wait, the e2e run, the failure diagnostics — each of
	// which must carry its own timeout-minutes, so a hang there ends on
	// GitHub's step timeout and the sum bounds them all.
	helmCapMinutes := (helmSeconds + 59) / 60
	const preambleBudgetMinutes = 6
	postInstallMinutes := 0
	for _, st := range job.Steps[step+1:] {
		if st.TimeoutMinutes <= 0 {
			t.Fatalf("cloud-e2e's step %q runs after the install with no timeout-minutes — a step with no bound of its own leaves the job cap unable to outlive it, and its hang would end on the runner kill; give it one", st.Name)
		}
		postInstallMinutes += st.TimeoutMinutes
	}
	if floor := preambleBudgetMinutes + helmCapMinutes + postInstallMinutes; job.TimeoutMinutes < floor {
		t.Fatalf("cloud-e2e timeout-minutes = %d — below the %d m every bound of the job sums to (%d m preamble budget + %d m helm cap for the job's list + %d m of the steps after the install); a lower cap lets the runner kill replace a verdict with a silence", job.TimeoutMinutes, floor, preambleBudgetMinutes, helmCapMinutes, postInstallMinutes)
	}
}

// yqRelease finds a workflow line that fetches a mikefarah/yq release;
// yqDownload reads the version such a download pins.
var (
	yqRelease  = regexp.MustCompile(`mikefarah/yq/releases/`)
	yqDownload = regexp.MustCompile(`mikefarah/yq/releases/download/v(\d+\.\d+\.\d+)/`)
)

// TestChartKindWaitToolchainPinsYq binds the helper's claim to the
// toolchain, and the yq of the local gate to CI's. scripts/chart-kind-wait.sh
// and docs/cloud-deployment.md say devbox provides yq-go, and `task
// chart:check` requires yq (ITERION_CHART_RENDER_REQUIRE=1): the root
// devbox.json must pin yq-go — mikefarah/yq, whose expressions the helper
// uses; nix's `yq` is the python wrapper the helper refuses. Without the
// pin a devbox env composed without the host PATH runs the chart gate red
// on a missing tool, and only a yq the host happens to carry hides it.
// devbox.lock must resolve that very pin, and every yq download of the CI
// workflows must fetch the version the lock resolves: the helper's verdict
// rides on yq's reading of the render (its tags), so a local gate and a CI
// gate on two versions would each prove the helper for a different reader,
// and nothing else reds on the drift. Runs everywhere.
func TestChartKindWaitToolchainPinsYq(t *testing.T) {
	root := filepath.Join("..", "..")
	raw, err := os.ReadFile(filepath.Join(root, "devbox.json"))
	if err != nil {
		t.Fatal(err)
	}
	var devbox struct {
		Packages []string `json:"packages"`
	}
	if err := json.Unmarshal(raw, &devbox); err != nil {
		t.Fatalf("devbox.json could not be read as JSON with a packages list: %v", err)
	}
	var pin string
	for _, p := range devbox.Packages {
		if p == "yq-go" || strings.HasPrefix(p, "yq-go@") {
			pin = p
		}
	}
	if pin == "" {
		t.Fatalf("devbox.json pins no yq-go (packages: %s) — scripts/chart-kind-wait.sh reads the render with mikefarah/yq and `task chart:check` requires it: a devbox env without the host's yq runs the chart gate red", strings.Join(devbox.Packages, ", "))
	}
	script, err := os.ReadFile(kindWaitScript(t))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(script), "yq-go") {
		t.Fatalf("scripts/chart-kind-wait.sh's missing-tool message must name the package devbox pins (%s)", pin)
	}

	// The lock: the version devbox installs for that pin; devbox locks an
	// unversioned pin as @latest.
	lockKey := pin
	if !strings.Contains(lockKey, "@") {
		lockKey += "@latest"
	}
	rawLock, err := os.ReadFile(filepath.Join(root, "devbox.lock"))
	if err != nil {
		t.Fatal(err)
	}
	var lock struct {
		Packages map[string]struct {
			Version string `json:"version"`
		} `json:"packages"`
	}
	if err := json.Unmarshal(rawLock, &lock); err != nil {
		t.Fatalf("devbox.lock could not be read as JSON with a packages map: %v", err)
	}
	locked := lock.Packages[lockKey].Version
	if locked == "" {
		t.Fatalf("devbox.json pins %s and devbox.lock resolves no version for %s — json and lock disagree, so the yq devbox installs is not the one devbox.json names; re-lock with devbox", pin, lockKey)
	}

	// Every yq download of the workflows: the version CI runs.
	workflows, err := filepath.Glob(filepath.Join(root, ".github", "workflows", "*.y*ml"))
	if err != nil {
		t.Fatal(err)
	}
	var downloads, drift []string
	for _, wf := range workflows {
		src, err := os.ReadFile(wf)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(src), "\n") {
			if !yqRelease.MatchString(line) {
				continue
			}
			site := fmt.Sprintf("%s:%d", filepath.Base(wf), i+1)
			m := yqDownload.FindStringSubmatch(line)
			if m == nil {
				t.Fatalf("%s fetches a mikefarah/yq release whose version this witness cannot read (latest, or a templated tag) — CI would run a yq nothing compares with the %s devbox locks: %s", site, locked, strings.TrimSpace(line))
			}
			downloads = append(downloads, site)
			if m[1] != locked {
				drift = append(drift, site+" v"+m[1])
			}
		}
	}
	if len(downloads) == 0 {
		t.Fatalf("no workflow under .github/workflows downloads a mikefarah/yq release — the helm-lint job runs the kind-wait witnesses with ITERION_CHART_RENDER_REQUIRE=1 and installs one; a scan that reads nothing compares nothing")
	}
	if len(drift) > 0 {
		t.Fatalf("devbox.lock resolves yq-go %s for the local gate and CI downloads another yq at %s — the helper's verdict rides on yq's reading of the render, so both gates run one version: move each download (version and digest) or the devbox pin", locked, strings.Join(drift, ", "))
	}
}

// runKindWaitWithYq runs the helper on values-dev.yaml with a fake yq
// leading PATH — a shell script of the body given — and returns its stdout,
// its stderr, the fake's path and the command error. helm is the real one.
func runKindWaitWithYq(t *testing.T, body string) (string, string, string, error) {
	t.Helper()
	helmBinary(t)
	bin := t.TempDir()
	fake := filepath.Join(bin, "yq")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr strings.Builder
	cmd := exec.Command("sh", kindWaitScript(t), "iterion", ".", "-f", "values-dev.yaml")
	cmd.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	return stdout.String(), stderr.String(), fake, err
}

// TestChartKindWaitRefusesAnotherYq pins the helper's yq probe: its reads
// are mikefarah/yq v4 expressions over a piped stream, the python wrapper of
// the same name reads jq syntax, and mikefarah's own releases differ in the
// operators and input forms they take, so the yq on PATH is probed with
// them before the render is read. A yq that does not print the owed answer
// is refused, and the refusal names the yq it found, what the probe printed
// (on one line) and what its --version says. One case per way to miss: the
// wrapper, a yq that spells a current mikefarah v4 and fails the reads, one
// that exits 0 with another answer, and one whose every call fails.
func TestChartKindWaitRefusesAnotherYq(t *testing.T) {
	const v4 = "echo 'yq (https://github.com/mikefarah/yq/) version v4.53.3'"
	for _, tc := range []struct {
		name string
		body string   // the fake yq's shell body
		said []string // what the refusal must quote
	}{
		{"the python wrapper", "echo 'yq 3.4.3'", []string{"printed 'yq 3.4.3'", "says: yq 3.4.3"}},
		{"a current mikefarah v4 spelling that fails the reads", "case $1 in --version) " + v4 + " ;; *) echo 'Error: unknown command \"ea\" for \"yq\"' >&2; echo \"Run 'yq --help' for usage.\" >&2; exit 1 ;; esac", []string{`printed 'Error: unknown command "ea" for "yq" Run 'yq --help' for usage.'`, "says: yq (https://github.com/mikefarah/yq/) version v4.53.3"}},
		{"another answer on a zero exit", "case $1 in --version) " + v4 + " ;; *) cat >/dev/null; echo null ;; esac", []string{"printed 'null'"}},
		{"a yq whose every call fails", "echo 'yq: version unreadable' >&2; exit 3", []string{"printed 'yq: version unreadable'", "says: yq: version unreadable"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stdout, stderr, fake, err := runKindWaitWithYq(t, tc.body)
			if err == nil {
				t.Fatalf("chart-kind-wait ran on a yq that fails its probe and printed %q — the probe owed a refusal", strings.TrimSpace(stdout))
			}
			if out := strings.TrimSpace(stdout); out != "" {
				t.Fatalf("chart-kind-wait refused and still printed %q — a caller reading stdout alone would install with it", out)
			}
			own := helperLines(stderr)
			for _, want := range append([]string{"yq at " + fake + " fails the probe", "'!!int Deployment/a, ConfigMap/b' is owed", "put yq-go on PATH"}, tc.said...) {
				if !strings.Contains(own, want) {
					t.Fatalf("the refusal must name the yq it found, what the probe printed against what is owed, what its --version says and the package to put on PATH (%q missing):\n%s", want, stderr)
				}
			}
		})
	}
}

// TestChartKindWaitReadsWithAnyYqThatPassesItsProbe pins the other side of
// the probe: `yq --version` decides nothing. mikefarah/yq releases before
// v4.30.3 print their version without the v (`version 4.27.5`), and a
// --version that fails is only quoted in a refusal: a yq that prints the
// probe's owed answer reads the render, and values-dev's wait is printed.
// The fake answers --version and hands every other call to the real yq.
func TestChartKindWaitReadsWithAnyYqThatPassesItsProbe(t *testing.T) {
	yqBinary(t)
	realYq, err := exec.LookPath("yq")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(realYq, "'") {
		t.Fatalf("the real yq's path %q holds a quote the fake's single-quoted exec cannot carry", realYq)
	}
	for _, tc := range []struct {
		name    string
		version string // the fake's --version body
	}{
		{"a mikefarah release that spells its version without the v", "echo 'yq (https://github.com/mikefarah/yq/) version 4.27.5'; exit 0"},
		{"a --version that fails", "echo 'yq: version unreadable' >&2; exit 3"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stdout, stderr, _, err := runKindWaitWithYq(t, "if [ \"$1\" = --version ]; then "+tc.version+"; fi\nexec '"+realYq+"' \"$@\"")
			if err != nil {
				t.Fatalf("chart-kind-wait refused a yq that passes its probe: %v — %s", err, strings.TrimSpace(stderr))
			}
			if got, want := strings.TrimSpace(stdout), "1920s"; got != want {
				t.Fatalf("chart-kind-wait = %q, want %q — values-dev's 1800 s deadline plus the margin, read by a yq that passes the probe", got, want)
			}
			if own := helperLines(stderr); own != "" {
				t.Fatalf("chart-kind-wait reads values-dev with a yq that passes its probe and still writes:\n%s", own)
			}
		})
	}
}

// chartTemplates parses every file of a chart's templates directory with
// text/template's own parser — the one helm loads them with — the function
// names left unchecked (helm's and sprig's are not this package's to list),
// and returns the trees by name: each file under its path relative to dir,
// each define or block under the name it declares. A name defined twice is
// the parser's own refusal.
func chartTemplates(t *testing.T, dir string) map[string]*parse.Tree {
	t.Helper()
	trees := map[string]*parse.Tree{}
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return parseTemplate(trees, filepath.ToSlash(rel), string(raw))
	})
	if err != nil {
		t.Fatalf("parse the templates of %s: %v", dir, err)
	}
	return trees
}

// parseTemplate parses one template text into trees under name, the
// defines it holds beside it.
func parseTemplate(trees map[string]*parse.Tree, name, text string) error {
	tree := parse.New(name)
	tree.Mode = parse.SkipFuncCheck
	_, err := tree.Parse(text, "", "", trees)
	return err
}

// walkTemplate calls visit on n and on every node beneath it, in source
// order, with the pipeline each node sits in (nil outside one).
func walkTemplate(n parse.Node, pipe *parse.PipeNode, visit func(parse.Node, *parse.PipeNode)) {
	visit(n, pipe)
	switch n := n.(type) {
	case *parse.ListNode:
		for _, c := range n.Nodes {
			walkTemplate(c, nil, visit)
		}
	case *parse.ActionNode:
		walkTemplate(n.Pipe, nil, visit)
	case *parse.PipeNode:
		for _, v := range n.Decl {
			walkTemplate(v, n, visit)
		}
		for _, c := range n.Cmds {
			walkTemplate(c, n, visit)
		}
	case *parse.CommandNode:
		for _, a := range n.Args {
			walkTemplate(a, pipe, visit)
		}
	case *parse.ChainNode:
		walkTemplate(n.Node, pipe, visit)
	case *parse.IfNode:
		walkBranch(&n.BranchNode, visit)
	case *parse.RangeNode:
		walkBranch(&n.BranchNode, visit)
	case *parse.WithNode:
		walkBranch(&n.BranchNode, visit)
	case *parse.TemplateNode:
		if n.Pipe != nil {
			walkTemplate(n.Pipe, nil, visit)
		}
	}
}

// walkBranch walks the pipeline and both lists of an if, range or with.
func walkBranch(b *parse.BranchNode, visit func(parse.Node, *parse.PipeNode)) {
	walkTemplate(b.Pipe, nil, visit)
	walkTemplate(b.List, nil, visit)
	if b.ElseList != nil {
		walkTemplate(b.ElseList, nil, visit)
	}
}

// stateFields maps each field `helm template` renders from the client's
// stand-ins, not the cluster's, to what it reads: .Capabilities
// (off-cluster, the client's defaults stand in for the cluster's version
// and API set) and .Release.IsInstall / IsUpgrade / Revision (a template
// render is always an install at revision 1). .Release.Name, .Namespace
// and .Service are the install's own arguments and render the same.
var stateFields = map[string]string{
	"Capabilities": ".Capabilities",
	"IsInstall":    ".Release.IsInstall",
	"IsUpgrade":    ".Release.IsUpgrade",
	"Revision":     ".Release.Revision",
}

// hashFuncs reduce what flows into them to a digest.
var hashFuncs = map[string]bool{"sha256sum": true, "sha1sum": true, "adler32sum": true}

// stateField judges the field names of one chain, whatever receiver it
// starts from — dot, $, an alias of the root such as $root or .root, a
// parenthesised pipeline: a state field anywhere in the chain is a read,
// named, unless Values comes first, below which every key is the
// operator's data whatever it is named.
func stateField(fields []string) string {
	for _, f := range fields {
		if f == "Values" {
			return ""
		}
		if read := stateFields[f]; read != "" {
			return read
		}
	}
	return ""
}

// hashedAfter reports whether a hash function follows cmd in its pipeline,
// so what cmd renders lands in a digest.
func hashedAfter(pipe *parse.PipeNode, cmd *parse.CommandNode) bool {
	if pipe == nil {
		return false
	}
	after := false
	for _, c := range pipe.Cmds {
		if c == cmd {
			after = true
			continue
		}
		if fn, ok := c.Args[0].(*parse.IdentifierNode); after && ok && hashFuncs[fn.Ident] {
			return true
		}
	}
	return false
}

// renderPathReads walks the render path of the template named root — that
// tree and, transitively, every template it includes or calls by a literal
// name — and names every read `helm template` cannot reproduce: a state
// field through any receiver, a key spelled like one (index, get or dig on
// the root reach it), a lookup (empty off-cluster), and what the walk
// cannot follow — an include of a computed name, a tpl, whose text comes
// from values — unless a hash function follows it in its pipeline (the
// pod's checksum/config annotation: a digest of the ConfigMap, never the
// kind, the labels or the deadline the helper reads). Each entry reads
// "<file>:<line>:<col> (<template>): `<node>` <what>", sorted.
func renderPathReads(t *testing.T, trees map[string]*parse.Tree, root string) []string {
	t.Helper()
	if _, ok := trees[root]; !ok {
		t.Fatalf("no template named %q to walk", root)
	}
	var reads []string
	seen := map[string]bool{root: true}
	queue := []string{root}
	for len(queue) > 0 {
		name := queue[0]
		queue = queue[1:]
		tree := trees[name]
		note := func(n parse.Node, what string) {
			loc, _ := tree.ErrorContext(n)
			reads = append(reads, fmt.Sprintf("%s (%s): `%s` %s", loc, name, n, what))
		}
		follow := func(target string) {
			if _, ok := trees[target]; !ok {
				t.Fatalf("%s includes %q, which no template defines", name, target)
			}
			if !seen[target] {
				seen[target] = true
				queue = append(queue, target)
			}
		}
		walkTemplate(tree.Root, nil, func(n parse.Node, pipe *parse.PipeNode) {
			switch n := n.(type) {
			case *parse.TemplateNode:
				follow(n.Name)
			case *parse.CommandNode:
				fn, ok := n.Args[0].(*parse.IdentifierNode)
				if !ok {
					return
				}
				switch fn.Ident {
				case "include":
					if len(n.Args) > 1 {
						if s, ok := n.Args[1].(*parse.StringNode); ok {
							follow(s.Text)
							return
						}
					}
					if !hashedAfter(pipe, n) {
						note(n, "includes a computed name, which this guard cannot follow")
					}
				case "tpl":
					if !hashedAfter(pipe, n) {
						note(n, "renders text through tpl, which this guard cannot read")
					}
				}
			case *parse.IdentifierNode:
				if n.Ident == "lookup" {
					note(n, "reads lookup")
				}
			case *parse.FieldNode:
				if read := stateField(n.Ident); read != "" {
					note(n, "reads "+read)
				}
			case *parse.VariableNode:
				if read := stateField(n.Ident[1:]); read != "" {
					note(n, "reads "+read)
				}
			case *parse.ChainNode:
				if read := stateField(n.Field); read != "" {
					note(n, "reads "+read)
				}
			case *parse.StringNode:
				if read := stateFields[n.Text]; read != "" {
					note(n, "names "+read+" as a key")
				}
			}
		})
	}
	slices.Sort(reads)
	return reads
}

// textReads parses one template text as the server Deployment's own file
// and returns what the guard names on it.
func textReads(t *testing.T, text string) []string {
	t.Helper()
	trees := map[string]*parse.Tree{}
	if err := parseTemplate(trees, "server-deployment.yaml", text); err != nil {
		t.Fatalf("parse %q: %v", text, err)
	}
	return renderPathReads(t, trees, "server-deployment.yaml")
}

// TestChartKindWaitRenderReadsNoClusterState guards the helper's premise:
// `helm template` is the install's own render only while the templates the
// server Deployment renders through read nothing the client cannot see —
// .Capabilities, lookup, .Release.IsInstall / IsUpgrade / Revision. None is
// read today; a template author who reaches for one on that path reds
// here with the drift named, instead of handing the install a wait sized
// from a Deployment it does not apply. The templates are read through
// text/template's own parser, so a read is found whatever its receiver —
// the chart hands its root on as `(dict "root" $ …)`, and `$root.Release`
// or `.root.Capabilities` is its idiom, not a corner — and what the walk
// cannot follow (a computed include, a tpl) is named too, unless only its
// digest renders. The path is server-deployment.yaml and the templates it
// includes, transitively; the other templates may read what they like, the
// helper never reads them. Runs everywhere.
func TestChartKindWaitRenderReadsNoClusterState(t *testing.T) {
	t.Run("the chart's server path", func(t *testing.T) {
		if reads := renderPathReads(t, chartTemplates(t, "templates"), "server-deployment.yaml"); len(reads) != 0 {
			t.Fatalf("the server Deployment renders through a read `helm template` cannot see — scripts/chart-kind-wait.sh sizes the install's wait from that render, and the install would apply a different Deployment (the helper's header names the condition):\n  %s", strings.Join(reads, "\n  "))
		}
	})

	// The guard on a scratch templates dir: a read on the path is named
	// through two includes and through the root handed on in a dict, and a
	// read in a template the server Deployment never includes is not the
	// helper's business.
	t.Run("follows the includes and ignores a template off the path", func(t *testing.T) {
		dir := t.TempDir()
		write := func(name, text string) {
			t.Helper()
			if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		write("server-deployment.yaml", "kind: Deployment\nmetadata:\n  name: {{ include \"scratch.name\" . }}\n  annotations:\n    checksum/config: {{ include (print $.Template.BasePath \"/configmap.yaml\") . | sha256sum }}\nspec:\n  {{- with (include \"scratch.scheduling\" (dict \"root\" $ \"component\" \"server\")) }}\n  {{- . | nindent 2 }}\n  {{- end }}\n")
		write("_helpers.tpl", `{{/* the name, through a nested if */}}
{{- define "scratch.name" -}}
{{- if .Values.x -}}{{ include "scratch.deep" . }}{{- else -}}x{{- end -}}
{{- end -}}
{{- define "scratch.deep" -}}
{{ (lookup "v1" "ConfigMap" .Release.Namespace "x").data.name }}
{{- end -}}
{{- define "scratch.scheduling" -}}
{{- $root := .root -}}
{{- if $root.Release.IsUpgrade }}upgraded: true{{ end }}
{{- end -}}
{{- define "scratch.aside" -}}
{{ .Capabilities.KubeVersion.Major }}
{{- end -}}
`)
		reads := renderPathReads(t, chartTemplates(t, dir), "server-deployment.yaml")
		want := []string{
			" (scratch.deep): `lookup` reads lookup",
			" (scratch.scheduling): `$root.Release.IsUpgrade` reads .Release.IsUpgrade",
		}
		if len(reads) != len(want) {
			t.Fatalf("a lookup two includes deep and a $root.Release.IsUpgrade behind the dict the root is handed on in must be the two reads named, got %q", reads)
		}
		for _, w := range want {
			if !slices.ContainsFunc(reads, func(r string) bool { return strings.HasSuffix(r, w) }) {
				t.Fatalf("no read ends with %q — a lookup two includes deep and a $root.Release.IsUpgrade behind the dict the root is handed on in must both be named, got %q", w, reads)
			}
		}
	})

	t.Run("each read is recognised whatever its receiver, the install's own arguments are not", func(t *testing.T) {
		for _, text := range []string{
			`{{ .Capabilities.KubeVersion.Major }}`,
			`{{ if $.Capabilities.APIVersions.Has "policy/v1" }}x{{ end }}`,
			`{{ lookup "v1" "Secret" .Release.Namespace "x" }}`,
			`{{ $s := (lookup "v1" "Secret" "ns" "x").data }}`,
			`{{ if .Release.IsUpgrade }}x{{ end }}`,
			`{{ if not .Release.IsInstall }}x{{ end }}`,
			`{{ .Release.Revision }}`,
			// The root reached through an alias: the chart's own idiom.
			`{{ $root := . }}{{ $root.Release.IsUpgrade }}`,
			`{{ .root.Capabilities.KubeVersion }}`,
			`{{ $root := .root }}{{ if $root.Capabilities.APIVersions.Has "policy/v1" }}x{{ end }}`,
			`{{ (index . "root").Capabilities.KubeVersion }}`,
			`{{ $rel := .Release }}{{ $rel.Revision }}`,
			`{{ with .Release }}{{ .IsUpgrade }}{{ end }}`,
			// A key lookup on the root or the release.
			`{{ index $ "Capabilities" }}`,
			`{{ get .Release "IsInstall" }}`,
			`{{ dig "Release" "Revision" 0 $ }}`,
			// Text the walk cannot follow.
			`{{ tpl .Values.server.extra . }}`,
			`{{ include (printf "iterion.%s" .Values.x) . }}`,
			`{{ $name := "x" }}{{ include $name . }}`,
		} {
			if reads := textReads(t, text); len(reads) != 1 {
				t.Fatalf("`%s` must be one read, got %q", text, reads)
			}
		}
		for _, text := range []string{
			`{{ .Release.Name }}`,
			`{{ $.Release.Namespace }}`,
			`{{ .Release.Service }}`,
			`{{ $root := .root }}{{ $root.Release.Name }}`,
			`{{ .Values.lookup }}`,
			`{{ .Values.Capabilities.enabled }}`,
			`{{ $.Values.Release.IsUpgrade }}`,
			`{{ .root.Values.Revision }}`,
			`{{ include (print $.Template.BasePath "/configmap.yaml") . | sha256sum }}`,
			`{{ tpl .Values.server.extra . | sha256sum }}`,
		} {
			if reads := textReads(t, text); len(reads) != 0 {
				t.Fatalf("`%s` renders the same off-cluster and must not be a read, got %q", text, reads)
			}
		}
	})
}
