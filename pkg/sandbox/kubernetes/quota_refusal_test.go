package kubernetes

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/sandbox"
)

// kubectlQuotaShim puts a fake kubectl first on PATH whose applies read the
// piped manifest and fail with $ITERION_TEST_REFUSAL on stderr when the
// manifest's kind matches $ITERION_TEST_REFUSE_KIND — kubectl's refusal
// shape, without a cluster. Every other apply succeeds and every `delete`
// argv is appended to the returned file, so a test can assert the rollback
// ran.
func kubectlQuotaShim(t *testing.T) (deletes string) {
	t.Helper()
	isolateExecEnvironment(t)
	dir := t.TempDir()
	deletes = filepath.Join(dir, "deletes.log")
	script := `#!/bin/sh
case "$*" in
  *" apply "*)
    manifest=$(cat)
    case "$manifest" in
      *'"kind": "'"$ITERION_TEST_REFUSE_KIND"'"'*)
        printf '%s\n' "$ITERION_TEST_REFUSAL" >&2
        exit 1 ;;
    esac
    exit 0 ;;
  *" delete "*) echo "$*" >> "$ITERION_TEST_KUBECTL_DELETES"; exit 0 ;;
esac
exit 0
`
	if err := os.WriteFile(filepath.Join(dir, "kubectl"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("ITERION_TEST_KUBECTL_DELETES", deletes)
	return deletes
}

// quotaRefusalText is the production refusal iterion.cloud logged on
// 2026-11-18: the ResourceQuota admission plugin's wording, carrying the
// quota name and the requested/used/limited amounts an operator acts on.
const quotaRefusalText = `Error from server (Forbidden): error when creating "STDIN": pods "iterion-run-01a0-run" is forbidden: exceeded quota: iterion-quota, requested: requests.memory=4Gi, used: requests.memory=126992Mi, limited: requests.memory=128Gi`

// quotaDriver builds a Driver over the shim with the default policy, the
// same construction the other Start tests use.
func quotaDriver(t *testing.T) *Driver {
	t.Helper()
	sched, err := schedulingFromEnv(envOf(nil))
	if err != nil {
		t.Fatal(err)
	}
	return &Driver{namespace: "ns", kubectl: "kubectl", logger: iterlog.New(iterlog.LevelInfo, io.Discard), sched: sched}
}

// The production defect: a pod the namespace ResourceQuota refuses at
// admission is the cluster saying it had no room — the same evidence the
// unplaced-pod path classifies — so Start's error must carry
// sandbox.ErrCapacity (failed_resumable + SANDBOX_CAPACITY, delayed NAK)
// with kubectl's message intact, and must not be mistaken for a phase
// timeout.
func TestStart_QuotaRefusedPodCarriesTheCapacitySentinel(t *testing.T) {
	kubectlQuotaShim(t)
	t.Setenv("ITERION_TEST_REFUSE_KIND", "Pod")
	t.Setenv("ITERION_TEST_REFUSAL", quotaRefusalText)
	d := quotaDriver(t)
	prepared := &Prepared{spec: sandbox.Spec{Image: "img", User: "1000:1000"}, workspace: "/workspace"}
	_, startErr := d.Start(context.Background(), prepared, sandbox.RunInfo{RunID: "01a0-run"})
	if startErr == nil {
		t.Fatal("Start must fail when the namespace quota refuses the pod")
	}
	if !errors.Is(startErr, sandbox.ErrCapacity) {
		t.Fatalf("a quota-refused pod must carry sandbox.ErrCapacity, got: %v", startErr)
	}
	if errors.Is(startErr, sandbox.ErrPhaseTimeout) {
		t.Fatalf("a quota refusal must not carry sandbox.ErrPhaseTimeout, got: %v", startErr)
	}
	for _, want := range []string{"iterion-quota", "requested: requests.memory=4Gi", "limited: requests.memory=128Gi"} {
		if !strings.Contains(startErr.Error(), want) {
			t.Fatalf("the error must keep kubectl's evidence %q, got: %v", want, startErr)
		}
	}
}

// A Forbidden that no later attempt changes — RBAC on the create, a quota
// demanding fields the manifest does not set — is not "no room": carrying
// ErrCapacity would send every redelivery into the same refusal.
func TestStart_ForbiddenPodWithoutQuotaEvidenceStaysTerminal(t *testing.T) {
	cases := []struct{ name, refusal string }{
		{"rbac", `Error from server (Forbidden): error when creating "STDIN": pods "iterion-run-01a0-run" is forbidden: User "system:serviceaccount:iterion:iterion-runner" cannot create resource "pods" in API group "" in the namespace "iterion"`},
		{"scoped quota", `Error from server (Forbidden): error when creating "STDIN": pods "iterion-run-01a0-run" is forbidden: failed quota: iterion-quota: must specify requests.cpu,requests.memory`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			kubectlQuotaShim(t)
			t.Setenv("ITERION_TEST_REFUSE_KIND", "Pod")
			t.Setenv("ITERION_TEST_REFUSAL", tc.refusal)
			d := quotaDriver(t)
			prepared := &Prepared{spec: sandbox.Spec{Image: "img", User: "1000:1000"}, workspace: "/workspace"}
			_, startErr := d.Start(context.Background(), prepared, sandbox.RunInfo{RunID: "01a0-run"})
			if startErr == nil {
				t.Fatal("Start must fail on a refused pod")
			}
			if errors.Is(startErr, sandbox.ErrCapacity) {
				t.Fatalf("a %s refusal must stay terminal (no sandbox.ErrCapacity), got: %v", tc.name, startErr)
			}
		})
	}
}

// The refused pod's per-run Secrets are already created when the pod apply
// fails: the rollback must still run on the new classification — a run
// parked resumable must not leak its plaintext-credential Secret until the
// redelivery replaces it.
func TestStart_QuotaRefusedPodStillDeletesItsSecret(t *testing.T) {
	deletes := kubectlQuotaShim(t)
	t.Setenv("ITERION_TEST_REFUSE_KIND", "Pod")
	t.Setenv("ITERION_TEST_REFUSAL", quotaRefusalText)
	d := quotaDriver(t)
	prepared := &Prepared{spec: sandbox.Spec{
		Image:       "img",
		User:        "1000:1000",
		SecretFiles: []sandbox.SecretFileMount{{Name: "forge_token", MountPath: "/run/iterion/secrets/forge_token", Value: []byte("token-v1")}},
	}, workspace: "/workspace"}
	_, startErr := d.Start(context.Background(), prepared, sandbox.RunInfo{RunID: "01a0-run"})
	if !errors.Is(startErr, sandbox.ErrCapacity) {
		t.Fatalf("a quota-refused pod must carry sandbox.ErrCapacity, got: %v", startErr)
	}
	raw, err := os.ReadFile(deletes)
	if err != nil {
		t.Fatalf("Start issued no kubectl delete: %v", err)
	}
	if !strings.Contains(string(raw), "delete secret iterion-run-01a0-run-secret-files") {
		t.Fatalf("the refused pod's file-secrets Secret must still be deleted, deletes:\n%s", raw)
	}
}

// The other Start applies are per-run resources under the same namespace
// quota (an object-count quota on secrets hits them first). They share the
// classification through one helper, witnessed here on the file-secrets
// Secret — the apply Start issues before the pod.
func TestStart_QuotaRefusedSecretCarriesTheCapacitySentinel(t *testing.T) {
	kubectlQuotaShim(t)
	t.Setenv("ITERION_TEST_REFUSE_KIND", "Secret")
	t.Setenv("ITERION_TEST_REFUSAL", `Error from server (Forbidden): error when creating "STDIN": secrets "iterion-run-01a0-run-secret-files" is forbidden: exceeded quota: obj-quota, requested: count/secrets=1, used: count/secrets=50, limited: count/secrets=50`)
	d := quotaDriver(t)
	prepared := &Prepared{spec: sandbox.Spec{
		Image:       "img",
		User:        "1000:1000",
		SecretFiles: []sandbox.SecretFileMount{{Name: "forge_token", MountPath: "/run/iterion/secrets/forge_token", Value: []byte("token-v1")}},
	}, workspace: "/workspace"}
	_, startErr := d.Start(context.Background(), prepared, sandbox.RunInfo{RunID: "01a0-run"})
	if startErr == nil {
		t.Fatal("Start must fail when the namespace quota refuses the Secret")
	}
	if !errors.Is(startErr, sandbox.ErrCapacity) {
		t.Fatalf("a quota-refused per-run Secret at start must carry sandbox.ErrCapacity, got: %v", startErr)
	}
}

// RefreshSecretFile runs MID-run: the sandbox is placed and possibly half
// through its work, so no error of its own may ever carry
// sandbox.ErrCapacity — the placement-failure lane would resume a run that
// already executed.
func TestRefreshSecretFile_QuotaTextIsNotAPlacementFailure(t *testing.T) {
	kubectlQuotaShim(t)
	t.Setenv("ITERION_TEST_REFUSE_KIND", "Secret")
	t.Setenv("ITERION_TEST_REFUSAL", `Error from server (Forbidden): error when creating "STDIN": secrets "iterion-run-01a0-run-secret-files" is forbidden: exceeded quota: obj-quota, requested: count/secrets=1, used: count/secrets=50, limited: count/secrets=50`)
	r := &Run{
		driver:                &Driver{kubectl: "kubectl", logger: iterlog.Nop()},
		namespace:             "ns",
		secretFilesSecretName: "iterion-run-01a0-run-secret-files",
		secretFiles:           []sandbox.SecretFileMount{{Name: "forge_token", MountPath: "/run/iterion/secrets/forge_token", Value: []byte("token-v1")}},
	}
	err := r.RefreshSecretFile(context.Background(), "forge_token", []byte("token-v2"))
	if err == nil {
		t.Fatal("RefreshSecretFile must fail when the apply is refused")
	}
	if errors.Is(err, sandbox.ErrCapacity) {
		t.Fatalf("a mid-run refresh refusal must never carry sandbox.ErrCapacity, got: %v", err)
	}
}

// A phase-timeout error is built from the same apply and can quote an
// earlier failure, but its text is the phase helper's own shape: it must
// pass through quotaRefusal unchanged, and a nil error stays nil.
func TestQuotaRefusal_PhaseTimeoutTextIsNotQuotaEvidence(t *testing.T) {
	phaseTimeout := fmt.Errorf("sandbox: workspace copy phase timed out after 2m0s (deadline 2m0s exceeded): %w",
		errors.Join(sandbox.ErrPhaseTimeout, context.DeadlineExceeded, fmt.Errorf("kubectl apply: exit status 1\noutput: lost")))
	got := quotaRefusal(phaseTimeout)
	if got.Error() != phaseTimeout.Error() || errors.Is(got, sandbox.ErrCapacity) {
		t.Fatalf("a phase-timeout error must pass through unchanged and carry no capacity sentinel, got: %v", got)
	}
	if quotaRefusal(nil) != nil {
		t.Fatal("quotaRefusal(nil) must stay nil")
	}
}
