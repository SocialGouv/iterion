package ciguard

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// The ARC kind probe (#981): the half a harness can exercise is the
// CLASSIFICATION — given the facts a real pod answered with, which hypothesis
// do they support. The capture half needs a pod and is read in the dispatched
// run; what is held here is that each verdict is reached for its own reason
// and for no other, because a diagnosis that reads the wrong cause sends the
// next change at the wrong layer (this ticket has already paid for that once:
// the 2026-09-08 docker-group hypothesis, refuted by two sibling jobs).
const (
	kindProbeScript       = "../../scripts/ci/arc-kind-probe.sh"
	kindProbeWorkflowPath = "../../.github/workflows/arc-kind-probe.yml"
	kindProbeJob          = "kind-on-arc"
)

// classifyProbe runs the shipped script over a facts file and returns the
// verdict, the reason it gives and the exit status.
func classifyProbe(t *testing.T, facts string) (verdict, because string, code int) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "facts")
	if err := os.WriteFile(path, []byte(facts), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", kindProbeScript, "--classify", path)
	cmd.Env = scriptEnv(t)
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		exit, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("run %s: %v\nstderr: %s", kindProbeScript, err, stderr.String())
		}
		code = exit.ExitCode()
	}
	for _, line := range strings.Split(stdout.String(), "\n") {
		switch {
		case strings.HasPrefix(line, "verdict="):
			verdict = strings.TrimPrefix(line, "verdict=")
		case strings.HasPrefix(line, "because="):
			because = strings.TrimPrefix(line, "because=")
		}
	}
	return verdict, because, code
}

// usableFacts is a pod on which everything worked: each case below is this
// one with the single fact its hypothesis turns on changed, so a case cannot
// pass for a reason it does not name.
const usableFacts = `docker_reachable=yes
docker_server=28.0.1
kind_setup_outcome=success
proxy_HTTPS_PROXY=unset
no_proxy=-
server=127.0.0.1:38145
server_direct=403
server_via_env=403
node_ip=172.18.0.2
node_ip_direct=403
kubectl_cluster_info=ok
`

func withFact(t *testing.T, changes map[string]string) string {
	t.Helper()
	var out []string
	seen := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSpace(usableFacts), "\n") {
		key := strings.SplitN(line, "=", 2)[0]
		if v, ok := changes[key]; ok {
			seen[key] = true
			out = append(out, key+"="+v)
			continue
		}
		out = append(out, line)
	}
	for key := range changes {
		if !seen[key] {
			t.Fatalf("fact %q is not in the usable capture — a case that changes a fact nobody records proves nothing", key)
		}
	}
	return strings.Join(out, "\n") + "\n"
}

// TestTheKindProbeNamesTheHypothesisItsFactsSupport holds one rule per
// hypothesis #981 leaves open, each distinguished by the single fact that
// separates it from its neighbour.
//
// Mutations seen red, each on its own case (one line of
// scripts/ci/arc-kind-probe.sh per mutation):
//
//   - the proxy rule dropped: "the proxy swallows the loopback API server"
//     falls through to kubectl-refused and the next change goes looking at
//     certificates;
//   - `answered` counting 000 as an answer: "nothing answers" reads as usable;
//   - the proxy rule placed after the unreachable rule: the proxied case is
//     reported as an unreachable endpoint;
//   - the exit status decoupled from the verdict (`verdict` always true): a
//     dispatched probe reports green whatever it found.
func TestTheKindProbeNamesTheHypothesisItsFactsSupport(t *testing.T) {
	for _, tc := range []struct {
		name        string
		facts       string
		wantVerdict string
		wantBecause string
		wantCode    int
	}{
		{
			name:        "everything worked: the scale set can host the job",
			facts:       usableFacts,
			wantVerdict: "usable",
			wantBecause: "kubectl reached it",
			wantCode:    0,
		},
		{
			// The hypothesis this probe exists to settle: kubectl is a Go
			// client and honours HTTPS_PROXY; the docker CLI and the Mongo
			// driver do not, which is why the sibling jobs stayed green.
			name: "an egress proxy takes the loopback API server with it",
			facts: withFact(t, map[string]string{
				"server_via_env":       "000",
				"proxy_HTTPS_PROXY":    "set",
				"no_proxy":             "localhost,.svc,.cluster.local",
				"kubectl_cluster_info": "failed",
			}),
			wantVerdict: "proxied-apiserver",
			wantBecause: "NO_PROXY is 'localhost,.svc,.cluster.local'",
			wantCode:    1,
		},
		{
			name: "the endpoint kind wrote is not reachable from this container",
			facts: withFact(t, map[string]string{
				"server_direct":        "000",
				"server_via_env":       "000",
				"kubectl_cluster_info": "failed",
			}),
			wantVerdict: "kubeconfig-endpoint-unreachable",
			wantBecause: "while the node's in-network address answers",
			wantCode:    1,
		},
		{
			name: "nothing in the container reaches the cluster at all",
			facts: withFact(t, map[string]string{
				"server_direct":        "000",
				"server_via_env":       "000",
				"node_ip_direct":       "000",
				"kubectl_cluster_info": "failed",
			}),
			wantVerdict: "apiserver-unreachable",
			wantBecause: "nor the node's in-network address answers",
			wantCode:    1,
		},
		{
			// The ticket's own words — "kubectl cannot authenticate" — are a
			// verdict of their own, and only reachable facts support it.
			name: "the API server answers but kubectl cannot talk to it",
			facts: withFact(t, map[string]string{
				"kubectl_cluster_info": "failed",
			}),
			wantVerdict: "kubectl-refused",
			wantBecause: "the credentials or the TLS material",
			wantCode:    1,
		},
		{
			name: "kind never brought a cluster up",
			facts: withFact(t, map[string]string{
				"kind_setup_outcome":   "failure",
				"server":               "-",
				"server_direct":        "-",
				"server_via_env":       "-",
				"node_ip":              "-",
				"node_ip_direct":       "-",
				"kubectl_cluster_info": "failed",
			}),
			wantVerdict: "cluster-not-created",
			wantBecause: "not in reaching it",
			wantCode:    1,
		},
		{
			// A cluster that came up while the kubeconfig names nothing is
			// still a creation failure, not a reachability one: measured
			// separately because a green action step with an empty kubeconfig
			// is exactly how a probe lies.
			name: "the action reported success but wrote no endpoint",
			facts: withFact(t, map[string]string{
				"server":               "-",
				"server_direct":        "-",
				"server_via_env":       "-",
				"kubectl_cluster_info": "failed",
			}),
			wantVerdict: "cluster-not-created",
			wantCode:    1,
		},
		{
			// A capture that stopped early is not a diagnosis. Without this
			// rule the absent docker_reachable reads as "no" and the probe
			// answers docker-unreachable — a cause drawn from a fact nobody
			// recorded, which is how a refuted hypothesis comes back to life.
			name:        "a truncated capture supports no verdict at all",
			facts:       "kind_setup_outcome=success\nserver=127.0.0.1:38145\n",
			wantVerdict: "capture-incomplete",
			wantBecause: "cannot support any verdict",
			wantCode:    1,
		},
		{
			// The refuted 2026-09-08 hypothesis, measured rather than read:
			// if it ever becomes true again, the probe says so first.
			name: "the daemon itself is unreachable",
			facts: withFact(t, map[string]string{
				"docker_reachable": "no",
			}),
			wantVerdict: "docker-unreachable",
			wantBecause: "sibling jobs that do reach it",
			wantCode:    1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			verdict, because, code := classifyProbe(t, tc.facts)
			if verdict != tc.wantVerdict {
				t.Errorf("verdict %q, want %q (because: %s)", verdict, tc.wantVerdict, because)
			}
			if tc.wantBecause != "" && !strings.Contains(because, tc.wantBecause) {
				t.Errorf("the reason given is %q, which does not carry %q — the run's reader acts on this sentence", because, tc.wantBecause)
			}
			if code != tc.wantCode {
				t.Errorf("exit %d, want %d — the exit status is the headline of a dispatched probe: 0 means the scale set can host kind", code, tc.wantCode)
			}
		})
	}
}

// TestTheKindProbeMeasuresTheScaleSetAndNothingElse holds the three choices
// that make the probe's answer mean what it says. Each is a way a probe stays
// green while measuring the wrong thing, which is worse than no probe: it
// closes a question with a fact nobody checked.
func TestTheKindProbeMeasuresTheScaleSetAndNothingElse(t *testing.T) {
	src := mustRead(t, kindProbeWorkflowPath)
	var triggers struct {
		On map[string]any `yaml:"on"`
	}
	if err := yaml.Unmarshal(src, &triggers); err != nil {
		t.Fatalf("parse the triggers of %s: %v", kindProbeWorkflowPath, err)
	}
	if len(triggers.On) != 1 || !hasKey(triggers.On, "workflow_dispatch") {
		t.Errorf("%s triggers on %v — it carries a plain `arc-runners` label, which is only safe while `workflow_dispatch` is its ONLY trigger (no fork and no bot can fire it). An event-driven trigger owes the three-clause routing expression tests.yml carries, byte for byte", kindProbeWorkflowPath, keysOf(triggers.On))
	}

	var wf struct {
		Jobs map[string]struct {
			RunsOn string `yaml:"runs-on"`
			Steps  []struct {
				Name string `yaml:"name"`
				Uses string `yaml:"uses"`
				Run  string `yaml:"run"`
				If   string `yaml:"if"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(src, &wf); err != nil {
		t.Fatalf("parse %s: %v", kindProbeWorkflowPath, err)
	}
	job, ok := wf.Jobs[kindProbeJob]
	if !ok {
		t.Fatalf("%s defines no job %q", kindProbeWorkflowPath, kindProbeJob)
	}
	if job.RunsOn != "arc-runners" {
		t.Errorf("job %q runs on %q — a probe of the scale set that runs anywhere else measures the wrong machine, and the `CI_SELF_HOSTED` lever must NOT reach it: a lever that quietly moves this job to ubuntu-latest turns a meaningless green into an answer", kindProbeJob, job.RunsOn)
	}

	// The cluster must come up through the action cloud-e2e uses, pinned to
	// the same sha: a probe that installs kind its own way measures its own
	// path, not the failing one.
	e2e := string(mustRead(t, workflowPaths[0]))
	var kindAction, captured string
	for _, st := range job.Steps {
		if strings.HasPrefix(st.Uses, "helm/kind-action@") {
			kindAction = st.Uses
		}
		if strings.Contains(st.Run, "arc-kind-probe.sh") {
			captured = st.If
		}
	}
	if kindAction == "" {
		t.Errorf("job %q brings no kind cluster up through helm/kind-action — there is nothing to measure", kindProbeJob)
	} else if !strings.Contains(e2e, kindAction) {
		t.Errorf("job %q uses %q, which %s does not use: the probe must take cloud-e2e's own path, same action, same sha", kindProbeJob, kindAction, workflowPaths[0])
	}
	if captured == "" {
		t.Errorf("the capture step does not run the probe under `if: always()` — a cluster that fails to come up is one of the answers, and the step that names it must still run (got if: %q)", captured)
	} else if !strings.Contains(captured, "always()") {
		t.Errorf("the capture step runs under if: %q, want always(): a failed setup is an answer, not a reason to skip the capture", captured)
	}
}

func hasKey(m map[string]any, key string) bool {
	_, ok := m[key]
	return ok
}
