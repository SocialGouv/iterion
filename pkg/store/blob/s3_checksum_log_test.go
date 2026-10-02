package blob

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go/logging"
)

// skippedValidationLog is the line the SDK logs for a GET whose response
// carries no checksum to validate.
const skippedValidationLog = "Response has no supported checksum"

// recordingLogger keeps what the SDK logs during one operation.
type recordingLogger struct {
	mu    sync.Mutex
	lines []string
}

func (r *recordingLogger) Logf(c logging.Classification, format string, v ...interface{}) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lines = append(r.lines, string(c)+" "+fmt.Sprintf(format, v...))
}

func (r *recordingLogger) String() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return strings.Join(r.lines, "\n")
}

// An object a store migration copied (rclone stores no checksum) comes
// back without any x-amz-checksum-* header — the fake gateway serves every
// GET that way. The SDK skips the validation of such a response and, by
// default, logs a WARN for each one: after a migration, one line per read
// of a migrated object. The client NewS3 builds does not log it.
func TestS3GetOfAnObjectStoredWithoutChecksumLogsNothing(t *testing.T) {
	ctx := context.Background()
	f, srv := newFakeS3(t, "bkt")
	key, err := artifactKey("run-1", "node-a", 0)
	if err != nil {
		t.Fatalf("artifactKey: %v", err)
	}
	seed(f, key)
	c := newTestS3Client(t, srv.URL, "bkt")

	// get reads the object through NewS3's client with a logger of its own
	// for this one call (whatever logger the client was built with), the
	// response checksum validated when present whatever the host's AWS
	// config says, plus the options given.
	get := func(opts ...func(*s3.Options)) string {
		rec := &recordingLogger{}
		opts = append([]func(*s3.Options){func(o *s3.Options) {
			o.Logger = rec
			o.ResponseChecksumValidation = aws.ResponseChecksumValidationWhenSupported
		}}, opts...)
		out, err := c.client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String("bkt"), Key: aws.String(key)}, opts...)
		if err != nil {
			t.Fatalf("GetObject: %v", err)
		}
		_, _ = io.Copy(io.Discard, out.Body)
		_ = out.Body.Close()
		return rec.String()
	}

	// Witness: the same client with that one log switched back on for this
	// call does log the line, so the assertion below can see it.
	if logged := get(func(o *s3.Options) { o.DisableLogOutputChecksumValidationSkipped = false }); !strings.Contains(logged, skippedValidationLog) {
		t.Fatalf("witness: with the skipped-validation log on, a checksum-less GET logged no %q, so this test cannot see the line it guards against; logged: %q", skippedValidationLog, logged)
	}
	if logged := get(); strings.Contains(logged, skippedValidationLog) {
		t.Fatalf("NewS3's client logged %q on a GET of an object stored without a checksum; logged: %q", skippedValidationLog, logged)
	}
}
