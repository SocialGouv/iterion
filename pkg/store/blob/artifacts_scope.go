package blob

import (
	"context"
	"fmt"
	"strings"

	"github.com/SocialGouv/iterion/pkg/store"
)

// ArtifactObjects is a single incarnation's artifact namespace. Like
// RunObjects it exposes neither client lifecycle nor scope rebinding.
type ArtifactObjects interface {
	PutArtifact(context.Context, string, string, int, []byte) error
	GetArtifact(context.Context, string, string, int) ([]byte, error)
	ListArtifactVersions(context.Context, string, string) ([]int, error)
	DeleteRunArtifacts(context.Context, string) error
}

// IncarnationArtifacts selects an immutable artifact namespace before I/O.
type IncarnationArtifacts interface {
	ArtifactsForScope(store.IncarnationDataScope) (ArtifactObjects, error)
}

// SelectScopedArtifacts requires incarnation capability and never uses format-only keys.
func SelectScopedArtifacts(client Client, scope store.IncarnationDataScope) (ArtifactObjects, error) {
	if _, err := scope.BlobPrefix(); err != nil {
		return nil, err
	}
	capable, ok := client.(IncarnationArtifacts)
	if !ok {
		return nil, fmt.Errorf("%w: incarnation blob artifacts", store.ErrRunDataCapability)
	}
	return capable.ArtifactsForScope(scope)
}

type s3ScopedArtifacts struct {
	client S3Client
	scope  store.IncarnationDataScope
}

func (c *S3Client) ArtifactsForScope(scope store.IncarnationDataScope) (ArtifactObjects, error) {
	if _, err := scope.BlobPrefix(); err != nil {
		return nil, err
	}
	return &s3ScopedArtifacts{client: *c, scope: scope}, nil
}

func (v *s3ScopedArtifacts) key(runID, nodeID string, version int) (string, error) {
	key, err := artifactKey(runID, nodeID, version)
	if err != nil {
		return "", err
	}
	return scopedRunDataKey(v.scope, runID, key)
}
func (v *s3ScopedArtifacts) PutArtifact(ctx context.Context, runID, nodeID string, version int, body []byte) error {
	key, err := v.key(runID, nodeID, version)
	if err != nil {
		return err
	}
	return v.client.putArtifactKey(ctx, key, body)
}
func (v *s3ScopedArtifacts) GetArtifact(ctx context.Context, runID, nodeID string, version int) ([]byte, error) {
	key, err := v.key(runID, nodeID, version)
	if err != nil {
		return nil, err
	}
	return v.client.getArtifactKey(ctx, key)
}
func (v *s3ScopedArtifacts) ListArtifactVersions(ctx context.Context, runID, nodeID string) ([]int, error) {
	key, err := v.key(runID, nodeID, 0)
	if err != nil {
		return nil, err
	}
	return v.client.listArtifactPrefix(ctx, strings.TrimSuffix(key, "0.json"))
}
func (v *s3ScopedArtifacts) DeleteRunArtifacts(ctx context.Context, runID string) error {
	key, err := artifactRunPrefix(runID)
	if err != nil {
		return err
	}
	prefix, err := scopedRunDataKey(v.scope, runID, key)
	if err != nil {
		return err
	}
	return v.client.deleteUnder(ctx, prefix, "incarnation artifact")
}

var _ IncarnationArtifacts = (*S3Client)(nil)
var _ ArtifactObjects = (*s3ScopedArtifacts)(nil)
