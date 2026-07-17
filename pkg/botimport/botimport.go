// Package botimport converts Claude-Code workflow scripts
// (.claude/workflows/*.js — the `export const meta` + `agent()` /
// `phase()` / `log()` shape) into DRAFT .bot workflows.
//
// The conversion is deliberately LOSSY and never executes any
// JavaScript: the source is parsed with the vendored goja parser (pure
// AST, no VM) and lowered construct-by-construct. Everything the
// lowering understands becomes real DSL (agents, schemas, prompts,
// bounded loops, conditional exits, fan-outs); everything it does not
// becomes an annotated `## IMPORT` placeholder plus a report entry —
// never a plausible-but-wrong translation. The output always carries a
// `## IMPORT REPORT` header and must compile (ir.Compile) before it is
// ever written to disk.
package botimport

import (
	"fmt"
)

// Result is a successful (possibly heavily annotated) import.
type Result struct {
	// BotSource is the generated .bot text, IMPORT REPORT included.
	BotSource string
	// WorkflowName is the sanitized meta.name (fallback: file stem).
	WorkflowName string
	// Report lists what mapped, what degraded, and what was dropped.
	Report *Report
}

// Options tunes an import.
type Options struct {
	// Name overrides the workflow name (default: meta.name, then the
	// source file stem).
	Name string
}

// Import converts one workflow script to a draft .bot. filename is
// used for positions and the fallback workflow name; src is the raw
// JS. The returned error is non-nil only when nothing usable could be
// produced (unparsable JS, no agent calls at all, or the generated
// draft fails to compile — the latter is a bug in the lowering, not
// in the input).
func Import(filename string, src []byte, opts Options) (*Result, error) {
	script, err := parseScript(filename, src)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", filename, err)
	}
	model := lower(script)
	if opts.Name != "" {
		model.WorkflowName = sanitizeIdent(opts.Name)
	}
	if len(model.Nodes) == 0 {
		return nil, fmt.Errorf("no agent() / parallel() / pipeline() calls found in %s — nothing to import", filename)
	}
	botSrc := emit(model)
	if err := validateDraft(filename, botSrc, model.Report); err != nil {
		return nil, err
	}
	// Validation may have appended warnings to the report; re-emit so
	// the embedded IMPORT REPORT header reflects the final state.
	botSrc = emit(model)
	return &Result{
		BotSource:    botSrc,
		WorkflowName: model.WorkflowName,
		Report:       model.Report,
	}, nil
}
