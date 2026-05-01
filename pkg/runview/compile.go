package runview

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"

	"github.com/SocialGouv/iterion/pkg/backend/mcp"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

// CompileWorkflow parses and compiles a .iter file at path. It returns
// the compiled workflow or an error with the first parse / compile
// diagnostic encountered.
//
// MCP server resolution is finalised against the file's directory so
// relative `command` paths in `mcp_server` blocks resolve correctly.
func CompileWorkflow(path string) (*ir.Workflow, error) {
	wf, _, err := compileWith(path, false)
	return wf, err
}

// CompileWorkflowWithHash is CompileWorkflow plus a SHA-256 hash of the
// raw source bytes. The hash is persisted in run.json so that resume
// can detect when the .iter file has changed under it (and require
// --force to proceed). Use this everywhere a workflow is loaded for
// execution; CompileWorkflow is for static-only callers (validate,
// diagram).
func CompileWorkflowWithHash(path string) (*ir.Workflow, string, error) {
	return compileWith(path, true)
}

func compileWith(path string, withHash bool) (*ir.Workflow, string, error) {
	src, err := os.ReadFile(path)
	if err != nil {
		return nil, "", fmt.Errorf("cannot read file: %w", err)
	}

	hash := ""
	if withHash {
		sum := sha256.Sum256(src)
		hash = hex.EncodeToString(sum[:])
	}

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
