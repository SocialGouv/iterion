package store

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestIncarnationAttachmentScratchRejectNamespaceLinks(t *testing.T) {
	for _, format := range []ValueFormat{LegacyValueFormat, ExactValueFormat} {
		for _, op := range []string{"write", "open", "list", "remove", "delete", "put-bank", "open-bank", "delete-bank"} {
			t.Run(fmt.Sprintf("%d/%s", format, op), func(t *testing.T) {
				s, ctx := directoryFixture(t, format)
				rec := AttachmentRecord{Name: "a", OriginalFilename: "body"}
				if err := s.WriteAttachment(ctx, "r", rec, strings.NewReader("original")); err != nil {
					t.Fatal(err)
				}
				if err := s.PutScratchBank(ctx, "r", strings.NewReader("original"), 8); err != nil {
					t.Fatal(err)
				}
				path, err := s.runPayloadPath(ctx, "r", "scratch-bank.tgz")
				if err != nil {
					t.Fatal(err)
				}
				root := filepath.Dir(path)
				held := root + "-held"
				if err = os.Rename(root, held); err != nil {
					t.Fatal(err)
				}
				if err = os.Symlink(held, root); err != nil {
					t.Fatal(err)
				}
				switch op {
				case "write":
					err = s.WriteAttachment(ctx, "r", rec, strings.NewReader("foreign!"))
				case "open":
					var f io.ReadCloser
					f, _, err = s.OpenAttachment(ctx, "r", "a")
					if f != nil {
						_ = f.Close()
					}
				case "list":
					_, err = s.ListAttachments(ctx, "r")
				case "remove":
					err = s.RemoveAttachment(ctx, "r", "a")
				case "delete":
					err = s.DeleteRunAttachments(ctx, "r")
				case "put-bank":
					err = s.PutScratchBank(ctx, "r", strings.NewReader("foreign!"), 8)
				case "open-bank":
					var f io.ReadCloser
					f, err = s.OpenScratchBank(ctx, "r")
					if f != nil {
						_ = f.Close()
					}
				case "delete-bank":
					err = s.DeleteScratchBank(ctx, "r")
				}
				if err == nil {
					t.Fatal("namespace symlink accepted")
				}
				for _, rel := range []string{"scratch-bank.tgz", "attachments/a/body"} {
					raw, e := os.ReadFile(filepath.Join(held, rel))
					if e != nil || string(raw) != "original" {
						t.Fatalf("outside payload changed: %s %q %v", rel, raw, e)
					}
				}
			})
		}
	}
}

func TestIncarnationAttachmentInvalidMetadataPreservesBody(t *testing.T) {
	for _, format := range []ValueFormat{LegacyValueFormat, ExactValueFormat} {
		t.Run(fmt.Sprint(format), func(t *testing.T) {
			s, ctx := directoryFixture(t, format)
			rec := AttachmentRecord{Name: "a", OriginalFilename: "body"}
			if err := s.WriteAttachment(ctx, "r", rec, strings.NewReader("original")); err != nil {
				t.Fatal(err)
			}
			rec.CreatedAt = time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)
			if err := s.WriteAttachment(ctx, "r", rec, strings.NewReader("foreign!")); err == nil {
				t.Fatal("invalid timestamp accepted")
			}
			f, _, err := s.OpenAttachment(ctx, "r", "a")
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			raw, err := io.ReadAll(f)
			if err != nil || string(raw) != "original" {
				t.Fatalf("rejected metadata overwrote body: %q %v", raw, err)
			}
		})
	}
}
