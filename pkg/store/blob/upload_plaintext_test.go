package blob

import (
	"bytes"
	"context"
	"io"
	"testing"
)

// The handler stages through the seam with a SEEKABLE body (an
// io.SectionReader over the multipart part): the client rewinds it to
// checksum and to retry, and over a plaintext endpoint — the compose stack,
// the chart's dev stack — it refuses an unseekable stream outright
// (TestGatewayCompat_UnseekableBodyOverPlaintextIsRefusedByTheClient). The
// seam's type says so; this is the PUT going through on such an endpoint.
func TestPutUploadStreamsASeekableBodyOverPlaintext(t *testing.T) {
	f, srv := newFakeS3(t, "b")
	c := newTestS3Client(t, srv.URL, "b")
	payload := bytes.Repeat([]byte("staged "), 1024)
	body := io.NewSectionReader(bytes.NewReader(payload), 0, int64(len(payload)))
	if err := c.PutUpload(context.Background(), "up-1", "brief.pdf", "application/pdf", body, int64(len(payload))); err != nil {
		t.Fatalf("PutUpload over plaintext with a seekable body: %v", err)
	}
	if !f.has("uploads/up-1/brief.pdf") {
		t.Fatal("the object did not land under uploads/<id>/<filename>")
	}
	rc, meta, err := c.GetUpload(context.Background(), "up-1", "brief.pdf")
	if err != nil {
		t.Fatalf("GetUpload: %v", err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	// The in-process double reports no Content-Length on GET; the bytes
	// are the contract here.
	if !bytes.Equal(got, payload) || (meta.Size != 0 && meta.Size != int64(len(payload))) {
		t.Fatalf("read back %d bytes (meta %d), want %d", len(got), meta.Size, len(payload))
	}
}
