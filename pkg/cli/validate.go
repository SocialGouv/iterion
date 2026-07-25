package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/SocialGouv/iterion/pkg/backend/mcp"
	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/bundlelint"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
	"github.com/SocialGouv/iterion/pkg/runview"
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
		bundleDir = filepath.Base(path)
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

	// Parse.
	pr := parser.Parse(path, string(src))
	for _, d := range pr.Diagnostics {
		result.ParseDiagnostics = append(result.ParseDiagnostics, d.Error())
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
		if p.Format == OutputJSON {
			p.JSON(result)
		} else {
			p.Header("Validate: " + path)
			for _, d := range result.ParseDiagnostics {
				p.Line("  %s", d)
			}
			p.Line("  result: INVALID (no workflow found)")
		}
		return fmt.Errorf("validation failed")
	}

	// Compile (includes static validation).
	cr := ir.Compile(pr.File)
	for _, d := range cr.Diagnostics {
		result.CompileDiagnostics = append(result.CompileDiagnostics, d.Error())
		if d.Severity == ir.SeverityError {
			result.Valid = false
		}
	}

	if cr.Workflow != nil {
		if err := mcp.PrepareWorkflow(cr.Workflow, filepath.Dir(path)); err != nil {
			result.CompileDiagnostics = append(result.CompileDiagnostics, err.Error())
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
		})
		for _, d := range diags {
			result.BundleDiagnostics = append(result.BundleDiagnostics, d.Error())
			if d.Severity == bundlelint.SeverityError {
				result.Valid = false
			}
		}
	}

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
		allDiags := append(result.ParseDiagnostics, result.CompileDiagnostics...)
		allDiags = append(allDiags, result.BundleDiagnostics...)
		if len(allDiags) > 0 {
			p.Blank()
			p.Line("  Diagnostics:")
			for _, d := range allDiags {
				p.Line("    %s", d)
			}
		}
		p.Blank()
		if result.Valid {
			p.Line("  result: OK")
		} else {
			p.Line("  result: INVALID")
		}
	}

	if !result.Valid {
		return fmt.Errorf("validation failed")
	}
	return nil
}
