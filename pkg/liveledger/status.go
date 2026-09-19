package liveledger

import (
	"fmt"
	"io"
	"time"
)

// StatusOptions bundles the paths the CLI needs. Kept in a struct so a
// caller can drive it from a test without touching global state.
type StatusOptions struct {
	// LedgerPath resolves to DefaultRelPath by default.
	LedgerPath string
	// TaskfilePath resolves to TaskfileRelPath by default.
	TaskfilePath string
	// WriteBack, when true, seeds a `never` row for every newly
	// enumerated recording target INTO THE FILE (the in-memory table
	// always shows every target). False by default in the CLI: a status
	// read is a read; seeding the committed ledger is an explicit
	// operator action (`-write-back`).
	WriteBack bool
}

// StatusResult is the summary of a Status run. Kept alongside the
// printed table so an automation script can consume it directly.
type StatusResult struct {
	// Targets is the list of live targets enumerated from the
	// Taskfile, sorted by name.
	Targets []LiveTarget
	// Ledger is the ledger the CLI resolved (with fresh never-rows
	// merged in when Options.WriteBack).
	Ledger *Ledger
	// Added counts the never-rows Options.WriteBack introduced.
	Added int
}

// Status enumerates the Taskfile's live targets, joins them against
// the ledger, optionally seeds `never` for any newly-added target,
// prints a sorted-by-staleness table to w, and returns the result.
func Status(w io.Writer, opts StatusOptions) (*StatusResult, error) {
	if opts.LedgerPath == "" {
		opts.LedgerPath = DefaultRelPath
	}
	if opts.TaskfilePath == "" {
		opts.TaskfilePath = TaskfileRelPath
	}
	targets, err := EnumerateLiveTargets(opts.TaskfilePath)
	if err != nil {
		return nil, err
	}
	ledger, err := Load(opts.LedgerPath)
	if err != nil {
		return nil, err
	}
	recording := make([]string, 0, len(targets))
	for _, tt := range targets {
		if tt.Records {
			recording = append(recording, tt.Name)
		}
	}
	added := ledger.EnsureNeverRows(recording)
	if opts.WriteBack && added > 0 {
		if err := ledger.Write(opts.LedgerPath); err != nil {
			return nil, err
		}
	}
	if err := renderTable(w, ledger); err != nil {
		return nil, err
	}
	return &StatusResult{Targets: targets, Ledger: ledger, Added: added}, nil
}

// renderTable writes a fixed-width, human-readable table of the ledger
// sorted by staleness (never first). Duration is printed as a compact
// string; cost as USD; `—` for zero values.
func renderTable(w io.Writer, ledger *Ledger) error {
	rows := ledger.SortByStaleness()
	if _, err := fmt.Fprintf(w, "%-46s %-20s %-8s %-10s %-8s %s\n", "target", "last_run", "verdict", "duration", "cost", "ref"); err != nil {
		return err
	}
	for _, r := range rows {
		if _, err := fmt.Fprintf(w, "%-46s %-20s %-8s %-10s %-8s %s\n",
			r.Target,
			formatTime(r.LastRun),
			string(r.Verdict),
			formatDuration(r.DurationSec),
			formatCost(r.CostUSD),
			formatRef(r.Ref),
		); err != nil {
			return err
		}
	}
	return nil
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	return t.UTC().Format("2006-01-02 15:04Z")
}

func formatDuration(sec int64) string {
	if sec <= 0 {
		return "—"
	}
	d := time.Duration(sec) * time.Second
	// Show as e.g. 9m12s / 1h05m; simple round-trip via Go's default is close enough.
	return d.String()
}

func formatCost(usd float64) string {
	if usd <= 0 {
		return "—"
	}
	return fmt.Sprintf("$%.2f", usd)
}

func formatRef(s string) string {
	if s == "" {
		return "—"
	}
	return s
}
