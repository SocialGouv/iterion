package chart_test

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/rendergate"
	"gopkg.in/yaml.v3"
)

// The chart render tests: `helm template` on the dev, prod and fixture
// values, then assertions on the manifests — the dead-selector gate of
// #2012 over every render, and the guards the templates promise. They run
// under `task chart:check` and the helm-lint CI job, both with
// ITERION_CHART_RENDER_REQUIRE=1 so a missing helm is red there; a plain
// `go test ./...` on a machine without helm skips them and says so.

const renderNamespace = "iterion"

func helmBinary(t *testing.T) string {
	t.Helper()
	path, err := exec.LookPath("helm")
	if err == nil {
		return path
	}
	if os.Getenv("ITERION_CHART_RENDER_REQUIRE") == "1" {
		t.Fatalf("helm is required here (ITERION_CHART_RENDER_REQUIRE=1) and not in PATH: %v", err)
	}
	t.Skipf("helm not in PATH (%v) — the chart render tests run under `task chart:check` and the helm-lint CI job", err)
	return ""
}

// render runs `helm template iterion . --namespace iterion <args>`.
func render(t *testing.T, args ...string) (string, error) {
	t.Helper()
	helm := helmBinary(t)
	cmd := exec.Command(helm, append([]string{"template", "iterion", ".", "--namespace", renderNamespace}, args...)...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("helm template %s: %v\n%s", strings.Join(args, " "), err, stderr.String())
	}
	return stdout.String(), nil
}

func mustRender(t *testing.T, args ...string) string {
	t.Helper()
	out, err := render(t, args...)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// mustRefuse asserts the render fails with a message carrying want.
func mustRefuse(t *testing.T, want string, args ...string) {
	t.Helper()
	_, err := render(t, args...)
	if err == nil {
		t.Fatalf("helm template %s rendered; want a refusal mentioning %q", strings.Join(args, " "), want)
	}
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("helm template %s refused, but not with %q:\n%v", strings.Join(args, " "), want, err)
	}
}

func manifests(t *testing.T, stream string) []map[string]any {
	t.Helper()
	dec := yaml.NewDecoder(strings.NewReader(stream))
	var out []map[string]any
	for {
		var doc any
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatalf("parse render: %v", err)
		}
		if m, ok := doc.(map[string]any); ok && m["kind"] != nil {
			out = append(out, m)
		}
	}
}

func find(docs []map[string]any, kind, name string) map[string]any {
	for _, d := range docs {
		meta, _ := d["metadata"].(map[string]any)
		if d["kind"] == kind && meta["name"] == name {
			return d
		}
	}
	return nil
}

func must(t *testing.T, docs []map[string]any, kind, name string) map[string]any {
	t.Helper()
	d := find(docs, kind, name)
	if d == nil {
		t.Fatalf("no %s %q in the render", kind, name)
	}
	return d
}

// dig walks maps by key and lists by index ("0").
func dig(v any, path ...string) any {
	for _, p := range path {
		switch c := v.(type) {
		case map[string]any:
			v = c[p]
		case []any:
			var i int
			if _, err := fmt.Sscanf(p, "%d", &i); err != nil || i < 0 || i >= len(c) {
				return nil
			}
			v = c[i]
		default:
			return nil
		}
	}
	return v
}

func podSpec(t *testing.T, docs []map[string]any, kind, name string) map[string]any {
	t.Helper()
	spec, _ := dig(must(t, docs, kind, name), "spec", "template", "spec").(map[string]any)
	if spec == nil {
		t.Fatalf("%s %s has no pod spec", kind, name)
	}
	return spec
}

func stringMap(v any) map[string]string {
	out := map[string]string{}
	m, _ := v.(map[string]any)
	for k, val := range m {
		out[k] = fmt.Sprint(val)
	}
	return out
}

func sameLabels(got map[string]string, want map[string]string) bool {
	if len(got) != len(want) {
		return false
	}
	for k, v := range want {
		if got[k] != v {
			return false
		}
	}
	return true
}

// judgeClean runs the dead-selector gate over a render and fails on any
// dead selector; it also refuses a render where nothing was judged.
func judgeClean(t *testing.T, stream string, minJudged int) {
	t.Helper()
	report, err := rendergate.Judge([]byte(stream))
	if err != nil {
		t.Fatalf("render gate: %v", err)
	}
	for _, f := range report.Dead() {
		t.Errorf("dead selector: %s", f)
	}
	for _, f := range report.Findings {
		if f.Verdict != rendergate.Dead {
			t.Logf("render gate: %s", f)
		}
	}
	if report.Judged < minJudged {
		t.Errorf("the gate judged %d selectors, want at least %d — a gate that judges nothing proves nothing (%s)", report.Judged, minJudged, report.Summary())
	}
}

const deadValkeySelector = `{"podAntiAffinity":{"requiredDuringSchedulingIgnoredDuringExecution":[{"topologyKey":"kubernetes.io/hostname","labelSelector":{"matchExpressions":[{"key":"app.kubernetes.io/name","operator":"In","values":["valkey"]}]}}]}}`

func TestChartRenderScheduling(t *testing.T) {
	ownLabels := func(component string) map[string]string {
		return map[string]string{"app.kubernetes.io/name": "iterion", "app.kubernetes.io/instance": "iterion", "app.kubernetes.io/component": component}
	}
	assertSpread := func(t *testing.T, spec map[string]any, want map[string]string, hard bool) {
		t.Helper()
		var term any
		if hard {
			term = dig(spec, "affinity", "podAntiAffinity", "requiredDuringSchedulingIgnoredDuringExecution", "0")
		} else {
			term = dig(spec, "affinity", "podAntiAffinity", "preferredDuringSchedulingIgnoredDuringExecution", "0", "podAffinityTerm")
		}
		if term == nil {
			t.Fatalf("no anti-affinity term (hard=%v) in %v", hard, dig(spec, "affinity"))
		}
		if got := dig(term, "topologyKey"); got != "kubernetes.io/hostname" {
			t.Errorf("anti-affinity topologyKey = %v", got)
		}
		if got := stringMap(dig(term, "labelSelector", "matchLabels")); !sameLabels(got, want) {
			t.Errorf("anti-affinity selector = %v, want the pod's own labels %v", got, want)
		}
		constraint := dig(spec, "topologySpreadConstraints", "0")
		if constraint == nil {
			t.Fatalf("no topologySpreadConstraint")
		}
		if got := fmt.Sprint(dig(constraint, "maxSkew")); got != "1" {
			t.Errorf("maxSkew = %v", got)
		}
		if got := dig(constraint, "whenUnsatisfiable"); got != "ScheduleAnyway" {
			t.Errorf("whenUnsatisfiable = %v", got)
		}
		if got := stringMap(dig(constraint, "labelSelector", "matchLabels")); !sameLabels(got, want) {
			t.Errorf("spread selector = %v, want %v", got, want)
		}
		if got := stringMap(dig(spec, "topologySpreadConstraints", "0", "labelSelector", "matchLabels")); len(got) == 0 {
			t.Errorf("the spread selector is empty")
		}
	}

	t.Run("every render is free of dead selectors", func(t *testing.T) {
		judgeClean(t, mustRender(t, "-f", "values-dev.yaml"), 4)
		judgeClean(t, mustRender(t, "-f", "values-prod.yaml"), 6)
		judgeClean(t, mustRender(t, "-f", "values-prod.yaml", "-f", "testdata/values-pools.yaml"), 10)
	})

	t.Run("the gate names a dead selector in a real render", func(t *testing.T) {
		// The valkeyha defect, on the chart's own server: an operator term
		// selecting a name no pod carries. The generated term stays alive, the
		// operator's is dead, and the finding names the workload and the key.
		out := mustRender(t, "-f", "values-prod.yaml", "--set-json", "server.affinity="+deadValkeySelector)
		report, err := rendergate.Judge([]byte(out))
		if err != nil {
			t.Fatal(err)
		}
		dead := report.Dead()
		if len(dead) != 1 {
			t.Fatalf("want exactly one dead selector, got %d: %+v", len(dead), report.Findings)
		}
		if dead[0].Kind != "Deployment" || dead[0].Name != "iterion" {
			t.Errorf("the finding names %s %s, want Deployment iterion", dead[0].Kind, dead[0].Name)
		}
		if strings.Join(dead[0].DeadKeys, ",") != "app.kubernetes.io/name" {
			t.Errorf("dead keys = %v", dead[0].DeadKeys)
		}
		if line := dead[0].String(); !strings.Contains(line, "Deployment iterion") || !strings.Contains(line, "app.kubernetes.io/name") {
			t.Errorf("printed line %q must name the workload and the key", line)
		}
	})

	t.Run("server and runner spread their own replicas", func(t *testing.T) {
		docs := manifests(t, mustRender(t, "-f", "values-prod.yaml"))
		assertSpread(t, podSpec(t, docs, "Deployment", "iterion"), ownLabels("server"), false)
		assertSpread(t, podSpec(t, docs, "Deployment", "iterion-runner"), ownLabels("runner"), false)
		for _, name := range []string{"iterion-server", "iterion-runner"} {
			pdb := must(t, docs, "PodDisruptionBudget", name)
			if got := fmt.Sprint(dig(pdb, "spec", "minAvailable")); got != "1" {
				t.Errorf("%s minAvailable = %v", name, got)
			}
		}
	})

	t.Run("each pool spreads on its own label and gets a budget above one replica", func(t *testing.T) {
		docs := manifests(t, mustRender(t, "-f", "values-prod.yaml", "-f", "testdata/values-pools.yaml"))
		alpha := ownLabels("runner-pool")
		alpha["iterion.io/runner-pool"] = "alpha"
		assertSpread(t, podSpec(t, docs, "Deployment", "iterion-runner-pool-alpha"), alpha, false)
		must(t, docs, "PodDisruptionBudget", "iterion-runner-pool-alpha")
		solo := ownLabels("runner-pool")
		solo["iterion.io/runner-pool"] = "solo"
		assertSpread(t, podSpec(t, docs, "Deployment", "iterion-runner-pool-solo"), solo, true)
		if find(docs, "PodDisruptionBudget", "iterion-runner-pool-solo") != nil {
			t.Errorf("a single-replica pool must not get a budget that would block its node's drain")
		}
	})

	t.Run("hard preset renders a required term", func(t *testing.T) {
		docs := manifests(t, mustRender(t, "-f", "values-prod.yaml", "--set", "server.scheduling.antiAffinity=hard"))
		assertSpread(t, podSpec(t, docs, "Deployment", "iterion"), ownLabels("server"), true)
	})

	t.Run("none renders the operator affinity alone", func(t *testing.T) {
		docs := manifests(t, mustRender(t, "-f", "values-prod.yaml", "--set", "server.scheduling.antiAffinity=none", "--set", "server.scheduling.spread.enabled=false"))
		spec := podSpec(t, docs, "Deployment", "iterion")
		if spec["affinity"] != nil || spec["topologySpreadConstraints"] != nil {
			t.Errorf("none + spread off must render nothing, got affinity=%v spread=%v", spec["affinity"], spec["topologySpreadConstraints"])
		}
	})

	t.Run("operator affinity merges over the generated one", func(t *testing.T) {
		node := `{"nodeAffinity":{"requiredDuringSchedulingIgnoredDuringExecution":{"nodeSelectorTerms":[{"matchExpressions":[{"key":"pool","operator":"In","values":["platform"]}]}]}}}`
		docs := manifests(t, mustRender(t, "-f", "values-prod.yaml", "--set-json", "server.affinity="+node))
		spec := podSpec(t, docs, "Deployment", "iterion")
		if dig(spec, "affinity", "nodeAffinity") == nil {
			t.Errorf("the operator's nodeAffinity is dropped")
		}
		assertSpread(t, spec, ownLabels("server"), false)
	})

	t.Run("a budget is honest about single replicas", func(t *testing.T) {
		noBudget := func(args ...string) {
			t.Helper()
			docs := manifests(t, mustRender(t, args...))
			if find(docs, "PodDisruptionBudget", "iterion-server") != nil {
				t.Errorf("%v: a server budget rendered on a single guaranteed replica", args)
			}
		}
		noBudget("-f", "values-prod.yaml", "--set", "server.hpa.enabled=false", "--set", "server.replicas=1")
		noBudget("-f", "values-prod.yaml", "--set", "server.hpa.minReplicas=1")
		docs := manifests(t, mustRender(t, "-f", "values-prod.yaml", "--set", "server.hpa.minReplicas=1", "--set", "server.pdb.enabled=true"))
		must(t, docs, "PodDisruptionBudget", "iterion-server")
		docs = manifests(t, mustRender(t, "-f", "values-prod.yaml", "--set", "server.replicas=3", "--set", "server.pdb.enabled=false"))
		if find(docs, "PodDisruptionBudget", "iterion-server") != nil {
			t.Errorf("pdb.enabled=false must drop the budget")
		}
		keda := []string{"-f", "values-prod.yaml", "--set", "runner.keda.enabled=true", "--set", "config.nats.monitoringEndpoint=nats:8222"}
		docs = manifests(t, mustRender(t, append(keda, "--set", "runner.keda.minReplicas=1")...))
		if find(docs, "PodDisruptionBudget", "iterion-runner") != nil {
			t.Errorf("a KEDA pool whose floor is one replica must not get a budget")
		}
		docs = manifests(t, mustRender(t, append(keda, "--set", "runner.keda.minReplicas=2")...))
		must(t, docs, "PodDisruptionBudget", "iterion-runner")
		docs = manifests(t, mustRender(t, "-f", "values-prod.yaml", "--set", "server.pdb.maxUnavailable=1"))
		pdb := must(t, docs, "PodDisruptionBudget", "iterion-server")
		if fmt.Sprint(dig(pdb, "spec", "maxUnavailable")) != "1" || dig(pdb, "spec", "minAvailable") != nil {
			t.Errorf("maxUnavailable must replace minAvailable, got %v", pdb["spec"])
		}
	})

	t.Run("unknown knob values are refused", func(t *testing.T) {
		mustRefuse(t, "scheduling.antiAffinity must be", "-f", "values-prod.yaml", "--set", "server.scheduling.antiAffinity=medium")
		mustRefuse(t, "whenUnsatisfiable must be", "-f", "values-prod.yaml", "--set", "runner.scheduling.spread.whenUnsatisfiable=Maybe")
		mustRefuse(t, "pdb.enabled must be", "-f", "values-prod.yaml", "--set", "server.pdb.enabled=sometimes")
	})
}

// TestChartRenderSubChartArchivesAreDeclared refuses a sub-chart archive
// left in charts/ that Chart.yaml no longer declares: helm renders every
// archive of that directory, declared or not, so a stale one silently
// resurrects the sub-chart it came from. Needs no helm.
func TestChartRenderSubChartArchivesAreDeclared(t *testing.T) {
	raw, err := os.ReadFile("Chart.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var chart struct {
		Dependencies []struct {
			Name string `yaml:"name"`
		} `yaml:"dependencies"`
	}
	if err := yaml.Unmarshal(raw, &chart); err != nil {
		t.Fatal(err)
	}
	declared := map[string]bool{}
	for _, dep := range chart.Dependencies {
		declared[dep.Name] = true
	}
	archives, _ := filepath.Glob("charts/*.tgz")
	version := regexp.MustCompile(`-[0-9][^/]*\.tgz$`)
	for _, archive := range archives {
		name := version.ReplaceAllString(filepath.Base(archive), "")
		if !declared[name] {
			t.Errorf("%s is not declared in Chart.yaml — helm renders every archive in charts/, declared or not; remove it (`task chart:dep-update` regenerates the directory)", archive)
		}
	}
}
