// Package main is the Iterion Desktop binary.
//
// The desktop binary wraps pkg/server (the editor HTTP server) and pkg/cli
// (RunEditor) inside a Wails v2 native window. It adds:
//
//   - Multi-project switching (pkg config under ~/.config/Iterion/)
//   - OS keychain integration (Mac Keychain, Win Cred Mgr, libsecret) for
//     LLM API keys, exposed at runtime via os.Setenv
//   - First-run onboarding (project picker, API key entry, CLI detection)
//   - Native menus, window-state persistence, single-instance enforcement
//   - Ed25519-signed auto-update via go-selfupdate
//
// Build tags:
//
//   - The Wails-importing files (main.go, app.go, menu.go, window_state.go,
//     server_host.go, single_instance.go, embed.go, keychain.go, updater.go)
//     carry `//go:build desktop` so the default `go test ./...` does not
//     require Wails as a Go module dep. Run `task desktop:build` (which
//     passes -tags desktop) on a host with the Wails CLI installed.
//   - Platform-neutral pieces (config.go, external_cli.go, path_fix.go) and
//     their tests build under the default tag set so CI always exercises
//     them.
//
// See docs/desktop-architecture.md for the full design rationale.
package main
