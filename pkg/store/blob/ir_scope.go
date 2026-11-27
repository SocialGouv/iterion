package blob

import (
	"context"
	"fmt"
	"strings"

	"github.com/SocialGouv/iterion/pkg/store"
)

// IRObjects is one incarnation's compiled IR and bundle snapshot namespace.
// Storage keys returned by IRBlobKeyForScope belong only to this view.
type IRObjects interface {
	PutIRBlob(context.Context, string, []byte) error
	GetIRBlob(context.Context, string) ([]byte, error)
	DeleteRunIR(context.Context, string) error
}

// IncarnationIR selects an immutable IR namespace without client lifecycle access.
type IncarnationIR interface {
	IRForScope(store.IncarnationDataScope) (IRObjects, error)
}

// SelectScopedIR requires incarnation capability and never falls back to legacy keys.
func SelectScopedIR(client Client, scope store.IncarnationDataScope) (IRObjects, error) {
	if _, err := scope.BlobPrefix(); err != nil {
		return nil, err
	}
	capable, ok := client.(IncarnationIR)
	if !ok {
		return nil, fmt.Errorf("%w: incarnation IR blobs", store.ErrRunDataCapability)
	}
	return capable.IRForScope(scope)
}

// IRBlobKeyForScope accepts the original run or one of its "-bundle-" ids (a
// bundle snapshot, or a compiled IR's content id).
// The owner comes from the retained binding, never by splitting an arbitrary ID.
func IRBlobKeyForScope(scope store.IncarnationDataScope, logicalID string) (string, error) {
	prefix, err := scope.BlobPrefix()
	if err != nil {
		return "", err
	}
	key, err := irBlobKey(logicalID)
	if err != nil {
		return "", err
	}
	owner := scope.Header().RunID
	if logicalID != owner {
		suffix, ok := strings.CutPrefix(logicalID, owner+store.IRBlobFamilyInfix)
		if !ok || suffix == "" {
			return "", store.ErrRunDataIdentityChanged
		}
	}
	return prefix + key, nil
}

type s3ScopedIR struct {
	client S3Client
	scope  store.IncarnationDataScope
}

// IRForScope copies the original scope into a view with no rebinding methods.
func (c *S3Client) IRForScope(scope store.IncarnationDataScope) (IRObjects, error) {
	if _, err := scope.BlobPrefix(); err != nil {
		return nil, err
	}
	return &s3ScopedIR{client: *c, scope: scope}, nil
}
func (v *s3ScopedIR) PutIRBlob(ctx context.Context, logicalID string, body []byte) error {
	key, err := IRBlobKeyForScope(v.scope, logicalID)
	if err != nil {
		return err
	}
	return v.client.putIRBlobKey(ctx, key, body)
}
func (v *s3ScopedIR) GetIRBlob(ctx context.Context, storageKey string) ([]byte, error) {
	prefix, err := v.scope.BlobPrefix()
	if err != nil {
		return nil, err
	}
	legacy, ok := strings.CutPrefix(storageKey, prefix)
	if !ok {
		return nil, store.ErrRunDataIdentityChanged
	}
	logicalID, err := IRBlobKeyRunID(legacy)
	if err != nil {
		return nil, err
	}
	canonical, err := IRBlobKeyForScope(v.scope, logicalID)
	if err != nil {
		return nil, err
	}
	if canonical != storageKey {
		return nil, store.ErrInvalidRunDataIdentity
	}
	return v.client.getIRBlobKey(ctx, storageKey)
}
func (v *s3ScopedIR) DeleteRunIR(ctx context.Context, runID string) error {
	if runID != v.scope.Header().RunID {
		return store.ErrRunDataIdentityChanged
	}
	prefix, err := v.scope.BlobPrefix()
	if err != nil {
		return err
	}
	return v.client.deleteUnder(ctx, prefix+"ir/", "incarnation IR")
}

var _ IncarnationIR = (*S3Client)(nil)
var _ IRObjects = (*s3ScopedIR)(nil)
