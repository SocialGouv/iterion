package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type stagingSwapReader struct {
	body    *bytes.Reader
	swap    func()
	swapped bool
}

func (r *stagingSwapReader) Read(p []byte) (int, error) {
	n, e := r.body.Read(p)
	if e == io.EOF && !r.swapped {
		r.swapped = true
		r.swap()
	}
	return n, e
}
func TestIncarnationStagingPreservesSourceDescriptor(t *testing.T) {
	for _, format := range []ValueFormat{LegacyValueFormat, ExactValueFormat} {
		for _, operation := range []string{"attachment", "scratch"} {
			t.Run(fmt.Sprintf("%d/%s", format, operation), func(t *testing.T) {
				s, ctx := directoryFixture(t, format)
				original := []byte("original")
				foreign := []byte("foreign!")
				reader := &stagingSwapReader{body: bytes.NewReader(original), swap: func() {
					entries, e := os.ReadDir(s.root)
					if e != nil {
						t.Fatal(e)
					}
					found := false
					for _, entry := range entries {
						if !strings.HasPrefix(entry.Name(), ".run-body-stage-") {
							continue
						}
						found = true
						p := filepath.Join(s.root, entry.Name())
						if e = os.Rename(p, p+"-held"); e != nil {
							t.Fatal(e)
						}
						if e = os.WriteFile(p, foreign, 0600); e != nil {
							t.Fatal(e)
						}
					}
					if !found {
						t.Fatal("stage name absent")
					}
				}}
				var opened io.ReadCloser
				var err error
				if operation == "attachment" {
					if err = s.WriteAttachment(ctx, "r", AttachmentRecord{Name: "a", OriginalFilename: "body.txt"}, reader); err != nil {
						t.Fatal(err)
					}
					var rec AttachmentRecord
					opened, rec, err = s.OpenAttachment(ctx, "r", "a")
					sum := sha256.Sum256(original)
					if rec.SHA256 != hex.EncodeToString(sum[:]) {
						t.Fatal("hash not from original stream", rec.SHA256)
					}
				} else {
					if err = s.PutScratchBank(ctx, "r", reader, int64(len(original))); err != nil {
						t.Fatal(err)
					}
					opened, err = s.OpenScratchBank(ctx, "r")
				}
				if err != nil {
					t.Fatal(err)
				}
				defer opened.Close()
				got, err := io.ReadAll(opened)
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(got, original) {
					t.Fatalf("source substituted: bytes=%q", got)
				}
				t.Logf("public %s preserved original bytes %q", operation, got)
			})
		}
	}
}

func TestIncarnationStagingCancellationPreservesPublishedBytes(t *testing.T) {
	for _, format := range []ValueFormat{LegacyValueFormat, ExactValueFormat} {
		for _, operation := range []string{"attachment", "scratch"} {
			t.Run(fmt.Sprintf("%d/%s", format, operation), func(t *testing.T) {
				s, base := directoryFixture(t, format)
				put := func(ctx context.Context, r io.Reader) error {
					if operation == "attachment" {
						return s.WriteAttachment(ctx, "r", AttachmentRecord{Name: "a", OriginalFilename: "body.txt"}, r)
					}
					return s.PutScratchBank(ctx, "r", r, 8)
				}
				if err := put(base, strings.NewReader("original")); err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithCancel(base)
				defer cancel()
				if err := put(ctx, &stagingSwapReader{body: bytes.NewReader([]byte("foreign!")), swap: cancel}); !errors.Is(err, context.Canceled) {
					t.Fatal("cancellation lost", err)
				}
				var f io.ReadCloser
				var err error
				if operation == "attachment" {
					f, _, err = s.OpenAttachment(base, "r", "a")
				} else {
					f, err = s.OpenScratchBank(base, "r")
				}
				if err != nil {
					t.Fatal(err)
				}
				defer f.Close()
				got, err := io.ReadAll(f)
				if err != nil || string(got) != "original" {
					t.Fatal("cancelled publication changed bytes", string(got), err)
				}
				entries, err := filepath.Glob(filepath.Join(s.root, ".run-body-stage-*"))
				if err != nil || len(entries) != 0 {
					t.Fatal("temporary residue", entries, err)
				}
			})
		}
	}
}
