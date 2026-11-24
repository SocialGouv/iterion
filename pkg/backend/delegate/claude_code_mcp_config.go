package delegate

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/SocialGouv/iterion/pkg/sandbox"
)

// The claude_code spawn's MCP catalog travels as a FILE, never inline on argv.
//
// `--mcp-config` carries the whole resolved catalog as one JSON argument, and
// that document holds each HTTP server's `headers` — among them
// `X-Iterion-Run`, the per-node host-MCP token. Since #2758 that token spends
// the tenant's connector credentials, so it is a credential in its own right,
// and argv is world-readable: /proc/<pid>/cmdline is mode 444 for the whole
// life of the process, on the host for a host spawn and on the host for a
// sandboxed one too (the inline document rides the `docker exec` client's own
// argv).
//
// The run log was already redacted (argvValueRedactedFlags) — that closed the
// copy an operator could read later and left the live one, which is the one an
// unprivileged process on the same host can read while the node runs.
//
// This is the same move the flag settings made for the same reason
// (claudeSettingsFiles: "the routing values can embed a credential … and argv
// is readable by every user of the host"), applied to the other
// credential-bearing flag.

// claudeMCPConfigFileName is the basename of the private file a spawn reads
// its MCP catalog from, on the host and in a sandbox alike.
const claudeMCPConfigFileName = "mcp-config.json"

// sandboxMCPConfigFile is the in-container path of a sandboxed spawn's MCP
// catalog. /tmp is writable in every sandbox image whatever the pinned User —
// the same ground sandboxSettingsFile stands on.
func sandboxMCPConfigFile(mark string) string {
	return "/tmp/iterion-delegate-" + mark + "." + claudeMCPConfigFileName
}

// sandboxSpawnFiles lists every private file a sandboxed spawn writes inside
// the container, so the exec that ends the spawn removes all of them.
//
// ONE list, read by both spawn paths: a file added here is cleaned up by the
// pass that writes it without a second site having to remember.
func sandboxSpawnFiles(mark string) []string {
	return []string{sandboxSettingsFile(mark), sandboxMCPConfigFile(mark)}
}

// takeInlineMCPConfig finds an INLINE `--mcp-config` document on a spawn's
// argv and removes the pair.
//
// Both spellings the CLIs accept are matched (`--flag value` and
// `--flag=value`): rewriting one and leaving the other is how the document
// would stay on argv for the shape nobody tested. A value that is NOT an
// inline document — a path the caller already moved to a file — is left
// exactly where it is: there is nothing to hide in a path, and rewriting it
// would double-write a file the caller owns.
func takeInlineMCPConfig(args []string) (doc string, rest []string, found bool) {
	inline := func(v string) bool { return strings.HasPrefix(strings.TrimSpace(v), "{") }
	for i := 0; i < len(args); i++ {
		if args[i] == "--mcp-config" && i+1 < len(args) {
			if !inline(args[i+1]) {
				continue
			}
			return args[i+1], slices.Concat(args[:i], args[i+2:]), true
		}
		if value, ok := strings.CutPrefix(args[i], "--mcp-config="); ok {
			if !inline(value) {
				continue
			}
			return value, slices.Concat(args[:i], args[i+1:]), true
		}
	}
	return "", args, false
}

// pinSandboxMCPConfig moves a sandboxed spawn's inline MCP catalog into a
// private file INSIDE the container and rewrites argv to read it from there.
//
// Written through sandbox.WriteFileExec, whose whole point is this one: the
// content goes over the writer's STDIN and the file lands 0600 in a 0700
// directory, so neither the host's argv nor the container's holds it. An
// in-container file is also the only shape that works for a sandbox: a host
// /tmp file is invisible to the CLI running under `docker exec`, which is what
// C082 was paid for.
//
// A write failure REFUSES the spawn rather than falling back to the inline
// document: a silent fallback would be the leak, present exactly when
// something is already wrong.
func pinSandboxMCPConfig(ctx context.Context, run sandbox.Run, mark string, args []string) ([]string, error) {
	doc, rest, found := takeInlineMCPConfig(args)
	if !found {
		return args, nil
	}
	// Parsed before it is written: a document the CLI would reject leaves the
	// node with no MCP servers at all, and discovering that as "the tool does
	// not exist" mid-run is the failure this check turns into a refusal.
	if !json.Valid([]byte(doc)) {
		return nil, fmt.Errorf("claude-code: the spawn's --mcp-config document is not valid JSON; refusing to start the CLI")
	}
	path := sandboxMCPConfigFile(mark)
	if err := sandbox.WriteFileExec(ctx, run, path, []byte(doc)); err != nil {
		return nil, fmt.Errorf("claude-code: write the MCP catalog inside the sandbox: %w", err)
	}
	return slices.Concat([]string{"--mcp-config", path}, rest), nil
}
