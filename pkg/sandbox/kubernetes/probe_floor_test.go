package kubernetes

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/sandbox"
)

// WITNESSES (ADR-162 decision 7, U5) on the start probe: before the
// first spawn, the untrusted run's sandbox start PROVES the floor — a
// probe that ignores the proxy env dials the cluster API service, the
// credential's own host and the runner pod's metrics port directly, and
// every one of those dials must FAIL. A non-enforcing CNI (kind's
// kindnetd) applies the per-run policy as a no-op and only this probe
// sees it: the run is refused typed. A trusted run probes nothing.
//
// Mutants: Driver.Start's probe call dropped → the kindnetd row starts
// the pod; the metrics target dropped from the target list → the probe
// row's exec capture; the script's proxy-env unset dropped → same
// capture; the probe-argv targets not split per element → the builder
// row.

// probeFloorStart drives the REAL Driver.Start (kubectl capture shim,
// pod-ready wait satisfied) with the given RunInfo and the probe reply
// the shim should script.
func probeFloorStart(t *testing.T, info sandbox.RunInfo, execReply string) error {
	t.Helper()
	kubectlCaptureShim(t)
	if execReply != "" {
		t.Setenv("ITERION_TEST_KUBECTL_EXEC_REPLY", execReply)
	}
	t.Setenv(PodIPEnvVar, "10.42.0.7")
	sched, err := schedulingFromEnv(envOf(map[string]string{PodReadyTimeoutEnvVar: "4m"}))
	if err != nil {
		t.Fatal(err)
	}
	d := &Driver{namespace: "ns", kubectl: "kubectl", logger: iterlog.New(iterlog.LevelInfo, io.Discard), sched: sched}
	prepared := &Prepared{spec: sandbox.Spec{Image: "img", User: "1000:1000"}, workspace: "/workspace"}
	_, err = d.Start(context.Background(), prepared, info)
	return err
}

// probeExecCapture reads the argv the probe's kubectl exec carried.
func probeExecCapture(t *testing.T) string {
	t.Helper()
	capture := os.Getenv("ITERION_TEST_KUBECTL_CAPTURE")
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(capture), filepath.Base(capture)+".exec"))
	if err != nil {
		t.Fatalf("no probe exec was recorded: %v", err)
	}
	return string(raw)
}

// TestStartProbesTheUntrustedRunsFloorBeforeTheFirstSpawn: the probe
// runs inside Driver.Start for an untrusted run, its dial list names
// exactly the three roads the policy closed, and its client ignores the
// proxy env — the one variable the floor cannot trust.
func TestStartProbesTheUntrustedRunsFloorBeforeTheFirstSpawn(t *testing.T) {
	t.Setenv("ITERION_METRICS_PORT", "9091")
	info := sandbox.RunInfo{
		RunID: "01a0-run", ProxyEndpoint: "http://10.42.0.7:39117",
		UntrustedRun: true, RunnerEgressPorts: []int{39117},
		EgressAllowlistHosts: []string{"example.com"},
	}
	if err := probeFloorStart(t, info, ""); err != nil {
		t.Fatalf("Start with the floor holding: %v", err)
	}
	exec := probeExecCapture(t)
	for _, want := range []string{
		"kubernetes.default.svc:443", // the cluster API service
		"example.com:443",            // the credential's own host
		"10.42.0.7:9091",             // the runner pod's metrics port
		"unset HTTP_PROXY",           // the client ignores the proxy env
	} {
		if !strings.Contains(exec, want) {
			t.Fatalf("the probe's exec does not name %q:\n%s", want, exec)
		}
	}

	// Trusted control: no probe — the floor is the fork lane's question.
	if err := probeFloorStart(t, sandbox.RunInfo{RunID: "01a0-run", ProxyEndpoint: "http://10.42.0.7:39117"}, ""); err != nil {
		t.Fatalf("trusted Start: %v", err)
	}
	capture := os.Getenv("ITERION_TEST_KUBECTL_CAPTURE")
	if _, err := os.ReadFile(capture + ".exec"); err == nil {
		t.Fatal("a trusted run probed the network floor — the probe is the untrusted run's")
	}
}

// TestANonEnforcingCNIRefusesTheForkRunAtTheProbe: kindnetd applies the
// per-run policy as a no-op and only the probe sees it — a dial that
// connects refuses the run TYPED, naming the road that stayed open. The
// metrics port is the witness target: a port on the runner pod no
// untrusted policy names.
func TestANonEnforcingCNIRefusesTheForkRunAtTheProbe(t *testing.T) {
	t.Setenv("ITERION_METRICS_PORT", "9091")
	info := sandbox.RunInfo{
		RunID: "01a0-run", ProxyEndpoint: "http://10.42.0.7:39117",
		UntrustedRun: true, RunnerEgressPorts: []int{39117},
	}
	err := probeFloorStart(t, info, "CONNECTED 10.42.0.7:9091")
	var probe *sandbox.NetworkFloorProbeError
	if !errors.As(err, &probe) {
		t.Fatalf("a non-enforcing CNI's fork Start = %v, want the typed network-floor refusal", err)
	}
	if probe.Target != "10.42.0.7:9091" || probe.Unproven {
		t.Fatalf("the refusal names %q (unproven=%v), want the runner's metrics port 10.42.0.7:9091", probe.Target, probe.Unproven)
	}
}

// TestAnUnprovableFloorRefusesTheRun: an image with no dial-capable
// tool proves nothing, and for an untrusted run an unwitnessed floor is
// the same verdict as a broken one — typed, fail closed.
func TestAnUnprovableFloorRefusesTheRun(t *testing.T) {
	info := sandbox.RunInfo{
		RunID: "01a0-run", ProxyEndpoint: "http://10.42.0.7:39117",
		UntrustedRun: true, RunnerEgressPorts: []int{39117},
	}
	err := probeFloorStart(t, info, "NOPROBE")
	var probe *sandbox.NetworkFloorProbeError
	if !errors.As(err, &probe) || !probe.Unproven {
		t.Fatalf("an unprovable floor's fork Start = %v, want the typed unproven refusal", err)
	}
}

// TestTheProbeCarriesOneTargetPerArgvElement: the probe is `sh -c
// script name t1 t2 …` — the targets are ARGV, not one joined string
// the script's "$@" would iterate as a single road.
func TestTheProbeCarriesOneTargetPerArgvElement(t *testing.T) {
	targets := []string{"kubernetes.default.svc:443", "example.com:443", "10.42.0.7:9091"}
	cmd := buildNetworkFloorProbe(targets)
	if len(cmd) != 4+len(targets) || cmd[0] != "sh" || cmd[1] != "-c" {
		t.Fatalf("the probe command = %v, want sh -c <script> <name> <one target per element>", cmd)
	}
	for i, want := range targets {
		if cmd[4+i] != want {
			t.Fatalf("probe argv #%d = %q, want the target %q", 4+i, cmd[4+i], want)
		}
	}
}
