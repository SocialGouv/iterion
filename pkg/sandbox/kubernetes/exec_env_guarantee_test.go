//go:build unix

package kubernetes

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/sandbox"
)

// Kubernetes transports workload Env as framed data. This native witness
// executes Run.Exec and checks that the client retains its ambient
// configuration while the target receives the explicit workload values. TestExecCmdPreservesDriverEnvironment in sandbox separately proves
// preservation of a non-nil driver environment at the common ExecCmd seam.
func TestExecPreservesDriverEnvironment(t *testing.T) {
	isolateTransportConfig(t)
	hostHelper, guestHelper := nativeExecTransport(t)
	t.Setenv(execTransportPrefix+"HOST_HELPER", hostHelper)
	t.Setenv(execTransportPrefix+"GUEST_HELPER", guestHelper)
	cfg, configErr := loadExecTransportConfig()
	if configErr != nil {
		t.Fatal(configErr)
	}
	witness := filepath.Join(t.TempDir(), "client-env")
	t.Setenv("CLIENT_ENV_WITNESS", witness)
	isolateExecEnvironment(t)
	const workload = "ITERION_TEST_KUBE_WORKLOAD"
	t.Setenv(workload, "")
	if err := os.Unsetenv(workload); err != nil {
		t.Fatal(err)
	}
	host := filepath.Join(t.TempDir(), "client-config")
	t.Setenv("KUBECONFIG", host)
	fake := filepath.Join(t.TempDir(), "kubectl")
	script := `#!/bin/sh
set -eu
[ -z "${ITERION_TEST_KUBE_WORKLOAD+x}" ] || exit 41
printf '%s' "$KUBECONFIG" > "$CLIENT_ENV_WITNESS"
while [ "$#" -gt 0 ] && [ "$1" != -- ]; do shift; done
[ "$#" -gt 0 ] || exit 42
shift
exec "$@"
`
	if err := os.WriteFile(fake, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	r := testRun()
	r.driver.kubectl = Kubectl(fake)
	r.driver.transport = cfg
	nonce := filepath.Base(t.TempDir())
	opts := sandbox.ExecOpts{Env: map[string]string{workload: nonce, "KUBECONFIG": "workload-config"}}
	result, err := r.Exec(context.Background(), []string{"sh", "-c", "printf 'workload=%s|%s' \"$" + workload + "\" \"$KUBECONFIG\""}, opts)
	if err != nil || result.ExitCode != 0 {
		t.Fatalf("fake kubectl: %+v %v", result, err)
	}
	gotHost, readErr := os.ReadFile(witness)
	if readErr != nil || string(gotHost) != host {
		t.Fatalf("host environment lost: %q %v", gotHost, readErr)
	}
	want := "workload=" + nonce + "|workload-config"
	if strings.TrimSpace(string(result.Stdout)) != want {
		t.Fatalf("TRANSPORT_LOST: got %q, want %q", result.Stdout, want)
	}
}
