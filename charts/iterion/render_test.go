package chart_test

import (
	"bytes"
	"encoding/json"
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
	return renderAs(t, "iterion", args...)
}

// renderAs is render under another release name.
func renderAs(t *testing.T, release string, args ...string) (string, error) {
	t.Helper()
	helm := helmBinary(t)
	cmd := exec.Command(helm, append([]string{"template", release, ".", "--namespace", renderNamespace}, args...)...)
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
// valuesFile writes an operator-shaped values file: the one place where
// `{}`, `[]` and null survive as written (sprig and --set read them away).
func valuesFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "values.yaml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func judgeClean(t *testing.T, stream string, minJudged int) {
	t.Helper()
	report, err := rendergate.JudgeWithOptions([]byte(stream), rendergate.Options{DefaultNamespace: renderNamespace})
	if err != nil {
		t.Fatalf("render gate: %v", err)
	}
	for _, f := range report.Dead() {
		t.Errorf("dead selector: %s", f)
	}
	for _, w := range report.Warnings {
		t.Errorf("the chart's own render must carry no defect: %s", w)
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
		report, err := rendergate.JudgeWithOptions([]byte(out), rendergate.Options{DefaultNamespace: renderNamespace})
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
		// A term list of the SAME kind replaces the generated one (lists do not
		// merge): the documented truth, so nobody expects both terms.
		preferred := `{"podAntiAffinity":{"preferredDuringSchedulingIgnoredDuringExecution":[{"weight":1,"podAffinityTerm":{"topologyKey":"zone","labelSelector":{"matchLabels":{"app.kubernetes.io/component":"runner"}}}}]}}`
		docs = manifests(t, mustRender(t, "-f", "values-prod.yaml", "--set-json", "server.affinity="+preferred))
		terms, _ := dig(podSpec(t, docs, "Deployment", "iterion"), "affinity", "podAntiAffinity", "preferredDuringSchedulingIgnoredDuringExecution").([]any)
		if len(terms) != 1 || dig(terms[0], "podAffinityTerm", "topologyKey") != "zone" {
			t.Errorf("an operator term list of the same kind must replace the generated one, got %v", terms)
		}
	})

	t.Run("a pool that spells an empty nodeSelector or tolerations sheds the runner's", func(t *testing.T) {
		// The block as written, even empty: sprig default reads {} and []
		// as absent, and a sovereign pool meant for its own nodes would keep
		// the fleet's selector and the tolerations that admit it to the
		// runner's tainted nodes.
		values := valuesFile(t, "runner:\n  nodeSelector: {workload: general}\n  tolerations: [{key: general, operator: Exists, effect: NoSchedule}]\nrunnerPools:\n  - name: sovereign\n    gateway: {baseUrl: https://gateway.sovereign.example, existingSecret: iterion-pool-sovereign-gateway}\n    nodeSelector: {}\n    tolerations: []\n  - name: shared\n    gateway: {baseUrl: https://gateway.shared.example, existingSecret: iterion-pool-shared-gateway}\n")
		docs := manifests(t, mustRender(t, "-f", "values-prod.yaml", "-f", values))
		sovereign := podSpec(t, docs, "Deployment", "iterion-runner-pool-sovereign")
		if sovereign["nodeSelector"] != nil || sovereign["tolerations"] != nil {
			t.Errorf("an empty block must opt out, got nodeSelector=%v tolerations=%v", sovereign["nodeSelector"], sovereign["tolerations"])
		}
		shared := podSpec(t, docs, "Deployment", "iterion-runner-pool-shared")
		if tolerations, _ := shared["tolerations"].([]any); dig(shared, "nodeSelector", "workload") != "general" || len(tolerations) != 1 {
			t.Errorf("a pool that spells nothing inherits, got nodeSelector=%v tolerations=%v", shared["nodeSelector"], shared["tolerations"])
		}
	})

	t.Run("a pool inherits the runner's placement and merges its own over it", func(t *testing.T) {
		runnerNode := `{"nodeAffinity":{"requiredDuringSchedulingIgnoredDuringExecution":{"nodeSelectorTerms":[{"matchExpressions":[{"key":"pool","operator":"In","values":["platform"]}]}]}}}`
		docs := manifests(t, mustRender(t, "-f", "values-prod.yaml", "-f", "testdata/values-pools.yaml",
			"--set", "runner.nodeSelector.disk=ssd", "--set-json", `runner.tolerations=[{"key":"dedicated","operator":"Equal","value":"runners","effect":"NoSchedule"}]`,
			"--set-json", "runner.affinity="+runnerNode,
			"--set-json", `runnerPools[0].affinity={"podAffinity":{"requiredDuringSchedulingIgnoredDuringExecution":[{"topologyKey":"zone","labelSelector":{"matchLabels":{"app.kubernetes.io/component":"server"}}}]}}`,
			"--set-json", `runnerPools[0].pdb={"enabled":false}`,
			"--set-json", `runnerPools[0].gateway={"baseUrl":"https://gateway.alpha.example","existingSecret":"iterion-pool-alpha-gateway"}`,
			"--set", "runnerPools[0].name=alpha", "--set", "runnerPools[0].replicas=2"))
		spec := podSpec(t, docs, "Deployment", "iterion-runner-pool-alpha")
		if dig(spec, "nodeSelector", "disk") != "ssd" {
			t.Errorf("the pool must inherit runner.nodeSelector, got %v", spec["nodeSelector"])
		}
		if tolerations, _ := spec["tolerations"].([]any); len(tolerations) != 1 {
			t.Errorf("the pool must inherit runner.tolerations, got %v", spec["tolerations"])
		}
		if dig(spec, "affinity", "nodeAffinity") == nil {
			t.Errorf("the pool must keep the runner's nodeAffinity under its own podAffinity")
		}
		if dig(spec, "affinity", "podAffinity") == nil {
			t.Errorf("the pool's own podAffinity is dropped")
		}
		if find(docs, "PodDisruptionBudget", "iterion-runner-pool-alpha") != nil {
			t.Errorf("a pool's pdb.enabled: false (a YAML bool) must drop its budget")
		}
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
		// Zero is a value, never replaced: minAvailable 0 allows every
		// disruption (the budget an operator writes to NOT block drains),
		// maxUnavailable 0 forbids them all.
		docs = manifests(t, mustRender(t, "-f", "values-prod.yaml", "--set", "server.pdb.minAvailable=0"))
		if pdb := must(t, docs, "PodDisruptionBudget", "iterion-server"); fmt.Sprint(dig(pdb, "spec", "minAvailable")) != "0" {
			t.Errorf("minAvailable 0 must render 0, got %v", pdb["spec"])
		}
		docs = manifests(t, mustRender(t, "-f", "values-prod.yaml", "--set", "server.pdb.maxUnavailable=0"))
		if pdb := must(t, docs, "PodDisruptionBudget", "iterion-server"); fmt.Sprint(dig(pdb, "spec", "maxUnavailable")) != "0" || dig(pdb, "spec", "minAvailable") != nil {
			t.Errorf("maxUnavailable 0 must render 0, got %v", pdb["spec"])
		}
		// "" and null are unset: the budget falls back to minAvailable 1,
		// never to an EMPTY budget — a PDB with neither field lets the
		// disruption controller evict every pod.
		docs = manifests(t, mustRender(t, "-f", "values-prod.yaml", "--set-json", `server.pdb.minAvailable=""`))
		if pdb := must(t, docs, "PodDisruptionBudget", "iterion-server"); fmt.Sprint(dig(pdb, "spec", "minAvailable")) != "1" || dig(pdb, "spec", "maxUnavailable") != nil {
			t.Errorf(`minAvailable "" must fall back to 1, got %v`, pdb["spec"])
		}
		pools := valuesFile(t, "runner:\n  pdb:\n    maxUnavailable: 1\nrunnerPools:\n  - name: pminempty\n    replicas: 2\n    gateway: {baseUrl: https://gateway.example, existingSecret: iterion-pool-gateway}\n    pdb: {minAvailable: \"\"}\n  - name: pmaxnull\n    replicas: 2\n    gateway: {baseUrl: https://gateway.example, existingSecret: iterion-pool-gateway}\n    pdb: {minAvailable: 2, maxUnavailable: null}\n")
		docs = manifests(t, mustRender(t, "-f", "values-prod.yaml", "-f", pools))
		if pdb := must(t, docs, "PodDisruptionBudget", "iterion-runner-pool-pminempty"); fmt.Sprint(dig(pdb, "spec", "maxUnavailable")) != "1" || dig(pdb, "spec", "minAvailable") != nil {
			t.Errorf("a pool's empty minAvailable keeps the runner's maxUnavailable, got %v", pdb["spec"])
		}
		if pdb := must(t, docs, "PodDisruptionBudget", "iterion-runner-pool-pmaxnull"); fmt.Sprint(dig(pdb, "spec", "minAvailable")) != "2" || dig(pdb, "spec", "maxUnavailable") != nil {
			t.Errorf("a pool's null maxUnavailable is unset and its minAvailable 2 stands, got %v", pdb["spec"])
		}
	})

	t.Run("unknown knob values are refused", func(t *testing.T) {
		mustRefuse(t, "scheduling.antiAffinity must be", "-f", "values-prod.yaml", "--set", "server.scheduling.antiAffinity=medium")
		mustRefuse(t, "whenUnsatisfiable must be", "-f", "values-prod.yaml", "--set", "runner.scheduling.spread.whenUnsatisfiable=Maybe")
		mustRefuse(t, "pdb.enabled must be", "-f", "values-prod.yaml", "--set", "server.pdb.enabled=sometimes")
		mustRefuse(t, "maxSkew must be at least 1", "-f", "values-prod.yaml", "--set", "server.scheduling.spread.maxSkew=0")
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

// ruleGroups indexes the alerting rules of the release's PrometheusRule by
// group name.
func ruleGroups(t *testing.T, docs []map[string]any) map[string][]map[string]any {
	t.Helper()
	pr := must(t, docs, "PrometheusRule", "iterion")
	groups, _ := dig(pr, "spec", "groups").([]any)
	out := map[string][]map[string]any{}
	for _, g := range groups {
		gm, _ := g.(map[string]any)
		name, _ := gm["name"].(string)
		rules, _ := gm["rules"].([]any)
		for _, r := range rules {
			rm, _ := r.(map[string]any)
			out[name] = append(out[name], rm)
		}
	}
	return out
}

// rule finds the rule with the alert name whose labels carry every given
// label; nil when none does.
func rule(rules []map[string]any, alert string, labels map[string]string) map[string]any {
	for _, r := range rules {
		if r["alert"] != alert {
			continue
		}
		got := stringMap(r["labels"])
		matched := true
		for k, v := range labels {
			if got[k] != v {
				matched = false
			}
		}
		if matched {
			return r
		}
	}
	return nil
}

func envMap(container any) map[string]map[string]any {
	out := map[string]map[string]any{}
	env, _ := dig(container, "env").([]any)
	for _, e := range env {
		m, _ := e.(map[string]any)
		name, _ := m["name"].(string)
		out[name] = m
	}
	return out
}

func TestChartRenderDatastoreAlerts(t *testing.T) {
	base := []string{"-f", "values-prod.yaml", "-f", "testdata/values-datastores.yaml"}
	rules := ruleGroups(t, manifests(t, mustRender(t, base...)))["iterion.datastores"]
	if len(rules) == 0 {
		t.Fatal("no iterion.datastores group in the render")
	}

	t.Run("a match or a selector PromQL cannot read is refused at render", func(t *testing.T) {
		// Prometheus drops the WHOLE rule file on one unparsable expression:
		// an unanchored RE2 fragment and PromQL's own string escapes only.
		mustRefuse(t, "must not be anchored", append(base, "--set", `metrics.prometheusRule.datastores.statefulsets[0].match=^.*-mongodb$`)...)
		mustRefuse(t, "missing closing", append(base, "--set", `metrics.prometheusRule.datastores.statefulsets[0].match=.*-mongodb(`)...)
		mustRefuse(t, "backslash PromQL cannot read", append(base, "--set-json", `metrics.prometheusRule.datastores.mongodb.selector="pod=~\"mongo-\\d+\""`)...)
		// The escapes PromQL knows pass: a regex escape written twice.
		mustRender(t, append(base, "--set-json", `metrics.prometheusRule.datastores.mongodb.selector="pod=~\"mongo-\\\\d+\""`)...)
	})

	t.Run("one replica rule and one restart rule per store", func(t *testing.T) {
		for _, store := range []string{"mongodb", "nats", "valkeyha", "valkeyfiler", "seaweedfs"} {
			r := rule(rules, "IterionDatastoreReplicasMissing", map[string]string{"datastore": store})
			if r == nil {
				t.Errorf("no IterionDatastoreReplicasMissing rule for %s", store)
				continue
			}
			expr := fmt.Sprint(r["expr"])
			if !strings.Contains(expr, "kube_statefulset_status_replicas_ready{") || !strings.Contains(expr, " < kube_statefulset_replicas{") || !strings.Contains(expr, `namespace="iterion"`) {
				t.Errorf("%s replica rule expr = %q", store, expr)
			}
			// The match is grouped: an alternation spans the whole name and the
			// pod suffix of the restart rule applies to every branch.
			if !strings.Contains(expr, `statefulset=~"(?:`) {
				t.Errorf("%s replica rule must group the match, got %q", store, expr)
			}
			restart := rule(rules, "IterionDatastoreRestarting", map[string]string{"datastore": store})
			if restart == nil {
				t.Errorf("no IterionDatastoreRestarting rule for %s", store)
			} else if !strings.Contains(fmt.Sprint(restart["expr"]), `pod=~"(?:`) || !strings.Contains(fmt.Sprint(restart["expr"]), `)-[0-9]+"`) {
				t.Errorf("%s restart rule must apply the pod suffix to a grouped match, got %q", store, restart["expr"])
			}
		}
	})

	t.Run("the store-level signals of the ticket", func(t *testing.T) {
		signals := []struct{ alert, family, series, threshold string }{
			{"IterionValkeyReplicasDisconnected", "valkey", "redis_connected_slaves{", "< 2"},
			{"IterionValkeyAOFWriteFailing", "valkey", "redis_aof_last_write_status{", "== 0"},
			{"IterionSeaweedFSVolumesMissing", "seaweedfs", "SeaweedFS_volumeServer_volumes{", "< 4"},
			{"IterionSeaweedFSNoLeader", "seaweedfs", "SeaweedFS_master_is_leader{", "== 0"},
			{"IterionMongoDBMemberOutOfSet", "mongodb", "mongodb_rs_members_state{", `member_state!~"PRIMARY|SECONDARY|ARBITER"`},
			{"IterionMongoDBMemberUnhealthy", "mongodb", "mongodb_rs_members_health{", "== 0"},
			{"IterionMongoDBNoPrimary", "mongodb", `member_state="PRIMARY"`, `< 1) and (count(mongodb_rs_members_state{namespace="iterion", pod=~".*mongodb.*"}) > 0)`},
			{"IterionSeaweedFSUnderReplicated", "seaweedfs", `* on(namespace, pod) group_left() (SeaweedFS_master_is_leader{`, "== 1)) > 0"},
		}
		for _, s := range signals {
			r := rule(rules, s.alert, map[string]string{"family": s.family})
			if r == nil {
				t.Errorf("no %s rule", s.alert)
				continue
			}
			expr := fmt.Sprint(r["expr"])
			if !strings.Contains(expr, s.series) || !strings.Contains(expr, s.threshold) || !strings.Contains(expr, `namespace="iterion"`) {
				t.Errorf("%s expr = %q, want %q and %q on the release namespace", s.alert, expr, s.series, s.threshold)
			}
		}
	})

	t.Run("every exporter family has an absent-signal rule", func(t *testing.T) {
		for _, family := range []string{"kube-state-metrics", "valkey", "seaweedfs", "seaweedfs-master", "mongodb"} {
			r := rule(rules, "IterionDatastoreSignalAbsent", map[string]string{"family": family})
			if r == nil {
				t.Errorf("no absent-signal rule for %s", family)
				continue
			}
			if expr := fmt.Sprint(r["expr"]); !strings.HasPrefix(expr, "absent(") {
				t.Errorf("%s absent rule expr = %q", family, expr)
			}
		}
	})

	t.Run("off by default and per family", func(t *testing.T) {
		docs := manifests(t, mustRender(t, "-f", "values-prod.yaml", "--set", "metrics.prometheusRule.enabled=true"))
		if groups := ruleGroups(t, docs); len(groups["iterion.datastores"]) != 0 {
			t.Errorf("the datastore pack rendered without datastores.enabled")
		}
		docs = manifests(t, mustRender(t, append(base, "--set", "metrics.prometheusRule.datastores.valkey.enabled=false")...))
		off := ruleGroups(t, docs)["iterion.datastores"]
		if rule(off, "IterionValkeyAOFWriteFailing", nil) != nil || rule(off, "IterionDatastoreSignalAbsent", map[string]string{"family": "valkey"}) != nil {
			t.Errorf("valkey rules rendered with the family off")
		}
		if rule(off, "IterionSeaweedFSVolumesMissing", nil) == nil {
			t.Errorf("the other families must stay when one is off")
		}
	})

	t.Run("a regex idiom in a match is quoted once, never a rule-file break", func(t *testing.T) {
		// A backslash is an ordinary regex idiom on names; both rules must
		// render it as PromQL does — escaped once inside the quoted string —
		// or Prometheus drops the WHOLE PrometheusRule on the first bad
		// expression.
		docs := manifests(t, mustRender(t, append(base, "--set-json", `metrics.prometheusRule.datastores.statefulsets=[{"name":"mongodb","match":"iterion-mongodb\\d*"}]`)...))
		rules := ruleGroups(t, docs)["iterion.datastores"]
		replica := fmt.Sprint(rule(rules, "IterionDatastoreReplicasMissing", map[string]string{"datastore": "mongodb"})["expr"])
		restart := fmt.Sprint(rule(rules, "IterionDatastoreRestarting", map[string]string{"datastore": "mongodb"})["expr"])
		if !strings.Contains(replica, `statefulset=~"(?:iterion-mongodb\\d*)"`) {
			t.Errorf("replica rule expr = %q", replica)
		}
		if !strings.Contains(restart, `pod=~"(?:iterion-mongodb\\d*)-[0-9]+"`) {
			t.Errorf("restart rule expr = %q", restart)
		}
		mustRefuse(t, "statefulsets[mongodb].match must be a one-line regex", append(base, "--set-json", `metrics.prometheusRule.datastores.statefulsets=[{"name":"mongodb","match":""}]`)...)
		mustRefuse(t, "datastores.valkey.selector must be a PromQL label matcher list", append(base, "--set-string", "metrics.prometheusRule.datastores.valkey.selector=valkeyfiler")...)
	})

	t.Run("the mongodb prefix names the exporter's collector", func(t *testing.T) {
		docs := manifests(t, mustRender(t, append(base, "--set", "metrics.prometheusRule.datastores.mongodb.metricPrefix=mongodb_")...))
		rules := ruleGroups(t, docs)["iterion.datastores"]
		for _, alert := range []string{"IterionMongoDBMemberUnhealthy", "IterionMongoDBMemberOutOfSet", "IterionMongoDBNoPrimary"} {
			r := rule(rules, alert, nil)
			if r == nil || strings.Contains(fmt.Sprint(r["expr"]), "mongodb_rs_") || !strings.Contains(fmt.Sprint(r["expr"]), "mongodb_members_") {
				t.Errorf("%s with the replicasetstatus prefix = %v", alert, r)
			}
		}
		mustRefuse(t, "metricPrefix must be a metric-name prefix", append(base, "--set-string", "metrics.prometheusRule.datastores.mongodb.metricPrefix=mongo db_")...)
	})

	t.Run("the namespace and the selectors are the operator's", func(t *testing.T) {
		docs := manifests(t, mustRender(t, append(base, "--set", "metrics.prometheusRule.datastores.namespace=data", "--set-string", `metrics.prometheusRule.datastores.valkey.selector=job="valkey-metrics"`)...))
		custom := ruleGroups(t, docs)["iterion.datastores"]
		r := rule(custom, "IterionValkeyReplicasDisconnected", nil)
		if r == nil || !strings.Contains(fmt.Sprint(r["expr"]), `{namespace="data", job="valkey-metrics"}`) {
			t.Errorf("valkey expr = %v", r)
		}
		if r := rule(custom, "IterionDatastoreReplicasMissing", map[string]string{"datastore": "nats"}); r == nil || !strings.Contains(fmt.Sprint(r["expr"]), `namespace="data"`) {
			t.Errorf("the replica rules must follow the namespace, got %v", r)
		}
	})

	judgeClean(t, mustRender(t, base...), 6)
}

func TestChartRenderBackup(t *testing.T) {
	const digest = "sha256:d5971950c2b370fb04dd3292541b5bda6d9103143fd7e345aeb435a399388afc"
	base := []string{"-f", "values-prod.yaml", "-f", "testdata/values-backup.yaml"}

	t.Run("absent when disabled", func(t *testing.T) {
		for _, values := range []string{"values-dev.yaml", "values-prod.yaml"} {
			for _, d := range manifests(t, mustRender(t, "-f", values)) {
				if d["kind"] == "CronJob" {
					t.Errorf("%s renders a CronJob with backup.enabled=false", values)
				}
			}
		}
	})

	t.Run("one pinned CronJob per tier, credentials by Secret only", func(t *testing.T) {
		out := mustRender(t, base...)
		docs := manifests(t, out)
		tiers := map[string]string{"daily": "sync", "weekly": "snapshot", "monthly": "snapshot"}
		for tier, mode := range tiers {
			cj := must(t, docs, "CronJob", "iterion-backup-"+tier)
			container := dig(cj, "spec", "jobTemplate", "spec", "template", "spec", "containers", "0")
			if got := dig(container, "image"); got != "rclone/rclone@"+digest {
				t.Errorf("%s image = %v, want the digest-pinned rclone", tier, got)
			}
			env := envMap(container)
			if v := dig(env["MODE"], "value"); v != mode {
				t.Errorf("%s MODE = %v, want %s", tier, v, mode)
			}
			for _, name := range []string{"RCLONE_CONFIG_SRC_ACCESS_KEY_ID", "RCLONE_CONFIG_SRC_SECRET_ACCESS_KEY", "RCLONE_CONFIG_DST_ACCESS_KEY_ID", "RCLONE_CONFIG_DST_SECRET_ACCESS_KEY"} {
				e := env[name]
				if e == nil || dig(e, "valueFrom", "secretKeyRef", "name") == nil || dig(e, "value") != nil {
					t.Errorf("%s: %s must come from a Secret, got %v", tier, name, e)
				}
			}
			if got := dig(env["RCLONE_CONFIG_DST_ACCESS_KEY_ID"], "valueFrom", "secretKeyRef", "name"); got != "iterion-backup-destination" {
				t.Errorf("%s: destination secret = %v", tier, got)
			}
			if got := dig(env["RCLONE_CONFIG_SRC_ACCESS_KEY_ID"], "valueFrom", "secretKeyRef", "name"); got != "iterion-storage" {
				t.Errorf("%s: source secret = %v, want the deployment's storage secret", tier, got)
			}
			if got := dig(cj, "spec", "concurrencyPolicy"); got != "Forbid" {
				t.Errorf("%s: concurrencyPolicy = %v", tier, got)
			}
			// The check follows EACH copy: the sync branch checks the mirror,
			// the snapshot branch checks the dated copy. A script that only
			// mentions `rclone check` somewhere proves neither. Leading
			// whitespace is dropped line by line: the block scalar re-indents.
			script := regexp.MustCompile(`(?m)^[ \t]+`).ReplaceAllString(fmt.Sprint(dig(container, "args", "0")), "")
			checks := map[string]string{
				"sync":     `rclone sync "$src" "$root" --backup-dir "dst:${DST_BUCKET}/${PREFIX}-deleted/${ts}" --max-delete "$MAX_DELETE" $EXTRA_ARGS` + "\n" + `rclone check "$src" "$root" --min-age "$start" $CHECK_ARGS`,
				"snapshot": `rclone copy "$src" "${root}/${ts}" $EXTRA_ARGS` + "\n" + `rclone check "$src" "${root}/${ts}" --min-age "$start" $CHECK_ARGS`,
			}
			for branch, want := range checks {
				if !strings.Contains(script, want) {
					t.Errorf("%s: the %s branch must check what existed when the copy started, script lacks:\n%s", tier, branch, want)
				}
			}
			// Retention: the listing is read before the loop (a failed listing
			// fails the Job) and only a timestamp-shaped directory is purged.
			if !strings.Contains(script, `dirs=$(rclone lsf --dirs-only "$aged")`) || !strings.Contains(script, `[0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9]T[0-9][0-9][0-9][0-9][0-9][0-9]Z)`) {
				t.Errorf("%s: the expiry must list first and purge timestamp names only, got:\n%s", tier, script)
			}
			if v := dig(env["MAX_DELETE"], "value"); v != "1000" {
				t.Errorf("%s MAX_DELETE = %v, want the 1000 default", tier, v)
			}
			// One directory per release in the destination bucket: two
			// deployments sharing a bucket must never mirror over each other.
			if v := dig(env["DST_BUCKET"], "value"); v != "iterion-artifacts-backup/iterion/iterion" {
				t.Errorf("%s DST_BUCKET = %v, want <bucket>/<namespace>/<fullname>", tier, v)
			}
			if got := dig(cj, "spec", "jobTemplate", "spec", "template", "spec", "automountServiceAccountToken"); got != false {
				t.Errorf("%s: rclone must not mount the ServiceAccount token, got %v", tier, got)
			}
			if got := dig(cj, "metadata", "labels", "iterion.io/backup-tier"); got != tier {
				t.Errorf("%s: tier label = %v", tier, got)
			}
		}
		if strings.Contains(out, "secretAccessKey:") || strings.Contains(out, "iterion-dev-secret") {
			t.Errorf("the render carries a credential-looking literal")
		}
		judgeClean(t, out, 6)
	})

	t.Run("the alert rules follow the tiers", func(t *testing.T) {
		docs := manifests(t, mustRender(t, append(base, "--set", "metrics.prometheusRule.enabled=true")...))
		rules := ruleGroups(t, docs)["iterion.backup"]
		for _, tier := range []string{"daily", "weekly", "monthly"} {
			// The LAST run of the tier failed, read from the CronJob's own
			// status so it resolves on the next green run — not the failed-pod
			// counter (1 forever after a retried-then-green Job), not a retained
			// failed Job (pages after the copy has recovered).
			failed := rule(rules, "IterionArtifactBackupFailed", map[string]string{"tier": tier})
			// A tier that never ran green has no last_successful series: the
			// `unless` branch pages its failed first run (the base rule on
			// kube_job_status_failed did; a `>` alone matches nothing).
			m := fmt.Sprintf(`{namespace="iterion", cronjob="iterion-backup-%s"}`, tier)
			want := fmt.Sprintf(`((kube_cronjob_status_last_schedule_time%s > kube_cronjob_status_last_successful_time%s) or on(namespace, cronjob) (kube_cronjob_status_last_schedule_time%s unless on(namespace, cronjob) kube_cronjob_status_last_successful_time%s)) and on(namespace, cronjob) (kube_cronjob_status_active%s == 0)`, m, m, m, m, m)
			if failed == nil || fmt.Sprint(failed["expr"]) != want {
				t.Errorf("failed rule for %s = %v, want expr %s", tier, failed, want)
			}
			if r := rule(rules, "IterionArtifactBackupStale", map[string]string{"tier": tier}); r == nil || !strings.Contains(fmt.Sprint(r["expr"]), `cronjob="iterion-backup-`+tier+`"`) {
				t.Errorf("stale rule for %s = %v", tier, r)
			}
			// Time-based: a `for:` of maxAgeHours restarts from zero on every
			// Prometheus restart, so an 800 h one would never fire.
			if r := rule(rules, "IterionArtifactBackupNeverSucceeded", map[string]string{"tier": tier}); r == nil || !strings.HasPrefix(fmt.Sprint(r["expr"]), "absent(") || !strings.Contains(fmt.Sprint(r["expr"]), `and on() (time() - kube_cronjob_created{namespace="iterion", cronjob="iterion-backup-`+tier+`"} > `) || fmt.Sprint(r["for"]) != "15m" {
				t.Errorf("never-succeeded rule for %s = %v", tier, r)
			}
		}
		docs = manifests(t, mustRender(t, "-f", "values-prod.yaml", "--set", "metrics.prometheusRule.enabled=true"))
		if len(ruleGroups(t, docs)["iterion.backup"]) != 0 {
			t.Errorf("backup rules rendered with backup disabled")
		}
	})

	t.Run("the guards refuse an unpinned image, a missing destination, missing source credentials and a bad tier", func(t *testing.T) {
		mustRefuse(t, "backup.destination.existingSecret is required", append(base, "--set", "backup.destination.existingSecret=")...)
		mustRefuse(t, "backup.destination.bucket is required", append(base, "--set", "backup.destination.bucket=")...)
		mustRefuse(t, "backup.destination.endpoint is required", append(base, "--set", "backup.destination.endpoint=")...)
		mustRefuse(t, "backup.image.digest must be", append(base, "--set", "backup.image.digest=1.71.1")...)
		mustRefuse(t, "backup.image.digest must be", append(base, "--set", "backup.image.digest=")...)
		mustRefuse(t, "needs the source credentials", append(base, "--set", "secrets.storage.existingSecret=", "--set", "secrets.storage.create=false")...)
		tier := func(mode string, keepDays, maxAge int) string {
			return fmt.Sprintf(`[{"name":"daily","schedule":"0 2 * * *","mode":%q,"prefix":"current","keepDays":%d,"maxAgeHours":%d}]`, mode, keepDays, maxAge)
		}
		mustRefuse(t, "mode must be sync or snapshot", append(base, "--set-json", "backup.tiers="+tier("mirror", 7, 36))...)
		mustRefuse(t, "keepDays must be at least 1", append(base, "--set-json", "backup.tiers="+tier("sync", 0, 36))...)
		mustRefuse(t, "maxAgeHours must be at least 1", append(base, "--set-json", "backup.tiers="+tier("sync", 7, 0))...)
		mustRefuse(t, "backup.tiers must list at least one tier", append(base, "--set-json", "backup.tiers=[]")...)
		two := func(a, b string) string {
			return fmt.Sprintf(`[{"name":"%s","schedule":"0 2 * * *","mode":"sync","prefix":"%s","keepDays":7,"maxAgeHours":36},{"name":"%s","schedule":"0 3 * * *","mode":"snapshot","prefix":"%s","keepDays":35,"maxAgeHours":192}]`, a, a, b, b)
		}
		// Two tiers on one prefix expire each other's copies: refused at render.
		mustRefuse(t, "is used by two tiers", append(base, "--set-json", "backup.tiers="+strings.Replace(two("daily", "weekly"), `"prefix":"weekly"`, `"prefix":"daily"`, 1))...)
		mustRefuse(t, "tier name \"daily\" is used twice", append(base, "--set-json", "backup.tiers="+strings.Replace(two("daily", "weekly"), `"name":"weekly"`, `"name":"daily"`, 1))...)
		mustRefuse(t, "is the deletion twin of prefix", append(base, "--set-json", "backup.tiers="+strings.Replace(two("daily", "weekly"), `"prefix":"weekly"`, `"prefix":"daily-deleted"`, 1))...)
		mustRefuse(t, "must name a directory of the destination bucket", append(base, "--set-json", "backup.tiers="+strings.Replace(two("daily", "weekly"), `"prefix":"weekly"`, `"prefix":".."`, 1))...)
		// rclone prints the credentials it read from the environment at debug
		// level: a verbosity flag is refused before it reaches a Job's logs.
		for _, flag := range []string{"-vv", "-v", "--verbose", "--log-level=DEBUG", "--dump=headers", "--log-file=/tmp/x"} {
			mustRefuse(t, "rclone prints the credentials", append(base, "--set-json", fmt.Sprintf(`backup.extraArgs=[%q]`, flag))...)
		}
		mustRefuse(t, "rclone prints the credentials", append(base, "--set-json", `backup.checkArgs=["--one-way","-vv"]`)...)
		// A numeric tier name would render an unquoted label the API refuses,
		// a fractional maxAgeHours would alert on its truncation.
		// A numeric tier name is a label VALUE: quoted, or the API server
		// refuses the unquoted integer the template would otherwise render.
		docs := manifests(t, mustRender(t, append(base, "--set-json", `backup.tiers=[{"name":"365","schedule":"0 2 * * *","mode":"sync","prefix":"current","keepDays":7,"maxAgeHours":36}]`)...))
		if got := dig(must(t, docs, "CronJob", "iterion-backup-365"), "metadata", "labels", "iterion.io/backup-tier"); got != "365" {
			t.Errorf("a numeric tier name must render a quoted label, got %v (%T)", got, got)
		}
		mustRefuse(t, "must be whole numbers", append(base, "--set-json", `backup.tiers=[{"name":"daily","schedule":"0 2 * * *","mode":"sync","prefix":"current","keepDays":7,"maxAgeHours":1.5}]`)...)
		mustRefuse(t, "backup", append(base, "--set-string", "backup.enabled=false")...)
	})

	t.Run("the destination path and the check flags are the operator's", func(t *testing.T) {
		docs := manifests(t, mustRender(t, append(base, "--set-string", "backup.destination.path=", "--set-json", `backup.checkArgs=[]`)...))
		container := dig(must(t, docs, "CronJob", "iterion-backup-daily"), "spec", "jobTemplate", "spec", "template", "spec", "containers", "0")
		env := envMap(container)
		if v := dig(env["DST_BUCKET"], "value"); v != "iterion-artifacts-backup" {
			t.Errorf("path \"\" must copy into the bucket root, got %v", v)
		}
		if v := dig(env["CHECK_ARGS"], "value"); v != "" {
			t.Errorf("checkArgs: [] must mean no flag, got %q", v)
		}
		docs = manifests(t, mustRender(t, append(base, "--set-string", "backup.destination.path=/team-a/")...))
		container = dig(must(t, docs, "CronJob", "iterion-backup-daily"), "spec", "jobTemplate", "spec", "template", "spec", "containers", "0")
		if v := dig(envMap(container)["DST_BUCKET"], "value"); v != "iterion-artifacts-backup/team-a" {
			t.Errorf("an explicit path is trimmed of its slashes, got %v", v)
		}
	})
}

func TestChartRenderDevStack(t *testing.T) {
	out := mustRender(t, "-f", "values-dev.yaml")
	docs := manifests(t, out)

	t.Run("no MinIO and no Bitnami left in the dev render", func(t *testing.T) {
		lower := strings.ToLower(out)
		for _, word := range []string{"minio", "bitnami"} {
			if strings.Contains(lower, word) {
				t.Errorf("the dev render still mentions %q", word)
			}
		}
	})

	t.Run("the dev stores render pinned, with the bucket hook", func(t *testing.T) {
		mongo := podSpec(t, docs, "Deployment", "iterion-mongodb")
		if img := fmt.Sprint(dig(mongo, "containers", "0", "image")); !strings.HasPrefix(img, "mongo:8.3") || !strings.Contains(img, "@sha256:") {
			t.Errorf("mongo image = %s, want a digest-pinned mongo 8.3", img)
		}
		// SERVER-121912: without glibc owning rseq, MongoDB 8.x exits at
		// start on Linux 6.19+ — the CI runners' kernel (cloud-e2e red on
		// main at 05c188cd5).
		if env := envMap(dig(mongo, "containers", "0")); env["GLIBC_TUNABLES"] == nil || fmt.Sprint(env["GLIBC_TUNABLES"]["value"]) != "glibc.pthread.rseq=1" {
			t.Errorf("mongo env GLIBC_TUNABLES = %v, want glibc.pthread.rseq=1 (SERVER-121912)", env["GLIBC_TUNABLES"])
		}
		if args := fmt.Sprint(dig(mongo, "containers", "0", "args")); !strings.Contains(args, "--replSet rs0") {
			t.Errorf("mongo args = %s, want a replica set", args)
		}
		svc := must(t, docs, "Service", "iterion-mongodb")
		if dig(svc, "spec", "publishNotReadyAddresses") != true {
			t.Errorf("the dev Mongo Service must publish the not-ready pod: rs.initiate needs the member host to route to the pod before it is Ready")
		}
		sw := podSpec(t, docs, "Deployment", "iterion-seaweedfs")
		if img := fmt.Sprint(dig(sw, "containers", "0", "image")); !strings.HasPrefix(img, "chrislusf/seaweedfs:4.48@sha256:") {
			t.Errorf("seaweedfs image = %s, want the digest-pinned 4.48", img)
		}
		if args := fmt.Sprint(dig(sw, "containers", "0", "args")); !strings.Contains(args, "-s3.autoCreateBucket=false") || !strings.Contains(args, "-master.telemetry=false") {
			t.Errorf("seaweedfs args = %s", args)
		}
		// The bucket belongs to the pod's own lifecycle: a pod restarted on an
		// emptyDir gets it back, where a Helm hook would have left it bucketless.
		postStart := fmt.Sprint(dig(sw, "containers", "0", "lifecycle", "postStart", "exec", "command", "2"))
		if !strings.Contains(postStart, "s3.bucket.create -name %s") || !strings.Contains(postStart, "s3.bucket.list") {
			t.Errorf("the gateway's postStart must create the bucket and key on the listing, got:\n%s", postStart)
		}
		if got := dig(envMap(dig(sw, "containers", "0"))["BUCKET"], "value"); got != "iterion-artifacts" {
			t.Errorf("BUCKET env = %v", got)
		}
		if find(docs, "Job", "iterion-seaweedfs-init") != nil {
			t.Errorf("no hook Job: the bucket is the pod's own")
		}
		for _, name := range []string{"iterion-mongodb", "iterion-seaweedfs"} {
			if got := dig(podSpec(t, docs, "Deployment", name), "automountServiceAccountToken"); got != false {
				t.Errorf("%s must not mount the namespace's ServiceAccount token, got %v", name, got)
			}
		}
		// The replica-set member host is the Service, the URI's host and the
		// member agree (a `localhost` member would initiate fine and strand
		// every client).
		probe := fmt.Sprint(dig(mongo, "containers", "0", "readinessProbe", "exec", "command", "3"))
		if !strings.Contains(probe, `host: "iterion-mongodb:27017"`) {
			t.Errorf("the member host must be the Service, got probe %s", probe)
		}
	})

	t.Run("the identity file is the chart's dev key pair and nothing anonymous", func(t *testing.T) {
		secret := must(t, docs, "Secret", "iterion-seaweedfs-s3")
		var identity struct {
			Identities []struct {
				Name        string `json:"name"`
				Credentials []struct {
					AccessKey string `json:"accessKey"`
					SecretKey string `json:"secretKey"`
				} `json:"credentials"`
				Actions []string `json:"actions"`
			} `json:"identities"`
		}
		if err := json.Unmarshal([]byte(fmt.Sprint(dig(secret, "stringData", "s3.json"))), &identity); err != nil {
			t.Fatalf("s3.json: %v", err)
		}
		if len(identity.Identities) != 1 || len(identity.Identities[0].Credentials) != 1 {
			t.Fatalf("want one identity with one credential, got %+v", identity)
		}
		if c := identity.Identities[0].Credentials[0]; c.AccessKey != "iterion-dev" || c.SecretKey != "iterion-dev-secret" {
			t.Errorf("the identity must be secrets.storage's pair, got %+v", c)
		}
		if identity.Identities[0].Name == "anonymous" {
			t.Errorf("an anonymous identity serves unsigned requests")
		}
		storage := must(t, docs, "Secret", "iterion-storage")
		if got := dig(storage, "stringData", "ITERION_S3_ACCESS_KEY_ID"); got != "iterion-dev" {
			t.Errorf("the server's key = %v, want the same pair as the gateway", got)
		}
	})

	t.Run("the config resolves the dev stores whatever the release name", func(t *testing.T) {
		cm := must(t, docs, "ConfigMap", "iterion-config")
		if got := dig(cm, "data", "ITERION_MONGO_URI"); got != "mongodb://iterion-mongodb:27017/?replicaSet=rs0" {
			t.Errorf("ITERION_MONGO_URI = %v", got)
		}
		if got := dig(cm, "data", "ITERION_S3_ENDPOINT"); got != "http://iterion-seaweedfs:8333" {
			t.Errorf("ITERION_S3_ENDPOINT = %v", got)
		}
		ci, err := renderAs(t, "ci", "-f", "values-dev.yaml")
		if err != nil {
			t.Fatal(err)
		}
		ciDocs := manifests(t, ci)
		cm = must(t, ciDocs, "ConfigMap", "ci-iterion-config")
		if got := dig(cm, "data", "ITERION_MONGO_URI"); got != "mongodb://ci-iterion-mongodb:27017/?replicaSet=rs0" {
			t.Errorf("release ci: ITERION_MONGO_URI = %v", got)
		}
		if got := dig(cm, "data", "ITERION_S3_ENDPOINT"); got != "http://ci-iterion-seaweedfs:8333" {
			t.Errorf("release ci: ITERION_S3_ENDPOINT = %v", got)
		}
		must(t, ciDocs, "Service", "ci-iterion-mongodb")
		must(t, ciDocs, "Service", "ci-iterion-seaweedfs")
		if probe := fmt.Sprint(dig(podSpec(t, ciDocs, "Deployment", "ci-iterion-mongodb"), "containers", "0", "readinessProbe", "exec", "command", "3")); !strings.Contains(probe, `host: "ci-iterion-mongodb:27017"`) {
			t.Errorf("release ci: the member host must follow the Service, got %s", probe)
		}
		// A release name Helm allows (53 chars) must not render names the API
		// refuses (> 63): the dev stores truncate like every chart name does.
		long, err := renderAs(t, strings.Repeat("r", 53), "-f", "values-dev.yaml")
		if err != nil {
			t.Fatal(err)
		}
		// Scoped to the dev stores: the chart's older objects (secrets,
		// configmap, runner, test pod) carry the same class and are not this
		// change's — noted as debt.
		for _, d := range manifests(t, long) {
			name := fmt.Sprint(dig(d, "metadata", "name"))
			if (strings.Contains(name, "-mongodb") || strings.Contains(name, "-seaweedfs")) && len(name) > 63 {
				t.Errorf("%s %s: %d characters", d["kind"], name, len(name))
			}
		}
		if _, err := render(t, "-f", "values-dev.yaml", "--set-json", "devStack=null"); err != nil {
			t.Errorf("devStack: null must render (the stores off), got %v", err)
		}
	})

	t.Run("the guards", func(t *testing.T) {
		mustRefuse(t, "needs secrets.storage.create=true", "-f", "values-dev.yaml", "--set", "secrets.storage.create=false")
		mustRefuse(t, "no secrets.storage.existingSecret", "-f", "values-dev.yaml", "--set", "secrets.storage.existingSecret=my-s3")
		mustRefuse(t, "must be pinned by digest", "-f", "values-dev.yaml", "--set", "devStack.seaweedfs.image=chrislusf/seaweedfs:4.48")
		mustRefuse(t, "must be pinned by digest", "-f", "values-dev.yaml", "--set", "devStack.mongodb.image=mongo:8.3")
		mustRefuse(t, "minio", "-f", "values-dev.yaml", "--set", "minio.enabled=true")
		mustRefuse(t, "mongodb", "-f", "values-dev.yaml", "--set", "mongodb.enabled=true")
		mustRefuse(t, "mongodb", "-f", "values-dev.yaml", "--set", "mongodb.enabled=false", "--set", "mongodb.auth.enabled=false")
	})

	t.Run("a values file that only turned the retired sub-charts off keeps rendering", func(t *testing.T) {
		// The production values before this change — and every GitOps copy of
		// them — carry `mongodb.enabled: false` / `minio.enabled: false`.
		docs := manifests(t, mustRender(t, "-f", "values-prod.yaml", "--set", "mongodb.enabled=false", "--set", "minio.enabled=false"))
		must(t, docs, "Deployment", "iterion")
		for _, name := range []string{"iterion-mongodb", "iterion-seaweedfs"} {
			if find(docs, "Deployment", name) != nil {
				t.Errorf("%s rendered from a retired key", name)
			}
		}
	})

	t.Run("the NetworkPolicy keeps the dev stores out, and a second one admits the app pods to them", func(t *testing.T) {
		docs := manifests(t, mustRender(t, "-f", "values-dev.yaml", "--set", "networkPolicy.enabled=true"))
		// The labels come from the rendered pod templates, never typed by
		// hand: a renamed component moves with them.
		podLabels := func(name string) map[string]string {
			return stringMap(dig(must(t, docs, "Deployment", name), "spec", "template", "metadata", "labels"))
		}
		stores := map[string]map[string]string{"mongodb": podLabels("iterion-mongodb"), "seaweedfs": podLabels("iterion-seaweedfs")}
		apps := map[string]map[string]string{"server": podLabels("iterion"), "runner": podLabels("iterion-runner")}
		matches := func(selector any, labels map[string]string) bool {
			matched, err := rendergate.SelectorMatches(selector, labels)
			if err != nil {
				t.Fatal(err)
			}
			return matched
		}
		appSel := dig(must(t, docs, "NetworkPolicy", "iterion"), "spec", "podSelector")
		guard := must(t, docs, "NetworkPolicy", "iterion-dev-stores")
		storeSel := dig(guard, "spec", "podSelector")
		ingress, _ := dig(guard, "spec", "ingress").([]any)
		if len(ingress) != 1 || fmt.Sprint(dig(guard, "spec", "policyTypes")) != "[Ingress]" {
			t.Fatalf("the stores policy is one ingress rule, egress open, got %v", guard["spec"])
		}
		from, _ := dig(ingress[0], "from").([]any)
		if len(from) != 1 {
			t.Fatalf("the stores policy admits one peer selector, got %v", from)
		}
		fromSel := dig(from[0], "podSelector")
		for name, labels := range stores {
			if matches(appSel, labels) {
				t.Errorf("the app policy selects the %s store: its ports would be closed to the app pods", name)
			}
			if !matches(storeSel, labels) {
				t.Errorf("the stores policy must select the %s store", name)
			}
			// The stores dial themselves: Mongo initiates its replica set
			// against its own Service (a hairpin with its own IP as source).
			if !matches(fromSel, labels) {
				t.Errorf("the stores policy must admit the %s store itself (hairpin)", name)
			}
		}
		for name, labels := range apps {
			if !matches(appSel, labels) {
				t.Errorf("the app policy must select the %s pods", name)
			}
			if matches(storeSel, labels) {
				t.Errorf("the stores policy selects the %s pods", name)
			}
			if !matches(fromSel, labels) {
				t.Errorf("the stores policy must admit the %s pods", name)
			}
		}
		ports := map[string]bool{}
		if list, _ := dig(ingress[0], "ports").([]any); true {
			for _, p := range list {
				ports[fmt.Sprint(dig(p, "port"))] = true
			}
		}
		if len(ports) != 2 || !ports["27017"] || !ports["8333"] {
			t.Errorf("the stores policy admits 27017 and 8333 only, got %v", ports)
		}
		// The app pods reach the release's own stores without an operator
		// rule: an egress to the dev stores on their ports, one to the nats
		// sub-chart's client port.
		egressTo := func(policy map[string]any, labels map[string]string) map[string]bool {
			ports := map[string]bool{}
			rules, _ := dig(policy, "spec", "egress").([]any)
			for _, r := range rules {
				to, _ := dig(r, "to").([]any)
				for _, peer := range to {
					if sel := dig(peer, "podSelector"); sel != nil && matches(sel, labels) {
						list, _ := dig(r, "ports").([]any)
						for _, p := range list {
							ports[fmt.Sprint(dig(p, "port"))] = true
						}
					}
				}
			}
			return ports
		}
		app := must(t, docs, "NetworkPolicy", "iterion")
		if ports := egressTo(app, stores["mongodb"]); !ports["27017"] {
			t.Errorf("the app policy must let the app pods dial the dev Mongo on 27017, got %v", ports)
		}
		if ports := egressTo(app, stores["seaweedfs"]); !ports["8333"] {
			t.Errorf("the app policy must let the app pods dial the dev SeaweedFS on 8333, got %v", ports)
		}
		nats := map[string]string{"app.kubernetes.io/name": "nats", "app.kubernetes.io/instance": "iterion", "app.kubernetes.io/component": "nats"}
		if ports := egressTo(app, nats); !ports["4222"] {
			t.Errorf("the app policy must let the app pods dial the nats sub-chart on 4222, got %v", ports)
		}
		prod := manifests(t, mustRender(t, "-f", "values-prod.yaml", "--set", "networkPolicy.enabled=true"))
		if find(prod, "NetworkPolicy", "iterion-dev-stores") != nil {
			t.Errorf("production renders a dev-stores policy")
		}
		for name, labels := range map[string]map[string]string{"mongodb": stores["mongodb"], "seaweedfs": stores["seaweedfs"], "nats": nats} {
			if ports := egressTo(must(t, prod, "NetworkPolicy", "iterion"), labels); len(ports) != 0 {
				t.Errorf("production has no %s of its own, yet its policy opens egress to one: %v", name, ports)
			}
		}
	})

	t.Run("a long fullnameOverride keeps the stores' names apart from the server's", func(t *testing.T) {
		// Cut after the suffix, three Deployments and three Services would
		// share one name and the Mongo URI would dial the server.
		over := strings.Repeat("f", 63)
		stream := mustRender(t, "-f", "values-dev.yaml", "--set", "fullnameOverride="+over)
		docs := manifests(t, stream)
		mongo, seaweed := strings.Repeat("f", 55)+"-mongodb", strings.Repeat("f", 53)+"-seaweedfs"
		for _, kind := range []string{"Deployment", "Service"} {
			for _, name := range []string{over, mongo, seaweed} {
				if find(docs, kind, name) == nil {
					t.Errorf("no %s %s", kind, name)
				}
			}
		}
		if !strings.Contains(stream, "mongodb://"+mongo+":27017") {
			t.Errorf("the Mongo URI must dial the store's own Service")
		}
	})

	t.Run("production renders no dev store", func(t *testing.T) {
		prod := manifests(t, mustRender(t, "-f", "values-prod.yaml"))
		for _, name := range []string{"iterion-mongodb", "iterion-seaweedfs"} {
			if find(prod, "Deployment", name) != nil {
				t.Errorf("%s rendered with values-prod", name)
			}
		}
		// The helpers resolve the dev stores ONLY when they are on: an AWS S3
		// deployment leaves config.s3.endpoint empty on purpose, and the Mongo
		// URI of production rides the auth Secret.
		cm := must(t, prod, "ConfigMap", "iterion-config")
		for _, key := range []string{"ITERION_MONGO_URI", "ITERION_S3_ENDPOINT"} {
			if got := dig(cm, "data", key); got != "" {
				t.Errorf("values-prod renders %s = %v, want empty", key, got)
			}
		}
	})

	judgeClean(t, out, 4)
}

// #2804: the server port reads X-Forwarded-For (#2673), so an ingress rule
// that admits every pod of the cluster lets any in-cluster workload forge
// the audit IP and rotate the rate-limit key. The rule is scoped to the
// ingress controller's namespaces — the chart ships ingress-nginx as the
// default — plus the release's own pods, with
// networkPolicy.ingress.extraAllow as the operator's escape hatch; an
// explicitly emptied configuration keeps the historical admit-all and warns
// loudly (annotation + render comment + install note), never refuses.
func TestChartRenderNetworkPolicyServerIngress(t *testing.T) {
	// serverRule returns the ingress rule that carries the server port.
	serverRule := func(t *testing.T, docs []map[string]any) map[string]any {
		t.Helper()
		rules, _ := dig(must(t, docs, "NetworkPolicy", "iterion"), "spec", "ingress").([]any)
		for _, r := range rules {
			rm, _ := r.(map[string]any)
			ports, _ := dig(rm, "ports").([]any)
			for _, p := range ports {
				if fmt.Sprint(dig(p, "port")) == "4891" {
					return rm
				}
			}
		}
		t.Fatalf("no ingress rule carries the server port")
		return nil
	}
	fromPeers := func(rule map[string]any) []any {
		peers, _ := dig(rule, "from").([]any)
		return peers
	}

	t.Run("configured controller namespaces scope the server port", func(t *testing.T) {
		docs := manifests(t, mustRender(t, "-f", "values-dev.yaml", "--set", "networkPolicy.enabled=true",
			"--set", "networkPolicy.ingress.controllerNamespaces[0]=ingress-nginx"))
		rule := serverRule(t, docs)
		peers := fromPeers(rule)
		if len(peers) == 0 {
			t.Fatalf("the server port rule carries no from: selector — every in-cluster pod can forge X-Forwarded-For (#2804)")
		}
		var namespacePeer, releasePeer bool
		for _, peer := range peers {
			if got := dig(peer, "namespaceSelector", "matchLabels", "kubernetes.io/metadata.name"); got == "ingress-nginx" {
				namespacePeer = true
			}
			if sel := dig(peer, "podSelector"); sel != nil {
				// The release's own pods, read from the rendered pod
				// templates, never typed by hand.
				for _, name := range []string{"iterion", "iterion-runner"} {
					labels := stringMap(dig(must(t, docs, "Deployment", name), "spec", "template", "metadata", "labels"))
					matched, err := rendergate.SelectorMatches(sel, labels)
					if err != nil {
						t.Fatal(err)
					}
					if !matched {
						t.Errorf("the release-pods peer must admit %s's pods", name)
					}
				}
				foreign := map[string]string{"app.kubernetes.io/name": "other", "app.kubernetes.io/instance": "other"}
				if matched, err := rendergate.SelectorMatches(sel, foreign); err != nil {
					t.Fatal(err)
				} else if matched {
					t.Errorf("the release-pods peer admits foreign pods")
				}
				releasePeer = true
			}
		}
		if !namespacePeer {
			t.Errorf("no namespaceSelector peer on kubernetes.io/metadata.name=ingress-nginx, got %v", peers)
		}
		if !releasePeer {
			t.Errorf("no podSelector peer for the release's own pods, got %v", peers)
		}
	})

	t.Run("extraAllow is the escape hatch", func(t *testing.T) {
		docs := manifests(t, mustRender(t, "-f", "values-dev.yaml", "--set", "networkPolicy.enabled=true",
			"--set-json", `networkPolicy.ingress.extraAllow=[{"ipBlock":{"cidr":"10.0.0.0/8"}}]`))
		peers := fromPeers(serverRule(t, docs))
		var ipBlock, releasePeer bool
		for _, peer := range peers {
			if dig(peer, "ipBlock", "cidr") == "10.0.0.0/8" {
				ipBlock = true
			}
			if dig(peer, "podSelector") != nil {
				releasePeer = true
			}
		}
		if !ipBlock {
			t.Errorf("the operator's extraAllow ipBlock is not among the peers, got %v", peers)
		}
		if !releasePeer {
			t.Errorf("extraAllow alone must still admit the release's own pods, got %v", peers)
		}
	})

	t.Run("the chart's default scopes the server port to the common ingress-nginx namespace", func(t *testing.T) {
		// Decided (#2804, question 51, verdict A): the chart SHIPS a default
		// controllerNamespaces — ingress-nginx, the common case — so a
		// networkPolicy.enabled=true with nothing said is scoped, not
		// admit-all.
		docs := manifests(t, mustRender(t, "-f", "values-dev.yaml", "--set", "networkPolicy.enabled=true"))
		peers := fromPeers(serverRule(t, docs))
		var namespacePeer bool
		for _, peer := range peers {
			if got := dig(peer, "namespaceSelector", "matchLabels", "kubernetes.io/metadata.name"); got == "ingress-nginx" {
				namespacePeer = true
			}
		}
		if !namespacePeer {
			t.Errorf("the default controllerNamespaces must scope the server port to ingress-nginx, got %v", peers)
		}
		if got := dig(must(t, docs, "NetworkPolicy", "iterion"), "metadata", "annotations", "iterion.io/ingress-admit-all"); got != nil {
			t.Errorf("a configured render must not carry the empty-config warning annotation, got %v", got)
		}
	})

	t.Run("unconfigured warns loudly and keeps the historical admit-all", func(t *testing.T) {
		// Decided (#2804, question 51, verdict A): the empty-config verdict
		// is WARN LOUDLY, never refuse the render — an upgrade of an
		// unconfigured release must not break. With the chart's default in
		// place the empty case is an explicit operator choice (BOTH lists
		// emptied): the rule keeps the historical from:-less admit-all, and
		// the warning rides the policy as an annotation and the render as a
		// comment (NOTES.txt carries it for helm install/upgrade).
		values := valuesFile(t, "networkPolicy:\n  enabled: true\n  ingress:\n    controllerNamespaces: []\n    extraAllow: []\n")
		out := mustRender(t, "-f", "values-dev.yaml", "-f", values)
		docs := manifests(t, out)
		if from := dig(serverRule(t, docs), "from"); from != nil {
			t.Errorf("both lists emptied must render no from: (the historical admit-all, kept for upgrade compatibility), got %v", from)
		}
		if got := dig(must(t, docs, "NetworkPolicy", "iterion"), "metadata", "annotations", "iterion.io/ingress-admit-all"); got == nil {
			t.Errorf("the empty-config render must carry the loud warning annotation on the policy")
		}
		if !strings.Contains(out, "WARNING") || !strings.Contains(out, "X-Forwarded-For") {
			t.Errorf("the empty-config render must carry the loud warning comment for `helm template` readers")
		}
	})

	t.Run("the metrics port stays a rule of its own", func(t *testing.T) {
		docs := manifests(t, mustRender(t, "-f", "values-dev.yaml", "--set", "networkPolicy.enabled=true", "--set", "metrics.enabled=true",
			"--set", "networkPolicy.ingress.controllerNamespaces[0]=ingress-nginx"))
		for _, p := range dig(serverRule(t, docs), "ports").([]any) {
			if fmt.Sprint(dig(p, "port")) == "9090" {
				t.Errorf("the metrics port rides the scoped rule: scoping it with the server port would break Prometheus scraping on upgrade")
			}
		}
		rules, _ := dig(must(t, docs, "NetworkPolicy", "iterion"), "spec", "ingress").([]any)
		var metricsRule map[string]any
		for _, r := range rules {
			rm, _ := r.(map[string]any)
			for _, p := range dig(rm, "ports").([]any) {
				if fmt.Sprint(dig(p, "port")) == "9090" {
					metricsRule = rm
				}
			}
		}
		if metricsRule == nil {
			t.Fatalf("no ingress rule carries the metrics port")
		}
		if from := dig(metricsRule, "from"); from != nil {
			t.Errorf("the metrics rule keeps today's unscoped render, got from: %v", from)
		}
	})
}

// The server's progress deadline: a knob the cloud Deployment carries and
// the local StatefulSet must never grow. `helm install --wait` reads a
// Deployment past its deadline as Failed even when the pod turns Ready a
// minute later — the install failure measured on a fresh kind node, where
// the kubelet serialised the image pulls (iterion#2780) — and
// progressDeadlineSeconds is a Deployment field only: rendered on a
// StatefulSet, the API server rejects the whole manifest.
//
// The ticket's own witness, `helm install --wait` on a cold cluster, is a
// live-cluster run and no gate member: it is named in the wave report as a
// manual proof, never claimed from here.
func TestChartRenderServerProgressDeadline(t *testing.T) {
	serverDeadline := func(t *testing.T, kind string, args ...string) any {
		t.Helper()
		docs := manifests(t, mustRender(t, args...))
		return dig(must(t, docs, kind, "iterion"), "spec", "progressDeadlineSeconds")
	}

	t.Run("unset renders nothing", func(t *testing.T) {
		if got := serverDeadline(t, "Deployment"); got != nil {
			t.Fatalf("spec.progressDeadlineSeconds = %v with the value unset, want absent — an install keeps the 600 s it was deployed with", got)
		}
	})

	t.Run("cloud carries the value set", func(t *testing.T) {
		got := serverDeadline(t, "Deployment", "--set", "server.progressDeadlineSeconds=1800")
		if fmt.Sprint(got) != "1800" {
			t.Fatalf("spec.progressDeadlineSeconds = %v, want 1800", got)
		}
	})

	t.Run("local never carries it", func(t *testing.T) {
		got := serverDeadline(t, "StatefulSet", "--set", "mode=local", "--set", "server.progressDeadlineSeconds=1800")
		if got != nil {
			t.Fatalf("the local StatefulSet grew progressDeadlineSeconds = %v — the API server rejects that manifest", got)
		}
	})

	t.Run("the dev values raise it where the install broke", func(t *testing.T) {
		got := serverDeadline(t, "Deployment", "-f", "values-dev.yaml")
		if fmt.Sprint(got) != "1800" {
			t.Fatalf("values-dev renders spec.progressDeadlineSeconds = %v, want 1800 (the cold-kind path `task chart:kind` installs)", got)
		}
	})

	// A zero renders NOTHING: `with` skips it, so Kubernetes' default
	// applies. Both values comments say so now — the runner's used to
	// promise "0 disables the deadline entirely", which this knob cannot
	// express and which this case pins.
	t.Run("zero renders nothing rather than removing the deadline", func(t *testing.T) {
		got := serverDeadline(t, "Deployment", "--set", "server.progressDeadlineSeconds=0")
		if got != nil {
			t.Fatalf("spec.progressDeadlineSeconds = %v for an explicit 0, want absent — `with` skips a zero and the values comment must keep saying so", got)
		}
	})
}
