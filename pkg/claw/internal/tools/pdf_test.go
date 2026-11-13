package tools

import (
	"bytes"
	"compress/zlib"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// testDecompressionBudget: the fixtures are a few hundred bytes; a few
// MiB of headroom never triggers the budget path.
const testDecompressionBudget = 8 << 20

// buildSimplePDF creates a minimal PDF with uncompressed text.
func buildSimplePDF(text string) []byte {
	contentStream := fmt.Sprintf("BT\n/F1 12 Tf\n(%s) Tj\nET", text)
	streamBytes := []byte(contentStream)

	var pdf bytes.Buffer
	pdf.WriteString("%PDF-1.4\n")

	obj1Offset := pdf.Len()
	pdf.WriteString("1 0 obj\n<< /Type /Catalog /Pages 2 0 R >>\nendobj\n")

	obj2Offset := pdf.Len()
	pdf.WriteString("2 0 obj\n<< /Type /Pages /Kids [3 0 R] /Count 1 >>\nendobj\n")

	obj3Offset := pdf.Len()
	pdf.WriteString("3 0 obj\n<< /Type /Page /Parent 2 0 R /Contents 4 0 R >>\nendobj\n")

	obj4Offset := pdf.Len()
	pdf.WriteString(fmt.Sprintf("4 0 obj\n<< /Length %d >>\nstream\n", len(streamBytes)))
	pdf.Write(streamBytes)
	pdf.WriteString("\nendstream\nendobj\n")

	xrefOffset := pdf.Len()
	pdf.WriteString("xref\n0 5\n")
	pdf.WriteString("0000000000 65535 f \n")
	pdf.WriteString(fmt.Sprintf("%010d 00000 n \n", obj1Offset))
	pdf.WriteString(fmt.Sprintf("%010d 00000 n \n", obj2Offset))
	pdf.WriteString(fmt.Sprintf("%010d 00000 n \n", obj3Offset))
	pdf.WriteString(fmt.Sprintf("%010d 00000 n \n", obj4Offset))

	pdf.WriteString("trailer\n<< /Size 5 /Root 1 0 R >>\n")
	pdf.WriteString(fmt.Sprintf("startxref\n%d\n%%%%EOF\n", xrefOffset))

	return pdf.Bytes()
}

// buildFlatePDF creates a minimal PDF with flate-compressed text.
func buildFlatePDF(text string) []byte {
	contentStream := fmt.Sprintf("BT\n/F1 12 Tf\n(%s) Tj\nET", text)

	var compressed bytes.Buffer
	w, _ := zlib.NewWriterLevel(&compressed, zlib.DefaultCompression)
	w.Write([]byte(contentStream))
	w.Close()

	compressedBytes := compressed.Bytes()

	var pdf bytes.Buffer
	pdf.WriteString("%PDF-1.4\n")

	obj1Offset := pdf.Len()
	pdf.WriteString("1 0 obj\n<< /Type /Catalog /Pages 2 0 R >>\nendobj\n")

	obj2Offset := pdf.Len()
	pdf.WriteString("2 0 obj\n<< /Type /Pages /Kids [3 0 R] /Count 1 >>\nendobj\n")

	obj3Offset := pdf.Len()
	pdf.WriteString("3 0 obj\n<< /Type /Page /Parent 2 0 R /Contents 4 0 R >>\nendobj\n")

	obj4Offset := pdf.Len()
	pdf.WriteString(fmt.Sprintf("4 0 obj\n<< /Length %d /Filter /FlateDecode >>\nstream\n", len(compressedBytes)))
	pdf.Write(compressedBytes)
	pdf.WriteString("\nendstream\nendobj\n")

	xrefOffset := pdf.Len()
	pdf.WriteString("xref\n0 5\n")
	pdf.WriteString("0000000000 65535 f \n")
	pdf.WriteString(fmt.Sprintf("%010d 00000 n \n", obj1Offset))
	pdf.WriteString(fmt.Sprintf("%010d 00000 n \n", obj2Offset))
	pdf.WriteString(fmt.Sprintf("%010d 00000 n \n", obj3Offset))
	pdf.WriteString(fmt.Sprintf("%010d 00000 n \n", obj4Offset))

	pdf.WriteString("trailer\n<< /Size 5 /Root 1 0 R >>\n")
	pdf.WriteString(fmt.Sprintf("startxref\n%d\n%%%%EOF\n", xrefOffset))

	return pdf.Bytes()
}

func TestExtractsUncompressedText(t *testing.T) {
	pdfBytes := buildSimplePDF("Hello World")
	text, err := ExtractTextFromBytes(pdfBytes, testDecompressionBudget)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if text != "Hello World" {
		t.Errorf("got %q, want %q", text, "Hello World")
	}
}

func TestExtractsFlateCompressedText(t *testing.T) {
	pdfBytes := buildFlatePDF("Compressed PDF Text")
	text, extractErr := ExtractTextFromBytes(pdfBytes, testDecompressionBudget)
	if extractErr != nil {
		t.Fatalf("extract: %v", extractErr)
	}
	if text != "Compressed PDF Text" {
		t.Errorf("got %q, want %q", text, "Compressed PDF Text")
	}
}

func TestHandlesTJArrayOperator(t *testing.T) {
	contentStream := "BT\n/F1 12 Tf\n[ (Hello) -120 ( World) ] TJ\nET"
	raw := fmt.Sprintf(
		"%%PDF-1.4\n1 0 obj\n<< /Type /Catalog >>\nendobj\n"+
			"2 0 obj\n<< /Length %d >>\nstream\n%s\nendstream\nendobj\n%%%%EOF\n",
		len(contentStream), contentStream,
	)
	text, extractErr := ExtractTextFromBytes([]byte(raw), testDecompressionBudget)
	if extractErr != nil {
		t.Fatalf("extract: %v", extractErr)
	}
	if text != "Hello World" {
		t.Errorf("got %q, want %q", text, "Hello World")
	}
}

func TestHandlesEscapedParentheses(t *testing.T) {
	content := `BT
(Hello \(World\)) Tj
ET`
	raw := fmt.Sprintf(
		"%%PDF-1.4\n1 0 obj\n<< /Length %d >>\nstream\n%s\nendstream\nendobj\n%%%%EOF\n",
		len(content), content,
	)
	text, extractErr := ExtractTextFromBytes([]byte(raw), testDecompressionBudget)
	if extractErr != nil {
		t.Fatalf("extract: %v", extractErr)
	}
	if text != "Hello (World)" {
		t.Errorf("got %q, want %q", text, "Hello (World)")
	}
}

func TestReturnsEmptyForNonPDFData(t *testing.T) {
	data := []byte("This is not a PDF file at all")
	text, err := ExtractTextFromBytes(data, testDecompressionBudget)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if text != "" {
		t.Errorf("expected empty, got %q", text)
	}
}

func TestExtractsTextFromFileOnDisk(t *testing.T) {
	pdfBytes := buildSimplePDF("Disk Test")
	dir := t.TempDir()
	pdfPath := filepath.Join(dir, "test.pdf")
	if err := os.WriteFile(pdfPath, pdfBytes, 0644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	text, err := ExtractText(pdfPath, testDecompressionBudget)
	if err != nil {
		t.Fatalf("extract text: %v", err)
	}
	if text != "Disk Test" {
		t.Errorf("got %q, want %q", text, "Disk Test")
	}
}

func TestLooksLikePDFPath(t *testing.T) {
	tests := []struct {
		input    string
		wantPath string
		wantOK   bool
	}{
		{"Please read /tmp/report.pdf", "/tmp/report.pdf", true},
		{"Check file.PDF now", "file.PDF", true},
		{"no pdf here", "", false},
		{`Check 'file.pdf' please`, "file.pdf", true},
		{`Check "doc.pdf" now`, "doc.pdf", true},
	}

	for _, tt := range tests {
		path, ok := LooksLikePDFPath(tt.input)
		if ok != tt.wantOK {
			t.Errorf("LooksLikePDFPath(%q): ok=%v, want %v", tt.input, ok, tt.wantOK)
		}
		if path != tt.wantPath {
			t.Errorf("LooksLikePDFPath(%q): path=%q, want %q", tt.input, path, tt.wantPath)
		}
	}
}

func TestMaybeExtractPDFFromPromptMissingFile(t *testing.T) {
	prompt := "Read /tmp/nonexistent-abc123.pdf please"
	_, _, ok := MaybeExtractPDFFromPrompt(prompt)
	if ok {
		t.Error("expected ok=false for missing file")
	}
}

func TestMaybeExtractPDFFromPromptExistingFile(t *testing.T) {
	pdfBytes := buildSimplePDF("Auto Extracted")
	dir := t.TempDir()
	pdfPath := filepath.Join(dir, "auto.pdf")
	if err := os.WriteFile(pdfPath, pdfBytes, 0644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	prompt := fmt.Sprintf("Summarize %s", pdfPath)
	path, text, ok := MaybeExtractPDFFromPrompt(prompt)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if path != pdfPath {
		t.Errorf("got path %q, want %q", path, pdfPath)
	}
	if text != "Auto Extracted" {
		t.Errorf("got text %q, want %q", text, "Auto Extracted")
	}
}
