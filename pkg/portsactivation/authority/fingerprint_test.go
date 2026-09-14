package authority

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/portsactivation/natsconfig"
)

func observedFingerprintFixture(t *testing.T) (*Record, *DeploymentCorroboration,
	*WorkloadSnapshot, *RBACSnapshot) {
	t.Helper()
	record, source, static, workloads, rbac, brokerSecret, observations := deploymentCorroborationFixture(t)
	result, err := corroborateDeploymentEvidence(t.Context(), record, source, static, workloads, rbac,
		deploymentEvidenceReaders{
			credential: func(_ context.Context, namespace, name, key string) (*CredentialSecretKey, error) {
				return credentialReaderFixture(namespace, name, key), nil
			},
			brokerSource: func(context.Context, string, string) (*BrokerConfigSecret, error) {
				return brokerSecret, nil
			},
			system: func(_ context.Context, record *Record, static *StaticAnalysis) (*SystemCorroboration, error) {
				return CorroborateSystemObservations(record, static, observations)
			},
		})
	if err != nil {
		t.Fatal(err)
	}
	return record, result, workloads, rbac
}

func TestDeploymentObservationDigestBindsStableAuthorityAndIgnoresVolatileCounts(t *testing.T) {
	record, result, workloads, rbac := observedFingerprintFixture(t)
	baseline, err := fingerprintDeploymentObservation(record, result, workloads, rbac)
	if err != nil || len(baseline) != 64 {
		t.Fatalf("valid observation has no SHA-256 identity: %q %v", baseline, err)
	}
	slices.Reverse(record.Credentials)
	slices.Reverse(record.Holders)
	slices.Reverse(workloads.Workloads)
	slices.Reverse(rbac.Roles)
	slices.Reverse(rbac.Bindings)
	result.System.Brokers[0].ObservedConnections += 3
	result.StartedAt = time.Now().UTC()
	result.CompletedAt = result.StartedAt.Add(time.Second)
	reordered, err := fingerprintDeploymentObservation(record, result, workloads, rbac)
	if err != nil || reordered != baseline {
		t.Fatalf("equivalent ordered inventory or connection churn changed observation identity: %q %q %v", baseline, reordered, err)
	}

	for _, tc := range []struct {
		name   string
		mutate func(*Record, *DeploymentCorroboration, *WorkloadSnapshot, *RBACSnapshot)
	}{
		{"holder build", func(record *Record, _ *DeploymentCorroboration, _ *WorkloadSnapshot, _ *RBACSnapshot) {
			record.Holders[0].BuildDigest = strings.Repeat("f", 64)
		}},
		{"build approval", func(record *Record, _ *DeploymentCorroboration, _ *WorkloadSnapshot, _ *RBACSnapshot) {
			record.BuildApprovals[0].CapabilityDigest = strings.Repeat("f", 64)
		}},
		{"workload revision", func(_ *Record, _ *DeploymentCorroboration, workloads *WorkloadSnapshot, _ *RBACSnapshot) {
			workloads.Workloads[0].ResourceVersion = "new-workload-revision"
		}},
		{"RBAC rule", func(_ *Record, _ *DeploymentCorroboration, _ *WorkloadSnapshot, rbac *RBACSnapshot) {
			rbac.Roles[0].Rules[0].Verbs = append(rbac.Roles[0].Rules[0].Verbs, "delete")
		}},
		{"RBAC revision", func(_ *Record, _ *DeploymentCorroboration, _ *WorkloadSnapshot, rbac *RBACSnapshot) {
			rbac.Bindings[0].ResourceVersion = "new-rbac-revision"
		}},
		{"static broker config", func(_ *Record, result *DeploymentCorroboration, _ *WorkloadSnapshot, _ *RBACSnapshot) {
			result.Static.Brokers[0].ConfigDigest = "sha256:" + strings.Repeat("f", 64)
		}},
		{"system broker config", func(_ *Record, result *DeploymentCorroboration, _ *WorkloadSnapshot, _ *RBACSnapshot) {
			result.System.Brokers[0].ConfigDigest = "sha256:" + strings.Repeat("f", 64)
		}},
		{"credential Secret", func(_ *Record, result *DeploymentCorroboration, _ *WorkloadSnapshot, _ *RBACSnapshot) {
			result.Credentials.Bindings[0].ResourceVersion = "new-secret-revision"
		}},
		{"authority Secret", func(_ *Record, result *DeploymentCorroboration, _ *WorkloadSnapshot, _ *RBACSnapshot) {
			result.AuthoritySecretRevision = "new-authority-revision"
		}},
		{"queue", func(record *Record, result *DeploymentCorroboration, _ *WorkloadSnapshot, _ *RBACSnapshot) {
			// A changed queue must first have a valid replacement topology. The
			// fingerprint refuses a mismatched record/result scope outright.
			result.Queue = natsconfig.QueueTopology{}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			record, result, workloads, rbac := observedFingerprintFixture(t)
			tc.mutate(record, result, workloads, rbac)
			changed, err := fingerprintDeploymentObservation(record, result, workloads, rbac)
			if err == nil && changed == baseline {
				t.Fatal("changed authority evidence retained the same observation identity")
			}
		})
	}
}
