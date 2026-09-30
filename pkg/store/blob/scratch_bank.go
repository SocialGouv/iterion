package blob

import (
	"context"
	"fmt"
	"io"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// ScratchBanker is the optional streaming surface for a parked run's
// scratch bank (store.ScratchBankStore, ADR-106). It lives under the run's
// sessions/ prefix, so DeleteRunBackendSessions sweeps it with the rest.
type ScratchBanker interface {
	PutScratchBank(ctx context.Context, runID string, body io.Reader, size int64) error
	OpenScratchBank(ctx context.Context, runID string) (io.ReadCloser, error)
	DeleteScratchBank(ctx context.Context, runID string) error
}

var _ ScratchBanker = (*S3Client)(nil)

// ScratchBankRef is the bank's ref beside the run's packed CLI sessions.
const ScratchBankRef = "scratch.tgz"

func (c *S3Client) PutScratchBank(ctx context.Context, runID string, body io.Reader, size int64) error {
	key, err := backendSessionKey(runID, ScratchBankRef)
	if err != nil {
		return err
	}
	if body == nil || size < 0 {
		return fmt.Errorf("blob: put scratch bank %s: no body or a negative size (%d)", key, size)
	}
	if _, err := c.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(c.bucket),
		Key:           aws.String(key),
		Body:          body,
		ContentLength: aws.Int64(size),
		ContentType:   aws.String("application/gzip"),
	}); err != nil {
		return fmt.Errorf("blob: put scratch bank %s: %w", key, err)
	}
	return nil
}

func (c *S3Client) OpenScratchBank(ctx context.Context, runID string) (io.ReadCloser, error) {
	key, err := backendSessionKey(runID, ScratchBankRef)
	if err != nil {
		return nil, err
	}
	out, err := c.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		if isS3NotFound(err) {
			return nil, fmt.Errorf("%w: %s", ErrArtifactNotFound, key)
		}
		return nil, fmt.Errorf("blob: open scratch bank %s: %w", key, err)
	}
	return out.Body, nil
}

func (c *S3Client) DeleteScratchBank(ctx context.Context, runID string) error {
	return c.DeleteBackendSession(ctx, runID, ScratchBankRef)
}
