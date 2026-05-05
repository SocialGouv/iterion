//go:build darwin

package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// pathFixSentinel marks the line in the shell's stdout that contains the
// real PATH. Using a sentinel insulates us from rc-file noise (banners,
// fortune programs, motd hooks) that might otherwise pollute our parse.
const pathFixSentinel = "__ITER_PATH_BEGIN__"

// applyMacOSPathFix runs the user's login shell once with `-ilc` to
// capture the PATH a Terminal session would see, then merges it into
// our process PATH.
//
// Why: a `.app` launched from Finder/Dock inherits the system minimal
// PATH (/usr/bin:/bin:/usr/sbin:/sbin). Tools installed via Homebrew
// (/opt/homebrew/bin), asdf, devbox, fnm, etc., are invisible to
// exec.LookPath unless we replicate the login-shell PATH.
//
// We deduplicate to keep the resulting PATH bounded (~ tens of entries)
// and never lose system paths the inheriter started with.
func applyMacOSPathFix() error {
	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/zsh"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// Sentinel-wrapped echo so we can grep for the real PATH line even if
	// the user's rc files emit banners.
	cmd := exec.CommandContext(ctx, shell, "-ilc", fmt.Sprintf("printf '%s%%s' \"$PATH\"", pathFixSentinel))
	out, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("login-shell PATH probe: %w", err)
	}
	got := string(out)
	idx := strings.Index(got, pathFixSentinel)
	if idx < 0 {
		return fmt.Errorf("login-shell PATH probe: sentinel not found in output (%d bytes)", len(got))
	}
	loginPath := strings.TrimSpace(got[idx+len(pathFixSentinel):])
	if loginPath == "" {
		return nil
	}
	merged := mergePaths(os.Getenv("PATH"), loginPath)
	if merged == "" {
		return nil
	}
	return os.Setenv("PATH", merged)
}

// mergePaths combines two ":"-delimited PATHs preserving order and removing
// duplicates. The first argument's order is preserved; entries from the
// second that aren't already present are appended.
func mergePaths(current, extra string) string {
	seen := map[string]bool{}
	var out []string
	add := func(p string) {
		p = strings.TrimSpace(p)
		if p == "" || seen[p] {
			return
		}
		seen[p] = true
		out = append(out, p)
	}
	for _, p := range strings.Split(current, ":") {
		add(p)
	}
	for _, p := range strings.Split(extra, ":") {
		add(p)
	}
	return strings.Join(out, ":")
}
