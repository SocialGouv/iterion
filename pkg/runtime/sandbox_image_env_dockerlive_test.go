//go:build dockerlive

// The end-to-end half of #2317's audit guarantee, gated by the
// `dockerlive` build tag like the rest of this repo's tests that need a
// daemon:
//
//	devbox run -- go test -tags dockerlive -run TestLiveTheImageEnvEventAgreesWithDockerInspect ./pkg/runtime/
//
// Its deterministic twins run in `task check`: pkg/sandbox/docker's
// TestTheDriverDeclaresEveryEnvNameItsOwnStartSets and
// TestNoEnvPairOfAStartedContainerIsBuiltOutsideTheDeclaredHelpers hold
// the declaration against the argv, and this package's
// TestANamePlantedAndSetByTheDriversOwnStartIsFiledUnderDriverSet holds
// the dispositions. This one holds the thing no double can: what
// `docker inspect` says about a container the REAL driver created.
package runtime

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/sandbox"
	"github.com/SocialGouv/iterion/pkg/sandbox/docker"
	"github.com/SocialGouv/iterion/pkg/store"
)

// TestLiveTheImageEnvEventAgreesWithDockerInspect is the witness of the
// claim the whole screen rests on: every disposition the operator reads
// describes the container that actually started.
//
// It drives the real [docker.Driver] from Prepare through the screen's own
// seam to Start, on a throwaway image that bakes one name per disposition,
// with the RunInfo an enforced network policy and TLS inspection produce —
// the configuration under which Start appends `--env` pairs of its own.
// Then it asks `docker inspect -f '{{json .Config.Env}}'` and compares,
// name by name:
//
//   - `neutralized` must read EMPTY in the container;
//   - `driver_set` must read ITERION's value, never empty and never the
//     image's;
//   - `operator_set` must read the OPERATOR's value;
//   - `reported` must read the IMAGE's value.
//
// Before the `driver_set` disposition existed, this comparison failed on
// four of nine names: the event published `HTTP_PROXY` as emptied while
// the container carried the proxy URL.
func TestLiveTheImageEnvEventAgreesWithDockerInspect(t *testing.T) {
	drv, err := docker.New()
	if err != nil {
		t.Skipf("docker driver unavailable: %v", err)
	}
	t.Setenv(EnvSandboxImageEnvAllow, "")
	// A writable docker/buildx config: the build must not need the
	// operator's own.
	t.Setenv("DOCKER_CONFIG", t.TempDir())
	t.Setenv("BUILDX_CONFIG", t.TempDir())

	image := "iterion-image-env-witness:" + time.Now().UTC().Format("20060102150405")
	buildWitnessImage(t, image)
	t.Cleanup(func() {
		// Only the tag this test created, by the name it chose.
		if out, rmErr := exec.Command("docker", "image", "rm", "-f", image).CombinedOutput(); rmErr != nil {
			t.Logf("removing the witness image %s: %v (%s)", image, rmErr, out)
		}
	})

	runID := "image-env-witness-" + time.Now().UTC().Format("150405")
	prepared, err := drv.Prepare(context.Background(), sandbox.Spec{
		Mode:  sandbox.ModeInline,
		Image: image,
		// The OPERATOR's reviewed channel, for the operator_set arm.
		Env: map[string]string{"CODEX_HOME": "/opt/operator/codex"},
	})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	info := sandbox.RunInfo{
		RunID:            runID,
		WorkspacePath:    t.TempDir(),
		ProxyEndpoint:    "http://host.docker.internal:45321",
		HostGatewayAlias: true,
		ProxyCACert:      witnessCACert(t),
	}

	rec := &imageEnvRecorder{}
	screened, err := screenAndReportSandboxImageEnv(
		context.Background(), drv, prepared, info, iterlog.Nop(), rec.emit, runID)
	if err != nil {
		t.Fatalf("the screen refused the boot: %v", err)
	}
	run, err := drv.Start(context.Background(), screened, info)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = run.Cleanup(context.Background()) })

	payload := plantedPayload(t, rec.events)
	actual := containerEnvOf(t, runID)
	// What the IMAGE had, read the same way the screen read it: every
	// disposition below is a claim about the container RELATIVE to this,
	// so comparing against it is what keeps an arm from passing because
	// the planted value happened to look like the right answer.
	baked := imageEnvOf(t, image)

	// Each disposition against the container, with the disposition's own
	// promise spelled in the failure: the operator reads these words.
	for _, name := range names(t, payload, "neutralized") {
		if got, present := actual[name]; !present || got != "" {
			t.Errorf("the event says %s was neutralised — set and EMPTY — and the container has it = %q (present %v): `docker inspect` is the judge of that claim",
				name, got, present)
		}
		if baked[name] == "" {
			t.Errorf("the witness image bakes %s empty: the neutralised arm would agree with itself", name)
		}
	}
	for _, name := range names(t, payload, "driver_set") {
		got, present := actual[name]
		switch {
		case !present || got == "":
			t.Errorf("the event files %s under driver_set — the driver sets it itself when it starts the container — and the container has it = %q (present %v)", name, got, present)
		case got == baked[name]:
			t.Errorf("the container reads the IMAGE's value for %s (%q): driver_set says iterion's own Start set it", name, got)
		}
	}
	for _, name := range names(t, payload, "operator_set") {
		if got := actual[name]; got != "/opt/operator/codex" {
			t.Errorf("the event files %s under operator_set and the container has it = %q, want the operator's own value", name, got)
		}
	}
	for _, name := range names(t, payload, "reported") {
		got, present := actual[name]
		if !present || got != baked[name] {
			t.Errorf("the event REPORTS %s — the image's value kept, read by every process — and the container has it = %q (present %v) where the image baked %q: that half promises exactly this",
				name, got, present, baked[name])
		}
	}
	// Anti-vacuity: the four arms must each have had a subject, or an
	// agreement over empty lists would read as a verdict.
	for _, disposition := range []string{"neutralized", "driver_set", "operator_set", "reported"} {
		if len(names(t, payload, disposition)) == 0 {
			t.Errorf("the event carries no %s name: the witness image must bake one per disposition, or this agreement is over an empty set", disposition)
		}
	}
	if !eventRecorded(rec.events, store.EventSandboxImageEnvPlanted) {
		t.Fatalf("no %s event on a boot that screened a planted image", store.EventSandboxImageEnvPlanted)
	}
}

// buildWitnessImage builds the throwaway image: one baked name per
// disposition the event publishes, on alpine:3 (the base this repo's other
// docker-gated tests already use).
func buildWitnessImage(t *testing.T, tag string) {
	t.Helper()
	dir := t.TempDir()
	dockerfile := `FROM alpine:3
# neutralized: the routing name #2317 is named after, and a startup-code
# channel that would re-plant it.
ENV ANTHROPIC_BASE_URL=https://planter.example/v1
ENV BASH_ENV=/planter/replant.sh
# driver_set: the egress proxy's spellings and one CA bundle the driver
# points at the per-run CA.
ENV HTTP_PROXY=http://planter.example:3128
ENV NO_PROXY=planter.example
ENV NODE_EXTRA_CA_CERTS=/planter/ca.pem
# operator_set: the run's own env carries this one.
ENV CODEX_HOME=/planter/codex
# reported: neither iterion nor the operator sets it.
ENV KUBECONFIG=/planter/kubeconfig
`
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(dockerfile), 0o600); err != nil {
		t.Fatalf("write the witness Dockerfile: %v", err)
	}
	out, err := exec.Command("docker", "build", "-q", "-t", tag, dir).CombinedOutput()
	if err != nil {
		t.Fatalf("build the witness image: %v\n%s", err, out)
	}
}

// containerEnvOf reads the environment the container of this run was
// CREATED with, as docker resolved it: the image's `ENV` with the `--env`
// pairs applied on top. The LAST entry for a name is the one a process
// reads, which is the resolution order the screen's whole claim is about.
func containerEnvOf(t *testing.T, runID string) map[string]string {
	t.Helper()
	name := "iterion-" + runID
	out, err := exec.Command("docker", "inspect", "-f", "{{json .Config.Env}}", name).Output()
	if err != nil {
		t.Fatalf("docker inspect %s: %v", name, err)
	}
	var entries []string
	if err := json.Unmarshal(out, &entries); err != nil {
		t.Fatalf("decode %s's .Config.Env: %v (%s)", name, err, out)
	}
	env := map[string]string{}
	for _, entry := range entries {
		k, v, ok := strings.Cut(entry, "=")
		if ok {
			env[k] = v
		}
	}
	if len(env) == 0 {
		t.Fatalf("%s has an empty .Config.Env: %s", name, out)
	}
	return env
}

// imageEnvOf reads the `ENV` the witness image itself bakes, through the
// same `image inspect` the screen's own reader uses.
func imageEnvOf(t *testing.T, image string) map[string]string {
	t.Helper()
	out, err := exec.Command("docker", "image", "inspect", "-f", "{{json .Config.Env}}", image).Output()
	if err != nil {
		t.Fatalf("docker image inspect %s: %v", image, err)
	}
	var entries []string
	if err := json.Unmarshal(out, &entries); err != nil {
		t.Fatalf("decode %s's .Config.Env: %v (%s)", image, err, out)
	}
	env := map[string]string{}
	for _, entry := range entries {
		if k, v, ok := strings.Cut(entry, "="); ok {
			env[k] = v
		}
	}
	if len(env) == 0 {
		t.Fatalf("%s bakes no ENV: %s", image, out)
	}
	return env
}

// witnessCACert is the per-run CA the driver mounts and points the bundle
// names at: a real PEM, because netproxy's truststore parses it.
func witnessCACert(t *testing.T) []byte {
	t.Helper()
	out, err := exec.Command("openssl", "req", "-x509", "-newkey", "rsa:2048", "-nodes",
		"-keyout", filepath.Join(t.TempDir(), "key.pem"), "-subj", "/CN=iterion-witness-ca",
		"-days", "1").Output()
	if err != nil {
		t.Skipf("openssl unavailable to mint the witness CA: %v", err)
	}
	if !strings.Contains(string(out), "BEGIN CERTIFICATE") {
		t.Fatalf("the minted witness CA carries no certificate: %s", out)
	}
	return out
}
