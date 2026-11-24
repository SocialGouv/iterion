package blob

import (
	"context"
	"github.com/SocialGouv/iterion/pkg/store"
	"strings"
)

var _ ValueFormatArtifacts = (*S3Client)(nil)

// Closed namespaces: format1's key is byte-identical to the historical key.
// No namespace chosen by a caller can widen a delete to another format.
func artifactKeyForFormat(raw int, runID, nodeID string, version int) (string, error) {
	f, err := store.NormalizeValueFormat(raw)
	if err != nil {
		return "", err
	}
	key, err := artifactKey(runID, nodeID, version)
	if err != nil {
		return "", err
	}
	if f == store.ExactValueFormat {
		key = "artifacts-v2/" + strings.TrimPrefix(key, "artifacts/")
	}
	return key, nil
}
func artifactRunPrefixForFormat(raw int, runID string) (string, error) {
	f, err := store.NormalizeValueFormat(raw)
	if err != nil {
		return "", err
	}
	key, err := artifactRunPrefix(runID)
	if err != nil {
		return "", err
	}
	if f == store.ExactValueFormat {
		key = "artifacts-v2/" + strings.TrimPrefix(key, "artifacts/")
	}
	return key, nil
}
func artifactNodePrefixForFormat(raw int, runID, nodeID string) (string, error) {
	key, err := artifactKeyForFormat(raw, runID, nodeID, 0)
	if err != nil {
		return "", err
	}
	return strings.TrimSuffix(key, "0.json"), nil
}
func (c *S3Client) PutArtifactForFormat(ctx context.Context, f int, r, n string, v int, body []byte) error {
	key, e := artifactKeyForFormat(f, r, n, v)
	if e != nil {
		return e
	}
	return c.putArtifactKey(ctx, key, body)
}
func (c *S3Client) GetArtifactForFormat(ctx context.Context, f int, r, n string, v int) ([]byte, error) {
	key, e := artifactKeyForFormat(f, r, n, v)
	if e != nil {
		return nil, e
	}
	return c.getArtifactKey(ctx, key)
}
func (c *S3Client) ListArtifactVersionsForFormat(ctx context.Context, f int, r, n string) ([]int, error) {
	prefix, e := artifactNodePrefixForFormat(f, r, n)
	if e != nil {
		return nil, e
	}
	return c.listArtifactPrefix(ctx, prefix)
}
func (c *S3Client) DeleteRunArtifactsForFormat(ctx context.Context, f int, r string) error {
	prefix, e := artifactRunPrefixForFormat(f, r)
	if e != nil {
		return e
	}
	return c.deleteUnder(ctx, prefix, "artifact")
}
