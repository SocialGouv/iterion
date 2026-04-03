package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"

	"github.com/SocialGouv/iterion/ir"
	"github.com/SocialGouv/iterion/mcp"
	"github.com/SocialGouv/iterion/parser"
)

// ValidateResult holds the outcome of a validate command.
type ValidateResult struct {
	File               string   `json:"file"`
	Valid              bool     `json:"valid"`
	WorkflowName       string   `json:"workflow_name,omitempty"`
	NodeCount          int      `json:"node_count,omitempty"`
	EdgeCount          int      `json:"edge_count,omitempty"`
	ParseDiagnostics   []string `json:"parse_diagnostics,omitempty"`
	CompileDiagnostics []string `json:"compile_diagnostics,omitempty"`
}

// RunValidate parses, compiles, and validates an .iter file.
func RunValidate(path string, p *Printer) error {
	src, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("cannot read file: %w", err)
	}

	result := &ValidateResult{File: path, Valid: true}

	// Parse.
	pr := parser.Parse(path, string(src))
	for _, d := range pr.Diagnostics {
		result.ParseDiagnostics = append(result.ParseDiagnostics, d.Error())
		if d.Severity == parser.SeverityError {
			result.Valid = false
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

	if p.Format == OutputJSON {
		p.JSON(result)
	} else {
		p.Header("Validate: " + path)
		if cr.Workflow != nil {
			p.KV("Workflow", result.WorkflowName)
			p.KV("Nodes", fmt.Sprintf("%d", result.NodeCount))
			p.KV("Edges", fmt.Sprintf("%d", result.EdgeCount))
		}
		allDiags := append(result.ParseDiagnostics, result.CompileDiagnostics...)
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

// compileWorkflow is a shared helper that parses and compiles a .iter file.
// Returns the compiled workflow or an error with diagnostics.
func compileWorkflow(path string) (*ir.Workflow, error) {
	src, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("cannot read file: %w", err)
	}

	pr := parser.Parse(path, string(src))
	for _, d := range pr.Diagnostics {
		if d.Severity == parser.SeverityError {
			return nil, fmt.Errorf("parse error: %s", d.Error())
		}
	}

	if pr.File == nil || len(pr.File.Workflows) == 0 {
		return nil, fmt.Errorf("no workflow found in %s", path)
	}

	cr := ir.Compile(pr.File)
	if cr.HasErrors() {
		for _, d := range cr.Diagnostics {
			if d.Severity == ir.SeverityError {
				return nil, fmt.Errorf("compile error: %s", d.Error())
			}
		}
	}
	if err := mcp.PrepareWorkflow(cr.Workflow, filepath.Dir(path)); err != nil {
		return nil, err
	}

	return cr.Workflow, nil
}

// compileWorkflowWithHash is like compileWorkflow but also returns a SHA-256
// hash of the .iter source, used to detect workflow changes on resume.
func compileWorkflowWithHash(path string) (*ir.Workflow, string, error) {
	src, err := os.ReadFile(path)
	if err != nil {
		return nil, "", fmt.Errorf("cannot read file: %w", err)
	}

	h := sha256.Sum256(src)
	hash := hex.EncodeToString(h[:])

	pr := parser.Parse(path, string(src))
	for _, d := range pr.Diagnostics {
		if d.Severity == parser.SeverityError {
			return nil, "", fmt.Errorf("parse error: %s", d.Error())
		}
	}

	if pr.File == nil || len(pr.File.Workflows) == 0 {
		return nil, "", fmt.Errorf("no workflow found in %s", path)
	}

	cr := ir.Compile(pr.File)
	if cr.HasErrors() {
		for _, d := range cr.Diagnostics {
			if d.Severity == ir.SeverityError {
				return nil, "", fmt.Errorf("compile error: %s", d.Error())
			}
		}
	}
	if err := mcp.PrepareWorkflow(cr.Workflow, filepath.Dir(path)); err != nil {
		return nil, "", err
	}

	return cr.Workflow, hash, nil
}
