package main

import (
	"context"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// CLIStatus is the wire-format reported to the frontend for each external
// CLI we can detect.
type CLIStatus struct {
	Name       string `json:"name"`
	Found      bool   `json:"found"`
	Path       string `json:"path,omitempty"`
	Version    string `json:"version,omitempty"`
	InstallURL string `json:"install_url"`
}

// knownCLI describes one external tool we look for. Adding a new entry
// here propagates to onboarding + the settings page automatically.
type knownCLI struct {
	Name       string
	Binary     string
	VersionArg string
	InstallURL string
}

var knownCLIs = []knownCLI{
	{Name: "claude", Binary: "claude", VersionArg: "--version", InstallURL: "https://docs.anthropic.com/en/docs/claude-code/install"},
	{Name: "codex", Binary: "codex", VersionArg: "--version", InstallURL: "https://github.com/openai/codex"},
	{Name: "git", Binary: "git", VersionArg: "--version", InstallURL: "https://git-scm.com/downloads"},
}

// looker abstracts exec.LookPath / exec.CommandContext for tests.
type looker interface {
	LookPath(string) (string, error)
	Run(ctx context.Context, name string, args ...string) (string, error)
}

type realLooker struct{}

func (realLooker) LookPath(name string) (string, error) { return exec.LookPath(name) }
func (realLooker) Run(ctx context.Context, name string, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	return string(out), err
}

// 60s in-memory cache so repeat UI polls don't fork a process per call.
var (
	cliCacheMu     sync.Mutex
	cliCache       []CLIStatus
	cliCachedAt    time.Time
	cliCacheTTL           = 60 * time.Second
	cliCacheLooker looker = realLooker{}
)

// DetectExternalCLIs probes for each tool in knownCLIs. force=true bypasses
// the 60s cache.
func DetectExternalCLIs(force bool) []CLIStatus {
	cliCacheMu.Lock()
	if !force && cliCache != nil && time.Since(cliCachedAt) < cliCacheTTL {
		out := append([]CLIStatus(nil), cliCache...)
		cliCacheMu.Unlock()
		return out
	}
	cliCacheMu.Unlock()

	out := detectWith(cliCacheLooker)

	cliCacheMu.Lock()
	cliCache = out
	cliCachedAt = time.Now()
	cliCacheMu.Unlock()
	return out
}

func detectWith(l looker) []CLIStatus {
	out := make([]CLIStatus, 0, len(knownCLIs))
	for _, kc := range knownCLIs {
		st := CLIStatus{Name: kc.Name, InstallURL: kc.InstallURL}
		path, err := l.LookPath(kc.Binary)
		if err != nil || path == "" {
			out = append(out, st)
			continue
		}
		st.Found = true
		st.Path = path
		// Best-effort version probe with a 2s timeout so a wedged binary
		// doesn't block the rest of the detection.
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		raw, err := l.Run(ctx, path, kc.VersionArg)
		cancel()
		if err == nil {
			st.Version = strings.TrimSpace(strings.SplitN(raw, "\n", 2)[0])
		}
		out = append(out, st)
	}
	return out
}
