//go:build desktop

package main

import (
	"log"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
)

func main() {
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
		BackgroundColour:       &options.RGBA{R: 10, G: 10, B: 10, A: 1},
		OnStartup:              app.onStartup,
		OnShutdown:             app.onShutdown,
		OnDomReady:             app.onDomReady,
		OnBeforeClose:          app.onBeforeClose,
		Bind:                   []interface{}{app},
		Menu:                   buildMenu(app),
	})
	if err != nil {
		log.Fatalf("iterion-desktop: wails run failed: %v", err)
	}
}
