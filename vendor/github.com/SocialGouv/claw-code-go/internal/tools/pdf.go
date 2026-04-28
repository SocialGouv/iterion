package tools

import (
	"bytes"
	"compress/flate"
	"compress/zlib"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// ExtractText reads a PDF file and extracts all readable text from BT/ET
// operators across all content streams. Non-text pages or encrypted PDFs
// yield an empty string rather than an error.
func ExtractText(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("failed to read PDF: %w", err)
	}
	return ExtractTextFromBytes(data), nil
}

// ExtractTextFromBytes extracts text from raw PDF bytes. This is the core
// extraction function, useful for testing without touching the filesystem.
func ExtractTextFromBytes(data []byte) string {
	var allText strings.Builder
	offset := 0

	for offset < len(data) {
		streamStart := bytes.Index(data[offset:], []byte("stream"))
		if streamStart < 0 {
			break
		}
		absStart := offset + streamStart

		// Skip past "stream\r\n" or "stream\n".
		contentStart := skipStreamEOL(data, absStart+len("stream"))

		endRel := bytes.Index(data[contentStart:], []byte("endstream"))
		if endRel < 0 {
			break
		}
		contentEnd := contentStart + endRel

		// Look backwards from "stream" for a FlateDecode hint (up to 512 bytes).
		dictWindowStart := absStart - 512
		if dictWindowStart < 0 {
			dictWindowStart = 0
		}
		dictWindow := data[dictWindowStart:absStart]
		isFlate := bytes.Contains(dictWindow, []byte("FlateDecode"))

		raw := data[contentStart:contentEnd]
		var streamBytes []byte
		if isFlate {
			decompressed, err := inflate(raw)
			if err != nil {
				offset = contentEnd
				continue
			}
			streamBytes = decompressed
		} else {
			streamBytes = raw
		}

		text := extractBTETText(streamBytes)
		if text != "" {
			if allText.Len() > 0 {
				allText.WriteByte('\n')
			}
			allText.WriteString(text)
		}

		offset = contentEnd
	}

	return allText.String()
}

// inflate decompresses zlib/deflate data. Tries zlib first, falls back to
// raw deflate for PDFs that omit the zlib header.
func inflate(data []byte) ([]byte, error) {
	// Try zlib first.
	r, err := zlib.NewReader(bytes.NewReader(data))
	if err == nil {
		buf, readErr := io.ReadAll(r)
		r.Close()
		if readErr == nil {
			return buf, nil
		}
	}
	// Fallback: raw deflate (no zlib header).
	fr := flate.NewReader(bytes.NewReader(data))
	buf, err := io.ReadAll(fr)
	fr.Close()
	if err != nil {
		return nil, fmt.Errorf("flate inflate error: %w", err)
	}
	return buf, nil
}

// extractBTETText extracts text from PDF content-stream operators between
// BT and ET markers.
//
// Handles: Tj (show string), TJ (show array), ' (next line + string),
// " (set spacing + next line + string).
func extractBTETText(stream []byte) string {
	// Lossy UTF-8 conversion
	text := toValidUTF8(stream)
	var result strings.Builder
	inBT := false

	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "BT" {
			inBT = true
			continue
		}
		if trimmed == "ET" {
			inBT = false
			continue
		}
		if !inBT {
			continue
		}

		// Tj operator: (text) Tj
		if strings.HasSuffix(trimmed, "Tj") {
			if s, ok := extractParenthesizedString(trimmed); ok {
				if result.Len() > 0 && !strings.HasSuffix(result.String(), "\n") {
					result.WriteByte(' ')
				}
				result.WriteString(s)
			}
		} else if strings.HasSuffix(trimmed, "TJ") {
			// TJ operator: [ (text) 123 (text) ] TJ
			extracted := extractTJArray(trimmed)
			if extracted != "" {
				if result.Len() > 0 && !strings.HasSuffix(result.String(), "\n") {
					result.WriteByte(' ')
				}
				result.WriteString(extracted)
			}
		} else if isNewlineShowOperator(trimmed) {
			// ' or " operator
			if s, ok := extractParenthesizedString(trimmed); ok {
				if result.Len() > 0 {
					result.WriteByte('\n')
				}
				result.WriteString(s)
			}
		}
	}

	return result.String()
}

// isNewlineShowOperator returns true when trimmed looks like a ' or " text-show operator.
func isNewlineShowOperator(trimmed string) bool {
	if len(trimmed) > 1 && strings.HasSuffix(trimmed, "'") {
		return true
	}
	if strings.HasSuffix(trimmed, "\"") && strings.Contains(trimmed, "(") {
		return true
	}
	return false
}

// extractParenthesizedString pulls text from the first (...) group, handling
// escaped parens and common PDF escape sequences.
func extractParenthesizedString(input string) (string, bool) {
	openIdx := strings.IndexByte(input, '(')
	if openIdx < 0 {
		return "", false
	}
	data := []byte(input)
	var result strings.Builder
	depth := 0
	i := openIdx

	for i < len(data) {
		switch data[i] {
		case '(':
			if depth > 0 {
				result.WriteByte('(')
			}
			depth++
		case ')':
			depth--
			if depth == 0 {
				return result.String(), true
			}
			result.WriteByte(')')
		case '\\':
			if i+1 < len(data) {
				i++
				switch data[i] {
				case 'n':
					result.WriteByte('\n')
				case 'r':
					result.WriteByte('\r')
				case 't':
					result.WriteByte('\t')
				case '\\':
					result.WriteByte('\\')
				case '(':
					result.WriteByte('(')
				case ')':
					result.WriteByte(')')
				default:
					if data[i] >= '0' && data[i] <= '7' {
						// Octal sequence — up to 3 digits.
						octal := uint32(data[i] - '0')
						for k := 0; k < 2; k++ {
							if i+1 < len(data) && data[i+1] >= '0' && data[i+1] <= '7' {
								i++
								octal = octal*8 + uint32(data[i]-'0')
							} else {
								break
							}
						}
						if octal < 128 {
							result.WriteByte(byte(octal))
						} else {
							// Write as UTF-8 rune if valid.
							var buf [4]byte
							n := utf8.EncodeRune(buf[:], rune(octal))
							result.Write(buf[:n])
						}
					} else {
						result.WriteByte(data[i])
					}
				}
			}
		default:
			result.WriteByte(data[i])
		}
		i++
	}

	return "", false // unbalanced
}

// extractTJArray extracts concatenated strings from a TJ array like
// [ (Hello) -120 (World) ] TJ.
func extractTJArray(input string) string {
	bracketStart := strings.IndexByte(input, '[')
	if bracketStart < 0 {
		return ""
	}
	bracketEnd := strings.LastIndexByte(input, ']')
	if bracketEnd < 0 || bracketEnd <= bracketStart {
		return ""
	}
	inner := input[bracketStart+1 : bracketEnd]

	var result strings.Builder
	data := []byte(inner)
	i := 0
	for i < len(data) {
		if data[i] == '(' {
			if s, ok := extractParenthesizedString(inner[i:]); ok {
				result.WriteString(s)
				// Skip past the closing paren.
				depth := 0
				for _, b := range data[i:] {
					i++
					if b == '(' {
						depth++
					} else if b == ')' {
						depth--
						if depth == 0 {
							break
						}
					}
				}
				continue
			}
		}
		i++
	}

	return result.String()
}

// skipStreamEOL skips the EOL after the "stream" keyword (either \r\n or \n).
func skipStreamEOL(data []byte, pos int) int {
	if pos < len(data) && data[pos] == '\r' {
		if pos+1 < len(data) && data[pos+1] == '\n' {
			return pos + 2
		}
		return pos + 1
	}
	if pos < len(data) && data[pos] == '\n' {
		return pos + 1
	}
	return pos
}

// toValidUTF8 converts bytes to a string, replacing invalid UTF-8 sequences.
func toValidUTF8(data []byte) string {
	if utf8.Valid(data) {
		return string(data)
	}
	return strings.ToValidUTF8(string(data), "\uFFFD")
}

// LooksLikePDFPath checks if a user-supplied text contains a PDF file reference.
// Returns the cleaned path and true if found, or empty string and false.
func LooksLikePDFPath(text string) (string, bool) {
	for _, token := range strings.Fields(text) {
		cleaned := strings.Trim(token, "'\"`")
		dotPos := strings.LastIndexByte(cleaned, '.')
		if dotPos > 0 && strings.EqualFold(cleaned[dotPos+1:], "pdf") {
			return cleaned, true
		}
	}
	return "", false
}

// MaybeExtractPDFFromPrompt auto-extracts text from a PDF path mentioned in
// a user prompt. Returns (path, text, true) when a .pdf path is detected and
// the file exists with extractable text, otherwise ("", "", false).
func MaybeExtractPDFFromPrompt(prompt string) (string, string, bool) {
	pdfPath, ok := LooksLikePDFPath(prompt)
	if !ok {
		return "", "", false
	}
	absPath, err := filepath.Abs(pdfPath)
	if err != nil {
		return "", "", false
	}
	if _, err := os.Stat(absPath); os.IsNotExist(err) {
		// Try original path too
		if _, err := os.Stat(pdfPath); os.IsNotExist(err) {
			return "", "", false
		}
		absPath = pdfPath
	}
	text, err := ExtractText(absPath)
	if err != nil || text == "" {
		return "", "", false
	}
	return pdfPath, text, true
}
