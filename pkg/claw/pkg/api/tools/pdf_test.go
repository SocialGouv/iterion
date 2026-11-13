package tools

import (
	"bytes"
	"compress/zlib"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// testBudget is far above anything the fixtures inflate to.
const testBudget = 8 << 20

// writeSimplePDF writes a minimal VALID one-page PDF (proper /Length,
// xref and startxref — the scraper is stricter than poppler) with an
// uncompressed BT/ET text object. The compressed/escaped variants are
// covered by the internal package's own tests, which this wrapper
// delegates to.
func writeSimplePDF(t *testing.T, dir string) string {
	t.Helper()
	contentStream := "BT\n/F1 12 Tf\n(reference spec text for the wrapper) Tj\nET"

	p := filepath.Join(dir, "spec.pdf")
	var pdf strings.Builder
	pdf.WriteString("%PDF-1.4\n")
	obj1 := pdf.Len()
	pdf.WriteString("1 0 obj\n<< /Type /Catalog /Pages 2 0 R >>\nendobj\n")
	obj2 := pdf.Len()
	pdf.WriteString("2 0 obj\n<< /Type /Pages /Kids [3 0 R] /Count 1 >>\nendobj\n")
	obj3 := pdf.Len()
	pdf.WriteString("3 0 obj\n<< /Type /Page /Parent 2 0 R /Contents 4 0 R >>\nendobj\n")
	obj4 := pdf.Len()
	pdf.WriteString(fmt.Sprintf("4 0 obj\n<< /Length %d >>\nstream\n%s\nendstream\nendobj\n", len(contentStream), contentStream))
	xref := pdf.Len()
	pdf.WriteString("xref\n0 5\n0000000000 65535 f \n")
	for _, off := range []int{obj1, obj2, obj3, obj4} {
		pdf.WriteString(fmt.Sprintf("%010d 00000 n \n", off))
	}
	pdf.WriteString("trailer\n<< /Size 5 /Root 1 0 R >>\n")
	pdf.WriteString(fmt.Sprintf("startxref\n%d\n%%%%EOF\n", xref))

	if err := os.WriteFile(p, []byte(pdf.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestExtractPDFTextReturnsTheTextObject(t *testing.T) {
	p := writeSimplePDF(t, t.TempDir())
	text, err := ExtractPDFText(p, testBudget)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if !strings.Contains(text, "reference spec text for the wrapper") {
		t.Fatalf("text object missing from extraction: %q", text)
	}
}

func TestExtractPDFTextOnNonPDFBytesIsEmptyNotAnError(t *testing.T) {
	// The documented contract: bytes with no BT/ET content streams
	// (e.g. a text file) yield an empty string, not an error — only a
	// READ failure errors.
	p := filepath.Join(t.TempDir(), "not-a-pdf.pdf")
	if err := os.WriteFile(p, []byte("plain text"), 0o600); err != nil {
		t.Fatal(err)
	}
	text, err := ExtractPDFText(p, testBudget)
	if err != nil {
		t.Fatalf("non-PDF bytes should not error: %v", err)
	}
	if text != "" {
		t.Fatalf("expected empty extraction, got %q", text)
	}
}

func TestExtractPDFTextOnAMissingFileIsAnError(t *testing.T) {
	if _, err := ExtractPDFText(filepath.Join(t.TempDir(), "absent.pdf"), testBudget); err == nil {
		t.Fatal("expected an error for a missing file")
	}
}

func TestExtractPDFTextFromBytesRefusesACompressedBomb(t *testing.T) {
	// ~65 MiB of zeros compress to a few dozen KiB: under any file-size
	// guard, miles over the budget. The budget must turn the bomb into
	// an error, not a 65 MiB allocation.
	var raw bytes.Buffer
	zw, _ := zlib.NewWriterLevel(&raw, zlib.BestCompression)
	if _, err := zw.Write(bytes.Repeat([]byte("0"), 65<<20)); err != nil {
		t.Fatal(err)
	}
	zw.Close()

	var pdf strings.Builder
	pdf.WriteString("%PDF-1.4\n")
	pdf.WriteString("1 0 obj\n<< /Length " + fmt.Sprint(raw.Len()) + " /Filter /FlateDecode >>\nstream\n")
	pdf.Write(raw.Bytes())
	pdf.WriteString("\nendstream\nendobj\ntrailer\n<< /Size 2 /Root 1 0 R >>\n%%EOF\n")

	_, err := ExtractPDFTextFromBytes([]byte(pdf.String()), testBudget)
	if !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("a compressed bomb must fail with ErrBudgetExceeded, got %v", err)
	}
}

// The budget's SECOND consumer: many small streams, each inflating
// under the cap, sum to the same allocation one bomb would make. Eight
// 6 MiB streams under an 8 MiB per-stream budget → 48 MiB total, err
// must be the budget — this is the witness the round-2 hole shipped
// through a green suite without.
func TestExtractPDFTextFromBytesRefusesAMultiStreamSum(t *testing.T) {
	const chunk = 6 << 20 // 6 MiB inflated, 10x under the per-stream budget
	var compressed bytes.Buffer
	zw, _ := zlib.NewWriterLevel(&compressed, zlib.BestCompression)
	if _, err := zw.Write(bytes.Repeat([]byte("0"), chunk)); err != nil {
		t.Fatal(err)
	}
	zw.Close()

	var pdf strings.Builder
	pdf.WriteString("%PDF-1.4\n")
	for i := 0; i < 8; i++ {
		pdf.WriteString("1 0 obj\n<< /Length " + fmt.Sprint(compressed.Len()) + " /Filter /FlateDecode >>\nstream\n")
		pdf.Write(compressed.Bytes())
		pdf.WriteString("\nendstream\nendobj\n")
	}
	pdf.WriteString("trailer\n<< /Size 2 /Root 1 0 R >>\n%%EOF\n")

	_, err := ExtractPDFTextFromBytes([]byte(pdf.String()), testBudget)
	if !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("a multi-stream sum over the budget must fail with ErrBudgetExceeded, got %v", err)
	}
}

// A positive multi-stream extraction: the scanner must CHAIN streams
// (offset jumps past each endstream, including the glued ">>stream"
// delimiter form — no newline, spec-tolerated, and exactly the shape
// the bare-"stream" needle mis-parsed and the "\nstream" needle
// skipped).
func TestExtractPDFTextFromBytesChainsMultipleStreams(t *testing.T) {
	build := func(first, second string) string {
		var pdf strings.Builder
		pdf.WriteString("%PDF-1.4\n")
		pdf.WriteString("1 0 obj\n<< /Length " + fmt.Sprint(len(first)) + " >>\nstream\n" + first + "\nendstream\nendobj\n")
		pdf.WriteString("2 0 obj\n<< /Length " + fmt.Sprint(len(second)) + " >>stream\n" + second + "\nendstream\nendobj\n")
		pdf.WriteString("trailer\n<< /Size 3 /Root 1 0 R >>\n%%EOF\n")
		return pdf.String()
	}

	text, err := ExtractPDFTextFromBytes([]byte(build("BT\n(alpha) Tj\nET", "BT\n(beta) Tj\nET")), testBudget)
	if err != nil {
		t.Fatalf("two-stream extraction: %v", err)
	}
	if !strings.Contains(text, "alpha") || !strings.Contains(text, "beta") {
		t.Fatalf("a chained multi-stream extraction lost a stream: %q", text)
	}
}
