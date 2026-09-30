package blob

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// skippedValidationLog is the line the SDK writes, through the logger
// LoadDefaultConfig installs on stderr, for a GET whose response carries
// no checksum to validate.
const skippedValidationLog = "Response has no supported checksum"

// captureStderr runs fn with os.Stderr redirected and returns what was
// written to it. The SDK's default logger binds os.Stderr when the config
// is loaded, so fn must load it (NewS3 does). Not for parallel tests.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	done := make(chan string)
	go func() {
		var b bytes.Buffer
		_, _ = io.Copy(&b, r)
		done <- b.String()
	}()
	old := os.Stderr
	os.Stderr = w
	func() {
		defer func() { os.Stderr = old }()
		fn()
	}()
	_ = w.Close()
	out := <-done
	_ = r.Close()
	return out
}

// An object a store migration copied (rclone stores no checksum) comes
// back without any x-amz-checksum-* header — the fake gateway serves every
// GET that way. The SDK skips the validation of such a response, and by
// default logs a WARN for each one: after a migration, one line per read
// of a migrated object. NewS3 keeps the validation and drops only that line.
func TestS3GetOfAnObjectStoredWithoutChecksumLogsNothing(t *testing.T) {
	// The SDK's default response-checksum mode, whatever the host sets, and
	// no shared AWS config from the host.
	t.Setenv("AWS_RESPONSE_CHECKSUM_VALIDATION", "when_supported")
	t.Setenv("AWS_CONFIG_FILE", filepath.Join(t.TempDir(), "config"))
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(t.TempDir(), "credentials"))

	ctx := context.Background()
	f, srv := newFakeS3(t, "bkt")
	key, err := artifactKey("run-1", "node-a", 0)
	if err != nil {
		t.Fatalf("artifactKey: %v", err)
	}
	f.mu.Lock()
	f.objects[key] = []byte(`{"v":1}`)
	f.mu.Unlock()

	// Witness: the SDK client LoadDefaultConfig builds, without NewS3's
	// option, does log the line through this harness on the same GET.
	witness := captureStderr(t, func() {
		cfg, err := awsconfig.LoadDefaultConfig(ctx,
			awsconfig.WithRegion("us-east-1"),
			awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider("test-key", "test-secret", "")))
		if err != nil {
			t.Fatalf("LoadDefaultConfig: %v", err)
		}
		raw := s3.NewFromConfig(cfg, func(o *s3.Options) {
			o.BaseEndpoint = aws.String(srv.URL)
			o.UsePathStyle = true
		})
		out, err := raw.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String("bkt"), Key: aws.String(key)})
		if err != nil {
			t.Fatalf("witness GetObject: %v", err)
		}
		_, _ = io.Copy(io.Discard, out.Body)
		_ = out.Body.Close()
	})
	if !strings.Contains(witness, skippedValidationLog) {
		t.Fatalf("witness: a plain SDK client logged no %q on a checksum-less GET, so this test cannot see the line it guards against; stderr: %q", skippedValidationLog, witness)
	}

	var body []byte
	got := captureStderr(t, func() {
		c := newTestS3Client(t, srv.URL, "bkt")
		body, err = c.GetArtifact(ctx, "run-1", "node-a", 0)
	})
	if err != nil {
		t.Fatalf("GetArtifact: %v", err)
	}
	if string(body) != `{"v":1}` {
		t.Fatalf("GetArtifact body = %q, want %q", body, `{"v":1}`)
	}
	if strings.Contains(got, skippedValidationLog) {
		t.Fatalf("NewS3's client logged %q on a GET of an object stored without a checksum; stderr: %q", skippedValidationLog, got)
	}
}
