//go:build desktop && !dev

package main

import "github.com/wailsapp/wails/v2/pkg/menu"

// addDevToolsItem is a no-op in release builds — DevTools entry is only
// surfaced when the binary is built with `-tags "desktop dev"`.
func addDevToolsItem(_ *menu.Menu, _ *App) {}
