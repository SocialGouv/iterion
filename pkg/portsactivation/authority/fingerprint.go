package authority

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/SocialGouv/iterion/pkg/portsactivation/natsconfig"
)

type brokerFingerprintClaim struct {
	ServerID    string `json:"server_id"`
	ServerName  string `json:"server_name"`
	Namespace   string `json:"namespace"`
	PodName     string `json:"pod_name"`
	Container   string `json:"container"`
	ImageDigest string `json:"image_digest"`
	ConfigPath  string `json:"config_path"`
}

type kubernetesFingerprintRevision struct {
	Kind            string `json:"kind"`
	Namespace       string `json:"namespace"`
	Name            string `json:"name"`
	UID             string `json:"uid"`
	ResourceVersion string `json:"resource_version"`
	Generation      int64  `json:"generation,omitempty"`
}

type systemFingerprintBroker struct {
	ServerID     string `json:"server_id"`
	ConfigDigest string `json:"config_digest"`
}

type deploymentObservationIdentity struct {
	Version                 int                             `json:"version"`
	AuthoritySecretUID      string                          `json:"authority_secret_uid"`
	AuthoritySecretRevision string                          `json:"authority_secret_revision"`
	DeploymentRevision      string                          `json:"deployment_revision"`
	Epoch                   uint64                          `json:"epoch"`
	Namespaces              []string                        `json:"namespaces"`
	Queue                   natsconfig.QueueTopology        `json:"queue"`
	Brokers                 []brokerFingerprintClaim        `json:"brokers"`
	Custody                 []CredentialCustody             `json:"custody"`
	Holders                 []CredentialHolder              `json:"holders"`
	BuildApprovals          []BuildApproval                 `json:"build_approvals"`
	Issuers                 []CredentialIssuer              `json:"issuers"`
	Writers                 []OperatorWriter                `json:"writers"`
	StaticBrokers           []StaticBroker                  `json:"static_brokers"`
	SystemBrokers           []systemFingerprintBroker       `json:"system_brokers"`
	Access                  []StaticAccess                  `json:"access"`
	BuildBindings           []BuildBinding                  `json:"build_bindings"`
	WorkloadRevisions       []kubernetesFingerprintRevision `json:"workload_revisions"`
	Roles                   []RBACRole                      `json:"roles"`
	Bindings                []RBACBinding                   `json:"bindings"`
	Launches                []BrokerLaunch                  `json:"launches"`
	BrokerSources           []BrokerConfigBinding           `json:"broker_sources"`
	CredentialBindings      []CredentialSourceBinding       `json:"credential_bindings"`
	ExternalHolders         []string                        `json:"external_holders"`
	WorkloadBindings        []WorkloadBinding               `json:"workload_bindings"`
	WorkerAccounts          []string                        `json:"worker_accounts"`
	PermittedWriters        []string                        `json:"permitted_writers"`
}

// fingerprintDeploymentObservation binds every stable observed identity but
// excludes timing and live connection counts. Kubernetes UID/resourceVersion
// are trusted API content identities; the observer separately checks the
// actual role rules, Pod specs and Secret bytes before reaching this point.
// This digest is only an observation identity. It omits backend identity and
// compatibility/exclusion proof, so it cannot authorize native admission.
func fingerprintDeploymentObservation(record *Record, result *DeploymentCorroboration,
	workloads *WorkloadSnapshot, rbac *RBACSnapshot) (string, error) {
	if record == nil || record.validate() != nil || result == nil || workloads == nil || rbac == nil ||
		result.Static == nil || result.Builds == nil || result.System == nil || result.Workloads == nil ||
		result.Credentials == nil || result.RBAC == nil || result.Queue != record.Queue ||
		result.Epoch != record.Epoch || result.DeploymentRevision != record.DeploymentRevision ||
		result.AuthoritySecretUID == "" || result.AuthoritySecretRevision == "" {
		return "", fmt.Errorf("distributed authority observation cannot be fingerprinted")
	}
	brokers := make([]brokerFingerprintClaim, 0, len(record.Brokers))
	for _, broker := range record.Brokers {
		brokers = append(brokers, brokerFingerprintClaim{broker.ServerID, broker.ServerName,
			broker.Namespace, broker.PodName, broker.Container, broker.ImageDigest, broker.ConfigPath})
	}
	slices.SortFunc(brokers, func(a, b brokerFingerprintClaim) int { return strings.Compare(a.ServerID, b.ServerID) })
	custody := slices.Clone(record.Credentials)
	for i := range custody {
		custody[i].HolderIDs = slices.Clone(custody[i].HolderIDs)
		custody[i].IssuerIDs = slices.Clone(custody[i].IssuerIDs)
		slices.Sort(custody[i].HolderIDs)
		slices.Sort(custody[i].IssuerIDs)
	}
	slices.SortFunc(custody, func(a, b CredentialCustody) int {
		if c := strings.Compare(a.Account, b.Account); c != 0 {
			return c
		}
		return strings.Compare(a.Identity, b.Identity)
	})
	holders := slices.Clone(record.Holders)
	slices.SortFunc(holders, func(a, b CredentialHolder) int { return strings.Compare(a.ID, b.ID) })
	buildApprovals := slices.Clone(record.BuildApprovals)
	slices.SortFunc(buildApprovals, func(a, b BuildApproval) int {
		return strings.Compare(a.ImageDigest, b.ImageDigest)
	})
	issuers := slices.Clone(record.Issuers)
	slices.SortFunc(issuers, func(a, b CredentialIssuer) int { return strings.Compare(a.ID, b.ID) })
	writers := slices.Clone(record.PermittedWriters)
	slices.SortFunc(writers, func(a, b OperatorWriter) int {
		return strings.Compare(a.Kind+"\x00"+a.Namespace+"\x00"+a.Name, b.Kind+"\x00"+b.Namespace+"\x00"+b.Name)
	})
	staticBrokers := slices.Clone(result.Static.Brokers)
	slices.SortFunc(staticBrokers, func(a, b StaticBroker) int { return strings.Compare(a.ServerID, b.ServerID) })
	systemBrokers := make([]systemFingerprintBroker, 0, len(result.System.Brokers))
	for _, broker := range result.System.Brokers {
		systemBrokers = append(systemBrokers, systemFingerprintBroker{broker.ServerID, broker.ConfigDigest})
	}
	slices.SortFunc(systemBrokers, func(a, b systemFingerprintBroker) int {
		return strings.Compare(a.ServerID, b.ServerID)
	})
	access := slices.Clone(result.Static.Access)
	for i := range access {
		access[i].Exposures = slices.Clone(access[i].Exposures)
		slices.SortFunc(access[i].Exposures, func(a, b natsconfig.AccessExposure) int {
			return strings.Compare(a.Surface+"\x00"+a.Direction+"\x00"+a.Witness,
				b.Surface+"\x00"+b.Direction+"\x00"+b.Witness)
		})
	}
	slices.SortFunc(access, func(a, b StaticAccess) int {
		return strings.Compare(a.ServerID+"\x00"+a.Account+"\x00"+a.Identity,
			b.ServerID+"\x00"+b.Account+"\x00"+b.Identity)
	})
	buildBindings := slices.Clone(result.Builds.Bindings)
	slices.SortFunc(buildBindings, func(a, b BuildBinding) int {
		return strings.Compare(a.Account+"\x00"+a.Principal+"\x00"+a.HolderID,
			b.Account+"\x00"+b.Principal+"\x00"+b.HolderID)
	})
	workloadRevisions := make([]kubernetesFingerprintRevision, 0, len(workloads.Workloads))
	for _, workload := range workloads.Workloads {
		workloadRevisions = append(workloadRevisions, kubernetesFingerprintRevision{
			Kind: workload.Kind, Namespace: workload.Namespace, Name: workload.Name,
			UID: workload.UID, ResourceVersion: workload.ResourceVersion, Generation: workload.Generation})
	}
	sortRevisions(workloadRevisions)
	roles := slices.Clone(rbac.Roles)
	slices.SortFunc(roles, func(a, b RBACRole) int {
		return strings.Compare(a.Namespace+"\x00"+a.Kind+"\x00"+a.Name,
			b.Namespace+"\x00"+b.Kind+"\x00"+b.Name)
	})
	bindings := slices.Clone(rbac.Bindings)
	slices.SortFunc(bindings, func(a, b RBACBinding) int {
		return strings.Compare(a.Namespace+"\x00"+a.Kind+"\x00"+a.Name,
			b.Namespace+"\x00"+b.Kind+"\x00"+b.Name)
	})
	launches := slices.Clone(result.BrokerLaunches)
	slices.SortFunc(launches, func(a, b BrokerLaunch) int { return strings.Compare(a.ServerID, b.ServerID) })
	brokerSources := slices.Clone(result.BrokerSources)
	slices.SortFunc(brokerSources, func(a, b BrokerConfigBinding) int { return strings.Compare(a.ServerID, b.ServerID) })
	credentialBindings := slices.Clone(result.Credentials.Bindings)
	slices.SortFunc(credentialBindings, func(a, b CredentialSourceBinding) int { return strings.Compare(a.HolderID, b.HolderID) })
	externalHolders := slices.Clone(result.Credentials.ExternalHolderIDs)
	slices.Sort(externalHolders)
	workloadBindings := slices.Clone(result.Workloads.Bindings)
	slices.SortFunc(workloadBindings, func(a, b WorkloadBinding) int {
		return strings.Compare(a.WorkloadUID+"\x00"+a.HolderID, b.WorkloadUID+"\x00"+b.HolderID)
	})
	workerAccounts := slices.Clone(result.RBAC.WorkerServiceAccounts)
	slices.Sort(workerAccounts)
	permittedWriters := slices.Clone(result.RBAC.PermittedWriters)
	slices.Sort(permittedWriters)
	namespaces := slices.Clone(record.Namespaces)
	slices.Sort(namespaces)
	identity := deploymentObservationIdentity{
		Version: 1, AuthoritySecretUID: result.AuthoritySecretUID,
		AuthoritySecretRevision: result.AuthoritySecretRevision,
		DeploymentRevision:      result.DeploymentRevision, Epoch: result.Epoch,
		Namespaces: namespaces, Queue: result.Queue, Brokers: brokers, Custody: custody,
		Holders: holders, BuildApprovals: buildApprovals, Issuers: issuers, Writers: writers,
		StaticBrokers: staticBrokers, SystemBrokers: systemBrokers, Access: access,
		BuildBindings: buildBindings, WorkloadRevisions: workloadRevisions,
		Roles: roles, Bindings: bindings, Launches: launches, BrokerSources: brokerSources,
		CredentialBindings: credentialBindings, ExternalHolders: externalHolders,
		WorkloadBindings: workloadBindings, WorkerAccounts: workerAccounts,
		PermittedWriters: permittedWriters,
	}
	raw, err := json.Marshal(identity)
	if err != nil {
		return "", fmt.Errorf("distributed authority observation cannot be encoded")
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

func sortRevisions(revisions []kubernetesFingerprintRevision) {
	slices.SortFunc(revisions, func(a, b kubernetesFingerprintRevision) int {
		return strings.Compare(a.Namespace+"\x00"+a.Kind+"\x00"+a.Name,
			b.Namespace+"\x00"+b.Kind+"\x00"+b.Name)
	})
}
