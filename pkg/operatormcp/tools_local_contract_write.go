package operatormcp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/SocialGouv/iterion/pkg/cli"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

// The write tool is intentionally scoped to standalone sources. Bundle
// manifests and multi-file source snapshots require their own transaction.
func handleLocalContractWrite(ctx context.Context, s *Server, raw json.RawMessage) (string, bool, error) {
	var args struct {
		FilePath       string `json:"file_path"`
		Source         string `json:"source"`
		ExpectedSHA256 string `json:"expected_sha256"`
	}
	if err := unmarshalArgs(raw, &args); err != nil {
		return "", false, err
	}
	if args.FilePath == "" || filepath.Ext(args.FilePath) != ".bot" || len(args.Source) == 0 || len(args.Source) > 1<<20 ||
		(args.ExpectedSHA256 != "absent" && (len(args.ExpectedSHA256) != 64 || strings.Trim(args.ExpectedSHA256, "0123456789abcdef") != "")) {
		return "", false, fmt.Errorf("file_path must be a standalone .bot, source must be 1..1048576 bytes, and expected_sha256 must be a lowercase SHA-256 or 'absent'")
	}
	target, err := localContractPath(s, args.FilePath)
	if err != nil {
		return "", false, err
	}
	parent := filepath.Dir(target)
	if err := ctx.Err(); err != nil {
		return "", false, err
	}
	parsed := parser.Parse(target, args.Source)
	for _, d := range parsed.Diagnostics {
		if d.Severity == parser.SeverityError {
			return "", false, fmt.Errorf("invalid contract source: %s", d.Error())
		}
	}
	if parsed.File == nil {
		return "", false, fmt.Errorf("invalid contract source: no workflow")
	}
	compiled := ir.Compile(parsed.File)
	for _, d := range compiled.Diagnostics {
		if d.Severity == ir.SeverityError {
			return "", false, fmt.Errorf("invalid contract source: %s", d.Error())
		}
	}
	if compiled.Workflow == nil || compiled.Workflow.RuntimeSemantics != ir.RuntimeSemanticsPortsV1 || compiled.Workflow.PublicView() == nil {
		return "", false, fmt.Errorf("contract source must compile as ports-v1 with a public workflow view")
	}

	// Serialize writes made through this MCP server; the hash is checked again
	// immediately before replacement so a stale Copi draft cannot silently
	// overwrite a newer local edit. Independent filesystem writers are outside
	// this cooperative lock and remain the caller's responsibility.
	s.spawnGate.Lock()
	defer s.spawnGate.Unlock()
	mode, err := checkContractWriteTarget(target, args.ExpectedSHA256)
	if err != nil {
		return "", false, err
	}
	tmp, err := os.CreateTemp(parent, ".iterion-contract-*.bot")
	if err != nil {
		return "", false, err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return "", false, err
	}
	if _, err := tmp.WriteString(args.Source); err != nil {
		tmp.Close()
		return "", false, err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return "", false, err
	}
	if err := tmp.Close(); err != nil {
		return "", false, err
	}
	validation, validateErr := captureJSON(func(p *cli.Printer) error { return cli.RunValidate(tmpPath, p) })
	if validateErr != nil && validation == "" {
		return "", false, fmt.Errorf("validate staged contract source: %w", validateErr)
	}
	var checked cli.ValidateResult
	if err := json.Unmarshal([]byte(validation), &checked); err != nil {
		return "", false, fmt.Errorf("decode staged validation: %w", err)
	}
	if !checked.Valid || checked.PublicView == nil {
		return "", false, fmt.Errorf("staged contract source failed complete validation: %v", checked.Diagnostics)
	}
	if err := ctx.Err(); err != nil {
		return "", false, err
	}
	if _, err := checkContractWriteTarget(target, args.ExpectedSHA256); err != nil {
		return "", false, err
	}
	if args.ExpectedSHA256 == "absent" {
		// Link creates the target only if absent; a concurrent creator cannot be
		// replaced even between the last hash check and this publication.
		if err := os.Link(tmpPath, target); err != nil {
			return "", false, fmt.Errorf("publish new contract source: %w", err)
		}
	} else if err := os.Rename(tmpPath, target); err != nil {
		return "", false, fmt.Errorf("replace contract source: %w", err)
	}
	if dir, err := os.Open(parent); err == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
	sum := sha256.Sum256([]byte(args.Source))
	view := compiled.Workflow.PublicView()
	return marshalContractWriteResult(target, hex.EncodeToString(sum[:]), view)
}

func localContractPath(s *Server, path string) (string, error) {
	if path == "" || filepath.Ext(path) != ".bot" {
		return "", fmt.Errorf("file_path must name a standalone .bot source")
	}
	base, err := filepath.EvalSymlinks(s.WorkDir)
	if err != nil {
		return "", err
	}
	base, err = filepath.Abs(base)
	if err != nil {
		return "", err
	}
	target, err := filepath.Abs(s.resolvePath(path))
	if err != nil {
		return "", err
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(target))
	if err != nil {
		return "", err
	}
	target = filepath.Join(parent, filepath.Base(target))
	rel, err := filepath.Rel(base, target)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("contract source must stay inside the MCP working directory")
	}
	return target, nil
}

func handleLocalContractRead(ctx context.Context, s *Server, raw json.RawMessage) (string, bool, error) {
	var args struct {
		FilePath      string `json:"file_path"`
		IncludeSource bool   `json:"include_source"`
	}
	if err := unmarshalArgs(raw, &args); err != nil {
		return "", false, err
	}
	path, err := localContractPath(s, args.FilePath)
	if err != nil {
		return "", false, err
	}
	if err := ctx.Err(); err != nil {
		return "", false, err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return "", false, err
	}
	if !info.Mode().IsRegular() {
		return "", false, fmt.Errorf("contract source is not a regular file")
	}
	source, err := os.ReadFile(path)
	if err != nil {
		return "", false, err
	}
	sum := sha256.Sum256(source)
	validation, validateErr := captureJSON(func(p *cli.Printer) error { return cli.RunValidate(path, p) })
	if validateErr != nil && validation == "" {
		return "", false, validateErr
	}
	var result cli.ValidateResult
	if err := json.Unmarshal([]byte(validation), &result); err != nil {
		return "", false, fmt.Errorf("decode validation result: %w", err)
	}
	again, err := os.ReadFile(path)
	if err != nil || sha256.Sum256(again) != sum {
		return "", false, fmt.Errorf("contract source changed while reading")
	}
	view := map[string]any{"file_path": path, "sha256": hex.EncodeToString(sum[:]), "valid": result.Valid,
		"public_view": result.PublicView, "conversion_draft": result.ConversionDraft}
	if args.IncludeSource {
		view["source"] = string(source)
		view["diagnostics"] = result.Diagnostics
	}
	returnValue, err := marshalText(view)
	return returnValue, false, err
}

func checkContractWriteTarget(path, expected string) (os.FileMode, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		if expected != "absent" {
			return 0, fmt.Errorf("contract source changed: expected an existing SHA-256")
		}
		return 0o644, nil
	}
	if err != nil {
		return 0, err
	}
	if !info.Mode().IsRegular() || expected == "absent" {
		return 0, fmt.Errorf("contract source changed or is not a regular file")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != expected {
		return 0, fmt.Errorf("contract source changed: expected_sha256 is stale")
	}
	return info.Mode().Perm(), nil
}

func marshalContractWriteResult(path, sha string, view *ir.PublicWorkflowView) (string, bool, error) {
	result, err := marshalText(map[string]any{"written": true, "file_path": path, "sha256": sha, "public_view": view})
	return result, false, err
}
