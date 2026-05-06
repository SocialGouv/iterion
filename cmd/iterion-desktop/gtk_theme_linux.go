//go:build desktop && linux

package main

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"time"
)

// applyLinuxSystemTheme primes GTK_THEME with a dark variant when the
// user's desktop reports `prefer-dark`, so WebKitGTK 2.40+ exposes
// `prefers-color-scheme: dark` to the WebView. Without this hook, GTK
// defaults to a light variant regardless of the system preference and the
// SPA's matchMedia listener (editor/src/store/theme.ts) sees light mode
// even on a dark Cinnamon/GNOME session. We respect any explicit
// GTK_THEME the user already set in their shell — the override always
// wins.
//
// The linuxdeploy-plugin-gtk AppRun hook does the same when the binary
// runs inside an AppImage; this Go-side path covers raw-binary launches
// (`./build/bin/iterion-desktop-linux-amd64`) and native installs (.deb /
// distro packaging).
func applyLinuxSystemTheme() {
	if os.Getenv("GTK_THEME") != "" {
		return
	}
	if !linuxSystemPrefersDark() {
		return
	}
	_ = os.Setenv("GTK_THEME", "Adwaita:dark")
}

// linuxSystemPrefersDark probes the running desktop for a dark preference.
// Cheap on cold start (single subprocess) and short-circuits on the most
// common signal (org.gnome.desktop.interface color-scheme), which both
// modern GNOME and modern Cinnamon expose under the same GSettings schema.
func linuxSystemPrefersDark() bool {
	if v := os.Getenv("XDG_CURRENT_DESKTOP"); v == "" {
		return false // headless / non-desktop session
	}

	// Bound the probes — gsettings on a misconfigured DBus can hang.
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	probes := [][]string{
		{"gsettings", "get", "org.gnome.desktop.interface", "color-scheme"},
		{"gsettings", "get", "org.cinnamon.desktop.interface", "gtk-theme"},
	}
	for _, args := range probes {
		out, err := exec.CommandContext(ctx, args[0], args[1:]...).Output()
		if err != nil {
			continue
		}
		s := strings.ToLower(string(out))
		if strings.Contains(s, "prefer-dark") || strings.Contains(s, "dark") {
			return true
		}
	}
	return false
}
