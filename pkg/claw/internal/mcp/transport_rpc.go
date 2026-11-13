package mcp

import (
	"bufio"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// FlushWriter is an optional interface that writers can implement to support
// explicit flushing after a frame is written.
type FlushWriter interface {
	Flush() error
}

// WriteLSPFrameTo writes a Content-Length framed payload to w.
// If w implements FlushWriter, Flush() is called after the payload is written.
func WriteLSPFrameTo(w io.Writer, payload []byte) error {
	header := fmt.Sprintf("Content-Length: %d\r\n\r\n", len(payload))
	if _, err := io.WriteString(w, header); err != nil {
		return err
	}
	if _, err := w.Write(payload); err != nil {
		return err
	}
	if f, ok := w.(FlushWriter); ok {
		return f.Flush()
	}
	return nil
}

// ReadLSPFrameFrom reads a single Content-Length framed payload from r.
// Returns (nil, nil) on clean EOF before any header is read.
func ReadLSPFrameFrom(r *bufio.Reader) ([]byte, error) {
	var contentLength int = -1
	firstHeader := true

	for {
		line, err := r.ReadString('\n')
		if err != nil {
			if err == io.EOF && firstHeader {
				return nil, nil
			}
			if err == io.EOF {
				return nil, fmt.Errorf("unexpected EOF while reading headers")
			}
			return nil, err
		}
		firstHeader = false

		trimmed := strings.TrimRight(line, "\r\n")
		if trimmed == "" {
			break // end of headers
		}

		parts := strings.SplitN(trimmed, ":", 2)
		if len(parts) == 2 && strings.TrimSpace(strings.ToLower(parts[0])) == "content-length" {
			n, err := strconv.Atoi(strings.TrimSpace(parts[1]))
			if err != nil {
				return nil, fmt.Errorf("invalid Content-Length: %w", err)
			}
			contentLength = n
		}
	}

	if contentLength < 0 {
		return nil, fmt.Errorf("missing Content-Length header")
	}

	payload := make([]byte, contentLength)
	if _, err := io.ReadFull(r, payload); err != nil {
		return nil, fmt.Errorf("reading payload: %w", err)
	}
	return payload, nil
}
