// Package piext embeds the iterion pi extension and materialises it for a run.
//
// pi ships no permission system, no MCP client, no subagents. The extension
// closes that gap from outside pi, through its public ExtensionAPI, and is
// loaded as `pi -e <path>`.
//
// # Why embedded rather than installed
//
// The asset is built from ../../../../pi-extension by esbuild into a single
// file with no runtime imports, committed, and compiled into the binary. That
// makes the extension's version STRUCTURALLY locked to the engine that drives
// it — same commit, same binary — so the Go↔extension contract cannot skew.
// The alternatives were worse: `pi install` mutates the operator's own pi
// configuration and re-resolves npm at every start, and `-e npm:…` needs
// network at run start, which is fatal under a sandbox egress policy.
//
// # Why `-e` and not .pi/extensions/
//
// pi's project-trust gate silently ignores `.pi/extensions/` in
// non-interactive modes, so an extension shipped that way would never load and
// never say so. CLI `-e` paths bypass trust resolution entirely — and iterion
// actively refuses the target repo's own `.pi/` (it would execute a checked-out
// repository's TypeScript inside the process holding the run's credentials).
package piext

import (
	"embed"
	"fmt"
	"os"
	"path/filepath"
)

// ContractVersion is the Go↔extension wire version. It must match
// CONTRACT_VERSION in pi-extension/src/config.ts; on a mismatch the extension
// registers nothing and says so loudly, rather than half-wiring a permission
// gate the operator would believe in.
const ContractVersion = "1"

// CtrlVersion is the control-channel envelope version, stamped on every reply.
const CtrlVersion = 1

//go:embed asset/iterion-pi.js
var assetFS embed.FS

const assetPath = "asset/iterion-pi.js"

// Materialise writes the extension into the run's workspace and returns its
// path plus a cleanup closure.
//
// The location is workspace-relative on purpose: a sandboxed run bind-mounts
// the workspace, so a file under os.TempDir() would be invisible inside the
// container — and this needs no extra mount and works identically on the
// kubernetes driver, which rejects host binds.
func Materialise(workDir string) (path string, cleanup func(), err error) {
	if workDir == "" {
		return "", func() {}, fmt.Errorf("piext: a WorkDir is required to materialise the extension")
	}
	body, err := assetFS.ReadFile(assetPath)
	if err != nil {
		return "", func() {}, fmt.Errorf("piext: read embedded asset: %w", err)
	}

	dir := filepath.Join(workDir, ".iterion", "pi")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", func() {}, fmt.Errorf("piext: create extension dir: %w", err)
	}
	path = filepath.Join(dir, "iterion-pi.js")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		return "", func() {}, fmt.Errorf("piext: write extension: %w", err)
	}
	return path, func() { _ = os.Remove(path) }, nil
}

// Asset returns the embedded bundle, for tests that assert on its content
// without touching the filesystem.
func Asset() ([]byte, error) { return assetFS.ReadFile(assetPath) }
