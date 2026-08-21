package blob

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

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

// Ping verifies the configured bucket is reachable and the credentials
// can read its metadata. Lightweight HEAD — no list, no read. Returns
// the wrapped SDK error on failure.
func (c *S3Client) Ping(ctx context.Context) error {
	_, err := c.client.HeadBucket(ctx, &s3.HeadBucketInput{
		Bucket: aws.String(c.bucket),
	})
	if err != nil {
		return fmt.Errorf("blob: head bucket %s: %w", c.bucket, err)
	}
	return nil
}

// Close releases idle connections held by the underlying HTTP client.
// AWS SDK v2 has no explicit client lifecycle, but each session keeps
// a connection pool that survives until process exit unless we close
// idle connections explicitly. Used by boot paths that need to clean
// up on partial-init failure.
func (c *S3Client) Close() error {
	if c == nil || c.client == nil {
		return nil
	}
	type idleConnCloser interface {
		CloseIdleConnections()
	}
	if hc, ok := c.client.Options().HTTPClient.(idleConnCloser); ok {
		hc.CloseIdleConnections()
	}
	return nil
}

// PutArtifact uploads body under the canonical artifact key. The
// upload is idempotent — re-PUTting the same (run, node, version)
// overwrites with byte-identical content per ArtifactKey contract.
func (c *S3Client) PutArtifact(ctx context.Context, runID, nodeID string, version int, body []byte) error {
	key, err := artifactKey(runID, nodeID, version)
	if err != nil {
		return err
	}
	_, err = c.client.PutObject(ctx, &s3.PutObjectInput{
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
	key, err := artifactKey(runID, nodeID, version)
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
// DeleteObjects ceiling). Best-effort: page list AND delete failures
// are accumulated rather than aborting the sweep — a single transient
// blip would otherwise leave thousands of orphaned objects.
//
// Returns the joined error if anything failed (callers can errors.Is
// against ctx.Err to distinguish cancellation from backend errors).
// Returns nil only when every page listed and every delete batch
// succeeded.
func (c *S3Client) DeleteRun(ctx context.Context, runID string) error {
	prefix := fmt.Sprintf("artifacts/%s/", runID)

	var collected []error
	pager := s3.NewListObjectsV2Paginator(c.client, &s3.ListObjectsV2Input{
		Bucket: aws.String(c.bucket),
		Prefix: aws.String(prefix),
	})
	for pager.HasMorePages() {
		// Honour cancellation eagerly so a SIGTERM doesn't try to
		// drain the entire prefix.
		if err := ctx.Err(); err != nil {
			collected = append(collected, err)
			break
		}
		page, err := pager.NextPage(ctx)
		if err != nil {
			collected = append(collected, fmt.Errorf("blob: list %s page: %w", prefix, err))
			continue
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
		// DeleteObjects returns 200 with a per-object error list rather
		// than a top-level error for cases like governance / legal
		// hold, eventual consistency, or slow replicas. With Quiet=true
		// the response only contains errors. Capturing them into
		// `collected` so the retention sweeper actually reports
		// orphans instead of silently leaving blobs behind.
		out, err := c.client.DeleteObjects(ctx, &s3.DeleteObjectsInput{
			Bucket: aws.String(c.bucket),
			Delete: &types.Delete{Objects: ids, Quiet: aws.Bool(true)},
		})
		if err != nil {
			collected = append(collected, fmt.Errorf("blob: delete page under %s: %w", prefix, err))
		}
		if out != nil {
			for _, oerr := range out.Errors {
				key := ""
				if oerr.Key != nil {
					key = *oerr.Key
				}
				code := ""
				if oerr.Code != nil {
					code = *oerr.Code
				}
				msg := ""
				if oerr.Message != nil {
					msg = *oerr.Message
				}
				collected = append(collected, fmt.Errorf("blob: delete %s: %s (%s)", key, msg, code))
			}
		}
	}
	if len(collected) > 0 {
		return errors.Join(collected...)
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

// ---------------------------------------------------------------------------
// Attachments
// ---------------------------------------------------------------------------

// PutAttachment uploads attachment bytes under the canonical key.
// Idempotent: re-PUTting the same key replaces the bytes.
func (c *S3Client) PutAttachment(ctx context.Context, runID, name, filename, contentType string, body []byte) error {
	key, err := attachmentKey(runID, name, filename)
	if err != nil {
		return err
	}
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	_, err = c.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(c.bucket),
		Key:         aws.String(key),
		Body:        bytes.NewReader(body),
		ContentType: aws.String(contentType),
	})
	if err != nil {
		return fmt.Errorf("blob: put attachment %s: %w", key, err)
	}
	return nil
}

// GetAttachment streams the attachment bytes back. Callers must Close.
func (c *S3Client) GetAttachment(ctx context.Context, runID, name, filename string) (io.ReadCloser, AttachmentMeta, error) {
	key, err := attachmentKey(runID, name, filename)
	if err != nil {
		return nil, AttachmentMeta{}, err
	}
	out, err := c.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		if isS3NotFound(err) {
			return nil, AttachmentMeta{}, fmt.Errorf("%w: %s", ErrArtifactNotFound, key)
		}
		return nil, AttachmentMeta{}, fmt.Errorf("blob: get attachment %s: %w", key, err)
	}
	meta := AttachmentMeta{}
	if out.ContentType != nil {
		meta.ContentType = *out.ContentType
	}
	if out.ContentLength != nil {
		meta.Size = *out.ContentLength
	}
	if out.LastModified != nil {
		meta.LastModified = *out.LastModified
	}
	return out.Body, meta, nil
}

// PresignAttachment emits a SigV4 GET URL valid for ttl. The presign
// expiry is clamped by S3 to 7 days (SigV4 ceiling).
func (c *S3Client) PresignAttachment(ctx context.Context, runID, name, filename string, ttl time.Duration) (string, error) {
	if ttl <= 0 {
		ttl = 10 * time.Minute
	}
	key, err := attachmentKey(runID, name, filename)
	if err != nil {
		return "", err
	}
	presigner := s3.NewPresignClient(c.client)
	req, err := presigner.PresignGetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(key),
	}, s3.WithPresignExpires(ttl))
	if err != nil {
		return "", fmt.Errorf("blob: presign attachment %s: %w", key, err)
	}
	return req.URL, nil
}

// DeleteAttachment removes a single attachment blob. Idempotent: a
// missing key returns nil so callers performing rollback don't error
// on a write that never reached S3.
func (c *S3Client) DeleteAttachment(ctx context.Context, runID, name, filename string) error {
	key, err := attachmentKey(runID, name, filename)
	if err != nil {
		return err
	}
	if _, err := c.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(key),
	}); err != nil {
		return fmt.Errorf("blob: delete attachment %s: %w", key, err)
	}
	return nil
}

// DeleteRunAttachments sweeps every blob under attachments/<runID>/.
// Mirrors DeleteRun's batched, best-effort semantics.
func (c *S3Client) DeleteRunAttachments(ctx context.Context, runID string) error {
	prefix, err := attachmentRunPrefix(runID)
	if err != nil {
		return err
	}

	var collected []error
	pager := s3.NewListObjectsV2Paginator(c.client, &s3.ListObjectsV2Input{
		Bucket: aws.String(c.bucket),
		Prefix: aws.String(prefix),
	})
	for pager.HasMorePages() {
		if err := ctx.Err(); err != nil {
			collected = append(collected, err)
			break
		}
		page, err := pager.NextPage(ctx)
		if err != nil {
			collected = append(collected, fmt.Errorf("blob: list %s page: %w", prefix, err))
			continue
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
		if _, err := c.client.DeleteObjects(ctx, &s3.DeleteObjectsInput{
			Bucket: aws.String(c.bucket),
			Delete: &types.Delete{Objects: ids, Quiet: aws.Bool(true)},
		}); err != nil {
			collected = append(collected, fmt.Errorf("blob: delete attachment page under %s: %w", prefix, err))
		}
	}
	if len(collected) > 0 {
		return errors.Join(collected...)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Tool-call I/O blobs (cloud ToolBlobStore twin)
// ---------------------------------------------------------------------------

// PutToolBlob uploads a per-tool-call I/O body under the canonical tool
// key. Idempotent: re-PUTting the same (run, tool_use_id, kind) replaces
// the bytes.
func (c *S3Client) PutToolBlob(ctx context.Context, runID, toolUseID, kind string, body []byte) error {
	key, err := toolBlobKey(runID, toolUseID, kind)
	if err != nil {
		return err
	}
	_, err = c.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(c.bucket),
		Key:         aws.String(key),
		Body:        bytes.NewReader(body),
		ContentType: aws.String("text/plain; charset=utf-8"),
	})
	if err != nil {
		return fmt.Errorf("blob: put tool blob %s: %w", key, err)
	}
	return nil
}

// GetToolBlobRange serves a byte window of a tool blob. It first HEADs
// the object for its total size (portable across S3/MinIO — a 416
// InvalidRange response carries a gateway-dependent Content-Range we
// don't want to parse), then issues a bounded Range GET. offset past the
// end returns (nil, total, true, nil) without a second request.
func (c *S3Client) GetToolBlobRange(ctx context.Context, runID, toolUseID, kind string, offset, limit int64) ([]byte, int64, bool, error) {
	key, err := toolBlobKey(runID, toolUseID, kind)
	if err != nil {
		return nil, 0, false, err
	}
	if offset < 0 {
		offset = 0
	}
	head, err := c.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		if isS3NotFound(err) {
			return nil, 0, false, fmt.Errorf("%w: %s", ErrArtifactNotFound, key)
		}
		return nil, 0, false, fmt.Errorf("blob: head tool blob %s: %w", key, err)
	}
	var total int64
	if head.ContentLength != nil {
		total = *head.ContentLength
	}
	if offset >= total {
		return nil, total, true, nil
	}
	readLen := total - offset
	if limit > 0 && limit < readLen {
		readLen = limit
	}
	// HTTP Range is inclusive on both ends: bytes=offset-(offset+readLen-1).
	rangeHdr := fmt.Sprintf("bytes=%d-%d", offset, offset+readLen-1)
	out, err := c.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(key),
		Range:  aws.String(rangeHdr),
	})
	if err != nil {
		if isS3NotFound(err) {
			return nil, total, false, fmt.Errorf("%w: %s", ErrArtifactNotFound, key)
		}
		return nil, total, false, fmt.Errorf("blob: get tool blob %s: %w", key, err)
	}
	defer out.Body.Close()
	data, err := io.ReadAll(out.Body)
	if err != nil {
		return nil, total, false, fmt.Errorf("blob: read tool blob %s: %w", key, err)
	}
	eof := offset+int64(len(data)) >= total
	return data, total, eof, nil
}

// PutIRBlob uploads an out-of-band compiled IR under ir/<runID>.json.
// Idempotent: re-PUTting the same run replaces the bytes.
func (c *S3Client) PutIRBlob(ctx context.Context, runID string, body []byte) error {
	key, err := irBlobKey(runID)
	if err != nil {
		return err
	}
	_, err = c.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(c.bucket),
		Key:         aws.String(key),
		Body:        bytes.NewReader(body),
		ContentType: aws.String("application/json"),
	})
	if err != nil {
		return fmt.Errorf("blob: put IR blob %s: %w", key, err)
	}
	return nil
}

// GetIRBlob fetches the IR bytes for the given storage key (as carried on
// queue.IRRef.StorageKey). The key is re-validated against the canonical
// ir/<run_id>.json shape before use so a tampered reference on the wire
// can never widen the key space. Returns ErrArtifactNotFound when absent.
func (c *S3Client) GetIRBlob(ctx context.Context, storageKey string) ([]byte, error) {
	key, err := validateIRBlobKey(storageKey)
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
		return nil, fmt.Errorf("blob: get IR blob %s: %w", key, err)
	}
	defer out.Body.Close()
	body, err := io.ReadAll(out.Body)
	if err != nil {
		return nil, fmt.Errorf("blob: read IR blob %s: %w", key, err)
	}
	return body, nil
}

// DeleteRunIR removes the ir/<runID>.json blob. Idempotent, best-effort.
func (c *S3Client) DeleteRunIR(ctx context.Context, runID string) error {
	key, err := irBlobKey(runID)
	if err != nil {
		return err
	}
	if _, err := c.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(key),
	}); err != nil {
		return fmt.Errorf("blob: delete IR blob %s: %w", key, err)
	}
	return nil
}

func (c *S3Client) PutBackendSession(ctx context.Context, runID, ref string, body []byte) error {
	key, err := backendSessionKey(runID, ref)
	if err != nil {
		return err
	}
	_, err = c.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(c.bucket),
		Key:         aws.String(key),
		Body:        bytes.NewReader(body),
		ContentType: aws.String("application/octet-stream"),
	})
	if err != nil {
		return fmt.Errorf("blob: put backend session %s: %w", key, err)
	}
	return nil
}

func (c *S3Client) GetBackendSession(ctx context.Context, runID, ref string) ([]byte, error) {
	key, err := backendSessionKey(runID, ref)
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
		return nil, fmt.Errorf("blob: get backend session %s: %w", key, err)
	}
	defer out.Body.Close()
	body, err := io.ReadAll(out.Body)
	if err != nil {
		return nil, fmt.Errorf("blob: read backend session %s: %w", key, err)
	}
	return body, nil
}

func (c *S3Client) DeleteBackendSession(ctx context.Context, runID, ref string) error {
	key, err := backendSessionKey(runID, ref)
	if err != nil {
		return err
	}
	if _, err := c.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(key),
	}); err != nil {
		return fmt.Errorf("blob: delete backend session %s: %w", key, err)
	}
	return nil
}

func (c *S3Client) DeleteRunBackendSessions(ctx context.Context, runID string) error {
	prefix, err := backendSessionRunPrefix(runID)
	if err != nil {
		return err
	}
	var collected []error
	pager := s3.NewListObjectsV2Paginator(c.client, &s3.ListObjectsV2Input{
		Bucket: aws.String(c.bucket),
		Prefix: aws.String(prefix),
	})
	for pager.HasMorePages() {
		if err := ctx.Err(); err != nil {
			collected = append(collected, err)
			break
		}
		page, err := pager.NextPage(ctx)
		if err != nil {
			collected = append(collected, fmt.Errorf("blob: list %s page: %w", prefix, err))
			continue
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
		if _, err := c.client.DeleteObjects(ctx, &s3.DeleteObjectsInput{
			Bucket: aws.String(c.bucket),
			Delete: &types.Delete{Objects: ids, Quiet: aws.Bool(true)},
		}); err != nil {
			collected = append(collected, fmt.Errorf("blob: delete session page under %s: %w", prefix, err))
		}
	}
	if len(collected) > 0 {
		return errors.Join(collected...)
	}
	return nil
}

// DeleteRunToolBlobs sweeps every blob under tools/<runID>/. Mirrors
// DeleteRunAttachments' batched, best-effort semantics.
func (c *S3Client) DeleteRunToolBlobs(ctx context.Context, runID string) error {
	prefix, err := toolBlobRunPrefix(runID)
	if err != nil {
		return err
	}

	var collected []error
	pager := s3.NewListObjectsV2Paginator(c.client, &s3.ListObjectsV2Input{
		Bucket: aws.String(c.bucket),
		Prefix: aws.String(prefix),
	})
	for pager.HasMorePages() {
		if err := ctx.Err(); err != nil {
			collected = append(collected, err)
			break
		}
		page, err := pager.NextPage(ctx)
		if err != nil {
			collected = append(collected, fmt.Errorf("blob: list %s page: %w", prefix, err))
			continue
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
		if _, err := c.client.DeleteObjects(ctx, &s3.DeleteObjectsInput{
			Bucket: aws.String(c.bucket),
			Delete: &types.Delete{Objects: ids, Quiet: aws.Bool(true)},
		}); err != nil {
			collected = append(collected, fmt.Errorf("blob: delete tool blob page under %s: %w", prefix, err))
		}
	}
	if len(collected) > 0 {
		return errors.Join(collected...)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Tool-produced artifact files (cloud RunFilesStore twin)
// ---------------------------------------------------------------------------

// PutRunFile streams one artifact file under runfiles/<runID>/<relPath>.
// Idempotent. Unlike attachments, run files may be large audio/video outputs,
// so the caller's reader is passed directly to the S3 client instead of first
// materialising the complete body in runner memory.
func (c *S3Client) PutRunFile(ctx context.Context, runID, relPath, contentType string, body io.Reader, size int64) error {
	key, err := runFileKey(runID, relPath)
	if err != nil {
		return err
	}
	if body == nil {
		return fmt.Errorf("blob: put run file %s: nil body", key)
	}
	if size < 0 {
		return fmt.Errorf("blob: put run file %s: negative size %d", key, size)
	}
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	_, err = c.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(c.bucket),
		Key:           aws.String(key),
		Body:          body,
		ContentLength: aws.Int64(size),
		ContentType:   aws.String(contentType),
	})
	if err != nil {
		return fmt.Errorf("blob: put run file %s: %w", key, err)
	}
	return nil
}

// ListRunFiles enumerates artifact files under runfiles/<runID>/,
// returning area-relative paths. Empty slice (no error) when none.
func (c *S3Client) ListRunFiles(ctx context.Context, runID string) ([]RunFileObject, error) {
	prefix, err := runFileRunPrefix(runID)
	if err != nil {
		return nil, err
	}
	var out []RunFileObject
	pager := s3.NewListObjectsV2Paginator(c.client, &s3.ListObjectsV2Input{
		Bucket: aws.String(c.bucket),
		Prefix: aws.String(prefix),
	})
	for pager.HasMorePages() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("blob: list run files %s: %w", prefix, err)
		}
		for _, obj := range page.Contents {
			if obj.Key == nil {
				continue
			}
			rel := strings.TrimPrefix(*obj.Key, prefix)
			if rel == "" {
				continue
			}
			info := RunFileObject{Path: rel}
			if obj.Size != nil {
				info.Size = *obj.Size
			}
			if obj.LastModified != nil {
				info.ModifiedAt = obj.LastModified.UTC()
			}
			out = append(out, info)
		}
	}
	return out, nil
}

// GetRunFile streams one artifact file. Callers must Close. Returns
// ErrArtifactNotFound when the key is absent.
func (c *S3Client) GetRunFile(ctx context.Context, runID, relPath string) (io.ReadCloser, RunFileObject, error) {
	key, err := runFileKey(runID, relPath)
	if err != nil {
		return nil, RunFileObject{}, err
	}
	out, err := c.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		if isS3NotFound(err) {
			return nil, RunFileObject{}, fmt.Errorf("%w: %s", ErrArtifactNotFound, key)
		}
		return nil, RunFileObject{}, fmt.Errorf("blob: get run file %s: %w", key, err)
	}
	// relPath was validated by runFileKey; report the cleaned prefix-
	// relative path so callers get a stable, area-relative value.
	rel := strings.TrimPrefix(key, fmt.Sprintf("runfiles/%s/", runID))
	info := RunFileObject{Path: rel}
	if out.ContentLength != nil {
		info.Size = *out.ContentLength
	}
	if out.LastModified != nil {
		info.ModifiedAt = out.LastModified.UTC()
	}
	return out.Body, info, nil
}

// DeleteRunFiles sweeps every blob under runfiles/<runID>/. Mirrors
// DeleteRunAttachments' batched, best-effort semantics.
func (c *S3Client) DeleteRunFiles(ctx context.Context, runID string) error {
	prefix, err := runFileRunPrefix(runID)
	if err != nil {
		return err
	}

	var collected []error
	pager := s3.NewListObjectsV2Paginator(c.client, &s3.ListObjectsV2Input{
		Bucket: aws.String(c.bucket),
		Prefix: aws.String(prefix),
	})
	for pager.HasMorePages() {
		if err := ctx.Err(); err != nil {
			collected = append(collected, err)
			break
		}
		page, err := pager.NextPage(ctx)
		if err != nil {
			collected = append(collected, fmt.Errorf("blob: list %s page: %w", prefix, err))
			continue
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
		if _, err := c.client.DeleteObjects(ctx, &s3.DeleteObjectsInput{
			Bucket: aws.String(c.bucket),
			Delete: &types.Delete{Objects: ids, Quiet: aws.Bool(true)},
		}); err != nil {
			collected = append(collected, fmt.Errorf("blob: delete run file page under %s: %w", prefix, err))
		}
	}
	if len(collected) > 0 {
		return errors.Join(collected...)
	}
	return nil
}

// Compile-time assertion that *S3Client implements Client.
var _ Client = (*S3Client)(nil)
