// Package cli implements the iterion command-line interface.
package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

// OutputFormat controls how results are rendered.
type OutputFormat int

const (
	OutputHuman OutputFormat = iota
	OutputJSON
)

// Printer writes structured output in the selected format.
type Printer struct {
	W      io.Writer
	Format OutputFormat
}

// NewPrinter creates a Printer writing to stdout.
func NewPrinter(format OutputFormat) *Printer {
	return &Printer{W: os.Stdout, Format: format}
}

// JSON emits v as indented JSON.
func (p *Printer) JSON(v interface{}) {
	enc := json.NewEncoder(p.W)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

// Line prints a formatted line.
func (p *Printer) Line(format string, args ...interface{}) {
	fmt.Fprintf(p.W, format+"\n", args...)
}

// Blank prints an empty line.
func (p *Printer) Blank() { fmt.Fprintln(p.W) }

// Header prints a section header.
func (p *Printer) Header(title string) {
	p.Line("── %s ──", title)
}

// KV prints a key-value pair with aligned formatting.
func (p *Printer) KV(key, value string) {
	p.Line("  %-16s %s", key+":", value)
}

// Table prints rows with column headers.
func (p *Printer) Table(headers []string, rows [][]string) {
	if len(rows) == 0 {
		p.Line("  (none)")
		return
	}

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
		hdr.WriteString(fmt.Sprintf("%-*s", widths[i], h))
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
			line.WriteString(fmt.Sprintf("%-*s", w, cell))
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
	default:
		return "[" + status + "]"
	}
}
