package rendergate_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/rendergate"
)

// valkeyha is the ovh-prod defect of #2012 in its exact shape: the valkey
// sub-chart installed under the alias valkeyha, its pods labelled with the
// alias, its anti-affinity still selecting the chart's bare name.
func valkeyha(selected string) string {
	return `
apiVersion: apps/v1
kind: StatefulSet
metadata:
  name: iterion-valkeyha
  namespace: iterion
spec:
  template:
    metadata:
      labels:
        app.kubernetes.io/name: valkeyha
        app.kubernetes.io/instance: iterion
    spec:
      affinity:
        podAntiAffinity:
          requiredDuringSchedulingIgnoredDuringExecution:
            - topologyKey: kubernetes.io/hostname
              labelSelector:
                matchExpressions:
                  - key: app.kubernetes.io/name
                    operator: In
                    values:
                      - ` + selected + `
`
}

// workload renders a Deployment whose pods carry the labels and whose pod
// spec is the given YAML fragment (indented under the template spec).
func workload(kind, name, namespace string, labels map[string]string, podSpec string) string {
	var b strings.Builder
	b.WriteString("apiVersion: apps/v1\nkind: " + kind + "\nmetadata:\n  name: " + name + "\n")
	if namespace != "" {
		b.WriteString("  namespace: " + namespace + "\n")
	}
	b.WriteString("spec:\n  template:\n    metadata:\n      labels:\n")
	for k, v := range labels {
		b.WriteString("        " + k + ": " + v + "\n")
	}
	b.WriteString("    spec:\n")
	for _, line := range strings.Split(strings.TrimRight(podSpec, "\n"), "\n") {
		if line != "" {
			b.WriteString("      " + line + "\n")
		}
	}
	return b.String()
}

func judge(t *testing.T, stream string) rendergate.Report {
	t.Helper()
	report, err := rendergate.Judge([]byte(stream))
	if err != nil {
		t.Fatalf("Judge: %v", err)
	}
	return report
}

func TestTheValkeyhaDefectIsNamed(t *testing.T) {
	report := judge(t, valkeyha("valkey"))
	dead := report.Dead()
	if len(dead) != 1 {
		t.Fatalf("want exactly one dead selector, got %d findings: %+v", len(dead), report.Findings)
	}
	f := dead[0]
	if f.Kind != "StatefulSet" || f.Name != "iterion-valkeyha" || f.Namespace != "iterion" {
		t.Errorf("the finding must name the workload, got %s %s in %q", f.Kind, f.Name, f.Namespace)
	}
	if len(f.DeadKeys) != 1 || f.DeadKeys[0] != "app.kubernetes.io/name" {
		t.Errorf("the finding must name the dead key, got %v", f.DeadKeys)
	}
	if f.Site != "affinity.podAntiAffinity.requiredDuringSchedulingIgnoredDuringExecution[0]" {
		t.Errorf("site = %q", f.Site)
	}
	if !strings.Contains(f.Detail, "app.kubernetes.io/name=valkeyha") {
		t.Errorf("the finding must say what the pods carry instead, got detail %q", f.Detail)
	}
	line := f.String()
	for _, want := range []string{"dead", "StatefulSet iterion-valkeyha", "app.kubernetes.io/name in (valkey)", "dead key(s) app.kubernetes.io/name", "valkeyha"} {
		if !strings.Contains(line, want) {
			t.Errorf("the printed line %q lacks %q", line, want)
		}
	}
	if report.Judged != 1 || report.PodTemplates != 1 {
		t.Errorf("judged=%d templates=%d, want 1 and 1", report.Judged, report.PodTemplates)
	}
}

func TestTheOwnSelectorIsAlive(t *testing.T) {
	report := judge(t, valkeyha("valkeyha"))
	if len(report.Findings) != 0 {
		t.Fatalf("a selector naming the pods' own label is alive, got %+v", report.Findings)
	}
	if report.Judged != 1 {
		t.Errorf("the alive selector must still count as judged, got %d", report.Judged)
	}
}

func TestSelectorSemantics(t *testing.T) {
	labels := map[string]string{"app": "web", "tier": "front"}
	spread := func(selector string) string {
		return workload("Deployment", "web", "", labels, "topologySpreadConstraints:\n  - topologyKey: kubernetes.io/hostname\n    maxSkew: 1\n    whenUnsatisfiable: ScheduleAnyway\n"+selector)
	}
	cases := []struct {
		name     string
		selector string
		deadKeys []string // nil = alive
	}{
		{"matchLabels equal", "    labelSelector:\n      matchLabels:\n        app: web\n        tier: front\n", nil},
		{"matchLabels wrong value", "    labelSelector:\n      matchLabels:\n        app: api\n", []string{"app"}},
		{"matchLabels absent key", "    labelSelector:\n      matchLabels:\n        app: web\n        zone: eu\n", []string{"zone"}},
		{"In holds", "    labelSelector:\n      matchExpressions:\n        - {key: app, operator: In, values: [api, web]}\n", nil},
		{"NotIn holds on another value", "    labelSelector:\n      matchExpressions:\n        - {key: app, operator: NotIn, values: [api]}\n", nil},
		{"NotIn holds on an absent key", "    labelSelector:\n      matchExpressions:\n        - {key: zone, operator: NotIn, values: [eu]}\n", nil},
		{"NotIn fails on the carried value", "    labelSelector:\n      matchExpressions:\n        - {key: app, operator: NotIn, values: [web]}\n", []string{"app"}},
		{"Exists holds", "    labelSelector:\n      matchExpressions:\n        - {key: tier, operator: Exists}\n", nil},
		{"Exists fails", "    labelSelector:\n      matchExpressions:\n        - {key: zone, operator: Exists}\n", []string{"zone"}},
		{"DoesNotExist holds", "    labelSelector:\n      matchExpressions:\n        - {key: zone, operator: DoesNotExist}\n", nil},
		{"DoesNotExist fails", "    labelSelector:\n      matchExpressions:\n        - {key: app, operator: DoesNotExist}\n", []string{"app"}},
		{"two dead keys are both named", "    labelSelector:\n      matchLabels:\n        app: api\n      matchExpressions:\n        - {key: zone, operator: Exists}\n", []string{"app", "zone"}},
		{"an empty selector matches every pod", "    labelSelector: {}\n", nil},
		{"a numeric value is compared as text", "    labelSelector:\n      matchLabels:\n        app: web\n      matchExpressions:\n        - {key: tier, operator: In, values: [front]}\n", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			report := judge(t, spread(c.selector))
			dead := report.Dead()
			if c.deadKeys == nil {
				if len(dead) != 0 {
					t.Fatalf("want alive, got %+v", dead)
				}
				return
			}
			if len(dead) != 1 {
				t.Fatalf("want one dead finding, got %+v", report.Findings)
			}
			if got := strings.Join(dead[0].DeadKeys, ","); got != strings.Join(c.deadKeys, ",") {
				t.Errorf("dead keys = %q, want %q", got, strings.Join(c.deadKeys, ","))
			}
		})
	}
}

func TestANullSelectorIsDead(t *testing.T) {
	labels := map[string]string{"app": "web"}
	stream := workload("Deployment", "web", "", labels,
		"topologySpreadConstraints:\n  - topologyKey: kubernetes.io/hostname\n    maxSkew: 1\n    whenUnsatisfiable: DoNotSchedule\n"+
			"affinity:\n  podAntiAffinity:\n    preferredDuringSchedulingIgnoredDuringExecution:\n      - weight: 100\n        podAffinityTerm:\n          topologyKey: kubernetes.io/hostname\n")
	report := judge(t, stream)
	dead := report.Dead()
	if len(dead) != 2 {
		t.Fatalf("a spread and a term without labelSelector select no pod, want 2 dead, got %+v", report.Findings)
	}
	sites := map[string]bool{}
	for _, f := range dead {
		if f.Selector != "<null>" {
			t.Errorf("%s: selector rendered %q, want <null>", f.Site, f.Selector)
		}
		sites[f.Site] = true
	}
	for _, want := range []string{"topologySpreadConstraints[0]", "affinity.podAntiAffinity.preferredDuringSchedulingIgnoredDuringExecution[0].podAffinityTerm"} {
		if !sites[want] {
			t.Errorf("no dead finding at %s, got %v", want, sites)
		}
	}
}

func TestCrossWorkloadIsReportedNotFatal(t *testing.T) {
	server := workload("Deployment", "server", "", map[string]string{"app": "server"},
		"affinity:\n  podAntiAffinity:\n    requiredDuringSchedulingIgnoredDuringExecution:\n      - topologyKey: kubernetes.io/hostname\n        labelSelector:\n          matchLabels:\n            app: runner\n")
	runner := workload("Deployment", "runner", "", map[string]string{"app": "runner"}, "")
	report := judge(t, server+"---\n"+runner)
	if len(report.Dead()) != 0 {
		t.Fatalf("keeping the server away from the runner is not dead, got %+v", report.Dead())
	}
	if len(report.Findings) != 1 || report.Findings[0].Verdict != rendergate.CrossWorkload {
		t.Fatalf("want one cross-workload finding, got %+v", report.Findings)
	}
	if !strings.Contains(report.Findings[0].Detail, "Deployment/runner") {
		t.Errorf("the finding must name the matched workload, got %q", report.Findings[0].Detail)
	}
	alone := judge(t, server)
	if len(alone.Dead()) != 1 {
		t.Fatalf("without the runner in the stream the same term is dead, got %+v", alone.Findings)
	}
}

func TestAPodAffinityNeedsSomePod(t *testing.T) {
	term := "affinity:\n  podAffinity:\n    requiredDuringSchedulingIgnoredDuringExecution:\n      - topologyKey: kubernetes.io/hostname\n        labelSelector:\n          matchLabels:\n            app: cache\n"
	web := workload("Deployment", "web", "", map[string]string{"app": "web"}, term)
	cache := workload("StatefulSet", "cache", "", map[string]string{"app": "cache"}, "")
	if report := judge(t, web+"---\n"+cache); len(report.Findings) != 0 {
		t.Fatalf("co-location with an existing pod is alive, got %+v", report.Findings)
	}
	report := judge(t, web)
	if dead := report.Dead(); len(dead) != 1 || dead[0].Site != "affinity.podAffinity.requiredDuringSchedulingIgnoredDuringExecution[0]" {
		t.Fatalf("co-location with nothing is dead, got %+v", report.Findings)
	}
}

func TestAPodDisruptionBudgetSelectingNoPodIsDead(t *testing.T) {
	pdb := func(namespace, app string) string {
		return "apiVersion: policy/v1\nkind: PodDisruptionBudget\nmetadata:\n  name: web\n  namespace: " + namespace + "\nspec:\n  minAvailable: 1\n  selector:\n    matchLabels:\n      app: " + app + "\n"
	}
	web := workload("Deployment", "web", "prod", map[string]string{"app": "web"}, "")
	if report := judge(t, web+"---\n"+pdb("prod", "web")); len(report.Findings) != 0 || report.Judged != 1 {
		t.Fatalf("a budget over existing pods is alive and judged, got %+v (judged %d)", report.Findings, report.Judged)
	}
	report := judge(t, web+"---\n"+pdb("prod", "wep"))
	dead := report.Dead()
	if len(dead) != 1 || dead[0].Kind != "PodDisruptionBudget" || dead[0].Site != "spec.selector" {
		t.Fatalf("a budget over a typo is dead, got %+v", report.Findings)
	}
	if len(dead[0].DeadKeys) != 1 || dead[0].DeadKeys[0] != "app" {
		t.Errorf("dead keys = %v", dead[0].DeadKeys)
	}
	elsewhere := judge(t, web+"---\n"+pdb("staging", "web"))
	if len(elsewhere.Dead()) != 1 {
		t.Fatalf("a budget in another namespace than its pods protects nothing, got %+v", elsewhere.Findings)
	}
	null := "apiVersion: policy/v1\nkind: PodDisruptionBudget\nmetadata:\n  name: web\n  namespace: prod\nspec:\n  minAvailable: 1\n"
	if report := judge(t, web+"---\n"+null); len(report.Dead()) != 1 {
		t.Fatalf("a budget without a selector selects nothing, got %+v", report.Findings)
	}
}

func TestTermsReachingOtherNamespacesAreUnjudged(t *testing.T) {
	term := func(extra string) string {
		return "affinity:\n  podAntiAffinity:\n    requiredDuringSchedulingIgnoredDuringExecution:\n      - topologyKey: kubernetes.io/hostname\n" + extra + "        labelSelector:\n          matchLabels:\n            app: web\n"
	}
	labels := map[string]string{"app": "web"}
	byNamespaceSelector := judge(t, workload("Deployment", "web", "prod", labels, term("        namespaceSelector:\n          matchLabels:\n            team: a\n")))
	if len(byNamespaceSelector.Findings) != 1 || byNamespaceSelector.Findings[0].Verdict != rendergate.Unjudged || byNamespaceSelector.Judged != 0 {
		t.Fatalf("a namespaceSelector term cannot be judged from the stream, got %+v", byNamespaceSelector)
	}
	byOtherNamespace := judge(t, workload("Deployment", "web", "prod", labels, term("        namespaces: [staging]\n")))
	if len(byOtherNamespace.Findings) != 1 || byOtherNamespace.Findings[0].Verdict != rendergate.Unjudged {
		t.Fatalf("a term naming another namespace is unjudged, got %+v", byOtherNamespace.Findings)
	}
	own := judge(t, workload("Deployment", "web", "prod", labels, term("        namespaces: [prod]\n")))
	if len(own.Findings) != 0 || own.Judged != 1 {
		t.Fatalf("a term naming its own namespace is judged like any other, got %+v", own)
	}
}

func TestEveryPodTemplateKindIsJudged(t *testing.T) {
	deadSpread := "topologySpreadConstraints:\n  - topologyKey: zone\n    maxSkew: 1\n    whenUnsatisfiable: ScheduleAnyway\n    labelSelector:\n      matchLabels:\n        app: nobody\n"
	for _, kind := range []string{"Deployment", "StatefulSet", "DaemonSet", "ReplicaSet", "Job"} {
		report := judge(t, workload(kind, "w", "", map[string]string{"app": "w"}, deadSpread))
		if dead := report.Dead(); len(dead) != 1 || dead[0].Kind != kind {
			t.Errorf("%s: want one dead finding of that kind, got %+v", kind, report.Findings)
		}
	}
	cronJob := "apiVersion: batch/v1\nkind: CronJob\nmetadata:\n  name: nightly\nspec:\n  schedule: '0 2 * * *'\n  jobTemplate:\n    spec:\n      template:\n        metadata:\n          labels:\n            app: nightly\n        spec:\n          topologySpreadConstraints:\n            - topologyKey: zone\n              maxSkew: 1\n              whenUnsatisfiable: ScheduleAnyway\n              labelSelector:\n                matchLabels:\n                  app: nobody\n"
	report := judge(t, cronJob)
	if dead := report.Dead(); len(dead) != 1 || dead[0].Kind != "CronJob" || dead[0].Name != "nightly" {
		t.Errorf("CronJob: want one dead finding, got %+v", report.Findings)
	}
	pod := "apiVersion: v1\nkind: Pod\nmetadata:\n  name: p\n  labels:\n    app: p\nspec:\n  topologySpreadConstraints:\n    - topologyKey: zone\n      maxSkew: 1\n      whenUnsatisfiable: ScheduleAnyway\n      labelSelector:\n        matchLabels:\n          app: p\n"
	if report := judge(t, pod); len(report.Findings) != 0 || report.Judged != 1 || report.PodTemplates != 1 {
		t.Errorf("Pod: its own labels are the template, got %+v", report)
	}
}

func TestMalformedStreamsAreRefused(t *testing.T) {
	if _, err := rendergate.Judge([]byte("kind: Deployment\nmetadata: [unclosed\n")); err == nil {
		t.Error("broken YAML must be an error, not a clean report")
	}
	bad := workload("Deployment", "w", "", map[string]string{"app": "w"},
		"topologySpreadConstraints:\n  - topologyKey: zone\n    labelSelector:\n      matchExpressions:\n        - {key: app, operator: Gt, values: ['1']}\n")
	_, err := rendergate.Judge([]byte(bad))
	if err == nil || !strings.Contains(err.Error(), "unknown operator") {
		t.Errorf("an operator the API would refuse must be refused here, got %v", err)
	}
	noKey := workload("Deployment", "w", "", map[string]string{"app": "w"},
		"topologySpreadConstraints:\n  - topologyKey: zone\n    labelSelector:\n      matchExpressions:\n        - {operator: Exists}\n")
	if _, err := rendergate.Judge([]byte(noKey)); err == nil || !strings.Contains(err.Error(), "empty key") {
		t.Errorf("a requirement without a key must be refused, got %v", err)
	}
}

func TestAnEmptyStreamJudgesNothing(t *testing.T) {
	report := judge(t, "# nothing rendered\n---\n---\napiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: c\n")
	if report.Judged != 0 || report.PodTemplates != 0 || len(report.Findings) != 0 {
		t.Fatalf("got %+v", report)
	}
	if !strings.Contains(report.Summary(), "0 pod templates, 0 selectors judged, 0 dead") {
		t.Errorf("summary = %q", report.Summary())
	}
}

func TestADuplicateKeyElsewhereIsAWarningNotADeath(t *testing.T) {
	// A real ovh-prod render carried ITERION_CANONICAL_REDIRECT twice in one
	// ConfigMap: the API server keeps the last, yaml.v3 refuses the document,
	// and a gate that refused with it never judged the dead selector that
	// followed in the same stream.
	configMap := "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: iterion-config\n  namespace: iterion\ndata:\n  ITERION_CANONICAL_REDIRECT: \"false\"\n  ITERION_MODE: cloud\n  ITERION_CANONICAL_REDIRECT: \"true\"\n"
	report := judge(t, configMap+"---\n"+valkeyha("valkey"))
	if len(report.Dead()) != 1 {
		t.Fatalf("the dead selector after the duplicate key must still be judged, got %+v", report.Findings)
	}
	if len(report.Warnings) != 1 {
		t.Fatalf("want one warning for the duplicate key, got %v", report.Warnings)
	}
	for _, want := range []string{"ConfigMap iterion-config", `"ITERION_CANONICAL_REDIRECT"`, "line 7", "line 9", "keeps the last"} {
		if !strings.Contains(report.Warnings[0], want) {
			t.Errorf("warning %q lacks %q", report.Warnings[0], want)
		}
	}
	docs, _, err := rendergate.Parse([]byte(configMap))
	if err != nil {
		t.Fatal(err)
	}
	data, _ := docs[0].Body["data"].(map[string]any)
	if data["ITERION_CANONICAL_REDIRECT"] != "true" {
		t.Errorf("the last definition must win like the API server, got %v", data["ITERION_CANONICAL_REDIRECT"])
	}
}

func TestAnAliasCycleIsRefusedNotFollowed(t *testing.T) {
	// `&x {a: *x}`: following the alias never ends. The gate must answer with
	// an error, not grow its stack until the runtime kills it.
	_, err := rendergate.Judge([]byte("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: c\ndata: &x\n  a: *x\n"))
	if err == nil || !strings.Contains(err.Error(), "alias *x reaches back into its own anchor") {
		t.Fatalf("want the cycle refused by name, got %v", err)
	}
	// A legitimate alias (the same anchor used twice, no cycle) still folds.
	report := judge(t, "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: c\ndata:\n  a: &shared {k: v}\n  b: *shared\n---\n"+valkeyha("valkeyha"))
	if len(report.Findings) != 0 || report.Judged != 1 {
		t.Fatalf("a shared anchor is fine, got %+v", report)
	}
}

func judgeIn(t *testing.T, namespace, stream string) rendergate.Report {
	t.Helper()
	report, err := rendergate.JudgeWithOptions([]byte(stream), rendergate.Options{DefaultNamespace: namespace})
	if err != nil {
		t.Fatalf("Judge: %v", err)
	}
	return report
}

func TestAListIsFlattenedLikeKubectlDoes(t *testing.T) {
	list := "apiVersion: v1\nkind: List\nitems:\n  - " + strings.ReplaceAll(strings.TrimPrefix(valkeyha("valkey"), "\n"), "\n", "\n    ")
	report := judge(t, list)
	if len(report.Dead()) != 1 || report.PodTemplates != 1 {
		t.Fatalf("the StatefulSet inside a List must be judged, got %+v", report)
	}
}

func TestObjectsWithoutANamespaceLandInTheReleaseNamespace(t *testing.T) {
	// A Helm umbrella writes no metadata.namespace into its own templates
	// while a sub-chart writes `iterion` into its StatefulSet: both land in
	// the release namespace at apply time. The budget the #2012 follow-up
	// needs is exactly that shape.
	budget := "apiVersion: policy/v1\nkind: PodDisruptionBudget\nmetadata:\n  name: iterion-valkeyha\nspec:\n  maxUnavailable: 1\n  selector:\n    matchLabels:\n      app.kubernetes.io/name: valkeyha\n      app.kubernetes.io/instance: iterion\n"
	stream := budget + "---\n" + valkeyha("valkeyha")
	if report := judgeIn(t, "iterion", stream); len(report.Findings) != 0 || report.Judged != 2 {
		t.Fatalf("with the release namespace the budget is alive, got %+v", report)
	}
	if report := judge(t, stream); len(report.Dead()) != 1 {
		t.Fatalf("without a release namespace the two objects stay apart, got %+v", report.Findings)
	}
	// A term naming the release namespace explicitly is judged, not unjudged.
	term := "affinity:\n  podAntiAffinity:\n    requiredDuringSchedulingIgnoredDuringExecution:\n      - topologyKey: kubernetes.io/hostname\n        namespaces: [iterion]\n        labelSelector:\n          matchLabels:\n            app: nobody\n"
	report := judgeIn(t, "iterion", workload("Deployment", "web", "", map[string]string{"app": "web"}, term))
	if dead := report.Dead(); len(dead) != 1 {
		t.Fatalf("a dead term naming the release namespace must be dead, got %+v", report.Findings)
	}
}

func TestMergeKeysAreFoldedLikeYAMLDefines(t *testing.T) {
	// Labels through a merge key: the pods DO carry them.
	viaMerge := "apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: web\nspec:\n  template:\n    metadata:\n      labels:\n        <<: &labels {app: web, tier: front}\n        extra: yes\n    spec:\n      topologySpreadConstraints:\n        - topologyKey: kubernetes.io/hostname\n          maxSkew: 1\n          whenUnsatisfiable: ScheduleAnyway\n          labelSelector:\n            matchLabels:\n              app: web\n"
	if report := judge(t, viaMerge); len(report.Findings) != 0 || report.Judged != 1 {
		t.Fatalf("labels reached through << are the pod's labels, got %+v", report)
	}
	// A dead term reached through a merge key is still dead.
	deadViaMerge := "apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: web\nspec:\n  template:\n    metadata:\n      labels:\n        app: web\n    spec:\n      affinity:\n        <<: &aff\n          podAntiAffinity:\n            requiredDuringSchedulingIgnoredDuringExecution:\n              - topologyKey: kubernetes.io/hostname\n                labelSelector:\n                  matchLabels:\n                    app: nobody\n"
	if report := judge(t, deadViaMerge); len(report.Dead()) != 1 {
		t.Fatalf("a dead term behind << must be found, got %+v", report.Findings)
	}
	// Document order, the later definition wins — the yaml.v2 semantics
	// kubectl and Helm apply through (not the YAML specification's).
	docs, _, err := rendergate.Parse([]byte("kind: ConfigMap\nmetadata:\n  name: c\ndata:\n  <<: {a: merged, b: merged}\n  a: own\n---\nkind: ConfigMap\nmetadata:\n  name: d\ndata:\n  a: own\n  <<: {a: merged}\n"))
	if err != nil {
		t.Fatal(err)
	}
	data, _ := docs[0].Body["data"].(map[string]any)
	if data["a"] != "own" || data["b"] != "merged" {
		t.Errorf("own key after the merge wins: got %v", data)
	}
	data, _ = docs[1].Body["data"].(map[string]any)
	if data["a"] != "merged" {
		t.Errorf("a merge after the own key overrides it, as yaml.v2 does: got %v", data)
	}
	// A list of mappings: the earlier entry wins (yaml.v2 walks it
	// backwards), and the whole merge overrides an own key written before.
	docs, _, err = rendergate.Parse([]byte("kind: ConfigMap\nmetadata:\n  name: c\ndata:\n  a: own\n  <<: [{a: first, b: first}, {a: second, c: second}]\n"))
	if err != nil {
		t.Fatal(err)
	}
	if data, _ := docs[0].Body["data"].(map[string]any); data["a"] != "first" || data["b"] != "first" || data["c"] != "second" {
		t.Errorf("list merge: got %v", data)
	}
	// An alias to a sequence is not a mapping: refused, as yaml.v2 does.
	if _, _, err = rendergate.Parse([]byte("kind: ConfigMap\nmetadata:\n  name: c\ndata:\n  l: &l [{a: x}]\n  m:\n    <<: *l\n")); err == nil || !strings.Contains(err.Error(), "must merge a mapping") {
		t.Errorf("an alias to a sequence must be refused, got %v", err)
	}
	// An alias into ANOTHER document: kubectl refuses the stream.
	if _, _, err = rendergate.Parse([]byte("kind: ConfigMap\nmetadata:\n  name: c\ndata: &d {a: x}\n---\nkind: ConfigMap\nmetadata:\n  name: e\ndata: *d\n")); err == nil || !strings.Contains(err.Error(), "another document") {
		t.Errorf("an alias across documents must be refused, got %v", err)
	}
	// An alias as a KEY names what it aliases, not the anchor.
	docs, _, err = rendergate.Parse([]byte("kind: ConfigMap\nmetadata:\n  name: c\ndata:\n  k: &k app\n  *k : web\n"))
	if err != nil {
		t.Fatal(err)
	}
	if data, _ := docs[0].Body["data"].(map[string]any); data["app"] != "web" {
		t.Errorf("alias key: got %v", data)
	}
}

func TestNodeBudgetNamesTheBoundInForce(t *testing.T) {
	_, err := rendergate.JudgeWithOptions([]byte(valkeyha("valkey")), rendergate.Options{MaxNodes: 10})
	if err == nil || !strings.Contains(err.Error(), "past 10 nodes") {
		t.Fatalf("the budget error names the bound in force, got %v", err)
	}
}

func TestNestedAndTypedListsAreFlattened(t *testing.T) {
	inner := strings.ReplaceAll(strings.TrimPrefix(valkeyha("valkey"), "\n"), "\n", "\n        ")
	nested := "apiVersion: v1\nkind: List\nitems:\n  - apiVersion: apps/v1\n    kind: StatefulSetList\n    items:\n      - " + strings.TrimSpace(inner) + "\n"
	report := judge(t, nested)
	if len(report.Dead()) != 1 || report.PodTemplates != 1 {
		t.Fatalf("a StatefulSet two lists deep must be judged, got %+v", report)
	}
}

func TestExcessiveAliasingIsRefused(t *testing.T) {
	var b strings.Builder
	b.WriteString("kind: ConfigMap\nmetadata:\n  name: bomb\ndata:\n  a0: &a0 [x, x, x, x, x, x, x, x, x, x]\n")
	for i := 1; i < 9; i++ {
		fmt.Fprintf(&b, "  a%d: &a%d [*a%d, *a%d, *a%d, *a%d, *a%d, *a%d, *a%d, *a%d, *a%d, *a%d]\n", i, i, i-1, i-1, i-1, i-1, i-1, i-1, i-1, i-1, i-1, i-1)
	}
	_, err := rendergate.Judge([]byte(b.String()))
	if err == nil || !strings.Contains(err.Error(), "excessive aliasing") {
		t.Fatalf("a billion-laughs document must be refused by name, got %v", err)
	}
}

func TestControllerAddedLabelsAreNotDead(t *testing.T) {
	// pod-template-hash is stamped on every Deployment pod at runtime: a term
	// requiring it exists is alive; one requiring it does NOT exist is dead.
	exists := workload("Deployment", "web", "", map[string]string{"app": "web"},
		"topologySpreadConstraints:\n  - topologyKey: zone\n    maxSkew: 1\n    whenUnsatisfiable: ScheduleAnyway\n    labelSelector:\n      matchLabels:\n        app: web\n      matchExpressions:\n        - {key: pod-template-hash, operator: Exists}\n")
	if report := judge(t, exists); len(report.Findings) != 0 {
		t.Fatalf("pod-template-hash exists on every Deployment pod, got %+v", report.Findings)
	}
	jobName := "apiVersion: batch/v1\nkind: Job\nmetadata:\n  name: j\nspec:\n  template:\n    metadata:\n      labels:\n        app: j\n    spec:\n      affinity:\n        podAntiAffinity:\n          requiredDuringSchedulingIgnoredDuringExecution:\n            - topologyKey: kubernetes.io/hostname\n              labelSelector:\n                matchLabels:\n                  batch.kubernetes.io/job-name: j\n"
	if report := judge(t, jobName); len(report.Findings) != 0 {
		t.Fatalf("the job name label is stamped on every Job pod, got %+v", report.Findings)
	}
	// A CronJob's jobs carry a generated name: a literal job-name is dead.
	cronName := "apiVersion: batch/v1\nkind: CronJob\nmetadata:\n  name: c\nspec:\n  schedule: '* * * * *'\n  jobTemplate:\n    spec:\n      template:\n        metadata:\n          labels:\n            app: c\n        spec:\n          affinity:\n            podAntiAffinity:\n              requiredDuringSchedulingIgnoredDuringExecution:\n                - topologyKey: kubernetes.io/hostname\n                  labelSelector:\n                    matchLabels:\n                      batch.kubernetes.io/job-name: c\n"
	if report := judge(t, cronName); len(report.Dead()) != 1 {
		t.Fatalf("a CronJob's job name is generated, a literal is dead, got %+v", report.Findings)
	}
	absent := workload("Deployment", "web", "", map[string]string{"app": "web"},
		"topologySpreadConstraints:\n  - topologyKey: zone\n    maxSkew: 1\n    whenUnsatisfiable: ScheduleAnyway\n    labelSelector:\n      matchExpressions:\n        - {key: pod-template-hash, operator: DoesNotExist}\n")
	if report := judge(t, absent); len(report.Dead()) != 1 {
		t.Fatalf("requiring the runtime label to be absent selects no pod, got %+v", report.Findings)
	}
	// A literal against a runtime key is a stale hash: dead, and said so.
	stale := workload("Deployment", "web", "", map[string]string{"app": "web"},
		"topologySpreadConstraints:\n  - topologyKey: zone\n    maxSkew: 1\n    whenUnsatisfiable: ScheduleAnyway\n    labelSelector:\n      matchExpressions:\n        - {key: pod-template-hash, operator: In, values: [abc123]}\n")
	if report := judge(t, stale); len(report.Dead()) != 1 || !strings.Contains(report.Dead()[0].Detail, "stamped at runtime") {
		t.Fatalf("a literal pod-template-hash is dead, got %+v", report.Findings)
	}
	// A bare ReplicaSet's pods carry no pod-template-hash; a Rollout's carry
	// rollouts-pod-template-hash only.
	for key, want := range map[string]int{"pod-template-hash": 1, "rollouts-pod-template-hash": 0} {
		rollout := strings.Replace(workload("Deployment", "web", "", map[string]string{"app": "web"},
			"topologySpreadConstraints:\n  - topologyKey: zone\n    maxSkew: 1\n    whenUnsatisfiable: ScheduleAnyway\n    labelSelector:\n      matchExpressions:\n        - {key: "+key+", operator: Exists}\n"), "kind: Deployment", "kind: Rollout", 1)
		if report := judge(t, rollout); len(report.Dead()) != want {
			t.Fatalf("a Rollout stamps rollouts-pod-template-hash only: %s Exists must give %d dead, got %+v", key, want, report.Findings)
		}
	}
	rs := workload("ReplicaSet", "web", "", map[string]string{"app": "web"},
		"topologySpreadConstraints:\n  - topologyKey: zone\n    maxSkew: 1\n    whenUnsatisfiable: ScheduleAnyway\n    labelSelector:\n      matchExpressions:\n        - {key: pod-template-hash, operator: Exists}\n")
	if report := judge(t, rs); len(report.Dead()) != 1 {
		t.Fatalf("a bare ReplicaSet carries no pod-template-hash, got %+v", report.Findings)
	}
	// A literal against a label that names ONE pod may match a pod the
	// manifests do not show: unjudged, not dead.
	ordinal := workload("StatefulSet", "db", "", map[string]string{"app": "db"},
		"affinity:\n  podAntiAffinity:\n    requiredDuringSchedulingIgnoredDuringExecution:\n      - topologyKey: kubernetes.io/hostname\n        labelSelector:\n          matchLabels:\n            statefulset.kubernetes.io/pod-name: db-0\n")
	if report := judge(t, ordinal); len(report.Findings) != 1 || report.Findings[0].Verdict != rendergate.Unjudged || len(report.Dead()) != 0 {
		t.Fatalf("a literal pod name is unjudged, got %+v", report.Findings)
	}
	// The completion index exists on the pods of an Indexed Job only.
	indexed := func(mode string) string {
		return "apiVersion: batch/v1\nkind: Job\nmetadata:\n  name: j\nspec:\n  completionMode: " + mode + "\n  template:\n    metadata:\n      labels:\n        app: j\n    spec:\n      topologySpreadConstraints:\n        - topologyKey: zone\n          maxSkew: 1\n          whenUnsatisfiable: ScheduleAnyway\n          labelSelector:\n            matchExpressions:\n              - {key: batch.kubernetes.io/job-completion-index, operator: Exists}\n"
	}
	if report := judge(t, indexed("Indexed")); len(report.Findings) != 0 {
		t.Fatalf("an Indexed Job stamps the completion index, got %+v", report.Findings)
	}
	if report := judge(t, indexed("NonIndexed")); len(report.Dead()) != 1 {
		t.Fatalf("a NonIndexed Job stamps no completion index, got %+v", report.Findings)
	}
	stateful := workload("StatefulSet", "db", "", map[string]string{"app": "db"},
		"topologySpreadConstraints:\n  - topologyKey: zone\n    maxSkew: 1\n    whenUnsatisfiable: ScheduleAnyway\n    labelSelector:\n      matchExpressions:\n        - {key: controller-revision-hash, operator: Exists}\n")
	if report := judge(t, stateful); len(report.Findings) != 0 {
		t.Fatalf("controller-revision-hash is stamped on every StatefulSet pod, got %+v", report.Findings)
	}
}

func TestMatchLabelKeysFollowTheAPIServer(t *testing.T) {
	// matchLabelKeys adds `key in (the pod's value)`: alive on a key the
	// template carries, and on the runtime pod-template-hash; a term whose
	// selector is otherwise dead stays dead.
	alive := workload("Deployment", "web", "", map[string]string{"app": "web", "tier": "front"},
		"affinity:\n  podAntiAffinity:\n    requiredDuringSchedulingIgnoredDuringExecution:\n      - topologyKey: kubernetes.io/hostname\n        matchLabelKeys: [tier, pod-template-hash]\n        labelSelector:\n          matchLabels:\n            app: web\n")
	if report := judge(t, alive); len(report.Findings) != 0 {
		t.Fatalf("matchLabelKeys on carried labels is alive, got %+v", report.Findings)
	}
	dead := workload("Deployment", "web", "", map[string]string{"app": "web"},
		"affinity:\n  podAntiAffinity:\n    requiredDuringSchedulingIgnoredDuringExecution:\n      - topologyKey: kubernetes.io/hostname\n        matchLabelKeys: [app]\n        labelSelector:\n          matchLabels:\n            app: nobody\n")
	if report := judge(t, dead); len(report.Dead()) != 1 {
		t.Fatalf("a dead selector stays dead whatever matchLabelKeys adds, got %+v", report.Findings)
	}
	// The same fold on a topologySpreadConstraint: a selector that matches
	// only a PEER until matchLabelKeys adds `app in (a)` is dead, not
	// cross-workload (the scheduler ANDs the keys into the spread selector).
	spreadA := workload("Deployment", "a", "", map[string]string{"app": "a", "tier": "x"},
		"topologySpreadConstraints:\n  - topologyKey: kubernetes.io/hostname\n    maxSkew: 1\n    whenUnsatisfiable: ScheduleAnyway\n    matchLabelKeys: [app]\n    labelSelector:\n      matchLabels:\n        tier: y\n")
	peerB := workload("Deployment", "b", "", map[string]string{"app": "b", "tier": "y"}, "")
	if report := judge(t, spreadA+"---\n"+peerB); len(report.Dead()) != 1 {
		t.Fatalf("a spread whose folded selector matches no pod is dead, got %+v", report.Findings)
	}
	mismatch := workload("Deployment", "web", "", map[string]string{"app": "web"},
		"affinity:\n  podAntiAffinity:\n    requiredDuringSchedulingIgnoredDuringExecution:\n      - topologyKey: kubernetes.io/hostname\n        mismatchLabelKeys: [app]\n        labelSelector:\n          matchLabels:\n            app: web\n")
	report := judge(t, mismatch)
	if len(report.Findings) != 1 || report.Findings[0].Verdict != rendergate.Unjudged || len(report.Dead()) != 0 {
		t.Fatalf("mismatchLabelKeys excludes the own pods by construction: unjudged, got %+v", report.Findings)
	}
}

func TestARolloutCarriesAPodTemplate(t *testing.T) {
	rollout := strings.Replace(workload("Deployment", "web", "", map[string]string{"app": "web"},
		"topologySpreadConstraints:\n  - topologyKey: zone\n    maxSkew: 1\n    whenUnsatisfiable: ScheduleAnyway\n    labelSelector:\n      matchLabels:\n        app: nobody\n"), "kind: Deployment", "kind: Rollout", 1)
	if report := judge(t, rollout); len(report.Dead()) != 1 || report.Dead()[0].Kind != "Rollout" {
		t.Fatalf("an Argo Rollout's template must be judged, got %+v", report)
	}
}
