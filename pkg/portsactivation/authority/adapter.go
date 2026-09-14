package authority

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/SocialGouv/iterion/pkg/portsactivation/natsconfig"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/store"
	natsclient "github.com/nats-io/nats.go"
)

const DefaultRefreshInterval = 20 * time.Second

// AdapterConfig contains only the server-side authority inputs. The system
// URL is used by the privileged observer; QueueClient is an ordinary queue
// connection and is never used to read NATS system subjects.
type AdapterConfig struct {
	KubectlBinary        string
	KubernetesContext    string
	AuthorityRef         string
	KubernetesNamespaces []string
	// ExpectedQueue is optional during bootstrap. When omitted, the adapter
	// first reads the trusted record to obtain the account/system-account
	// names, then the production observer binds that exact topology. Callers
	// that already know the complete queue should set it to make the binding
	// happen before the first observation.
	ExpectedQueue natsconfig.QueueTopology
	SystemNATSURL string
	QueueClient   *natsclient.Conn
	// Census is supplied by the ordinary queue connection. It must return a
	// complete, fresh capability corroboration bound to the same record and
	// backend; the adapter never accepts caller-supplied proof fields.
	Census func(context.Context, *Record, time.Time) (*CensusCorroboration, error)
	// RequireCensus is set by the production server wiring. Unit adapters may
	// leave it false when exercising only record/observation projection.
	RequireCensus bool
}

func (c AdapterConfig) validate() error {
	if c.KubectlBinary == "" || c.AuthorityRef == "" || len(c.KubernetesNamespaces) == 0 ||
		c.SystemNATSURL == "" || c.QueueClient == nil {
		return fmt.Errorf("distributed authority adapter is missing a privileged or queue input")
	}
	return nil
}

// DeploymentAdapter is the production authority adapter. It performs the
// bounded read-only observation and projects it into a safe proof; it never
// changes activation policy itself.
type DeploymentAdapter struct {
	Config  AdapterConfig
	Observe func(context.Context, AdapterConfig) (*DeploymentCorroboration, error)
	Record  func(context.Context, AdapterConfig) (*Record, error)
}

func (a *DeploymentAdapter) observe(ctx context.Context) (*DeploymentCorroboration, error) {
	if a == nil || a.Config.validate() != nil {
		return nil, fmt.Errorf("distributed authority adapter is not configured")
	}
	if a.Observe != nil {
		return a.Observe(ctx, a.Config)
	}
	queue := a.Config.ExpectedQueue
	if queue.Validate() != nil {
		// The account and system-account are intentionally sourced from the
		// trusted record. The second read is bounded and the observer itself
		// re-reads the Secret around the complete observation, so a rotation
		// cannot silently cross the record/observation boundary.
		record, err := a.readRecord(ctx)
		if err != nil {
			return nil, err
		}
		queue = record.Queue
	}
	return ObserveDeploymentWithQueueAndCensus(ctx, a.Config.KubectlBinary, a.Config.KubernetesContext,
		a.Config.AuthorityRef, a.Config.KubernetesNamespaces, queue,
		a.Config.SystemNATSURL, a.Config.QueueClient, a.Config.Census)
}

func (a *DeploymentAdapter) readRecord(ctx context.Context) (*Record, error) {
	if a.Record != nil {
		return a.Record(ctx, a.Config)
	}
	var record *Record
	var err error
	if a.Config.ExpectedQueue.Validate() == nil {
		record, _, err = ReadDeploymentRecord(ctx, a.Config.KubectlBinary, a.Config.KubernetesContext,
			a.Config.AuthorityRef, a.Config.KubernetesNamespaces, a.Config.ExpectedQueue)
	} else {
		record, _, err = ReadDeploymentRecordUnbound(ctx, a.Config.KubectlBinary, a.Config.KubernetesContext,
			a.Config.AuthorityRef, a.Config.KubernetesNamespaces)
	}
	return record, err
}

// Probe performs one privileged observation and creates a candidate proof.
// Callers choose the policy/proof revisions so a probe can target either a
// fresh activation or a freshness renewal without changing operator policy.
func (a *DeploymentAdapter) Probe(ctx context.Context, storeIdentity string,
	policyRevision, proofRevision uint64, expiresAt time.Time) (*store.PortDistributedProof, error) {
	if storeIdentity == "" || policyRevision == 0 || proofRevision == 0 || expiresAt.IsZero() {
		return nil, fmt.Errorf("distributed authority probe has an incomplete target")
	}
	record, err := a.readRecord(ctx)
	if err != nil {
		return nil, err
	}
	observation, err := a.observe(ctx)
	if err != nil {
		return nil, err
	}
	if observation.Epoch != record.Epoch || observation.DeploymentRevision != record.DeploymentRevision ||
		observation.Queue != record.Queue || observation.AuthoritySecretUID == "" {
		return nil, fmt.Errorf("distributed authority observation changed its trusted record")
	}
	if a.Config.RequireCensus && observation.Census == nil {
		return nil, fmt.Errorf("distributed authority observation lacks a fresh capability census")
	}
	return BuildDistributedProof(record, observation, storeIdentity, policyRevision, proofRevision, expiresAt)
}

// ProbeForRecord loads the record first so the builder can bind the observed
// epoch/revision/queue to the exact operator source. A new activation advances
// policy revision; an existing enabled activation keeps its policy for a
// renewal when the caller explicitly requests that mode.
func (a *DeploymentAdapter) ProbeForRecord(ctx context.Context, record *Record,
	storeIdentity string, policyRevision, proofRevision uint64, expiresAt time.Time) (*store.PortDistributedProof, error) {
	if record == nil {
		return nil, fmt.Errorf("distributed authority probe has no record")
	}
	if err := record.validate(); err != nil {
		return nil, err
	}
	if storeIdentity == "" || policyRevision == 0 || proofRevision == 0 || expiresAt.IsZero() {
		return nil, fmt.Errorf("distributed authority probe has an incomplete target")
	}
	observation, err := a.observe(ctx)
	if err != nil {
		return nil, err
	}
	if observation.Epoch != record.Epoch || observation.DeploymentRevision != record.DeploymentRevision ||
		observation.Queue != record.Queue || observation.AuthoritySecretUID == "" {
		return nil, fmt.Errorf("distributed authority observation changed its trusted record")
	}
	if a.Config.RequireCensus && observation.Census == nil {
		return nil, fmt.Errorf("distributed authority observation lacks a fresh capability census")
	}
	return BuildDistributedProof(record, observation, storeIdentity, policyRevision, proofRevision, expiresAt)
}

// Authority coordinates the trusted probe, candidate snapshot and activation
// CAS surfaces. The candidate is kept separate from the currently active
// proof, so a failed or superseded probe never interrupts existing admission.
type Authority struct {
	Adapter          *DeploymentAdapter
	Activations      store.PortActivationStore
	Proofs           store.PortDistributedProofStore
	Candidates       store.PortDistributedProofCandidateStore
	StoreIdentity    string
	CapabilityDigest string
}

func (a *Authority) Probe(ctx context.Context) (*store.PortDistributedProof, error) {
	if a == nil || a.Adapter == nil || a.Activations == nil || a.Candidates == nil ||
		a.Proofs == nil || a.StoreIdentity == "" {
		return nil, fmt.Errorf("distributed authority persistence is incomplete")
	}
	current, err := a.Activations.LoadPortActivation(ctx)
	if err != nil {
		return nil, err
	}
	policy, proofRevision := uint64(1), uint64(1)
	if current != nil {
		if err := current.Validate(); err != nil {
			return nil, err
		}
		if current.Revision >= math.MaxInt64-1 || current.ProofRevision >= math.MaxInt64-1 {
			return nil, fmt.Errorf("distributed authority revision cannot advance")
		}
		policy, proofRevision = current.Revision+1, current.ProofRevision+1
	}
	now := time.Now().UTC()
	proof, err := a.Adapter.Probe(ctx, a.StoreIdentity, policy, proofRevision, now.Add(store.PortDistributedProofMaxAge))
	if err != nil {
		return nil, err
	}
	if err := a.Candidates.SavePortDistributedProofCandidate(ctx, proof); err != nil {
		return nil, err
	}
	return proof, nil
}

// Activate consumes only the latest candidate and atomically advances the
// operator policy. It accepts no caller-supplied proof fields.
func (a *Authority) Activate(ctx context.Context, expectedPolicy uint64) (*store.PortActivation, error) {
	if a == nil || a.Activations == nil || a.Proofs == nil || a.Candidates == nil || a.StoreIdentity == "" {
		return nil, fmt.Errorf("distributed authority persistence is incomplete")
	}
	current, err := a.Activations.LoadPortActivation(ctx)
	if err != nil {
		return nil, err
	}
	actual := uint64(0)
	if current != nil {
		actual = current.Revision
	}
	if actual != expectedPolicy || expectedPolicy >= math.MaxInt64 {
		return nil, fmt.Errorf("%w: distributed activation policy changed", store.ErrRunConflict)
	}
	nextPolicy := expectedPolicy + 1
	candidate, err := a.Candidates.LoadPortDistributedProofCandidate(ctx)
	now := time.Now().UTC()
	if err != nil || candidate == nil || candidate.PolicyRevision != expectedPolicy+1 ||
		candidate.StoreIdentity != a.StoreIdentity || now.Before(candidate.VerifiedAt) || !now.Before(candidate.ExpiresAt) {
		return nil, fmt.Errorf("%w: no fresh distributed proof candidate", store.ErrPortActivation)
	}
	if err := candidate.Validate(); err != nil {
		return nil, err
	}
	if a.CapabilityDigest == "" {
		return nil, fmt.Errorf("%w: distributed capability digest is not configured", store.ErrPortActivation)
	}
	record := &store.PortActivation{Version: store.PortActivationVersion,
		Revision: nextPolicy, ProofRevision: candidate.ProofRevision, Enabled: true,
		Scope: store.PortActivationDistributed, StoreIdentity: a.StoreIdentity,
		ProofDigest: candidate.ProofDigest, CapabilityDigest: a.CapabilityDigest,
		QueueVersion: queue.SchemaVersion, ConsumerAccessEvidence: "verified deployment authority observation",
		VerifiedAt: candidate.VerifiedAt, ExpiresAt: candidate.ExpiresAt}
	writer, ok := a.Activations.(store.PortDistributedActivationWriter)
	if !ok {
		return nil, fmt.Errorf("%w: distributed backend cannot atomically publish proof and activation", store.ErrPortActivation)
	}
	if err := writer.SavePortDistributedActivation(ctx, expectedPolicy, candidate, record); err != nil {
		return nil, err
	}
	if err := a.Candidates.ClearPortDistributedProofCandidate(ctx); err != nil {
		return nil, err
	}
	return record, nil
}
