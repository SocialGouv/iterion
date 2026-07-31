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

// Materialise writes the extension into root and returns its path plus a
// cleanup closure. Callers pass delegate.Task.StateDir's result, which is
// readable from inside the sandbox and, wherever the run allows it, outside the
// target repository's checkout.
//
// That the file leaves the checkout matters more here than for any other
// artifact iterion writes: this extension IS the permission gate — a gated node
// fails rather than run without it. A gate whose implementation sits in a
// directory the sandboxed agent has bash access to undermines itself.
//
// When the run has nowhere better the root is workspace-relative: a sandboxed
// run reaches the workspace, so a file under os.TempDir() would be invisible
// inside the container, and this needs no extra mount — which matters on the
// kubernetes driver, where host binds are rejected outright.
//
// This writes the HOST copy only. A driver whose workspace is a COPY of the
// host's (kubernetes tar-streams it at pod start) never sees a later host
// write, so the caller must also mirror the file into the sandbox — see
// delegate.mirrorStateFileIntoSandbox. Getting that wrong is not subtle: pi
// exits 1 with `Extension path does not exist`.
//
// Each call owns a UNIQUE file. Parallel branches share one WorkDir, so a
// fixed name let one node's deferred cleanup delete the file another node's pi
// had not read yet (`pi -e <missing path>` → a handshake failure), on top of a
// torn read while the same path was being rewritten. The window is widest
// under the sandbox, where the host writes the file and a container that still
// has to boot reads it across the bind mount.
func Materialise(root string) (path string, cleanup func(), err error) {
	if root == "" {
		return "", func() {}, fmt.Errorf("piext: a state dir is required to materialise the extension")
	}
	body, err := assetFS.ReadFile(assetPath)
	if err != nil {
		return "", func() {}, fmt.Errorf("piext: read embedded asset: %w", err)
	}

	if err := os.MkdirAll(root, 0o750); err != nil {
		return "", func() {}, fmt.Errorf("piext: create extension dir: %w", err)
	}
	// The name must keep the .js suffix: pi loads it as an ES module.
	f, err := os.CreateTemp(root, "iterion-pi-*.js")
	if err != nil {
		return "", func() {}, fmt.Errorf("piext: create extension file: %w", err)
	}
	path = f.Name()
	_, werr := f.Write(body)
	cerr := f.Close()
	if werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Chmod(path, 0o600)
	}
	if werr != nil {
		_ = os.Remove(path)
		return "", func() {}, fmt.Errorf("piext: write extension: %w", werr)
	}
	return path, func() { _ = os.Remove(path) }, nil
}

// Asset returns the embedded bundle, for tests that assert on its content
// without touching the filesystem.
func Asset() ([]byte, error) { return assetFS.ReadFile(assetPath) }
