package kubernetes

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/sandbox"
)

// WITNESSES (ADR-162 decision 7, U5) on the per-run policy an untrusted
// run's sandbox carries, through the REAL renderer (BuildNetworkPolicy)
// and the REAL composition site (Driver.Start, over the kubectl capture
// shim): the untrusted run's policy names EXACTLY the proxy's port and
// the run's own listener ports on the runner pod, and drops kube-dns —
// the proxy resolves runner-side, the container reaches it by IP. A
// trusted run keeps today's policy: any port on the runner pod, DNS on
// 53 both ways.
//
// Mutants: BuildNetworkPolicy's Untrusted branch dropped (or its ports
// scope, or the kube-dns guard) → the narrowed rows; Driver.Start's
// info.UntrustedRun/EgressPorts pass-through dropped → the Start row
// renders the trusted shape.

func egressRules(t *testing.T, doc map[string]any) []any {
	t.Helper()
	spec, _ := doc["spec"].(map[string]any)
	rules, _ := spec["egress"].([]any)
	return rules
}

func firstEgressRule(t *testing.T, doc map[string]any) map[string]any {
	t.Helper()
	rules := egressRules(t, doc)
	if len(rules) == 0 {
		t.Fatal("the policy carries no egress rule")
	}
	r, _ := rules[0].(map[string]any)
	return r
}

// TestTheUntrustedRunsPolicyNamesExactlyItsOwnPortsAndDropsDNS.
func TestTheUntrustedRunsPolicyNamesExactlyItsOwnPortsAndDropsDNS(t *testing.T) {
	base := NetworkPolicyInput{
		Namespace: "iterion", Name: "iterion-run-r1", RunID: "run_1",
		RunnerPodIP: "10.42.0.7",
	}
	base.Untrusted, base.EgressPorts = true, []int{39117, 41234}
	out, err := BuildNetworkPolicy(base)
	if err != nil {
		t.Fatalf("BuildNetworkPolicy: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatalf("the rendered policy is not JSON: %v", err)
	}

	// Exactly ONE egress rule: the runner pod's /32, port-scoped to the
	// run's own listeners, every port TCP.
	if rules := egressRules(t, doc); len(rules) != 1 {
		t.Fatalf("an untrusted run's policy has %d egress rules, want exactly one (the runner pod, port-scoped):\n%s", len(rules), out)
	}
	rule := firstEgressRule(t, doc)
	to, _ := rule["to"].([]any)
	if len(to) != 1 {
		t.Fatalf("the egress rule names %d destinations, want the runner pod alone: %v", len(to), rule)
	}
	ipBlock, _ := to[0].(map[string]any)["ipBlock"].(map[string]any)
	if ipBlock["cidr"] != "10.42.0.7/32" {
		t.Fatalf("the egress rule's destination = %v, want the runner pod's /32", to)
	}
	ports, _ := rule["ports"].([]any)
	if len(ports) != 2 {
		t.Fatalf("the runner-pod rule carries ports %v, want exactly the proxy's and the run's listener's", rule["ports"])
	}
	for i, want := range []int{39117, 41234} {
		p, _ := ports[i].(map[string]any)
		if p["protocol"] != "TCP" || p["port"] != float64(want) {
			t.Fatalf("egress port #%d = %v, want TCP %d — the proxy and the listener are both TCP", i+1, p, want)
		}
	}

	// kube-dns is DROPPED: no rule names it, no port 53 anywhere, in
	// either protocol — the container resolves nothing; the proxy does.
	if strings.Contains(string(out), "kube-dns") || strings.Contains(string(out), `"port": 53`) {
		t.Fatalf("the untrusted run's policy still admits cluster DNS:\n%s", out)
	}

	// Trusted control: today's shape — the runner pod on ANY port (no
	// ports key on its rule), kube-dns on 53 UDP and TCP.
	base.Untrusted, base.EgressPorts = false, nil
	out, err = BuildNetworkPolicy(base)
	if err != nil {
		t.Fatalf("BuildNetworkPolicy (trusted): %v", err)
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatalf("the trusted policy is not JSON: %v", err)
	}
	if rules := egressRules(t, doc); len(rules) != 2 {
		t.Fatalf("the trusted policy has %d egress rules, want today's two (runner pod any port, kube-dns)", len(rules))
	}
	if _, hasPorts := firstEgressRule(t, doc)["ports"]; hasPorts {
		t.Fatalf("the trusted runner-pod rule is port-scoped (%v) — a trusted run keeps every port", firstEgressRule(t, doc))
	}
	if !strings.Contains(string(out), "kube-dns") || !strings.Contains(string(out), `"port": 53`) {
		t.Fatalf("the trusted policy dropped kube-dns:\n%s", out)
	}
}

// TestTheUntrustedPolicyRefusesToRenderWithoutItsPorts: fail closed —
// an untrusted policy that named no port would degrade to any-port
// egress toward the runner pod, every other run's listener included.
func TestTheUntrustedPolicyRefusesToRenderWithoutItsPorts(t *testing.T) {
	for _, row := range []struct {
		name string
		in   NetworkPolicyInput
	}{
		{"no port at all", NetworkPolicyInput{Namespace: "ns", Name: "x", RunID: "r1", RunnerPodIP: "10.0.0.1", Untrusted: true}},
		{"port zero", NetworkPolicyInput{Namespace: "ns", Name: "x", RunID: "r1", RunnerPodIP: "10.0.0.1", Untrusted: true, EgressPorts: []int{0}}},
		{"port over 65535", NetworkPolicyInput{Namespace: "ns", Name: "x", RunID: "r1", RunnerPodIP: "10.0.0.1", Untrusted: true, EgressPorts: []int{70000}}},
	} {
		t.Run(row.name, func(t *testing.T) {
			if _, err := BuildNetworkPolicy(row.in); err == nil {
				t.Fatal("expected the untrusted policy to refuse rendering — a portless rule admits every port")
			}
		})
	}
}

// TestStartWritesTheUntrustedRunsPortScopedPolicy drives the REAL
// Driver.Start over the kubectl capture shim: the RunInfo the runtime
// hands the driver (UntrustedRun, RunnerEgressPorts) reaches the
// NetworkPolicy actually applied — the one composition site.
func TestStartWritesTheUntrustedRunsPortScopedPolicy(t *testing.T) {
	for _, row := range []struct {
		name      string
		info      sandbox.RunInfo
		wantPorts bool
	}{
		{
			"fork lane", sandbox.RunInfo{
				RunID: "01a0-run", ProxyEndpoint: "http://10.42.0.7:39117",
				UntrustedRun: true, RunnerEgressPorts: []int{39117, 41234},
			}, true,
		},
		{
			"trusted run (the control)", sandbox.RunInfo{
				RunID: "01a0-run", ProxyEndpoint: "http://10.42.0.7:39117",
			}, false,
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			capture := kubectlCaptureShim(t)
			t.Setenv(PodIPEnvVar, "10.42.0.7")
			sched, err := schedulingFromEnv(envOf(map[string]string{PodReadyTimeoutEnvVar: "4m"}))
			if err != nil {
				t.Fatal(err)
			}
			d := &Driver{namespace: "ns", kubectl: "kubectl", logger: iterlog.New(iterlog.LevelInfo, io.Discard), sched: sched}
			prepared := &Prepared{spec: sandbox.Spec{Image: "img", User: "1000:1000"}, workspace: "/workspace"}
			if _, err := d.Start(context.Background(), prepared, row.info); err != nil {
				t.Fatalf("Start: %v", err)
			}
			doc := appliedNetworkPolicy(t, capture)
			rule := firstEgressRule(t, doc)
			ports, hasPorts := rule["ports"].([]any)
			if hasPorts != row.wantPorts {
				t.Fatalf("the applied policy's runner-pod rule carries ports = %v (%v), want %v", hasPorts, rule["ports"], row.wantPorts)
			}
			if row.wantPorts {
				if len(ports) != 2 || ports[0].(map[string]any)["port"] != float64(39117) || ports[1].(map[string]any)["port"] != float64(41234) {
					t.Fatalf("the applied port scope = %v, want exactly [39117 41234] (the proxy's and the run's listener's)", rule["ports"])
				}
				raw, _ := json.Marshal(doc)
				if strings.Contains(string(raw), "kube-dns") {
					t.Fatalf("the untrusted run's applied policy still admits kube-dns:\n%s", raw)
				}
			} else {
				raw, _ := json.Marshal(doc)
				if !strings.Contains(string(raw), "kube-dns") {
					t.Fatalf("the trusted run's applied policy dropped kube-dns:\n%s", raw)
				}
			}
		})
	}
}

// appliedNetworkPolicy returns the NetworkPolicy Start piped to
// `kubectl apply`.
func appliedNetworkPolicy(t *testing.T, capture string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(capture)
	if err != nil {
		t.Fatalf("nothing was applied: %v", err)
	}
	for _, doc := range strings.Split(string(raw), "\n---\n") {
		doc = strings.TrimSpace(doc)
		if doc == "" {
			continue
		}
		var obj map[string]any
		if err := json.Unmarshal([]byte(doc), &obj); err != nil {
			t.Fatalf("applied manifest is not JSON: %v\n%s", err, doc)
		}
		if obj["kind"] == "NetworkPolicy" {
			return obj
		}
	}
	t.Fatalf("no NetworkPolicy among the applied manifests:\n%s", raw)
	return nil
}
