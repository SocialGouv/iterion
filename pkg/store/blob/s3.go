package blob

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	smithy "github.com/aws/smithy-go"
)

// ErrArtifactNotFound is returned by GetArtifact / ListArtifactVersions
// when the requested key (or prefix) has no objects in the bucket.
// Callers should match with errors.Is so cloud retention sweepers and
// migration tools can distinguish missing-blob from transient backend
// errors.
var ErrArtifactNotFound = errors.New("blob: artifact not found")

// S3Client is the AWS-S3-compatible blob backend. It speaks v4 SigV4
// to AWS S3 directly when Endpoint is empty, and to a generic S3
// gateway (MinIO, Wasabi, etc.) when Endpoint is set with
// UsePathStyle=true.
type S3Client struct {
	client *s3.Client
	bucket string
}

// NewS3 constructs an S3-backed Client from the connection settings.
// Static credentials are used when both AccessKeyID and SecretAccessKey
// are non-empty; otherwise the SDK falls back to its default credential
// chain (env vars, EC2 instance role, IRSA on EKS, etc.).
//
// Endpoint and UsePathStyle support MinIO and other S3 gateways: for
// AWS S3 itself, leave Endpoint empty and UsePathStyle=false.
func NewS3(ctx context.Context, cfg Config) (*S3Client, error) {
	if cfg.Bucket == "" {
		return nil, fmt.Errorf("blob: S3 bucket name is required")
	}

	loadOpts := []func(*awsconfig.LoadOptions) error{}
	if cfg.Region != "" {
		loadOpts = append(loadOpts, awsconfig.WithRegion(cfg.Region))
	}
	if cfg.AccessKeyID != "" && cfg.SecretAccessKey != "" {
		loadOpts = append(loadOpts, awsconfig.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(cfg.AccessKeyID, cfg.SecretAccessKey, ""),
		))
	}

	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, loadOpts...)
	if err != nil {
		return nil, fmt.Errorf("blob: load aws config: %w", err)
	}

	clientOpts := []func(*s3.Options){}
	if cfg.Endpoint != "" {
		clientOpts = append(clientOpts, func(o *s3.Options) {
			o.BaseEndpoint = aws.String(cfg.Endpoint)
		})
	}
	if cfg.UsePathStyle {
		clientOpts = append(clientOpts, func(o *s3.Options) {
			o.UsePathStyle = true
		})
	}

	return &S3Client{
		client: s3.NewFromConfig(awsCfg, clientOpts...),
		bucket: cfg.Bucket,
	}, nil
}

// PutArtifact uploads body under the canonical artifact key. The
// upload is idempotent — re-PUTting the same (run, node, version)
// overwrites with byte-identical content per ArtifactKey contract.
func (c *S3Client) PutArtifact(ctx context.Context, runID, nodeID string, version int, body []byte) error {
	key := artifactKey(runID, nodeID, version)
	_, err := c.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(c.bucket),
		Key:         aws.String(key),
		Body:        bytes.NewReader(body),
		ContentType: aws.String("application/json"),
	})
	if err != nil {
		return fmt.Errorf("blob: put %s: %w", key, err)
	}
	return nil
}

// GetArtifact fetches the previously-PUT body. Returns
// ErrArtifactNotFound when the key is absent so callers can branch
// without parsing AWS error codes themselves.
func (c *S3Client) GetArtifact(ctx context.Context, runID, nodeID string, version int) ([]byte, error) {
	key := artifactKey(runID, nodeID, version)
	out, err := c.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		if isS3NotFound(err) {
			return nil, fmt.Errorf("%w: %s", ErrArtifactNotFound, key)
		}
		return nil, fmt.Errorf("blob: get %s: %w", key, err)
	}
	defer out.Body.Close()

	body, err := io.ReadAll(out.Body)
	if err != nil {
		return nil, fmt.Errorf("blob: read %s: %w", key, err)
	}
	return body, nil
}

// ListArtifactVersions enumerates every persisted version under
// artifacts/<runID>/<nodeID>/. Returns ErrArtifactNotFound when the
// prefix has no objects so callers don't need to special-case empty
// slices vs missing nodes.
func (c *S3Client) ListArtifactVersions(ctx context.Context, runID, nodeID string) ([]int, error) {
	prefix := fmt.Sprintf("artifacts/%s/%s/", runID, nodeID)
	versions := []int{}

	pager := s3.NewListObjectsV2Paginator(c.client, &s3.ListObjectsV2Input{
		Bucket: aws.String(c.bucket),
		Prefix: aws.String(prefix),
	})
	for pager.HasMorePages() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("blob: list %s: %w", prefix, err)
		}
		for _, obj := range page.Contents {
			if obj.Key == nil {
				continue
			}
			name := strings.TrimPrefix(*obj.Key, prefix)
			name = strings.TrimSuffix(name, ".json")
			v, err := strconv.Atoi(name)
			if err != nil {
				// Stray non-numeric object — skip silently. The
				// canonical key layout never produces these; a third
				// party writing under our prefix is the operator's
				// problem to clean up.
				continue
			}
			versions = append(versions, v)
		}
	}
	if len(versions) == 0 {
		return nil, fmt.Errorf("%w: %s", ErrArtifactNotFound, prefix)
	}
	return versions, nil
}

// DeleteRun sweeps every artifact under artifacts/<runID>/. We page
// through the list and batch-delete in chunks of 1000 (the S3
// DeleteObjects ceiling). Best-effort: a single page failure logs and
// continues so retention pressure doesn't pile up because of one
// transient error.
func (c *S3Client) DeleteRun(ctx context.Context, runID string) error {
	prefix := fmt.Sprintf("artifacts/%s/", runID)

	pager := s3.NewListObjectsV2Paginator(c.client, &s3.ListObjectsV2Input{
		Bucket: aws.String(c.bucket),
		Prefix: aws.String(prefix),
	})
	for pager.HasMorePages() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return fmt.Errorf("blob: list %s for delete: %w", prefix, err)
		}
		if len(page.Contents) == 0 {
			continue
		}
		ids := make([]types.ObjectIdentifier, 0, len(page.Contents))
		for _, obj := range page.Contents {
			if obj.Key == nil {
				continue
			}
			ids = append(ids, types.ObjectIdentifier{Key: obj.Key})
		}
		if len(ids) == 0 {
			continue
		}
		_, err = c.client.DeleteObjects(ctx, &s3.DeleteObjectsInput{
			Bucket: aws.String(c.bucket),
			Delete: &types.Delete{Objects: ids, Quiet: aws.Bool(true)},
		})
		if err != nil {
			return fmt.Errorf("blob: delete page under %s: %w", prefix, err)
		}
	}
	return nil
}

// isS3NotFound matches both the typed NoSuchKey error and the bare
// 404 surfaced when HeadObject is invoked behind GetObject — both
// shapes occur depending on the gateway and SDK code path.
func isS3NotFound(err error) bool {
	var nsk *types.NoSuchKey
	if errors.As(err, &nsk) {
		return true
	}
	var nf *types.NotFound
	if errors.As(err, &nf) {
		return true
	}
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.ErrorCode() {
		case "NoSuchKey", "NotFound", "404":
			return true
		}
	}
	return false
}

// Compile-time assertion that *S3Client implements Client.
var _ Client = (*S3Client)(nil)
