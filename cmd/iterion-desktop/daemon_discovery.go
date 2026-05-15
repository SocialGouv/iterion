//go:build desktop

package main

import (
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// daemonInfo is the on-disk shape of a per-project daemon's discovery
// file. Each `iterion-desktop --server-only --project=<dir>` writes one
// of these to ~/.iterion/daemons/<project-key>.json so the GUI can find
// the daemon for the project the user wants to open without scanning
// every running process.
//
// The legacy global ~/.iterion/desktop.json is still written for
// backward compatibility with the iterion-control MCP, but it points at
// the most-recently-launched daemon — multi-project discovery uses the
// per-project files instead.
type daemonInfo struct {
	URL        string `json:"url"`
	PID        int    `json:"pid"`
	ProjectDir string `json:"project_dir"`
	StartedAt  string `json:"started_at"`
}

const daemonRegistryDirName = "daemons"

// daemonRegistryDir is ~/.iterion/daemons. Per-project files land here.
func daemonRegistryDir() string {
	base := desktopDataDir()
	if base == "" {
		return ""
	}
	return filepath.Join(base, daemonRegistryDirName)
}

// daemonInfoPath returns the absolute path to a project's daemon.json.
// Uses encodeProjectDirKey so the same project always lands at the same
// file (matching the convention pkg/store uses for runs/storedir).
func daemonInfoPath(projectDir string) string {
	dir := daemonRegistryDir()
	if dir == "" || projectDir == "" {
		return ""
	}
	return filepath.Join(dir, encodeProjectDirKey(projectDir)+".json")
}

// writeDaemonInfo atomically updates the per-project daemon.json. Called
// by the daemon on startup; the partner removeDaemonInfo runs on signal
// shutdown so a freshly-killed daemon doesn't leave a stale pointer.
// Failures are silent — the daemon serves regardless, the only impact
// is the GUI can't find it via the registry (it would have to scan).
func writeDaemonInfo(projectDir, url string) {
	target := daemonInfoPath(projectDir)
	if target == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return
	}
	info := daemonInfo{
		URL:        url,
		PID:        os.Getpid(),
		ProjectDir: projectDir,
		StartedAt:  time.Now().UTC().Format(time.RFC3339),
	}
	payload, err := json.MarshalIndent(info, "", "  ")
	if err != nil {
		return
	}
	tmp := target + ".tmp"
	if err := os.WriteFile(tmp, payload, 0o600); err != nil {
		return
	}
	_ = os.Rename(tmp, target)
}

// removeDaemonInfo clears the per-project daemon.json on shutdown.
func removeDaemonInfo(projectDir string) {
	target := daemonInfoPath(projectDir)
	if target == "" {
		return
	}
	_ = os.Remove(target)
}

// findDaemonForProject returns the live daemon URL for the given project
// directory, or ("", false) if none is found OR the recorded daemon is
// dead (pid gone or port not accepting). Stale entries are silently
// pruned so the GUI doesn't attach to a corpse.
func findDaemonForProject(projectDir string) (string, bool) {
	target := daemonInfoPath(projectDir)
	if target == "" {
		return "", false
	}
	raw, err := os.ReadFile(target)
	if err != nil {
		return "", false
	}
	var info daemonInfo
	if err := json.Unmarshal(raw, &info); err != nil {
		_ = os.Remove(target)
		return "", false
	}
	if !daemonAlive(info.PID, info.URL) {
		_ = os.Remove(target)
		return "", false
	}
	return info.URL, true
}

// listLiveDaemons returns every daemon currently registered with a live
// pid + accepting port. Used by the quit popup to surface the list of
// projects whose daemons would survive a GUI close, so the operator
// can decide whether to stop them or keep them running.
func listLiveDaemons() []daemonInfo {
	dir := daemonRegistryDir()
	if dir == "" {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	out := make([]daemonInfo, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		var info daemonInfo
		if err := json.Unmarshal(raw, &info); err != nil {
			continue
		}
		if !daemonAlive(info.PID, info.URL) {
			_ = os.Remove(filepath.Join(dir, e.Name()))
			continue
		}
		out = append(out, info)
	}
	return out
}

// daemonAlive combines a pid-liveness probe (signal 0) with a TCP dial
// against the daemon's port. Both must succeed: pid alone allows a
// crashed daemon to look alive, port alone could let us attach to a
// totally unrelated process that grabbed the same port after a crash.
func daemonAlive(pid int, url string) bool {
	if pid <= 0 || url == "" {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	if err := proc.Signal(syscall.Signal(0)); err != nil {
		return false
	}
	hostPort := strings.TrimRight(
		strings.TrimPrefix(strings.TrimPrefix(url, "http://"), "https://"),
		"/",
	)
	conn, err := net.DialTimeout("tcp", hostPort, 500*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// activeRunsOnDaemon probes a daemon's HTTP surface for in-flight runs
// and returns the count of entries with status=="running". Returns -1
// on any probe failure so callers fall back to the safe path (don't
// auto-stop a daemon we can't introspect).
//
// We count by `status` rather than the daemon's `Active` flag because
// `Active` only flags runs the daemon's own runview.Manager is driving
// — i.e. runs the daemon spawned. CLI-launched runs (`iterion run …`
// against the same store-dir) write status=running but never appear
// in the manager's handle map, so an `Active`-only filter undercounts
// them and the close-confirmation dialog gets skipped: the operator
// closes the GUI window, the daemon SIGTERMs silently, the CLI run
// keeps progressing on its own subprocess but loses HTTP visibility
// (no live log stream, no runs list refresh) until the daemon comes
// back. Counting by status preserves the dialog for both flavours;
// the dialog message already speaks to "background daemons" rather
// than the runs themselves.
//
// Timeout is intentionally tight: this blocks the GUI shutdown path.
func activeRunsOnDaemon(url string) int {
	if url == "" {
		return -1
	}
	base := strings.TrimRight(url, "/")
	client := &http.Client{Timeout: 1500 * time.Millisecond}
	resp, err := client.Get(base + "/api/runs?status=running")
	if err != nil {
		return -1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return -1
	}
	var body struct {
		Runs []struct {
			Status string `json:"status"`
		} `json:"runs"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return -1
	}
	n := 0
	for _, r := range body.Runs {
		if r.Status == "running" {
			n++
		}
	}
	return n
}

// totalActiveRunsAcrossDaemons sums activeRunsOnDaemon for every entry. As
// soon as a single probe fails (returns -1), the function bails with -1 so
// onBeforeClose falls back to the confirmation dialog rather than silently
// killing a daemon we can't verify.
func totalActiveRunsAcrossDaemons(daemons []daemonInfo) int {
	total := 0
	for _, d := range daemons {
		n := activeRunsOnDaemon(d.URL)
		if n < 0 {
			return -1
		}
		total += n
	}
	return total
}

// encodeProjectDirKey mirrors pkg/store/storedir.go's encodeWorkDirKey
// without importing the store package (which would drag the runtime in
// via init chains). Same encoding rules: forward + backward separators
// become "-", colons too (Windows drive letters), and a leading "-" is
// guaranteed so the result is a single safe filesystem component.
//
//	/home/user/work/<project>  ->  -home-user-work-<project>
//	C:\users\u\<project>       ->  -C-users-u-<project>
//
// Keep this in sync with pkg/store/storedir.go if that encoding changes.
func encodeProjectDirKey(absPath string) string {
	p := filepath.ToSlash(absPath)
	p = strings.ReplaceAll(p, `\`, "-")
	p = strings.ReplaceAll(p, ":", "-")
	p = strings.ReplaceAll(p, "/", "-")
	if !strings.HasPrefix(p, "-") {
		p = "-" + p
	}
	return p
}
