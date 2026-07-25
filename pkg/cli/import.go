package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/SocialGouv/iterion/pkg/botimport"
)

// ImportOptions configures RunImport.
type ImportOptions struct {
	// File is the workflow script to import (.js).
	File string
	// Out is the output .bot path. Empty: <workflow-name>.bot next to
	// the source file.
	Out string
	// Name overrides the workflow name (default meta.name, then stem).
	Name string
	// DryRun prints the draft instead of writing it.
	DryRun bool
}

type importReportJSON struct {
	Mapped       []string `json:"mapped,omitempty"`
	Holes        []string `json:"holes,omitempty"`
	Placeholders []string `json:"placeholders,omitempty"`
	Dropped      []string `json:"dropped,omitempty"`
}

type importResultJSON struct {
	WorkflowName   string           `json:"workflow_name"`
	OutPath        string           `json:"out_path,omitempty"`
	DryRun         bool             `json:"dry_run,omitempty"`
	NeedsAttention bool             `json:"needs_attention"`
	Report         importReportJSON `json:"report"`
	BotSource      string           `json:"bot_source,omitempty"`
}

// RunImport converts a Claude-Code workflow script (.js) into a draft
// .bot file. The conversion is lossy by contract: everything the
// lowering can't express becomes an annotated `## IMPORT` marker plus
// a report entry, and the draft is compile-checked before any write.
func RunImport(opts ImportOptions, p *Printer) error {
	if err := importable(opts.File); err != nil {
		return err
	}
	src, err := os.ReadFile(opts.File)
	if err != nil {
		return fmt.Errorf("read %s: %w", opts.File, err)
	}
	res, err := botimport.Import(opts.File, src, botimport.Options{Name: opts.Name})
	if err != nil {
		return err
	}

	outPath := opts.Out
	if outPath == "" {
		outPath = filepath.Join(filepath.Dir(opts.File), res.WorkflowName+".bot")
	}

	if opts.DryRun {
		if p.Format == OutputJSON {
			p.JSON(importResultJSON{
				WorkflowName:   res.WorkflowName,
				DryRun:         true,
				NeedsAttention: res.Report.NeedsAttention(),
				Report:         reportJSON(res.Report),
				BotSource:      res.BotSource,
			})
			return nil
		}
		p.Line("%s", res.BotSource)
		return nil
	}

	// A draft must never silently replace an existing workflow.
	if _, err := os.Stat(outPath); err == nil {
		return fmt.Errorf("refusing to overwrite existing %s — pass --out to pick another path", outPath)
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("stat %s: %w", outPath, err)
	}
	if err := os.WriteFile(outPath, []byte(res.BotSource), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", outPath, err)
	}

	if p.Format == OutputJSON {
		p.JSON(importResultJSON{
			WorkflowName:   res.WorkflowName,
			OutPath:        outPath,
			NeedsAttention: res.Report.NeedsAttention(),
			Report:         reportJSON(res.Report),
		})
		return nil
	}
	p.Line("Imported %s → %s (workflow %s)", opts.File, outPath, res.WorkflowName)
	summarize := func(label string, entries []botimport.ReportEntry) {
		if len(entries) == 0 {
			return
		}
		p.Line("  %s: %d", label, len(entries))
	}
	summarize("mapped", res.Report.Mapped)
	summarize("holes (vars to fill)", res.Report.Holes)
	summarize("placeholders", res.Report.Placeholders)
	summarize("dropped", res.Report.Dropped)
	if res.Report.NeedsAttention() {
		p.Line("Review the ## IMPORT REPORT header in %s before running the draft.", outPath)
	}
	p.Line("Validate with: iterion validate %s", outPath)
	return nil
}

func reportJSON(r *botimport.Report) importReportJSON {
	return importReportJSON{
		Mapped:       botimport.FormatEntries(r.Mapped),
		Holes:        botimport.FormatEntries(r.Holes),
		Placeholders: botimport.FormatEntries(r.Placeholders),
		Dropped:      botimport.FormatEntries(r.Dropped),
	}
}

// importable is a light sanity gate on the input extension so a .bot
// passed by mistake gets a clear message, not a JS parse error.
func importable(path string) error {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".js", ".mjs":
		return nil
	}
	return fmt.Errorf("iterion import expects a workflow script (.js/.mjs), got %s", filepath.Base(path))
}
