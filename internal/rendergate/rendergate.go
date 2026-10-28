// Package rendergate judges a rendered Kubernetes manifest stream — the
// output of `helm template`, `kustomize build` or an Argo CD manifest dump —
// for selectors that select nothing.
//
// The class it refuses: a podAntiAffinity term, a topologySpreadConstraint,
// a podAffinity term or a PodDisruptionBudget whose label selector names a
// label that no pod of the stream carries. Neither `helm lint` nor the API
// server objects to such a selector: the scheduler simply finds no pod to
// keep apart, and the workload it was meant to spread lands wherever —
// which is how three valkeyha replicas held on three nodes by luck alone
// (#2012: `app.kubernetes.io/name In [valkey]` against pods labelled
// `valkeyha`, the sub-chart's alias). The gate confronts every such
// selector with the labels of the pod templates of the same namespace and
// names the workload and the dead key.
package rendergate

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Verdict grades one judged selector.
type Verdict string

const (
	// Dead marks a selector matching no pod template of the stream, the
	// workload's own pods included. Fatal: nothing it names exists.
	Dead Verdict = "dead"
	// CrossWorkload marks an anti-affinity or spread selector that matches
	// other pod templates of the namespace but none of the workload's own
	// pods — legitimate for keeping two workloads apart, suspicious for a
	// term meant to spread replicas. Reported, never fatal.
	CrossWorkload Verdict = "cross-workload"
	// Unjudged marks a term reaching into namespaces the stream does not
	// show (`namespaces` or `namespaceSelector`): nothing can be proven
	// about it from the render alone.
	Unjudged Verdict = "unjudged"
)

// Finding is one selector the gate could not prove alive.
type Finding struct {
	Kind, Name, Namespace string
	// Site is the field path of the selector below the pod spec (or the
	// object for a PodDisruptionBudget).
	Site string
	// Selector is the selector in the kubectl notation.
	Selector string
	Verdict  Verdict
	// DeadKeys are the keys of the requirements that fail against the
	// workload's own pod labels (for a PodDisruptionBudget: against every
	// pod template of its namespace).
	DeadKeys []string
	// Detail says what the pods carry instead, or which other workloads the
	// selector matches.
	Detail string
}

// String renders the finding on one line, the way the gate prints it.
func (f Finding) String() string {
	ns := f.Namespace
	if ns == "" {
		ns = "(release namespace)"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s: %s %s in %s: %s selector %s", f.Verdict, f.Kind, f.Name, ns, f.Site, f.Selector)
	switch f.Verdict {
	case Dead:
		if len(f.DeadKeys) > 0 {
			fmt.Fprintf(&b, " — dead key(s) %s", strings.Join(f.DeadKeys, ", "))
		}
	}
	if f.Detail != "" {
		fmt.Fprintf(&b, " (%s)", f.Detail)
	}
	return b.String()
}

// Report is the verdict over one stream.
type Report struct {
	// PodTemplates counts the pod templates the stream carries.
	PodTemplates int
	// Judged counts the selectors confronted with pod labels, dead or alive.
	// A gate that judged nothing proves nothing: callers assert it.
	Judged   int
	Findings []Finding
	// Warnings are the stream's defects outside the selector class — a
	// mapping key defined twice in one document, which the API server
	// resolves by keeping the last (a real render carried one). Reported,
	// never fatal, so the gate judges the stream it was given.
	Warnings []string
}

// Dead returns the fatal findings.
func (r Report) Dead() []Finding {
	var out []Finding
	for _, f := range r.Findings {
		if f.Verdict == Dead {
			out = append(out, f)
		}
	}
	return out
}

// Summary is the one-line count the gate prints last.
func (r Report) Summary() string {
	counts := map[Verdict]int{}
	for _, f := range r.Findings {
		counts[f.Verdict]++
	}
	return fmt.Sprintf("render-gate: %d pod templates, %d selectors judged, %d dead, %d cross-workload, %d unjudged",
		r.PodTemplates, r.Judged, counts[Dead], counts[CrossWorkload], counts[Unjudged])
}

// Judge parses the stream and confronts every scheduling and disruption
// selector with the pod templates of its namespace.
func Judge(stream []byte) (Report, error) {
	docs, warnings, err := Parse(stream)
	if err != nil {
		return Report{}, err
	}
	var templates []podTemplate
	var pdbs []object
	for _, doc := range docs {
		switch doc.kind {
		case "PodDisruptionBudget":
			pdbs = append(pdbs, doc)
		default:
			t, ok := templateOf(doc)
			if ok {
				templates = append(templates, t)
			}
		}
	}
	report := Report{PodTemplates: len(templates), Warnings: warnings}
	for _, t := range templates {
		if err := judgeTemplate(t, templates, &report); err != nil {
			return Report{}, err
		}
	}
	for _, p := range pdbs {
		if err := judgePDB(p, templates, &report); err != nil {
			return Report{}, err
		}
	}
	return report, nil
}

// object is one manifest of the stream.
type object struct {
	kind, name, namespace string
	body                  map[string]any
}

// Body is the folded document.
func (o object) Body() map[string]any { return o.body }

// Parse splits a multi-document YAML stream into its manifests, keeping
// every document that carries a `kind`. Documents are read as node trees
// and folded by hand: a mapping key defined twice keeps its last value,
// as the API server does, and is returned as a warning instead of the
// error yaml.v3 would raise — a gate that dies on a duplicate key in some
// ConfigMap never judges the selectors it exists for.
func Parse(stream []byte) ([]object, []string, error) {
	dec := yaml.NewDecoder(bytes.NewReader(stream))
	var out []object
	var warnings []string
	for i := 0; ; i++ {
		var node yaml.Node
		err := dec.Decode(&node)
		if errors.Is(err, io.EOF) {
			return out, warnings, nil
		}
		if err != nil {
			return nil, nil, fmt.Errorf("document %d: %w", i+1, err)
		}
		var duplicates []string
		doc, err := fold(&node, &duplicates)
		if err != nil {
			return nil, nil, fmt.Errorf("document %d: %w", i+1, err)
		}
		body, ok := doc.(map[string]any)
		if !ok {
			continue
		}
		kind, _ := body["kind"].(string)
		if kind == "" {
			continue
		}
		meta, _ := body["metadata"].(map[string]any)
		name, _ := meta["name"].(string)
		namespace, _ := meta["namespace"].(string)
		for _, d := range duplicates {
			warnings = append(warnings, fmt.Sprintf("document %d (%s %s): %s", i+1, kind, name, d))
		}
		out = append(out, object{kind: kind, name: name, namespace: namespace, body: body})
	}
}

// fold turns a yaml.v3 node tree into the generic shape (map[string]any,
// []any, scalars) the judges read, recording every mapping key defined
// twice; the last definition wins, which is what the API server keeps.
func fold(n *yaml.Node, duplicates *[]string) (any, error) {
	switch n.Kind {
	case yaml.DocumentNode:
		if len(n.Content) == 0 {
			return nil, nil
		}
		return fold(n.Content[0], duplicates)
	case yaml.AliasNode:
		return fold(n.Alias, duplicates)
	case yaml.MappingNode:
		m := make(map[string]any, len(n.Content)/2)
		firstLine := map[string]int{}
		for i := 0; i+1 < len(n.Content); i += 2 {
			key := n.Content[i].Value
			if line, seen := firstLine[key]; seen {
				*duplicates = append(*duplicates, fmt.Sprintf("mapping key %q defined at line %d and again at line %d — the API server keeps the last", key, line, n.Content[i].Line))
			} else {
				firstLine[key] = n.Content[i].Line
			}
			v, err := fold(n.Content[i+1], duplicates)
			if err != nil {
				return nil, err
			}
			m[key] = v
		}
		return m, nil
	case yaml.SequenceNode:
		list := make([]any, 0, len(n.Content))
		for _, c := range n.Content {
			v, err := fold(c, duplicates)
			if err != nil {
				return nil, err
			}
			list = append(list, v)
		}
		return list, nil
	case yaml.ScalarNode:
		var v any
		if err := n.Decode(&v); err != nil {
			return nil, fmt.Errorf("line %d: %w", n.Line, err)
		}
		return v, nil
	default:
		return nil, nil
	}
}

// podTemplate is the pod half of a workload: its labels and its spec.
type podTemplate struct {
	kind, name, namespace string
	labels                map[string]string
	spec                  map[string]any
}

// templateOf extracts the pod template of the kinds that carry one.
func templateOf(doc object) (podTemplate, bool) {
	spec, _ := doc.body["spec"].(map[string]any)
	var tpl map[string]any
	switch doc.kind {
	case "Pod":
		tpl = doc.body
	case "Deployment", "StatefulSet", "DaemonSet", "ReplicaSet", "ReplicationController", "Job":
		tpl, _ = spec["template"].(map[string]any)
	case "CronJob":
		jobTemplate, _ := spec["jobTemplate"].(map[string]any)
		jobSpec, _ := jobTemplate["spec"].(map[string]any)
		tpl, _ = jobSpec["template"].(map[string]any)
	default:
		return podTemplate{}, false
	}
	if tpl == nil {
		return podTemplate{}, false
	}
	meta, _ := tpl["metadata"].(map[string]any)
	podSpec, _ := tpl["spec"].(map[string]any)
	return podTemplate{
		kind:      doc.kind,
		name:      doc.name,
		namespace: doc.namespace,
		labels:    stringMap(meta["labels"]),
		spec:      podSpec,
	}, true
}

// judgeTemplate confronts the anti-affinity, affinity and spread selectors
// of one pod template with its own labels and with its namespace peers.
func judgeTemplate(t podTemplate, all []podTemplate, report *Report) error {
	affinity, _ := t.spec["affinity"].(map[string]any)
	for _, kind := range []string{"podAntiAffinity", "podAffinity"} {
		block, _ := affinity[kind].(map[string]any)
		required, _ := block["requiredDuringSchedulingIgnoredDuringExecution"].([]any)
		for i, term := range required {
			site := fmt.Sprintf("affinity.%s.requiredDuringSchedulingIgnoredDuringExecution[%d]", kind, i)
			if err := judgeTerm(t, all, site, term, kind == "podAntiAffinity", report); err != nil {
				return err
			}
		}
		preferred, _ := block["preferredDuringSchedulingIgnoredDuringExecution"].([]any)
		for i, weighted := range preferred {
			site := fmt.Sprintf("affinity.%s.preferredDuringSchedulingIgnoredDuringExecution[%d].podAffinityTerm", kind, i)
			w, _ := weighted.(map[string]any)
			if err := judgeTerm(t, all, site, w["podAffinityTerm"], kind == "podAntiAffinity", report); err != nil {
				return err
			}
		}
	}
	spreads, _ := t.spec["topologySpreadConstraints"].([]any)
	for i, c := range spreads {
		site := fmt.Sprintf("topologySpreadConstraints[%d]", i)
		constraint, _ := c.(map[string]any)
		sel, err := parseSelector(constraint["labelSelector"])
		if err != nil {
			return fmt.Errorf("%s %s: %s: %w", t.kind, t.name, site, err)
		}
		judgeOwn(t, all, site, sel, report)
	}
	return nil
}

// judgeTerm judges one podAffinityTerm. Anti-affinity terms are judged
// against the workload's own pods (a cross-workload match is reported, not
// fatal); affinity terms only need SOME pod of the namespace to match.
func judgeTerm(t podTemplate, all []podTemplate, site string, raw any, anti bool, report *Report) error {
	term, _ := raw.(map[string]any)
	sel, err := parseSelector(term["labelSelector"])
	if err != nil {
		return fmt.Errorf("%s %s: %s: %w", t.kind, t.name, site, err)
	}
	if reachesOtherNamespaces(term, t.namespace) {
		report.Findings = append(report.Findings, Finding{
			Kind: t.kind, Name: t.name, Namespace: t.namespace, Site: site,
			Selector: sel.String(), Verdict: Unjudged,
			Detail: "the term names namespaces the stream does not show",
		})
		return nil
	}
	if anti {
		judgeOwn(t, all, site, sel, report)
		return nil
	}
	report.Judged++
	if matched := peersMatching(sel, t, all, true); len(matched) > 0 {
		return nil
	}
	report.Findings = append(report.Findings, Finding{
		Kind: t.kind, Name: t.name, Namespace: t.namespace, Site: site,
		Selector: sel.String(), Verdict: Dead,
		Detail: fmt.Sprintf("matches none of the %d pod template(s) of the namespace", countNamespace(all, t.namespace)),
	})
	return nil
}

// judgeOwn is the anti-affinity / spread rule: the selector must match the
// workload's OWN pod labels; matching only other workloads is reported as
// cross-workload; matching nothing is dead, with the failing keys named.
func judgeOwn(t podTemplate, all []podTemplate, site string, sel selector, report *Report) {
	report.Judged++
	if sel.matches(t.labels) {
		return
	}
	finding := Finding{Kind: t.kind, Name: t.name, Namespace: t.namespace, Site: site, Selector: sel.String()}
	if others := peersMatching(sel, t, all, false); len(others) > 0 {
		finding.Verdict = CrossWorkload
		finding.Detail = "matches " + strings.Join(others, ", ") + " but none of its own pods"
		report.Findings = append(report.Findings, finding)
		return
	}
	finding.Verdict = Dead
	finding.DeadKeys = sel.failing(t.labels)
	finding.Detail = carried(t.labels, finding.DeadKeys)
	report.Findings = append(report.Findings, finding)
}

// judgePDB confronts a PodDisruptionBudget's selector with every pod
// template of its namespace: a budget that selects no pod protects nothing.
func judgePDB(p object, all []podTemplate, report *Report) error {
	spec, _ := p.body["spec"].(map[string]any)
	sel, err := parseSelector(spec["selector"])
	if err != nil {
		return fmt.Errorf("PodDisruptionBudget %s: spec.selector: %w", p.name, err)
	}
	report.Judged++
	var dead []string
	first := true
	for _, t := range all {
		if t.namespace != p.namespace {
			continue
		}
		if sel.matches(t.labels) {
			return nil
		}
		if first {
			dead = sel.failing(t.labels)
			first = false
		} else {
			dead = intersect(dead, sel.failing(t.labels))
		}
	}
	report.Findings = append(report.Findings, Finding{
		Kind: p.kind, Name: p.name, Namespace: p.namespace, Site: "spec.selector",
		Selector: sel.String(), Verdict: Dead, DeadKeys: dead,
		Detail: fmt.Sprintf("matches none of the %d pod template(s) of the namespace", countNamespace(all, p.namespace)),
	})
	return nil
}

// reachesOtherNamespaces reports a term the stream cannot judge: one with a
// namespaceSelector, or a namespaces list naming anything but the own one.
func reachesOtherNamespaces(term map[string]any, own string) bool {
	if _, ok := term["namespaceSelector"]; ok && term["namespaceSelector"] != nil {
		return true
	}
	namespaces, _ := term["namespaces"].([]any)
	for _, ns := range namespaces {
		if fmt.Sprint(ns) != own {
			return true
		}
	}
	return false
}

// peersMatching lists the `Kind/name` of the namespace's pod templates the
// selector matches, the workload itself included when self is true.
func peersMatching(sel selector, t podTemplate, all []podTemplate, self bool) []string {
	var out []string
	for _, p := range all {
		if p.namespace != t.namespace {
			continue
		}
		if !self && p.kind == t.kind && p.name == t.name {
			continue
		}
		if sel.matches(p.labels) {
			out = append(out, p.kind+"/"+p.name)
		}
	}
	return out
}

func countNamespace(all []podTemplate, namespace string) int {
	n := 0
	for _, p := range all {
		if p.namespace == namespace {
			n++
		}
	}
	return n
}

// carried says what the pods carry for the dead keys.
func carried(labels map[string]string, keys []string) string {
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		if v, ok := labels[k]; ok {
			parts = append(parts, fmt.Sprintf("the pods carry %s=%s", k, v))
		} else {
			parts = append(parts, fmt.Sprintf("the pods carry no %s label", k))
		}
	}
	return strings.Join(parts, "; ")
}

func intersect(a, b []string) []string {
	keep := map[string]bool{}
	for _, k := range b {
		keep[k] = true
	}
	var out []string
	for _, k := range a {
		if keep[k] {
			out = append(out, k)
		}
	}
	return out
}

// requirement is one matchExpressions entry.
type requirement struct {
	key, operator string
	values        []string
}

// selector is a metav1.LabelSelector: a null selector matches no pod, an
// empty one matches every pod, otherwise every matchLabels entry and every
// matchExpressions requirement must hold.
type selector struct {
	present bool
	labels  map[string]string
	exprs   []requirement
}

func parseSelector(raw any) (selector, error) {
	if raw == nil {
		return selector{}, nil
	}
	body, ok := raw.(map[string]any)
	if !ok {
		return selector{}, fmt.Errorf("labelSelector is not a mapping (%T)", raw)
	}
	sel := selector{present: true, labels: stringMap(body["matchLabels"])}
	exprs, _ := body["matchExpressions"].([]any)
	for i, e := range exprs {
		m, _ := e.(map[string]any)
		key, _ := m["key"].(string)
		op, _ := m["operator"].(string)
		switch op {
		case "In", "NotIn", "Exists", "DoesNotExist":
		default:
			return selector{}, fmt.Errorf("matchExpressions[%d]: unknown operator %q (In, NotIn, Exists, DoesNotExist)", i, op)
		}
		if key == "" {
			return selector{}, fmt.Errorf("matchExpressions[%d]: empty key", i)
		}
		var values []string
		rawValues, _ := m["values"].([]any)
		for _, v := range rawValues {
			values = append(values, fmt.Sprint(v))
		}
		sel.exprs = append(sel.exprs, requirement{key: key, operator: op, values: values})
	}
	return sel, nil
}

func (s selector) matches(labels map[string]string) bool {
	return s.present && len(s.failing(labels)) == 0
}

// failing lists the keys of the requirements the labels do not satisfy,
// matchLabels first, then the expressions in their order.
func (s selector) failing(labels map[string]string) []string {
	var out []string
	keys := make([]string, 0, len(s.labels))
	for k := range s.labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if v, ok := labels[k]; !ok || v != s.labels[k] {
			out = append(out, k)
		}
	}
	for _, r := range s.exprs {
		v, ok := labels[r.key]
		holds := false
		switch r.operator {
		case "In":
			holds = ok && contains(r.values, v)
		case "NotIn":
			holds = !ok || !contains(r.values, v)
		case "Exists":
			holds = ok
		case "DoesNotExist":
			holds = !ok
		}
		if !holds {
			out = append(out, r.key)
		}
	}
	return out
}

// String renders the selector in the kubectl notation; `<null>` for the
// absent selector (which matches no pod) and `{}` for the empty one (every
// pod).
func (s selector) String() string {
	if !s.present {
		return "<null>"
	}
	var parts []string
	keys := make([]string, 0, len(s.labels))
	for k := range s.labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		parts = append(parts, k+"="+s.labels[k])
	}
	for _, r := range s.exprs {
		switch r.operator {
		case "In":
			parts = append(parts, fmt.Sprintf("%s in (%s)", r.key, strings.Join(r.values, ",")))
		case "NotIn":
			parts = append(parts, fmt.Sprintf("%s notin (%s)", r.key, strings.Join(r.values, ",")))
		case "Exists":
			parts = append(parts, r.key)
		case "DoesNotExist":
			parts = append(parts, "!"+r.key)
		}
	}
	return "{" + strings.Join(parts, ", ") + "}"
}

func contains(values []string, v string) bool {
	for _, x := range values {
		if x == v {
			return true
		}
	}
	return false
}

// stringMap flattens a YAML mapping to string values (a numeric label value
// is still a label value to the scheduler).
func stringMap(raw any) map[string]string {
	out := map[string]string{}
	m, _ := raw.(map[string]any)
	for k, v := range m {
		if v == nil {
			out[k] = ""
			continue
		}
		out[k] = fmt.Sprint(v)
	}
	return out
}
