package authority

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/portsactivation/natsconfig"
)

func deploymentCorroborationFixture(t *testing.T) (*Record, SecretSource, *StaticAnalysis,
	*WorkloadSnapshot, *RBACSnapshot, *BrokerConfigSecret, []natsconfig.SystemObservation) {
	t.Helper()
	record, workloads := reconciledFixture(t)
	_, brokerWorkloads, brokerSecret := brokerConfigSecretFixture(t)
	workloads.Workloads = append(workloads.Workloads, brokerWorkloads.Workloads...)
	_, source, rbac := rbacBoundaryFixture(t)
	static, err := analyzeStaticWithParser(t.Context(), record,
		func(context.Context, natsconfig.Sources) (*natsconfig.Result, error) {
			return parsedStaticFixture(false), nil
		})
	if err != nil {
		t.Fatal(err)
	}
	_, _, observations := systemCorroborationFixture(t)
	return record, source, static, workloads, rbac, brokerSecret, observations
}

func TestDeploymentObservationRequiresEveryCorroboratingLayer(t *testing.T) {
	record, source, static, workloads, rbac, brokerSecret, observations := deploymentCorroborationFixture(t)
	readers := deploymentEvidenceReaders{
		credential: func(_ context.Context, namespace, name, key string) (*CredentialSecretKey, error) {
			return credentialReaderFixture(namespace, name, key), nil
		},
		brokerSource: func(_ context.Context, _, _ string) (*BrokerConfigSecret, error) {
			return brokerSecret, nil
		},
		system: func(_ context.Context, record *Record, static *StaticAnalysis) (*SystemCorroboration, error) {
			return CorroborateSystemObservations(record, static, observations)
		},
	}
	result, err := corroborateDeploymentEvidence(t.Context(), record, source, static, workloads, rbac, readers)
	if err != nil || result.AuthoritySecretUID != source.UID || result.Epoch != record.Epoch ||
		len(result.Builds.Bindings) != 1 ||
		len(result.BrokerLaunches) != 1 || len(result.BrokerSources) != 1 ||
		len(result.Credentials.Bindings) != 2 || len(result.Workloads.Bindings) != 4 ||
		len(result.System.Brokers) != 1 || len(result.RBAC.WorkerServiceAccounts) != 1 {
		t.Fatalf("complete read-only deployment observation was rejected: %+v %v", result, err)
	}
	encoded, err := json.Marshal(result)
	if err != nil || strings.Contains(string(encoded), "private-password") || strings.Contains(string(encoded), "super-secret") {
		t.Fatal("deployment observation exposed credential or NATS source material")
	}
	brokerSecret.data["main.conf"] = []byte("changed")
	if _, err := corroborateDeploymentEvidence(t.Context(), record, source, static, workloads, rbac, readers); err == nil {
		t.Fatal("deployment observation ignored a changed broker source Secret")
	}
	brokerSecret.data["main.conf"] = []byte(record.Brokers[0].sources.Files["main.conf"])
	rbac.Bindings[0].Subjects[0].Name = "unknown-writer"
	if _, err := corroborateDeploymentEvidence(t.Context(), record, source, static, workloads, rbac, readers); err == nil {
		t.Fatal("deployment observation ignored an unapproved authority writer")
	}
	rbac.Bindings[0].Subjects[0].Name = "deploy-operator"
	observations[0].Connections[0].User = "unknown"
	if _, err := corroborateDeploymentEvidence(t.Context(), record, source, static, workloads, rbac, readers); err == nil {
		t.Fatal("deployment observation ignored an unknown NATS connection")
	}
	observations[0].Connections[0].User = "sys"
	readers.credential = func(_ context.Context, namespace, name, key string) (*CredentialSecretKey, error) {
		secret := credentialReaderFixture(namespace, name, key)
		if namespace == "worker" {
			secret.value = []byte("nats://unknown:private-password@nats.example:4222")
		}
		return secret, nil
	}
	if _, err := corroborateDeploymentEvidence(t.Context(), record, source, static, workloads, rbac, readers); err == nil {
		t.Fatal("deployment observation ignored a credential principal mismatch")
	}
	readers.credential = func(_ context.Context, namespace, name, key string) (*CredentialSecretKey, error) {
		return credentialReaderFixture(namespace, name, key), nil
	}
	workloads.Workloads = append(workloads.Workloads,
		heldWorkload(t, record.Holders[0], "Deployment", "old-runner", "uid-old"))
	if _, err := corroborateDeploymentEvidence(t.Context(), record, source, static, workloads, rbac, readers); err == nil {
		t.Fatal("deployment observation ignored an old dormant workload")
	}
	workloads.Workloads = workloads.Workloads[:len(workloads.Workloads)-1]
	brokerPod := &workloads.Workloads[len(workloads.Workloads)-1]
	brokerPod.status = []byte(`{"phase":"Pending"}`)
	if _, err := corroborateDeploymentEvidence(t.Context(), record, source, static, workloads, rbac, readers); err == nil {
		t.Fatal("deployment observation ignored an unready broker Pod")
	}
}
