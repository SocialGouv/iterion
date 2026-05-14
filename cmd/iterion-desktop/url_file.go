//go:build desktop

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// desktopURLFileName is the discovery file written into the iterion data
// dir on every server (re)start. External processes that want to drive
// the desktop programmatically — e.g. the iterion-control MCP server —
// read it to find the loopback HTTP API endpoint, which would otherwise
// be unreachable (random ephemeral port + Wails-only binding).
const desktopURLFileName = "desktop.json"

// desktopURLState is the on-disk shape of the discovery file.
type desktopURLState struct {
	URL       string `json:"url"`
	PID       int    `json:"pid"`
	StartedAt string `json:"started_at"`
}

// writeDesktopURLFile atomically updates ~/.iterion/desktop.json with
// the current server URL, the desktop PID, and the timestamp. Atomic
// rename means external readers either see the previous URL or the new
// one — never a half-written file.
//
// Failures are non-fatal: the desktop launches normally even if the
// discovery file can't be written (e.g. read-only home), the only
// affected feature is autonomous control via MCP.
func writeDesktopURLFile(url string) {
	dir := desktopDataDir()
	if dir == "" {
		return
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	state := desktopURLState{
		URL:       url,
		PID:       os.Getpid(),
		StartedAt: time.Now().UTC().Format(time.RFC3339),
	}
	payload, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return
	}
	target := filepath.Join(dir, desktopURLFileName)
	tmp := target + ".tmp"
	if err := os.WriteFile(tmp, payload, 0o600); err != nil {
		return
	}
	_ = os.Rename(tmp, target)
}

// removeDesktopURLFile clears the discovery file on shutdown so MCP
// callers don't try to hit a dead port. Failure is silent — a stale
// file is harmless (the MCP server will get a connection error and
// surface it).
func removeDesktopURLFile() {
	dir := desktopDataDir()
	if dir == "" {
		return
	}
	_ = os.Remove(filepath.Join(dir, desktopURLFileName))
}

// desktopDataDir mirrors store.globalIterionDataDir without importing
// the store package (which would pull the runtime in via init chains).
// Resolution order: $ITERION_HOME → $HOME/.iterion → "" (skip the
// discovery file).
func desktopDataDir() string {
	if dir := strings.TrimRight(os.Getenv("ITERION_HOME"), string(filepath.Separator)); dir != "" {
		return dir
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, ".iterion")
	}
	return ""
}
