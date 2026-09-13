package portsactivation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/store"
)

const ProofVersion = 1

type Inspection struct {
	Scope                   string                `json:"scope"`
	StoreIdentity           string                `json:"store_identity"`
	CapabilityDigest        string                `json:"capability_digest"`
	QueueVersion            int                   `json:"queue_version"`
	HasActivationStore      bool                  `json:"has_activation_store"`
	HasFileStore            bool                  `json:"has_file_store"`
	HasImmutableFileCapture bool                  `json:"has_immutable_file_capture"`
	Activation              *store.PortActivation `json:"activation,omitempty"`
}

func Inspect(ctx context.Context, s store.RunStore) (*Inspection, error) {
	scope := store.PortActivationLocal
	identity := s.Root()
	if identity == "" {
		scope, identity = store.PortActivationDistributed, "unverified-distributed-store"
	}
	if scope == store.PortActivationLocal {
		var err error
		identity, err = filepath.EvalSymlinks(identity)
		if err != nil {
			return nil, err
		}
	}
	result := &Inspection{Scope: scope, StoreIdentity: identity, CapabilityDigest: CapabilityDigest(scope), QueueVersion: queue.SchemaVersion,
		HasActivationStore:      store.AsPortActivationStore(s) != nil,
		HasFileStore:            store.AsRunFilesStore(s) != nil,
		HasImmutableFileCapture: store.AsPortFilesStore(s) != nil}
	if activation := store.AsPortActivationStore(s); activation != nil {
		var err error
		result.Activation, err = activation.LoadPortActivation(ctx)
		if err != nil {
			return nil, err
		}
	}
	return result, nil
}

// Proof is a reviewable snapshot of the exact local scope and binary. The
// digest detects editing between probe and activate; the activation record
// separately binds to the current binary capability and expires.
type Proof struct {
	Version                int       `json:"version"`
	Scope                  string    `json:"scope"`
	StoreIdentity          string    `json:"store_identity"`
	CapabilityDigest       string    `json:"capability_digest"`
	QueueVersion           int       `json:"queue_version"`
	ExclusiveStoreAttested bool      `json:"exclusive_store_attested"`
	VerifiedAt             time.Time `json:"verified_at"`
	ExpiresAt              time.Time `json:"expires_at"`
	Digest                 string    `json:"digest"`
}

func proofDigest(p Proof) (string, error) {
	p.Digest = ""
	raw, err := json.Marshal(p)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

func ProbeLocal(ctx context.Context, s store.RunStore, exclusiveStoreAttested bool) (*Proof, error) {
	inspection, err := Inspect(ctx, s)
	if err != nil {
		return nil, err
	}
	if inspection.Scope != store.PortActivationLocal || !inspection.HasActivationStore || !inspection.HasFileStore || !inspection.HasImmutableFileCapture {
		return nil, fmt.Errorf("%w: local native store capabilities are incomplete", store.ErrPortActivation)
	}
	if !exclusiveStoreAttested {
		return nil, fmt.Errorf("%w: local store access has not been confirmed exclusive of incompatible automation", store.ErrPortActivation)
	}
	info, err := os.Stat(inspection.StoreIdentity)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode().Perm()&0o022 != 0 {
		return nil, fmt.Errorf("%w: local store is not a private directory", store.ErrPortActivation)
	}
	if store.RunDataDirectory("pc1_probe") != store.NativeRunsDirectory || store.RunDataDirectory("legacy_probe") != "runs" || queue.SchemaVersion <= queue.LegacySchemaVersion {
		return nil, fmt.Errorf("%w: native namespace or queue capabilities are inconsistent", store.ErrPortActivation)
	}
	now := time.Now().UTC()
	p := &Proof{Version: ProofVersion, Scope: store.PortActivationLocal, StoreIdentity: inspection.StoreIdentity,
		CapabilityDigest: inspection.CapabilityDigest, QueueVersion: inspection.QueueVersion, ExclusiveStoreAttested: true,
		VerifiedAt: now, ExpiresAt: now.Add(24 * time.Hour)}
	p.Digest, err = proofDigest(*p)
	return p, err
}

func ActivateLocal(ctx context.Context, s store.RunStore, proof Proof) (*store.PortActivation, error) {
	now := time.Now().UTC()
	if proof.Version != ProofVersion || proof.Scope != store.PortActivationLocal || !proof.ExclusiveStoreAttested ||
		proof.VerifiedAt.IsZero() || proof.VerifiedAt.After(now) || !now.Before(proof.ExpiresAt) ||
		proof.ExpiresAt.After(proof.VerifiedAt.Add(24*time.Hour)) {
		return nil, fmt.Errorf("%w: expired or incomplete local proof", store.ErrPortActivation)
	}
	digest, err := proofDigest(proof)
	if err != nil {
		return nil, err
	}
	if digest != proof.Digest {
		return nil, fmt.Errorf("%w: proof payload changed", store.ErrPortActivation)
	}
	if _, err := ProbeLocal(ctx, s, true); err != nil {
		return nil, err
	}
	inspection, err := Inspect(ctx, s)
	if err != nil {
		return nil, err
	}
	if inspection.Scope != proof.Scope || inspection.StoreIdentity != proof.StoreIdentity || inspection.CapabilityDigest != proof.CapabilityDigest || inspection.QueueVersion != proof.QueueVersion {
		return nil, fmt.Errorf("%w: store or binary changed after probe", store.ErrPortActivation)
	}
	activation := store.AsPortActivationStore(s)
	if activation == nil {
		return nil, fmt.Errorf("%w: store has no activation CAS", store.ErrPortActivation)
	}
	revision := uint64(0)
	if inspection.Activation != nil {
		revision = inspection.Activation.Revision
	}
	record := &store.PortActivation{Version: store.PortActivationVersion, Revision: revision + 1, Enabled: true,
		Scope: proof.Scope, ProofDigest: proof.Digest, CapabilityDigest: proof.CapabilityDigest,
		QueueVersion: proof.QueueVersion, VerifiedAt: proof.VerifiedAt, ExpiresAt: proof.ExpiresAt}
	if err := activation.SavePortActivation(ctx, revision, record); err != nil {
		return nil, err
	}
	return record, nil
}

func Disable(ctx context.Context, s store.RunStore) (*store.PortActivation, error) {
	activation := store.AsPortActivationStore(s)
	if activation == nil {
		return nil, fmt.Errorf("%w: store has no activation CAS", store.ErrPortActivation)
	}
	current, err := activation.LoadPortActivation(ctx)
	if err != nil {
		return nil, err
	}
	if current == nil {
		return nil, fmt.Errorf("%w: no activation record", store.ErrPortActivation)
	}
	next := *current
	next.Revision++
	next.Enabled = false
	if err := activation.SavePortActivation(ctx, current.Revision, &next); err != nil {
		return nil, err
	}
	return &next, nil
}
