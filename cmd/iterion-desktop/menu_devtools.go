//go:build desktop && dev

package main

import (
	"github.com/wailsapp/wails/v2/pkg/menu"
	"github.com/wailsapp/wails/v2/pkg/menu/keys"
	wruntime "github.com/wailsapp/wails/v2/pkg/runtime"
)

// addDevToolsItem appends a "Toggle DevTools" entry only in dev builds.
// We don't ship DevTools to release builds — the webview is meant to look
// and feel like a native app.
func addDevToolsItem(parent *menu.Menu, a *App) {
	parent.AddText("Toggle DevTools", keys.Combo("i", keys.OptionOrAltKey, keys.CmdOrCtrlKey), func(_ *menu.CallbackData) {
		wruntime.WindowExecJS(a.ctx, "console.log('devtools toggle')")
	})
}
