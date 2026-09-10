package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/SocialGouv/iterion/pkg/backend/mcp"
	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/bundlelint"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
	"github.com/SocialGouv/iterion/pkg/internal/appinfo"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/skilllib"
)

// ValidateResult holds the outcome of a validate command.
type ValidateResult struct {
	File               string   `json:"file"`
	Valid              bool     `json:"valid"`
	WorkflowName       string   `json:"workflow_name,omitempty"`
	NodeCount          int      `json:"node_count,omitempty"`
	EdgeCount          int      `json:"edge_count,omitempty"`
	BundleName         string   `json:"bundle_name,omitempty"`
	BundleVersion      string   `json:"bundle_version,omitempty"`
	ParseDiagnostics   []string `json:"parse_diagnostics,omitempty"`
	CompileDiagnostics []string `json:"compile_diagnostics,omitempty"`
	// BundleDiagnostics holds manifest↔workflow consistency findings
	// (bundlelint, C2xx). Kept separate from CompileDiagnostics so the
	// studio can distinguish DSL-level from manifest-level issues.
	BundleDiagnostics []string `json:"bundle_diagnostics,omitempty"`
	// Diagnostics is the structured form of the three lists above: one
	// object per finding with its stage, code, severity, source position,
	// message and fix line — what a validate loop (an agent, an editor, the
	// MCP local_validate tool) acts on. The string lists stay for readers
	// that only print.
	Diagnostics []ValidateDiagnostic `json:"diagnostics,omitempty"`
}

// ValidateDiagnostic is one finding of `iterion validate` in the shape a
// tool can act on: where (file:line:column when the stage could attribute
// one), what (code + message) and the one-line fix.
type ValidateDiagnostic struct {
	Source   string `json:"source"` // parse | compile | bundle
	Code     string `json:"code,omitempty"`
	Severity string `json:"severity"` // error | warning
	File     string `json:"file,omitempty"`
	Line     int    `json:"line,omitempty"`
	Column   int    `json:"column,omitempty"`
	Message  string `json:"message"`
	Hint     string `json:"hint,omitempty"`
	NodeID   string `json:"node_id,omitempty"`
	EdgeID   string `json:"edge_id,omitempty"`
}

// formatDiagnostic renders one finding for the human output: the
// position when known, then severity, code and message on one line, and
// the fix line indented beneath it.
func formatDiagnostic(d ValidateDiagnostic) []string {
	var b strings.Builder
	if d.Line > 0 {
		fmt.Fprintf(&b, "%s:%d:%d: ", d.File, d.Line, d.Column)
	}
	b.WriteString(d.Severity)
	if d.Code != "" {
		fmt.Fprintf(&b, " [%s]", d.Code)
	}
	b.WriteString(": ")
	b.WriteString(d.Message)
	lines := []string{b.String()}
	if d.Hint != "" {
		lines = append(lines, "    fix: "+d.Hint)
	}
	return lines
}

// sortValidateDiagnostics orders the findings the way a reader edits: by
// source position, then code, node, edge and message. A finding with no
// position (a global compile check, a bundle check) goes LAST: it is
// usually a consequence of a positioned one — `entry node "a" not found`
// because a tab on line 2 broke the declaration — and must not be the first
// line the operator reads. The compiler already orders its own list this
// way; RunValidate concatenates the parse, compile and bundle stages, so the
// same key is applied once more across them, otherwise a parse finding on
// line 12 precedes a compile finding on line 7.
func sortValidateDiagnostics(ds []ValidateDiagnostic) {
	sort.SliceStable(ds, func(i, j int) bool {
		a, b := ds[i], ds[j]
		if (a.Line == 0) != (b.Line == 0) {
			return a.Line != 0
		}
		if a.File != b.File {
			return a.File < b.File
		}
		if a.Line != b.Line {
			return a.Line < b.Line
		}
		if a.Column != b.Column {
			return a.Column < b.Column
		}
		if a.Code != b.Code {
			return a.Code < b.Code
		}
		if a.NodeID != b.NodeID {
			return a.NodeID < b.NodeID
		}
		if a.EdgeID != b.EdgeID {
			return a.EdgeID < b.EdgeID
		}
		return a.Message < b.Message
	})
}

// printDiagnostics writes the structured findings under a heading.
func printDiagnostics(p *Printer, diags []ValidateDiagnostic) {
	if len(diags) == 0 {
		return
	}
	p.Blank()
	p.Line("  Diagnostics:")
	for _, d := range diags {
		for _, l := range formatDiagnostic(d) {
			p.Line("    %s", l)
		}
	}
}

// RunValidate parses, compiles, and validates a .bot file or `.botz`
// archive. For bundles, the workflow source is extracted to a cache
// directory and validated; bundle metadata (name, version) is reported
// alongside the workflow result.
func RunValidate(path string, p *Printer) error {
	path = ResolveRecipePath(path)
	if err := requireWorkflowPathExists(path); err != nil {
		return err
	}

	// Bundle dispatch: detect .botz or directory bundles and unpack before
	// validating, via the shared helper (same path as run/resume/doctor).
	// Plain .bot paths fall through with a nil bundle.
	bundleHandle, iterPath, kind, cleanup, err := openBundleOrFile(path)
	if err != nil {
		return fmt.Errorf("cannot open %s: %w", path, err)
	}
	defer func() { _ = cleanup() }()

	var bundleName, bundleVersion, bundleDir string
	if bundleHandle != nil && bundleHandle.Manifest != nil {
		bundleName = bundleHandle.Manifest.Name
		bundleVersion = bundleHandle.Manifest.Version
	}
	switch kind {
	case bundle.KindBundle:
		// A .botz extracts to a cache dir, so the bundle's name-on-disk (used
		// by the per-bot-memory stability check) is the archive's stem.
		bundleDir = strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	case bundle.KindBundleDir:
		// The bundle's OWN root, never the path the operator typed: a bare
		// main.bot promoted to its bundle by openBundleOrFile would else
		// name the FILE, and the per-bot-memory name-stability check (C230)
		// would refuse `validate bots/x/main.bot` on a bundle whose three
		// names agree — while `validate bots/x` passes.
		bundleDir = filepath.Base(bundleHandle.Dir)
	}
	path = iterPath

	src, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("cannot read file: %w", err)
	}

	result := &ValidateResult{
		File:          path,
		Valid:         true,
		BundleName:    bundleName,
		BundleVersion: bundleVersion,
	}

	// Parse — under the path in full: an include resolves beside the file
	// so named, and the compiler refuses a relative name rather than read
	// beside whatever the process sits in.
	parsePath := path
	if abs, err := filepath.Abs(path); err == nil {
		parsePath = abs
	}
	pr := parser.Parse(parsePath, string(src))
	for _, d := range pr.Diagnostics {
		result.ParseDiagnostics = append(result.ParseDiagnostics, d.Error())
		result.Diagnostics = append(result.Diagnostics, ValidateDiagnostic{
			Source:   "parse",
			Code:     string(d.Code),
			Severity: d.Severity.String(),
			File:     d.File,
			Line:     d.Line,
			Column:   d.Column,
			Message:  d.Message,
			Hint:     d.Hint,
		})
		if d.Severity == parser.SeverityError {
			result.Valid = false
		}
	}

	// Bundle prompts must merge into the AST before ir.Compile validates
	// node-level prompt references.
	if bundleHandle != nil && pr.File != nil {
		if err := runview.MergeBundlePrompts(pr.File, bundleHandle); err != nil {
			return fmt.Errorf("bundle: merge prompts: %w", err)
		}
	}

	if pr.File == nil || len(pr.File.Workflows) == 0 {
		result.Valid = false
		sortValidateDiagnostics(result.Diagnostics)
		if p.Format == OutputJSON {
			p.JSON(result)
		} else {
			p.Header("Validate: " + path)
			printDiagnostics(p, result.Diagnostics)
			p.Line("  result: INVALID (no workflow found)")
		}
		return validationFailed(p)
	}

	// Compile (includes static validation).
	cr := ir.Compile(pr.File)
	for _, d := range cr.Diagnostics {
		result.CompileDiagnostics = append(result.CompileDiagnostics, d.Error())
		result.Diagnostics = append(result.Diagnostics, ValidateDiagnostic{
			Source:   "compile",
			Code:     string(d.Code),
			Severity: d.Severity.String(),
			File:     d.File,
			Line:     d.Line,
			Column:   d.Column,
			Message:  d.Message,
			Hint:     d.Hint,
			NodeID:   d.NodeID,
			EdgeID:   d.EdgeID,
		})
		if d.Severity == ir.SeverityError {
			result.Valid = false
		}
	}

	if cr.Workflow != nil {
		if err := mcp.PrepareWorkflow(cr.Workflow, filepath.Dir(path)); err != nil {
			result.CompileDiagnostics = append(result.CompileDiagnostics, err.Error())
			result.Diagnostics = append(result.Diagnostics, ValidateDiagnostic{
				Source:   "compile",
				Severity: "error",
				Message:  err.Error(),
			})
			result.Valid = false
		}
		result.WorkflowName = cr.Workflow.Name
		result.NodeCount = len(cr.Workflow.Nodes)
		result.EdgeCount = len(cr.Workflow.Edges)
	}

	// Bundle consistency: cross-check the manifest against the compiled
	// workflow (var maps, forge secret, capabilities, per-bot-memory name
	// stability). Only runs for bundles; plain .bot files have no manifest.
	if bundleHandle != nil && bundleHandle.Manifest != nil && cr.Workflow != nil {
		diags := bundlelint.CheckConsistency(bundlelint.Input{
			Manifest:    bundleHandle.Manifest,
			Workflow:    cr.Workflow,
			Frontmatter: bundle.ParseFrontmatter(src), // reuse the bytes already read
			DirName:     bundleDir,
			Skills:      scanBundleSkills(bundleHandle.SkillsDir),
			// The engine contract (C250/C251), held against THIS binary — the
			// author's local half of the guard the push admission and the
			// runner apply on a deployment.
			EngineBuild: appinfo.FullVersion(),
		})
		for _, d := range diags {
			result.BundleDiagnostics = append(result.BundleDiagnostics, d.Error())
			msg := d.Message
			if d.Field != "" {
				msg = d.Field + ": " + msg
			}
			result.Diagnostics = append(result.Diagnostics, ValidateDiagnostic{
				Source:   "bundle",
				Code:     string(d.Code),
				Severity: d.Severity.String(),
				Message:  msg,
				Hint:     d.Hint,
			})
			if d.Severity == bundlelint.SeverityError {
				result.Valid = false
			}
		}
	}

	sortValidateDiagnostics(result.Diagnostics)
	if p.Format == OutputJSON {
		p.JSON(result)
	} else {
		p.Header("Validate: " + path)
		if bundleName != "" || bundleVersion != "" {
			p.KV("Bundle", bundleName+" "+bundleVersion)
		}
		if cr.Workflow != nil {
			p.KV("Workflow", result.WorkflowName)
			p.KV("Nodes", fmt.Sprintf("%d", result.NodeCount))
			p.KV("Edges", fmt.Sprintf("%d", result.EdgeCount))
		}
		printDiagnostics(p, result.Diagnostics)
		p.Blank()
		if result.Valid {
			p.Line("  result: OK")
		} else {
			p.Line("  result: INVALID")
		}
	}

	if !result.Valid {
		return validationFailed(p)
	}
	return nil
}

// validationFailed is the non-zero exit of an invalid workflow. In --json mode
// the result already carries the verdict and every finding, so the error is
// marked ErrReported and the CLI prints nothing more.
func validationFailed(p *Printer) error {
	if p.Format == OutputJSON {
		return fmt.Errorf("validation failed: %w", ErrReported)
	}
	return fmt.Errorf("validation failed")
}

// scanBundleSkills reads a bundle's skills/*.md frontmatter (name +
// description) for the routability lint (C231–C234), using the shared
// skilllib parser so it never drifts from run-time discovery. An empty or
// missing dir yields no docs (the checks then no-op). Non-.md entries and
// unreadable files are skipped silently — the lint judges routability of the
// skills that exist, not filesystem health.
func scanBundleSkills(skillsDir string) []bundlelint.SkillDoc {
	if skillsDir == "" {
		return nil
	}
	entries, err := os.ReadDir(skillsDir)
	if err != nil {
		return nil
	}
	var docs []bundlelint.SkillDoc
	for _, e := range entries {
		if e.IsDir() || !strings.EqualFold(filepath.Ext(e.Name()), ".md") {
			continue
		}
		f, err := os.Open(filepath.Join(skillsDir, e.Name()))
		if err != nil {
			continue
		}
		name, desc := skilllib.ScanFrontmatter(f)
		_ = f.Close()
		docs = append(docs, bundlelint.SkillDoc{
			Path:        filepath.Join(bundle.DirSkills, e.Name()),
			Name:        name,
			Description: desc,
		})
	}
	return docs
}
