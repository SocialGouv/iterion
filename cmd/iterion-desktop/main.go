//go:build desktop

package main

import (
	_ "embed"
	"log"
	"os"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"github.com/wailsapp/wails/v2/pkg/options/linux"
)

// Window icon for Linux. Wails calls gtk_window_set_icon with these
// bytes at window creation, which posts _NET_WM_ICON on the X window.
// Without this, Linux WMs fall back to the StartupWMClass → .desktop
// → hicolor theme lookup — fragile across DE upgrades (Cinnamon 6.6.3
// → 6.6.4 + mutter / gnome-shell upgrades broke the lookup on Mint,
// leaving every iterion window with a generic icon). Embedding bypasses
// the matcher entirely.
//
// The canonical appicon source is build/appicon.png at the repo root;
// the file here is a copy because cmd/iterion-desktop/build is a
// symlink and //go:embed cannot traverse symlinks. Keep the two
// in sync — see scripts/desktop/build-deb.sh for the packaging side.
//
//go:embed appicon.png
var appIcon []byte

func main() {
	// Source ~/.iterion/env (or $ITERION_ENV_FILE) BEFORE the editor
	// server starts so provider credentials a launching shell didn't
	// export are still available to the runtime — notably so
	// ClawBackend.executeViaSandboxRunner can forward OPENAI_API_KEY
	// and friends into the sandbox runner. .desktop-launched runs
	// have no shell to source ~/.bashrc; this is the file-based
	// equivalent for that path.
	loadIterionEnvFile()

	// `--server-only` runs the HTTP server headless (no Wails GUI),
	// so runs survive `iterion-desktop` GUI rebuild + relaunch cycles.
	// Typical operator workflow:
	//   1. start the daemon once:  iterion-desktop --server-only &
	//   2. launch the GUI normally; GUI detects the running daemon
	//      via ~/.iterion/desktop.json and proxies to its URL instead
	//      of starting its own server
	//   3. close + rebuild + relaunch the GUI as often as needed; the
	//      daemon (and any in-flight runs) keeps running
	// In headless mode we skip Wails, GTK, macOS PATH fixes — none of
	// that is reachable without a windowing system anyway.
	for _, a := range os.Args[1:] {
		if a == "--server-only" || a == "--headless" {
			runHeadless()
			return
		}
	}

	// Prime GTK with the system's color-scheme preference (Linux only)
	// before Wails boots the GTK runtime. No-op on macOS / Windows.
	applyLinuxSystemTheme()

	app := NewApp()

	// AssetServer architecture (see docs/adr/004-desktop-asset-proxy.md):
	//
	// We plug a reverse-proxy http.Handler in as the AssetServer's Handler
	// and leave Assets nil so EVERY request goes through the proxy. This
	// keeps the WebView's main origin on the AssetServer URL (wails:// on
	// Mac/Linux, http://wails.localhost on Windows) — the only origin where
	// Wails injects /wails/runtime.js + /wails/ipc.js into HTML responses
	// and where bindings under window.go.main.App.* are reachable. The
	// proxy forwards to the embedded pkg/server (HTTP API + SPA static
	// assets), and Wails detects the index.html response (text/html) and
	// rewrites it to inject the runtime — solving the cross-origin gap
	// the previous bootstrap-stub redirect pattern left behind.
	//
	// Assets MUST stay nil here. With Assets set to a stub fs containing
	// an index.html, AssetServer would try Assets first and serve the stub
	// before ever calling our Handler — defeating the proxy. The stub
	// embed in embed.go is kept only as a safety net for future debugging
	// (it is unused at runtime).
	//
	// WebSocket carve-out: AssetServer rejects WS upgrades with 501 (it is
	// HTTP-only by design). The editor SPA dials WS endpoints directly at
	// the local server's http://127.0.0.1:<port>/api/ws[/runs/...], using
	// ?t=<sessionToken> for cross-origin authentication.
	err := wails.Run(&options.App{
		Title:     "Iterion",
		Width:     1400,
		Height:    900,
		MinWidth:  800,
		MinHeight: 600,
		AssetServer: &assetserver.Options{
			Assets:  nil,
			Handler: newAssetProxyHandler(app),
		},
		// Wails defaults bindings to the startURL origin only. The editor
		// SPA loads on that startURL via the proxy, so the default is
		// sufficient. We list the loopback origins explicitly so the
		// allowlist is reviewable in one place; in practice no SPA code
		// runs on http://127.0.0.1:* in the desktop binary (only the
		// proxy's outbound calls do).
		BindingsAllowedOrigins: "http://127.0.0.1:*,http://localhost:*",
		// Surface the WebView's native context menu (incl. "Inspect Element")
		// in production builds. Combined with the `-devtools` build flag this
		// gives users a working inspector via right-click; the keyboard
		// counterpart on Linux/WebKit2GTK is Ctrl+Shift+F12 (Wails's
		// hard-wired hotkey, see window.go's InstallF12Hotkey). Without this
		// option Wails calls DisableContextMenu() and right-click is a no-op.
		EnableDefaultContextMenu: true,
		BackgroundColour:         &options.RGBA{R: 10, G: 10, B: 10, A: 255},
		// Linux: embed the appicon bytes so Wails calls
		// gtk_window_set_icon at window creation. Without this Wails
		// leaves _NET_WM_ICON unset and the WM has to fall back to a
		// StartupWMClass → .desktop → theme lookup that breaks across
		// DE upgrades. ProgramName sets g_set_prgname() so window
		// grouping matches the .desktop filename across compositors.
		Linux: &linux.Options{
			Icon:        appIcon,
			ProgramName: "iterion-desktop",
		},
		OnStartup:     app.onStartup,
		OnShutdown:    app.onShutdown,
		OnDomReady:    app.onDomReady,
		OnBeforeClose: app.onBeforeClose,
		Bind:          []interface{}{app},
		Menu:          buildMenu(app),
	})
	if err != nil {
		log.Fatalf("iterion-desktop: wails run failed: %v", err)
	}
}
