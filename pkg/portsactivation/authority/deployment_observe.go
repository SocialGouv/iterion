package authority

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/SocialGouv/iterion/pkg/portsactivation/natsconfig"
	natsclient "github.com/nats-io/nats.go"
)

// DeploymentCorroboration contains no Secret values or NATS configuration
// source bytes. It is a bounded read-only observation, not a verified
// activation snapshot: compatible build exclusion, non-RBAC authorizers,
// credential issuers and effective queue topology still need verification.
type DeploymentCorroboration struct {
	StartedAt               time.Time                 `json:"started_at"`
	CompletedAt             time.Time                 `json:"completed_at"`
	AuthoritySecretUID      string                    `json:"authority_secret_uid"`
	AuthoritySecretRevision string                    `json:"authority_secret_revision"`
	ObservationDigest       string                    `json:"observation_digest"`
	DeploymentRevision      string                    `json:"deployment_revision"`
	Epoch                   uint64                    `json:"epoch"`
	Queue                   natsconfig.QueueTopology  `json:"queue"`
	Static                  *StaticAnalysis           `json:"static"`
	Builds                  *BuildCorroboration       `json:"builds"`
	System                  *SystemCorroboration      `json:"system"`
	QueueClient             *ObservedClientBinding    `json:"queue_client,omitempty"`
	Workloads               *WorkloadReconciliation   `json:"workloads"`
	BrokerLaunches          []BrokerLaunch            `json:"broker_launches"`
	BrokerSources           []BrokerConfigBinding     `json:"broker_sources"`
	Credentials             *CredentialReconciliation `json:"credentials"`
	RBAC                    *RBACBoundary             `json:"rbac"`
	Census                  *CensusCorroboration      `json:"census,omitempty"`
}

type deploymentEvidenceReaders struct {
	credential   func(context.Context, string, string, string) (*CredentialSecretKey, error)
	brokerSource func(context.Context, string, string) (*BrokerConfigSecret, error)
	system       func(context.Context, *Record, *StaticAnalysis) (*SystemCorroboration, error)
	queueClient  *natsclient.Conn
	census       func(context.Context, *Record, time.Time) (*CensusCorroboration, error)
}

// ObserveDeployment performs the privileged read-only portion of a
// distributed probe with server-owned configuration. No result from this
// function can enable native admission. The caller must still verify
// compatibility/exclusion and persist a restricted structured proof.
func ObserveDeployment(ctx context.Context, kubectlBinary, kubeContext, authorityRef string,
	namespaces []string, expectedQueue natsconfig.QueueTopology, systemURL string) (*DeploymentCorroboration, error) {
	return observeDeployment(ctx, kubectlBinary, kubeContext, authorityRef,
		namespaces, expectedQueue, systemURL, nil, nil)
}

// ObserveDeploymentWithQueue additionally binds the caller's ordinary NATS
// connection to its broker-assigned client ID, account and principal. It is
// still read-only corroboration and cannot authorize a native launch.
func ObserveDeploymentWithQueue(ctx context.Context, kubectlBinary, kubeContext, authorityRef string,
	namespaces []string, expectedQueue natsconfig.QueueTopology, systemURL string,
	queueClient *natsclient.Conn) (*DeploymentCorroboration, error) {
	if queueClient == nil {
		return nil, fmt.Errorf("distributed authority needs an ordinary NATS queue connection")
	}
	return observeDeployment(ctx, kubectlBinary, kubeContext, authorityRef,
		namespaces, expectedQueue, systemURL, queueClient, nil)
}

// ObserveDeploymentWithQueueAndCensus is the server-only variant used by the
// production authority adapter. The callback reads the ordinary connection's
// capability snapshot and validates it against the already corroborated
// record, store identity and runner epoch. Keeping the callback outside this
// package avoids giving the authority observer a second NATS connection or
// access to queue credentials.
func ObserveDeploymentWithQueueAndCensus(ctx context.Context, kubectlBinary, kubeContext, authorityRef string,
	namespaces []string, expectedQueue natsconfig.QueueTopology, systemURL string,
	queueClient *natsclient.Conn,
	census func(context.Context, *Record, time.Time) (*CensusCorroboration, error)) (*DeploymentCorroboration, error) {
	if queueClient == nil {
		return nil, fmt.Errorf("distributed authority needs an ordinary NATS queue connection")
	}
	return observeDeployment(ctx, kubectlBinary, kubeContext, authorityRef,
		namespaces, expectedQueue, systemURL, queueClient, census)
}

func observeDeployment(ctx context.Context, kubectlBinary, kubeContext, authorityRef string,
	namespaces []string, expectedQueue natsconfig.QueueTopology, systemURL string,
	queueClient *natsclient.Conn,
	census func(context.Context, *Record, time.Time) (*CensusCorroboration, error)) (*DeploymentCorroboration, error) {
	started := time.Now().UTC()
	probeCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	record, source, err := ReadDeploymentRecord(probeCtx, kubectlBinary, kubeContext,
		authorityRef, namespaces, expectedQueue)
	if err != nil {
		return nil, err
	}
	var static *StaticAnalysis
	var workloads *WorkloadSnapshot
	var rbac *RBACSnapshot
	var sourceErrors [3]error
	var reads sync.WaitGroup
	reads.Add(3)
	go func() {
		defer reads.Done()
		static, sourceErrors[0] = AnalyzeStatic(probeCtx, record)
	}()
	go func() {
		defer reads.Done()
		workloads, sourceErrors[1] = ReadWorkloadSnapshot(probeCtx, kubectlBinary, kubeContext, namespaces)
	}()
	go func() {
		defer reads.Done()
		rbac, sourceErrors[2] = ReadRBACSnapshot(probeCtx, kubectlBinary, kubeContext, namespaces)
	}()
	reads.Wait()
	if err := probeCtx.Err(); err != nil {
		return nil, err
	}
	for _, err := range sourceErrors {
		if err != nil {
			return nil, err
		}
	}
	result, err := corroborateDeploymentEvidence(probeCtx, record, *source, static, workloads, rbac,
		deploymentEvidenceReaders{
			queueClient: queueClient,
			credential: func(ctx context.Context, namespace, name, key string) (*CredentialSecretKey, error) {
				return ReadCredentialSecretKey(ctx, kubectlBinary, kubeContext, namespace, name, key)
			},
			brokerSource: func(ctx context.Context, namespace, name string) (*BrokerConfigSecret, error) {
				return ReadBrokerConfigSecret(ctx, kubectlBinary, kubeContext, namespace, name)
			},
			system: func(ctx context.Context, record *Record, static *StaticAnalysis) (*SystemCorroboration, error) {
				connection, err := DialSystem(ctx, systemURL)
				if err != nil {
					return nil, err
				}
				defer connection.Close()
				return ObserveAndCorroborateSystem(ctx, connection, record, static)
			},
			census: census,
		})
	if err != nil {
		return nil, err
	}
	parts := strings.Split(authorityRef, "/") // validated by ReadDeploymentRecord
	latest, err := ReadAuthoritySecret(probeCtx, kubectlBinary, kubeContext, parts[0], parts[1])
	if err != nil || latest.UID != source.UID || latest.ResourceVersion != source.ResourceVersion ||
		!bytes.Equal(latest.Material(), source.Material()) {
		return nil, fmt.Errorf("distributed authority Secret changed during observation")
	}
	if err := probeCtx.Err(); err != nil {
		return nil, err
	}
	result.ObservationDigest, err = fingerprintDeploymentObservation(record, result, workloads, rbac)
	if err != nil {
		return nil, err
	}
	result.StartedAt = started
	result.CompletedAt = time.Now().UTC()
	return result, nil
}

func corroborateDeploymentEvidence(ctx context.Context, record *Record, source SecretSource,
	static *StaticAnalysis, snapshot *WorkloadSnapshot, rbac *RBACSnapshot,
	readers deploymentEvidenceReaders) (*DeploymentCorroboration, error) {
	if record == nil || record.validate() != nil || source.UID == "" || source.ResourceVersion == "" ||
		static == nil || snapshot == nil || rbac == nil || readers.credential == nil ||
		readers.brokerSource == nil || readers.system == nil {
		return nil, fmt.Errorf("distributed authority observation lacks a required evidence source")
	}
	workloads, err := ReconcileWorkloads(record, snapshot)
	if err != nil {
		return nil, err
	}
	builds, err := CorroborateProtectedBuilds(record, static)
	if err != nil {
		return nil, err
	}
	launches, err := ReconcileBrokerLaunch(record, snapshot)
	if err != nil {
		return nil, err
	}
	boundary, err := AnalyzeRBACBoundary(record, source, rbac)
	if err != nil {
		return nil, err
	}
	var credentials *CredentialReconciliation
	var brokerSources []BrokerConfigBinding
	var system *SystemCorroboration
	var evidenceErrors [3]error
	var checks sync.WaitGroup
	checks.Add(3)
	go func() {
		defer checks.Done()
		credentials, evidenceErrors[0] = reconcileCredentialSecretsWithReader(ctx, record, readers.credential)
	}()
	go func() {
		defer checks.Done()
		brokerSources, evidenceErrors[1] = reconcileBrokerConfigSecretsWithReader(ctx, record, snapshot, readers.brokerSource)
	}()
	go func() {
		defer checks.Done()
		system, evidenceErrors[2] = readers.system(ctx, record, static)
	}()
	checks.Wait()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	for _, err := range evidenceErrors {
		if err != nil {
			return nil, err
		}
	}
	var queueBinding *ObservedClientBinding
	if readers.queueClient != nil {
		queueBinding, err = BindQueueConnection(record, system, readers.queueClient)
		if err != nil {
			return nil, err
		}
	}
	var census *CensusCorroboration
	if readers.census != nil {
		census, err = readers.census(ctx, record, time.Now().UTC())
		if err != nil {
			return nil, err
		}
		if census == nil {
			return nil, fmt.Errorf("distributed capability census returned no corroboration")
		}
	}
	return &DeploymentCorroboration{AuthoritySecretUID: source.UID,
		AuthoritySecretRevision: source.ResourceVersion, DeploymentRevision: record.DeploymentRevision,
		Epoch: record.Epoch, Queue: record.Queue, Static: static, Builds: builds, System: system,
		QueueClient: queueBinding, Workloads: workloads,
		BrokerLaunches: launches, BrokerSources: brokerSources, Credentials: credentials, RBAC: boundary,
		Census: census}, nil
}
