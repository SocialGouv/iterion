// Package cli implements the iterion command-line interface.
package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/store"
)

// OutputFormat controls how results are rendered.
type OutputFormat int

const (
	OutputHuman OutputFormat = iota
	OutputJSON
)

// Printer writes structured output in the selected format. Its every write
// goes out through write: in human mode the text is made inert for a
// terminal — what a server, a run or a file relayed may carry escape
// sequences — and in JSON mode the encoding goes out as is, for a machine.
// Raw is the one explicit exception: bytes an operator asked for whole.
type Printer struct {
	W      io.Writer
	Format OutputFormat
}

// NewPrinter creates a Printer writing to stdout.
func NewPrinter(format OutputFormat) *Printer {
	return &Printer{W: os.Stdout, Format: format}
}

// JSON emits v as indented JSON. If encoding fails (non-marshalable
// value: channels, cycles, etc.), write a JSON error envelope to
// stderr so machine consumers see a parse-able failure signal rather
// than an empty stdout that looks like a clean success.
func (p *Printer) JSON(v any) {
	var buf strings.Builder
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		fmt.Fprintf(os.Stderr, "{\"error\":\"cli: json encode failed: %s\"}\n",
			strings.ReplaceAll(err.Error(), `"`, `'`))
		return
	}
	p.write(buf.String())
}

// Line prints a formatted line.
func (p *Printer) Line(format string, args ...any) {
	p.write(fmt.Sprintf(format+"\n", args...))
}

// Blank prints an empty line.
func (p *Printer) Blank() { p.write("\n") }

// Raw writes b as it is, in either mode: bytes an operator asked for whole —
// a run's log, a workflow's source — shown as their producer wrote them.
func (p *Printer) Raw(b []byte) { _, _ = p.W.Write(b) }

// write is the Printer's one way out: s made inert for a terminal in human
// mode (inertText), as it is in JSON mode.
func (p *Printer) write(s string) {
	if p.Format != OutputJSON {
		s = inertText(s)
	}
	_, _ = io.WriteString(p.W, s)
}

// Header prints a section header.
func (p *Printer) Header(title string) {
	p.Line("── %s ──", title)
}

// KV prints a key-value pair with aligned formatting. A value bounded for
// the terminal keeps the remedy a run's error ends on (store.ClipRunError).
func (p *Printer) KV(key, value string) {
	p.Line("  %-16s %s", key+":", terminalText(store.ClipRunError(value, 2000), 2000))
}

// Table prints rows with column headers.
func (p *Printer) Table(headers []string, rows [][]string) {
	if len(rows) == 0 {
		p.Line("  (none)")
		return
	}
	// Cells carry what a server relays: made inert for the terminal, on a
	// copy — the caller's rows are its own.
	inert := make([][]string, len(rows))
	for i, row := range rows {
		inert[i] = make([]string, len(row))
		for j, cell := range row {
			inert[i][j] = terminalText(cell, 300)
		}
	}
	rows = inert

	// Compute column widths.
	widths := make([]int, len(headers))
	for i, h := range headers {
		widths[i] = len(h)
	}
	for _, row := range rows {
		for i, cell := range row {
			if i < len(widths) && len(cell) > widths[i] {
				widths[i] = len(cell)
			}
		}
	}

	// Print header.
	var hdr strings.Builder
	for i, h := range headers {
		if i > 0 {
			hdr.WriteString("  ")
		}
		fmt.Fprintf(&hdr, "%-*s", widths[i], h)
	}
	p.Line("  %s", hdr.String())

	// Print separator.
	var sep strings.Builder
	for i, w := range widths {
		if i > 0 {
			sep.WriteString("  ")
		}
		sep.WriteString(strings.Repeat("─", w))
	}
	p.Line("  %s", sep.String())

	// Print rows.
	for _, row := range rows {
		var line strings.Builder
		for i, cell := range row {
			if i > 0 {
				line.WriteString("  ")
			}
			w := 0
			if i < len(widths) {
				w = widths[i]
			}
			fmt.Fprintf(&line, "%-*s", w, cell)
		}
		p.Line("  %s", line.String())
	}
}

// FormatTime formats a time for human display.
func FormatTime(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	return t.Format("2006-01-02 15:04:05 UTC")
}

// FormatDuration formats a duration for human display.
func FormatDuration(d time.Duration) string {
	if d < time.Second {
		return fmt.Sprintf("%dms", d.Milliseconds())
	}
	if d < time.Minute {
		return fmt.Sprintf("%.1fs", d.Seconds())
	}
	return fmt.Sprintf("%.1fm", d.Minutes())
}

// StatusIcon returns a human-friendly icon for a run status.
// PrintError writes a structured error message to w, made inert for a
// terminal (inertText): an error carries what a server or a run's
// processes wrote. If the error is a RuntimeError it includes the error
// code, node, and hint.
func PrintError(w io.Writer, err error) {
	var b strings.Builder
	var rtErr *runtime.RuntimeError
	if errors.As(err, &rtErr) {
		fmt.Fprintf(&b, "error [%s]: %s\n", rtErr.Code, rtErr.Message)
		if rtErr.NodeID != "" {
			fmt.Fprintf(&b, "  node: %s\n", rtErr.NodeID)
		}
		if rtErr.Hint != "" {
			fmt.Fprintf(&b, "  hint: %s\n", rtErr.Hint)
		}
	} else {
		fmt.Fprintf(&b, "error: %v\n", err)
	}
	_, _ = io.WriteString(w, inertText(b.String()))
}

func StatusIcon(status string) string {
	switch status {
	case "running":
		return "[running]"
	case "paused_waiting_human":
		return "[paused]"
	case "finished":
		return "[done]"
	case "failed":
		return "[FAIL]"
	case "cancelled":
		return "[CANCEL]"
	default:
		return "[" + status + "]"
	}
}
