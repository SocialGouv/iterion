package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const PortActivationVersion = 4
const PortLaunchAdmissionVersion = 1

const PortDistributedProofMaxAge = time.Minute

const (
	PortActivationLocal       = "local"
	PortActivationDistributed = "distributed"
)

var ErrPortActivation = errors.New("store: native runtime activation is unavailable or unproven")

// PortActivation is an epoch-fenced launch authorization, separate from any
// run document. A disabled record retains its revision and evidence so an
// operator can roll back new launches while capable binaries still resume
// existing native runs. ProofDigest identifies the reviewed proof, while
// CapabilityDigest binds the binary's native runtime capabilities. Operators
// must re-probe when the evidence or binary changes.
type PortActivation struct {
	Version int `json:"version" bson:"version"`
	// Revision is the operator policy revision recorded in run admissions.
	// Renewing observations never changes it, so Disable cannot be starved
	// by the authority refresher. Version 3 records require a new probe.
	Revision               uint64                      `json:"policy_revision" bson:"policy_revision"`
	ProofRevision          uint64                      `json:"proof_revision" bson:"proof_revision"`
	Enabled                bool                        `json:"enabled" bson:"enabled"`
	Scope                  string                      `json:"scope" bson:"scope"`
	StoreIdentity          string                      `json:"store_identity" bson:"store_identity"`
	ProofDigest            string                      `json:"proof_digest" bson:"proof_digest"`
	CapabilityDigest       string                      `json:"capability_digest" bson:"capability_digest"`
	QueueVersion           int                         `json:"queue_version" bson:"queue_version"`
	ConsumerAccessEvidence string                      `json:"consumer_access_evidence,omitempty" bson:"consumer_access_evidence,omitempty"`
	VerifiedAt             time.Time                   `json:"verified_at" bson:"verified_at"`
	ExpiresAt              time.Time                   `json:"expires_at" bson:"expires_at"`
	RefreshLease           *PortActivationRefreshLease `json:"refresh_lease,omitempty" bson:"refresh_lease,omitempty"`
}

// PortLaunchAdmission is the immutable authorization observed when a native
// run was created. It lets a queued or pre-created run continue after the
// operator disables new launches, without treating a bare native ID as proof.
type PortLaunchAdmission struct {
	// An absent version identifies admissions persisted before the separate
	// recovery digest existed. They remain readable and immutable.
	Version            int       `json:"admission_version,omitempty" bson:"admission_version,omitempty"`
	Scope              string    `json:"scope" bson:"scope"`
	StoreIdentity      string    `json:"store_identity" bson:"store_identity"`
	ProofDigest        string    `json:"proof_digest" bson:"proof_digest"`
	CapabilityDigest   string    `json:"capability_digest" bson:"capability_digest"`
	ResumeDigest       string    `json:"resume_digest" bson:"resume_digest"`
	ActivationRevision uint64    `json:"activation_revision" bson:"activation_revision"`
	AdmittedAt         time.Time `json:"admitted_at" bson:"admitted_at"`
	ExpiresAt          time.Time `json:"expires_at" bson:"expires_at"`
}

func (a *PortLaunchAdmission) Validate() error {
	legacy := a != nil && a.Version == 0 && a.ResumeDigest == ""
	current := a != nil && a.Version == PortLaunchAdmissionVersion &&
		len(a.ResumeDigest) == 64 && strings.Trim(a.ResumeDigest, "0123456789abcdef") == ""
	if a == nil || (a.Scope != PortActivationLocal && a.Scope != PortActivationDistributed) ||
		a.StoreIdentity == "" || a.ActivationRevision == 0 || len(a.ProofDigest) != 64 || strings.Trim(a.ProofDigest, "0123456789abcdef") != "" ||
		len(a.CapabilityDigest) != 64 || strings.Trim(a.CapabilityDigest, "0123456789abcdef") != "" ||
		(!legacy && !current) ||
		a.AdmittedAt.IsZero() || !a.AdmittedAt.Before(a.ExpiresAt) {
		return fmt.Errorf("%w: malformed native run admission", ErrPortActivation)
	}
	return nil
}

func (a *PortActivation) Validate() error {
	if a == nil || a.Version != PortActivationVersion || a.Revision == 0 || a.Revision > math.MaxInt64 ||
		a.ProofRevision == 0 || a.ProofRevision > math.MaxInt64 ||
		(a.Scope != PortActivationLocal && a.Scope != PortActivationDistributed) || a.StoreIdentity == "" ||
		len(a.ProofDigest) != 64 || strings.Trim(a.ProofDigest, "0123456789abcdef") != "" ||
		len(a.CapabilityDigest) != 64 || strings.Trim(a.CapabilityDigest, "0123456789abcdef") != "" ||
		a.VerifiedAt.IsZero() || !a.ExpiresAt.After(a.VerifiedAt) {
		return fmt.Errorf("%w: malformed activation record", ErrPortActivation)
	}
	if a.Scope == PortActivationDistributed && (a.QueueVersion < 15 || a.ConsumerAccessEvidence == "" ||
		a.ExpiresAt.After(a.VerifiedAt.Add(PortDistributedProofMaxAge))) {
		return fmt.Errorf("%w: distributed activation lacks queue/consumer evidence", ErrPortActivation)
	}
	if a.RefreshLease != nil && (a.Scope != PortActivationDistributed || a.RefreshLease.Validate() != nil) {
		return fmt.Errorf("%w: malformed authority refresh lease", ErrPortActivation)
	}
	return nil
}

type PortActivationStore interface {
	LoadPortActivation(ctx context.Context) (*PortActivation, error)
	SavePortActivation(ctx context.Context, expectedRevision uint64, next *PortActivation) error
}

// PortDistributedActivationVerifier is supplied by a trusted deployment
// authority, not by the activation record. A manually written evidence string
// cannot establish which principals can fetch the shared durable consumer.
// The production Mongo store intentionally does not provide this capability
// until a fleet and queue-access census is implemented.
type PortDistributedActivationVerifier interface {
	VerifyPortDistributedActivation(context.Context, *PortActivation, time.Time) error
}

func verifyPortDistributedActivation(ctx context.Context, s RunStore, record *PortActivation, now time.Time) error {
	if record.Scope != PortActivationDistributed {
		return nil
	}
	if now.Before(record.VerifiedAt) || !now.Before(record.ExpiresAt) || now.After(record.VerifiedAt.Add(PortDistributedProofMaxAge)) {
		return fmt.Errorf("%w: distributed proof is stale or from the future", ErrPortActivation)
	}
	verifier, ok := s.(PortDistributedActivationVerifier)
	if !ok {
		return fmt.Errorf("%w: no trusted distributed fleet and queue-access verifier", ErrPortActivation)
	}
	if err := verifier.VerifyPortDistributedActivation(ctx, record, now); err != nil {
		return fmt.Errorf("%w: distributed access verification failed: %v", ErrPortActivation, err)
	}
	proofStore, ok := s.(PortDistributedProofStore)
	if !ok {
		return fmt.Errorf("%w: no trusted distributed proof snapshot", ErrPortActivation)
	}
	proof, err := proofStore.LoadPortDistributedProof(ctx)
	if err != nil || proof == nil || proof.PolicyRevision != record.Revision ||
		proof.ProofRevision != record.ProofRevision || proof.StoreIdentity != record.StoreIdentity ||
		proof.ProofDigest != record.ProofDigest || !now.Before(proof.ExpiresAt) || proof.Validate() != nil {
		return fmt.Errorf("%w: distributed proof snapshot is absent or does not match activation", ErrPortActivation)
	}
	return nil
}

// PortStoreIdentity names the exact storage boundary an activation can admit.
// Fail closed for distributed stores that cannot report their backend scope.
func PortStoreIdentity(s RunStore) (string, error) {
	if root := s.Root(); root != "" {
		return filepath.EvalSymlinks(root)
	}
	identified, ok := s.(interface{ PortBackendIdentity() string })
	if !ok || identified.PortBackendIdentity() == "" {
		return "", fmt.Errorf("%w: distributed store has no stable backend identity", ErrPortActivation)
	}
	return identified.PortBackendIdentity(), nil
}

func AsPortActivationStore(s RunStore) PortActivationStore {
	a, _ := s.(PortActivationStore)
	return a
}

func RequirePortActivation(ctx context.Context, s RunStore, scope string, now time.Time) error {
	a := AsPortActivationStore(s)
	if a == nil {
		return fmt.Errorf("%w: store has no activation capability", ErrPortActivation)
	}
	record, err := a.LoadPortActivation(ctx)
	if err != nil {
		return err
	}
	if record == nil || !record.Enabled || record.Scope != scope || !now.Before(record.ExpiresAt) {
		return fmt.Errorf("%w: no active %s proof", ErrPortActivation, scope)
	}
	identity, err := PortStoreIdentity(s)
	if err != nil || record.StoreIdentity != identity {
		return fmt.Errorf("%w: store identity changed since activation", ErrPortActivation)
	}
	if err := verifyPortDistributedActivation(ctx, s, record, now); err != nil {
		return err
	}
	return nil
}

func RequirePortActivationCapability(ctx context.Context, s RunStore, scope, capabilityDigest string, now time.Time) error {
	_, err := ActivePortActivationCapability(ctx, s, scope, capabilityDigest, now)
	return err
}

func ActivePortActivationCapability(ctx context.Context, s RunStore, scope, capabilityDigest string, now time.Time) (*PortActivation, error) {
	a := AsPortActivationStore(s)
	if a == nil {
		return nil, fmt.Errorf("%w: store has no activation capability", ErrPortActivation)
	}
	record, err := a.LoadPortActivation(ctx)
	if err != nil {
		return nil, err
	}
	if record == nil || !record.Enabled || record.Scope != scope || !now.Before(record.ExpiresAt) {
		return nil, fmt.Errorf("%w: no active %s proof", ErrPortActivation, scope)
	}
	if record.CapabilityDigest != capabilityDigest {
		return nil, fmt.Errorf("%w: binary capability changed since activation", ErrPortActivation)
	}
	identity, err := PortStoreIdentity(s)
	if err != nil || record.StoreIdentity != identity {
		return nil, fmt.Errorf("%w: store identity changed since activation", ErrPortActivation)
	}
	if err := verifyPortDistributedActivation(ctx, s, record, now); err != nil {
		return nil, err
	}
	return record, nil
}

func (s *FilesystemRunStore) portActivationPath() string {
	return filepath.Join(s.root, "port_activation_v1.json")
}

func (s *FilesystemRunStore) portDistributedProofPath() string {
	return filepath.Join(s.root, "port_distributed_proof_v1.json")
}

func (s *FilesystemRunStore) LoadPortDistributedProof(ctx context.Context) (*PortDistributedProof, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(s.portDistributedProofPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var proof PortDistributedProof
	if err := json.Unmarshal(raw, &proof); err != nil {
		return nil, fmt.Errorf("%w: unreadable distributed proof", ErrPortActivation)
	}
	if err := proof.Validate(); err != nil {
		return nil, err
	}
	return &proof, nil
}

func (s *FilesystemRunStore) SavePortDistributedProof(ctx context.Context, expectedPolicy uint64, proof *PortDistributedProof) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	lock, err := acquireFileLockRetry(filepath.Join(s.root, ".port_distributed_proof_v1.lock"), "distributed proof", 5*time.Second)
	if err != nil {
		return err
	}
	defer lock.Unlock()
	current, err := s.LoadPortDistributedProof(ctx)
	if err != nil {
		return err
	}
	actual := uint64(0)
	if current != nil {
		actual = current.PolicyRevision
	}
	if actual != expectedPolicy {
		return fmt.Errorf("%w: distributed proof policy changed", ErrRunConflict)
	}
	if proof == nil || (expectedPolicy == 0 && proof.PolicyRevision != 1) ||
		(expectedPolicy != 0 && proof.PolicyRevision != expectedPolicy && proof.PolicyRevision != expectedPolicy+1) {
		return fmt.Errorf("%w: invalid distributed proof write", ErrPortActivation)
	}
	proof.Snapshot, err = canonicalPortSnapshot(proof.Snapshot)
	if err != nil || proof.Validate() != nil {
		return fmt.Errorf("%w: invalid distributed proof snapshot", ErrPortActivation)
	}
	var encoded bytes.Buffer
	encoder := json.NewEncoder(&encoded)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(proof); err != nil {
		return err
	}
	raw := bytes.TrimSuffix(encoded.Bytes(), []byte{'\n'})
	return WriteFileAtomic(s.portDistributedProofPath(), raw, filePerm)
}

func (s *FilesystemRunStore) LoadPortActivation(ctx context.Context) (*PortActivation, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(s.portActivationPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var record PortActivation
	if err := json.Unmarshal(raw, &record); err != nil {
		return nil, fmt.Errorf("%w: unreadable activation record: %v", ErrPortActivation, err)
	}
	if err := record.Validate(); err != nil {
		return nil, err
	}
	return &record, nil
}

func (s *FilesystemRunStore) SavePortActivation(ctx context.Context, expectedRevision uint64, next *PortActivation) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := next.Validate(); err != nil {
		return err
	}
	if next.Revision != expectedRevision+1 {
		return fmt.Errorf("%w: activation revision", ErrRunConflict)
	}
	lock, err := acquireFileLockRetry(filepath.Join(s.root, ".port_activation_v1.lock"), "native activation", 5*time.Second)
	if err != nil {
		return err
	}
	defer lock.Unlock()
	current, err := s.LoadPortActivation(ctx)
	if err != nil {
		return err
	}
	actual := uint64(0)
	if current != nil {
		actual = current.Revision
	}
	if actual != expectedRevision {
		return fmt.Errorf("%w: activation revision %d != %d", ErrRunConflict, actual, expectedRevision)
	}
	raw, err := json.Marshal(next)
	if err != nil {
		return err
	}
	return WriteFileAtomic(s.portActivationPath(), raw, filePerm)
}
