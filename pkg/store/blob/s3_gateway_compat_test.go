package blob

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"testing"
	"time"
)

// Compatibility bench against a REAL S3 gateway.
//
// s3_roundtrip_test.go proves the client against an in-process double that
// answers whatever the double was written to answer. That says nothing about
// a gateway the double does not model: SigV4 details, trailing checksums,
// ListObjectsV2 continuation tokens, DeleteObjects per-object results,
// presigned-URL verification. Those only show up against the artefact that
// actually runs.
//
// Every case here drives the SAME exported S3Client methods the server and
// runner use, with no gateway-specific branch. Point it at a gateway with:
//
//	ITERION_TEST_S3_ENDPOINT=http://host:8333 \
//	ITERION_TEST_S3_ACCESS_KEY_ID=... ITERION_TEST_S3_SECRET_ACCESS_KEY=... \
//	ITERION_TEST_S3_BUCKET=iterion-artifacts \
//	go test -run TestGatewayCompat ./pkg/store/blob/
//
// Absent endpoint = skip, so the default suite is unchanged.

func gatewayClient(t *testing.T) (*S3Client, string) {
	t.Helper()
	endpoint := os.Getenv("ITERION_TEST_S3_ENDPOINT")
	if endpoint == "" {
		t.Skip("ITERION_TEST_S3_ENDPOINT unset: no live gateway to exercise")
	}
	bucket := os.Getenv("ITERION_TEST_S3_BUCKET")
	if bucket == "" {
		bucket = "iterion-artifacts"
	}
	c, err := NewS3(context.Background(), Config{
		Region:          "us-east-1",
		Bucket:          bucket,
		Endpoint:        endpoint,
		UsePathStyle:    true,
		AccessKeyID:     os.Getenv("ITERION_TEST_S3_ACCESS_KEY_ID"),
		SecretAccessKey: os.Getenv("ITERION_TEST_S3_SECRET_ACCESS_KEY"),
	})
	if err != nil {
		t.Fatalf("NewS3: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c, endpoint
}

// runID keeps each case in its own prefix so a case never observes or sweeps
// another's objects, and a re-run of the suite starts clean.
func gatewayRunID(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf("compat-%s-%d", strings.ToLower(strings.ReplaceAll(t.Name(), "/", "-")), time.Now().UnixNano())
}

// HeadBucket — what /readyz gates on.
func TestGatewayCompat_PingHeadBucket(t *testing.T) {
	c, _ := gatewayClient(t)
	if err := c.Ping(context.Background()); err != nil {
		t.Fatalf("Ping (HeadBucket): %v", err)
	}
}

// PutObject / GetObject / ListObjectsV2(prefix) — the artifact round-trip,
// mirroring TestS3Client_ArtifactUploadRoundTripAndLayout's assertions but
// against the live gateway.
func TestGatewayCompat_ArtifactRoundTripAndLayout(t *testing.T) {
	c, _ := gatewayClient(t)
	ctx := context.Background()
	run := gatewayRunID(t)

	bodies := map[int][]byte{
		0: []byte(`{"verdict":"first pass"}`),
		1: []byte(`{"verdict":"second pass","note":"héllo ✓"}`),
		2: []byte(`{"verdict":"third"}`),
	}
	for v, b := range bodies {
		if err := c.PutArtifact(ctx, run, "review", v, b); err != nil {
			t.Fatalf("PutArtifact v%d: %v", v, err)
		}
	}
	for v, b := range bodies {
		back, err := c.GetArtifact(ctx, run, "review", v)
		if err != nil {
			t.Fatalf("GetArtifact v%d: %v", v, err)
		}
		if !bytes.Equal(back, b) {
			t.Fatalf("v%d round-trip: got %q want %q", v, back, b)
		}
	}

	versions, err := c.ListArtifactVersions(ctx, run, "review")
	if err != nil {
		t.Fatalf("ListArtifactVersions: %v", err)
	}
	sort.Ints(versions)
	if len(versions) != 3 || versions[0] != 0 || versions[2] != 2 {
		t.Fatalf("versions = %v, want [0 1 2]", versions)
	}

	// Re-PUT overwrites in place rather than creating a second object.
	if err := c.PutArtifact(ctx, run, "review", 1, []byte(`{"verdict":"rewritten"}`)); err != nil {
		t.Fatalf("re-PutArtifact: %v", err)
	}
	versions, err = c.ListArtifactVersions(ctx, run, "review")
	if err != nil {
		t.Fatalf("ListArtifactVersions after re-put: %v", err)
	}
	if len(versions) != 3 {
		t.Fatalf("a re-upload changed the object count: %v", versions)
	}
	back, err := c.GetArtifact(ctx, run, "review", 1)
	if err != nil || string(back) != `{"verdict":"rewritten"}` {
		t.Fatalf("re-upload did not overwrite: %q (%v)", back, err)
	}

	if err := c.DeleteRun(ctx, run); err != nil {
		t.Fatalf("DeleteRun cleanup: %v", err)
	}
}

// A missing key must surface as ErrArtifactNotFound: the gateway's 404 shape
// (typed NoSuchKey vs a bare 404) has to survive isS3NotFound.
func TestGatewayCompat_MissingArtifactMapsToNotFound(t *testing.T) {
	c, _ := gatewayClient(t)
	ctx := context.Background()
	run := gatewayRunID(t)

	if _, err := c.GetArtifact(ctx, run, "node", 0); !errors.Is(err, ErrArtifactNotFound) {
		t.Fatalf("GetArtifact on a missing key: %v, want ErrArtifactNotFound", err)
	}
	if _, err := c.ListArtifactVersions(ctx, run, "node"); !errors.Is(err, ErrArtifactNotFound) {
		t.Fatalf("ListArtifactVersions on an empty prefix: %v, want ErrArtifactNotFound", err)
	}
	if _, _, _, err := c.GetToolBlobRange(ctx, run, "tu-404", "output", 0, 0); !errors.Is(err, ErrArtifactNotFound) {
		t.Fatalf("GetToolBlobRange on a missing key: %v, want ErrArtifactNotFound", err)
	}
	if _, _, err := c.GetAttachment(ctx, run, "att", "missing.txt"); !errors.Is(err, ErrArtifactNotFound) {
		t.Fatalf("GetAttachment on a missing key: %v, want ErrArtifactNotFound", err)
	}
}

// DeleteRun sweeps exactly one run's prefix through the batch DeleteObjects
// API, and leaves the neighbouring run alone.
func TestGatewayCompat_DeleteRunSweepsOnlyThatPrefix(t *testing.T) {
	c, _ := gatewayClient(t)
	ctx := context.Background()
	runA, runB := gatewayRunID(t)+"-a", gatewayRunID(t)+"-b"

	for _, a := range []struct {
		run, node string
		ver       int
	}{
		{runA, "plan", 0}, {runA, "plan", 1}, {runA, "implement", 0},
		{runB, "plan", 0},
	} {
		if err := c.PutArtifact(ctx, a.run, a.node, a.ver, []byte(`{}`)); err != nil {
			t.Fatalf("seed %s/%s/%d: %v", a.run, a.node, a.ver, err)
		}
	}

	if err := c.DeleteRun(ctx, runA); err != nil {
		t.Fatalf("DeleteRun: %v", err)
	}
	if _, err := c.ListArtifactVersions(ctx, runA, "plan"); !errors.Is(err, ErrArtifactNotFound) {
		t.Fatalf("swept prefix still lists: %v", err)
	}
	if _, err := c.GetArtifact(ctx, runA, "plan", 1); !errors.Is(err, ErrArtifactNotFound) {
		t.Fatalf("swept artifact still readable: %v", err)
	}
	vers, err := c.ListArtifactVersions(ctx, runB, "plan")
	if err != nil || len(vers) != 1 {
		t.Fatalf("neighbouring run damaged: %v (%v)", vers, err)
	}
	// Sweeping an empty prefix is a no-op, not an error.
	if err := c.DeleteRun(ctx, gatewayRunID(t)+"-never"); err != nil {
		t.Fatalf("DeleteRun on an empty prefix: %v", err)
	}
	if err := c.DeleteRun(ctx, runB); err != nil {
		t.Fatalf("DeleteRun cleanup: %v", err)
	}
}

// ListObjectsV2 pagination past the 1000-key page, and DeleteObjects with a
// FULL 1000-key batch. A gateway that ignores continuation-token paging
// silently truncates every version listing and orphans every object past the
// first page.
func TestGatewayCompat_ListPaginationAndFullDeleteBatch(t *testing.T) {
	if testing.Short() {
		t.Skip("-short: skipping the 1100-object pagination case")
	}
	c, _ := gatewayClient(t)
	ctx := context.Background()
	run := gatewayRunID(t)

	const n = 1100 // > one 1000-key page, so paging AND a full delete batch
	for v := 0; v < n; v++ {
		if err := c.PutArtifact(ctx, run, "bulk", v, []byte(`{}`)); err != nil {
			t.Fatalf("PutArtifact v%d: %v", v, err)
		}
	}
	versions, err := c.ListArtifactVersions(ctx, run, "bulk")
	if err != nil {
		t.Fatalf("ListArtifactVersions: %v", err)
	}
	if len(versions) != n {
		t.Fatalf("listing returned %d versions, want %d (continuation-token paging dropped objects)", len(versions), n)
	}
	sort.Ints(versions)
	if versions[0] != 0 || versions[n-1] != n-1 {
		t.Fatalf("listing bounds = [%d..%d], want [0..%d]", versions[0], versions[n-1], n-1)
	}

	if err := c.DeleteRun(ctx, run); err != nil {
		t.Fatalf("DeleteRun over %d objects: %v", n, err)
	}
	if _, err := c.ListArtifactVersions(ctx, run, "bulk"); !errors.Is(err, ErrArtifactNotFound) {
		t.Fatalf("prefix not empty after DeleteRun: %v", err)
	}
}

// HeadObject + bounded Range GET — how the studio tails a tool's output.
func TestGatewayCompat_ToolBlobHeadAndRange(t *testing.T) {
	c, _ := gatewayClient(t)
	ctx := context.Background()
	run := gatewayRunID(t)

	body := []byte("0123456789abcdefghijklmnopqrstuvwxyz")
	if err := c.PutToolBlob(ctx, run, "tu-1", "output", body); err != nil {
		t.Fatalf("PutToolBlob: %v", err)
	}

	// Whole object: total from HeadObject's Content-Length, eof true.
	data, total, eof, err := c.GetToolBlobRange(ctx, run, "tu-1", "output", 0, 0)
	if err != nil {
		t.Fatalf("GetToolBlobRange(0,0): %v", err)
	}
	if total != int64(len(body)) {
		t.Fatalf("HeadObject Content-Length = %d, want %d", total, len(body))
	}
	if !bytes.Equal(data, body) || !eof {
		t.Fatalf("full read: %q eof=%v", data, eof)
	}

	// A bounded window: Range must be honoured, not answered with the whole
	// object (a gateway that ignores Range returns 200 + everything, and the
	// studio's tail then re-renders the log from the top on every poll).
	data, total, eof, err = c.GetToolBlobRange(ctx, run, "tu-1", "output", 10, 6)
	if err != nil {
		t.Fatalf("GetToolBlobRange(10,6): %v", err)
	}
	if string(data) != "abcdef" {
		t.Fatalf("Range bytes=10-15 returned %q, want %q", data, "abcdef")
	}
	if eof {
		t.Fatalf("eof true mid-object (total=%d)", total)
	}

	// Offset at/past the end: no second request, eof true, no error.
	data, total, eof, err = c.GetToolBlobRange(ctx, run, "tu-1", "output", int64(len(body)), 0)
	if err != nil || len(data) != 0 || !eof || total != int64(len(body)) {
		t.Fatalf("offset at end: data=%q total=%d eof=%v err=%v", data, total, eof, err)
	}

	if err := c.DeleteRunToolBlobs(ctx, run); err != nil {
		t.Fatalf("DeleteRunToolBlobs: %v", err)
	}
}

// Attachments: PutObject with a caller Content-Type, GetObject returning that
// Content-Type + Content-Length + Last-Modified, then DeleteObject.
func TestGatewayCompat_AttachmentMetadataRoundTrip(t *testing.T) {
	c, _ := gatewayClient(t)
	ctx := context.Background()
	run := gatewayRunID(t)

	payload := []byte("id,label\n1,héllo ✓\n")
	if err := c.PutAttachment(ctx, run, "input", "data.csv", "text/csv; charset=utf-8", payload); err != nil {
		t.Fatalf("PutAttachment: %v", err)
	}
	rc, meta, err := c.GetAttachment(ctx, run, "input", "data.csv")
	if err != nil {
		t.Fatalf("GetAttachment: %v", err)
	}
	got, readErr := io.ReadAll(rc)
	_ = rc.Close()
	if readErr != nil {
		t.Fatalf("read attachment: %v", readErr)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("attachment bytes = %q, want %q", got, payload)
	}
	if meta.ContentType != "text/csv; charset=utf-8" {
		t.Errorf("Content-Type = %q, want %q", meta.ContentType, "text/csv; charset=utf-8")
	}
	if meta.Size != int64(len(payload)) {
		t.Errorf("Content-Length = %d, want %d", meta.Size, len(payload))
	}
	if meta.LastModified.IsZero() {
		t.Errorf("Last-Modified absent: %v", meta.LastModified)
	}

	if err := c.DeleteAttachment(ctx, run, "input", "data.csv"); err != nil {
		t.Fatalf("DeleteAttachment: %v", err)
	}
	if _, _, err := c.GetAttachment(ctx, run, "input", "data.csv"); !errors.Is(err, ErrArtifactNotFound) {
		t.Fatalf("attachment readable after delete: %v", err)
	}
	// Deleting an absent key stays a no-op (rollback paths rely on it).
	if err := c.DeleteAttachment(ctx, run, "input", "data.csv"); err != nil {
		t.Fatalf("DeleteAttachment on a missing key: %v", err)
	}
}

// PresignGetObject: the URL must fetch the bytes with NO credentials, and the
// gateway must REFUSE a tampered or expired one. A gateway that does not
// verify the presigned signature turns every attachment link into a public,
// permanent one.
func TestGatewayCompat_PresignedURLIsHonouredAndVerified(t *testing.T) {
	c, _ := gatewayClient(t)
	ctx := context.Background()
	run := gatewayRunID(t)

	payload := []byte("presigned-body\n")
	if err := c.PutAttachment(ctx, run, "out", "report.txt", "text/plain", payload); err != nil {
		t.Fatalf("PutAttachment: %v", err)
	}
	t.Cleanup(func() { _ = c.DeleteRunAttachments(context.Background(), run) })

	raw, err := c.PresignAttachment(ctx, run, "out", "report.txt", 10*time.Minute)
	if err != nil {
		t.Fatalf("PresignAttachment: %v", err)
	}
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("presigned URL unparseable: %v", err)
	}
	if u.Query().Get("X-Amz-Signature") == "" {
		t.Fatalf("presigned URL carries no X-Amz-Signature: %s", u.RawQuery)
	}

	client := &http.Client{Timeout: 30 * time.Second}

	// 1. As issued: 200 + the bytes, no Authorization header.
	body, status := fetch(t, client, raw)
	if status != http.StatusOK {
		t.Fatalf("presigned GET status = %d, want 200 (body %q)", status, body)
	}
	if !bytes.Equal(body, payload) {
		t.Fatalf("presigned GET body = %q, want %q", body, payload)
	}

	// 2. Tampered signature: must be refused. Accepting it means the
	//    signature is decoration.
	tampered := *u
	q := tampered.Query()
	sig := q.Get("X-Amz-Signature")
	q.Set("X-Amz-Signature", flipLastHex(sig))
	tampered.RawQuery = q.Encode()
	body, status = fetch(t, client, tampered.String())
	if status == http.StatusOK {
		t.Fatalf("a presigned URL with a mangled signature was SERVED (200): %q", body)
	}
	if status != http.StatusForbidden {
		t.Logf("tampered signature refused with %d (not 403): %q", status, body)
	}

	// 3. Tampered key (same signature, different object): must be refused.
	swapped := *u
	swapped.Path = strings.Replace(swapped.Path, "report.txt", "secret.txt", 1)
	body, status = fetch(t, client, swapped.String())
	if status == http.StatusOK {
		t.Fatalf("a presigned URL re-pointed at another key was SERVED (200): %q", body)
	}

	// 4. Expired: presign with a short TTL, wait past it, expect a refusal.
	//
	// The TTL is 5s, not 1s, because SigV4 writes X-Amz-Date truncated DOWN
	// to the second: the effective lifetime is (ttl-1s, ttl], so a 1s URL can
	// be born already expired. Measured on this gateway — first refusal at
	// 405ms for ttl=1s, 1.21s for 2s, 4.04s for 5s, 9.06s for 10s, i.e. ~1s
	// early throughout. 5s leaves >4s of margin for the fresh fetch, and the
	// 7s wait is past any truncation.
	const shortTTL = 5 * time.Second
	shortURL, err := c.PresignAttachment(ctx, run, "out", "report.txt", shortTTL)
	if err != nil {
		t.Fatalf("PresignAttachment(%s): %v", shortTTL, err)
	}
	if _, status = fetch(t, client, shortURL); status != http.StatusOK {
		t.Fatalf("a %s presigned URL was refused while still fresh: %d", shortTTL, status)
	}
	time.Sleep(shortTTL + 2*time.Second)
	body, status = fetch(t, client, shortURL)
	if status == http.StatusOK {
		t.Fatalf("an EXPIRED presigned URL was still served (200): %q", body)
	}
	if status != http.StatusForbidden {
		t.Logf("expired URL refused with %d (not 403): %q", status, body)
	}
}

// PutRunFile streams a run's tool outputs. The production caller
// (store/mongo.UploadRunFiles) hands it an *io.SectionReader over the file,
// so the body is seekable and aws-sdk-go-v2 computes its default CRC32 as a
// REQUEST HEADER by reading the body twice. This is the shape that must work.
func TestGatewayCompat_RunFileStreamingUploadAndListing(t *testing.T) {
	c, _ := gatewayClient(t)
	ctx := context.Background()
	run := gatewayRunID(t)

	// Non-repeating bytes: a compressible or repetitive payload can hide a
	// framing bug that a checksum would otherwise catch.
	payload := make([]byte, 256*1024)
	for i := range payload {
		payload[i] = byte((i*7 + i/251) % 251)
	}
	// Seekable, exactly like newRunFileUploadReader's *io.SectionReader.
	reader := io.NewSectionReader(bytes.NewReader(payload), 0, int64(len(payload)))
	if err := c.PutRunFile(ctx, run, "out/blob.bin", "application/octet-stream", reader, int64(len(payload))); err != nil {
		t.Fatalf("PutRunFile (seekable reader, %d bytes): %v", len(payload), err)
	}

	rc, info, err := c.GetRunFile(ctx, run, "out/blob.bin")
	if err != nil {
		t.Fatalf("GetRunFile: %v", err)
	}
	got, readErr := io.ReadAll(rc)
	_ = rc.Close()
	if readErr != nil {
		t.Fatalf("read run file: %v", readErr)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("run file round-trip corrupted: %d bytes back, want %d (first difference at %d)",
			len(got), len(payload), firstDiff(got, payload))
	}
	if info.Size != int64(len(payload)) {
		t.Errorf("Content-Length = %d, want %d", info.Size, len(payload))
	}

	files, err := c.ListRunFiles(ctx, run)
	if err != nil {
		t.Fatalf("ListRunFiles: %v", err)
	}
	if len(files) != 1 || files[0].Path != "out/blob.bin" {
		t.Fatalf("ListRunFiles = %+v, want one entry out/blob.bin", files)
	}
	if files[0].Size != int64(len(payload)) {
		t.Errorf("listed Size = %d, want %d", files[0].Size, len(payload))
	}
	if files[0].ModifiedAt.IsZero() {
		t.Errorf("listed LastModified absent")
	}

	if err := c.DeleteRunFiles(ctx, run); err != nil {
		t.Fatalf("DeleteRunFiles: %v", err)
	}
	files, err = c.ListRunFiles(ctx, run)
	if err != nil || len(files) != 0 {
		t.Fatalf("after DeleteRunFiles: %+v (%v)", files, err)
	}
}

// IR blob + backend session: the remaining PutObject/GetObject/DeleteObject
// key families the server uses, so no prefix is left unexercised.
func TestGatewayCompat_IRBlobAndBackendSession(t *testing.T) {
	c, _ := gatewayClient(t)
	ctx := context.Background()
	run := gatewayRunID(t)

	ir := []byte(`{"nodes":[{"id":"plan"}]}`)
	if err := c.PutIRBlob(ctx, run, ir); err != nil {
		t.Fatalf("PutIRBlob: %v", err)
	}
	back, err := c.GetIRBlob(ctx, fmt.Sprintf("ir/%s.json", run))
	if err != nil {
		t.Fatalf("GetIRBlob: %v", err)
	}
	if !bytes.Equal(back, ir) {
		t.Fatalf("IR round-trip: %q want %q", back, ir)
	}
	if err := c.DeleteRunIR(ctx, run); err != nil {
		t.Fatalf("DeleteRunIR: %v", err)
	}
	if _, err := c.GetIRBlob(ctx, fmt.Sprintf("ir/%s.json", run)); !errors.Is(err, ErrArtifactNotFound) {
		t.Fatalf("IR readable after delete: %v", err)
	}

	sess := []byte(`{"session":"abc"}`)
	if err := c.PutBackendSession(ctx, run, "claude", sess); err != nil {
		t.Fatalf("PutBackendSession: %v", err)
	}
	back, err = c.GetBackendSession(ctx, run, "claude")
	if err != nil || !bytes.Equal(back, sess) {
		t.Fatalf("GetBackendSession: %q (%v)", back, err)
	}
	if err := c.DeleteRunBackendSessions(ctx, run); err != nil {
		t.Fatalf("DeleteRunBackendSessions: %v", err)
	}
	if _, err := c.GetBackendSession(ctx, run, "claude"); !errors.Is(err, ErrArtifactNotFound) {
		t.Fatalf("session readable after sweep: %v", err)
	}
}

// An UNSEEKABLE body over a plaintext endpoint is refused by
// aws-sdk-go-v2 itself, before any byte reaches the wire: with
// RequestChecksumCalculation=when_supported (the default since
// service/s3 v1.73) it can neither rewind to compute a header checksum nor
// use an aws-chunked trailer without TLS.
//
// The point of this case is attribution: the SAME refusal comes back from
// the in-process double, so it is a property of the CLIENT and of `http://`,
// not of the gateway under test. Whatever gateway sits behind
// ITERION_S3_ENDPOINT, a plaintext endpoint has this limit.
func TestGatewayCompat_UnseekableBodyOverPlaintextIsRefusedByTheClient(t *testing.T) {
	ctx := context.Background()
	payload := bytes.Repeat([]byte("x"), 4096)
	const want = "unseekable stream is not supported without TLS and trailing checksum"

	// Arm A — the in-process double (plain http httptest server).
	_, srv := newFakeS3(t, "b")
	fakeClient := newTestS3Client(t, srv.URL, "b")
	errFake := fakeClient.PutRunFile(ctx, "run-x", "a.bin",
		"application/octet-stream", struct{ io.Reader }{bytes.NewReader(payload)}, int64(len(payload)))
	if errFake == nil || !strings.Contains(errFake.Error(), want) {
		t.Fatalf("in-process double: got %v, want an error containing %q", errFake, want)
	}

	// Arm B — the live gateway, if one is configured: identical refusal.
	if os.Getenv("ITERION_TEST_S3_ENDPOINT") == "" {
		t.Log("no live gateway configured; arm A alone already places the limit in the client")
		return
	}
	c, endpoint := gatewayClient(t)
	if !strings.HasPrefix(endpoint, "http://") {
		t.Skipf("endpoint %s is not plaintext; the limit does not apply", redact(endpoint))
	}
	errLive := c.PutRunFile(ctx, gatewayRunID(t), "a.bin",
		"application/octet-stream", struct{ io.Reader }{bytes.NewReader(payload)}, int64(len(payload)))
	if errLive == nil || !strings.Contains(errLive.Error(), want) {
		t.Fatalf("live gateway: got %v, want the SAME client-side error containing %q", errLive, want)
	}
	t.Logf("same client-side refusal on both arms: %v", want)
}

func fetch(t *testing.T, client *http.Client, raw string) ([]byte, int) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, raw, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", redact(raw), err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	return body, resp.StatusCode
}

// flipLastHex changes exactly one hex digit so the signature stays
// well-formed (same length, same alphabet) and only its VALUE is wrong.
func flipLastHex(sig string) string {
	if sig == "" {
		return "0"
	}
	last := sig[len(sig)-1]
	repl := byte('0')
	if last == '0' {
		repl = '1'
	}
	return sig[:len(sig)-1] + string(repl)
}

func redact(raw string) string {
	if i := strings.Index(raw, "?"); i >= 0 {
		return raw[:i] + "?<signed>"
	}
	return raw
}

func firstDiff(a, b []byte) int {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			return i
		}
	}
	return n
}

// In an HA deployment several S3-gateway replicas sit behind one Service, so a
// PUT and its GET land on DIFFERENT pods. That only works if the gateways
// share one metadata store: with the chart's default per-replica LevelDB they
// do not, and a read lands on a gateway that has never heard of the object.
//
// Set ITERION_TEST_S3_ENDPOINT_B to a SECOND gateway of the same cluster to
// exercise it.
func TestGatewayCompat_SecondGatewaySeesTheFirstsWrites(t *testing.T) {
	endpointB := os.Getenv("ITERION_TEST_S3_ENDPOINT_B")
	if endpointB == "" {
		t.Skip("ITERION_TEST_S3_ENDPOINT_B unset: single-gateway bench")
	}
	a, _ := gatewayClient(t)
	bucket := os.Getenv("ITERION_TEST_S3_BUCKET")
	if bucket == "" {
		bucket = "iterion-artifacts"
	}
	b, err := NewS3(context.Background(), Config{
		Region: "us-east-1", Bucket: bucket, Endpoint: endpointB, UsePathStyle: true,
		AccessKeyID:     os.Getenv("ITERION_TEST_S3_ACCESS_KEY_ID"),
		SecretAccessKey: os.Getenv("ITERION_TEST_S3_SECRET_ACCESS_KEY"),
	})
	if err != nil {
		t.Fatalf("NewS3(B): %v", err)
	}
	t.Cleanup(func() { _ = b.Close() })

	ctx := context.Background()
	run := gatewayRunID(t)
	if err := b.Ping(ctx); err != nil {
		t.Fatalf("Ping on gateway B: %v", err)
	}

	// Written on A.
	want := []byte(`{"written":"on A"}`)
	if err := a.PutArtifact(ctx, run, "review", 0, want); err != nil {
		t.Fatalf("PutArtifact on A: %v", err)
	}
	t.Cleanup(func() { _ = a.DeleteRun(context.Background(), run) })

	// Read on B: bytes AND listing.
	got, err := b.GetArtifact(ctx, run, "review", 0)
	if err != nil {
		t.Fatalf("GetArtifact on B for an object written on A: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("B read %q, want %q", got, want)
	}
	versions, err := b.ListArtifactVersions(ctx, run, "review")
	if err != nil || len(versions) != 1 {
		t.Fatalf("ListArtifactVersions on B = %v (%v), want one version", versions, err)
	}

	// And the reverse: deleted on B, gone on A.
	if err := b.DeleteRun(ctx, run); err != nil {
		t.Fatalf("DeleteRun on B: %v", err)
	}
	if _, err := a.GetArtifact(ctx, run, "review", 0); !errors.Is(err, ErrArtifactNotFound) {
		t.Fatalf("A still serves an object B deleted: %v", err)
	}
}
