package runview

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/SocialGouv/iterion/pkg/backend/mcp"
	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/dsl/ast"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

// computeWorkflowHash hashes the workflow source bytes together with
// any bundle resources whose content materially affects the compiled
// workflow. Used by `iterion resume`'s change-detection: if the hash
// changes between original run and resume, the operator must pass
// --force.
//
// Without bundle inclusion, a bundle upgrade that swaps a prompt body
// produces the same hash as the original run — silently changing what
// the agent reads on resume.
func computeWorkflowHash(src []byte, b *bundle.Bundle, _ *ast.File) string {
	h := sha256.New()
	h.Write(src)
	if b != nil && b.PromptsDir != "" {
		if entries, err := os.ReadDir(b.PromptsDir); err == nil {
			names := make([]string, 0, len(entries))
			for _, e := range entries {
				if e.IsDir() {
					continue
				}
				if !strings.HasSuffix(strings.ToLower(e.Name()), ".md") {
					continue
				}
				names = append(names, e.Name())
			}
			sort.Strings(names)
			for _, name := range names {
				body, err := os.ReadFile(filepath.Join(b.PromptsDir, name))
				if err != nil {
					continue
				}
				h.Write([]byte("\x00bundle.prompt:"))
				h.Write([]byte(name))
				h.Write([]byte{0})
				h.Write(body)
			}
		}
	}
	sum := h.Sum(nil)
	return hex.EncodeToString(sum)
}

// CompileWorkflow parses and compiles a .iter file at path. It returns
// the compiled workflow or an error with the first parse / compile
// diagnostic encountered.
//
// MCP server resolution is finalised against the file's directory so
// relative `command` paths in `mcp_server` blocks resolve correctly.
func CompileWorkflow(path string) (*ir.Workflow, error) {
	wf, _, err := compileWith(path, "", false, nil)
	return wf, err
}

// CompileWorkflowWithHash is CompileWorkflow plus a SHA-256 hash of the
// raw source bytes. The hash is persisted in run.json so that resume
// can detect when the .iter file has changed under it (and require
// --force to proceed). Use this everywhere a workflow is loaded for
// execution; CompileWorkflow is for static-only callers (validate,
// diagram).
func CompileWorkflowWithHash(path string) (*ir.Workflow, string, error) {
	return compileWith(path, "", true, nil)
}

// CompileBundleWorkflow is CompileWorkflowWithHash specialised for bundle
// inputs: it merges the bundle's prompts/*.md into the AST File before
// compilation, so node-level prompt references resolve against bundle
// resources during static validation.
func CompileBundleWorkflow(path string, b *bundle.Bundle) (*ir.Workflow, string, error) {
	return compileWith(path, "", true, b)
}

// CompileWorkflowFromSource is the cloud-mode entry point: the .iter
// content is supplied verbatim (uploaded by the studio SPA). Path is
// retained as a logical label for diagnostics + MCP relative-path
// resolution; when empty, MCP resolution falls back to the current
// working directory.
func CompileWorkflowFromSource(path, source string) (*ir.Workflow, string, error) {
	return compileWith(path, source, true, nil)
}

// compileForLaunch picks the right compile path for a Launch / Resume:
// inline source when supplied, on-disk file otherwise. Used by the
// cloud-mode publisher path so a missing FilePath isn't fatal as long
// as the caller uploaded the source.
func compileForLaunch(path, source string) (*ir.Workflow, string, error) {
	if source != "" {
		return CompileWorkflowFromSource(path, source)
	}
	return CompileWorkflowWithHash(path)
}

// ResolveBundleFromFilePath inspects filePath and, when it looks like
// the canonical entrypoint of a directory bundle (named main.bot or
// main.iter, in a parent dir that carries `skills/` or
// `manifest.yaml`), opens the parent as a bundle so the engine can
// mirror skills/, recipes/, attachments/ into the workspace at run
// time. Returns nil when filePath is empty, not the canonical name,
// the parent has no bundle markers, or OpenDir fails (best-effort).
//
// Mirrors the auto-promotion the CLI does in pkg/cli/run.go (F-NEW-4).
// Without this, studio launches of `iterion run examples/whats-next/main.bot`
// silently produce empty `.claude/skills/` and prompts that reference
// `repo-survey.md` fail with `no such file or directory`.
func ResolveBundleFromFilePath(filePath string) *bundle.Bundle {
	if filePath == "" {
		return nil
	}
	abs, err := filepath.Abs(filePath)
	if err != nil {
		return nil
	}
	base := filepath.Base(abs)
	if base != "main.bot" && base != "main.iter" {
		return nil
	}
	parent := filepath.Dir(abs)
	hasMarker := false
	for _, marker := range []string{"skills", "manifest.yaml"} {
		if _, err := os.Stat(filepath.Join(parent, marker)); err == nil {
			hasMarker = true
			break
		}
	}
	if !hasMarker {
		return nil
	}
	b, err := bundle.OpenDir(parent)
	if err != nil {
		return nil
	}
	return b
}

func compileWith(path, inline string, withHash bool, b *bundle.Bundle) (*ir.Workflow, string, error) {
	var src []byte
	if inline != "" {
		src = []byte(inline)
	} else {
		body, err := os.ReadFile(path)
		if err != nil {
			return nil, "", fmt.Errorf("cannot read file: %w", err)
		}
		src = body
	}

	parserPath := path
	if parserPath == "" {
		parserPath = "<inline>"
	}
	pr := parser.Parse(parserPath, string(src))
	for _, d := range pr.Diagnostics {
		if d.Severity == parser.SeverityError {
			return nil, "", fmt.Errorf("parse error: %s", d.Error())
		}
	}
	if pr.File == nil || len(pr.File.Workflows) == 0 {
		return nil, "", fmt.Errorf("no workflow found in %s", parserPath)
	}

	// Bundle prompts must merge into the AST before ir.Compile so the
	// validator sees them when resolving node-level prompt references.
	if b != nil {
		if err := MergeBundlePrompts(pr.File, b); err != nil {
			return nil, "", err
		}
	}

	// Compute the workflow hash AFTER the bundle merge so bundle prompt
	// changes invalidate `iterion resume`'s change-detection. Hashing
	// just the .iter source bytes (the previous behaviour) let a bundle
	// upgrade swap the prompt body without bumping the hash, defeating
	// the whole point of the gate.
	hash := ""
	if withHash {
		hash = computeWorkflowHash(src, b, pr.File)
	}

	cr := ir.Compile(pr.File)
	if cr.HasErrors() {
		for _, d := range cr.Diagnostics {
			if d.Severity == ir.SeverityError {
				return nil, "", fmt.Errorf("compile error: %s", d.Error())
			}
		}
	}
	mcpDir := "."
	if path != "" {
		mcpDir = filepath.Dir(path)
	}
	if err := mcp.PrepareWorkflow(cr.Workflow, mcpDir); err != nil {
		return nil, "", err
	}

	return cr.Workflow, hash, nil
}
