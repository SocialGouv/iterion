package pisdk

import (
	"bufio"
	"encoding/json"
	"io"
	"strings"
)

// Ported from packages/coding-agent/src/modes/rpc/jsonl.ts.
//
// The framing is LF-only, and that is a contract rather than an incidental
// choice: payload strings legitimately contain U+2028 / U+2029, which Node's
// readline also breaks on — pi's own module carries a comment saying readline
// therefore "does not implement strict JSONL framing". Go's ReadString('\n')
// is compliant by construction; the only extra rule is trimming a trailing CR,
// which pi's emitLine does too.

// MarshalLine serialises one JSONL record, newline included.
func MarshalLine(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return append(raw, '\n'), nil
}

// ScanLines calls onLine for each LF-terminated record read from r, until EOF
// or a read error. A trailing fragment with no newline is delivered too (pi's
// onEnd does the same), so a record cut short by process exit is still seen.
//
// There is deliberately no line-length cap. A single tool_execution_end
// carrying a large file read is routinely megabytes, and bufio.Scanner's
// default 64 KiB limit would truncate the stream mid-run — a corruption that
// presents as a mysterious protocol error rather than as a size problem.
func ScanLines(r io.Reader, onLine func(line string)) error {
	br := bufio.NewReaderSize(r, 64*1024)
	var pending strings.Builder

	for {
		chunk, err := br.ReadString('\n')
		if chunk != "" {
			pending.WriteString(chunk)
			if strings.HasSuffix(chunk, "\n") {
				onLine(trimRecord(pending.String()))
				pending.Reset()
			}
		}
		if err != nil {
			if rest := pending.String(); rest != "" {
				onLine(trimRecord(rest))
			}
			if err == io.EOF {
				return nil
			}
			return err
		}
	}
}

// trimRecord strips the record's LF and any CR that preceded it.
func trimRecord(s string) string {
	s = strings.TrimSuffix(s, "\n")
	return strings.TrimSuffix(s, "\r")
}
