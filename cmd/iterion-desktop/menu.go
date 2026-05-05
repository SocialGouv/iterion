//go:build desktop

package main

import (
	goruntime "runtime"

	"github.com/wailsapp/wails/v2/pkg/menu"
	"github.com/wailsapp/wails/v2/pkg/menu/keys"
	wruntime "github.com/wailsapp/wails/v2/pkg/runtime"
)

// buildMenu constructs the native application menu. Items that need to
// re-render dynamically (Open Recent ▶) consult the App at click time
// rather than baking the list at construction time, since recents change
// without a window reload.
func buildMenu(a *App) *menu.Menu {
	m := menu.NewMenu()

	// On macOS the AppMenu (with about/services/quit) is a separate
	// top-level item; we still add a File menu for cross-platform parity.
	if goruntime.GOOS == "darwin" {
		m.Append(menu.AppMenu())
	}

	// File ────────────────────────────────────────────────────────────────
	fileMenu := m.AddSubmenu("File")
	fileMenu.AddText("New project…", keys.CmdOrCtrl("n"), func(_ *menu.CallbackData) {
		wruntime.EventsEmit(a.ctx, eventMenuNewProject)
	})
	fileMenu.AddText("Open project…", keys.CmdOrCtrl("o"), func(_ *menu.CallbackData) {
		wruntime.EventsEmit(a.ctx, eventMenuOpenProject)
	})
	fileMenu.AddText("Switch project…", keys.CmdOrCtrl("p"), func(_ *menu.CallbackData) {
		wruntime.EventsEmit(a.ctx, eventMenuSwitchProject)
	})
	fileMenu.AddSeparator()
	fileMenu.AddText("Settings", keys.CmdOrCtrl(","), func(_ *menu.CallbackData) {
		wruntime.EventsEmit(a.ctx, eventMenuSettings)
	})
	fileMenu.AddSeparator()
	if goruntime.GOOS != "darwin" {
		fileMenu.AddText("Quit", keys.CmdOrCtrl("q"), func(_ *menu.CallbackData) {
			a.Quit()
		})
	}

	// Edit ────────────────────────────────────────────────────────────────
	editMenu := m.AddSubmenu("Edit")
	editMenu.AddText("Undo", keys.CmdOrCtrl("z"), nil)
	editMenu.AddText("Redo", keys.Combo("z", keys.ShiftKey, keys.CmdOrCtrlKey), nil)
	editMenu.AddSeparator()
	editMenu.AddText("Cut", keys.CmdOrCtrl("x"), nil)
	editMenu.AddText("Copy", keys.CmdOrCtrl("c"), nil)
	editMenu.AddText("Paste", keys.CmdOrCtrl("v"), nil)
	editMenu.AddText("Select All", keys.CmdOrCtrl("a"), nil)

	// View ────────────────────────────────────────────────────────────────
	viewMenu := m.AddSubmenu("View")
	viewMenu.AddText("Reload", keys.CmdOrCtrl("r"), func(_ *menu.CallbackData) {
		wruntime.WindowReload(a.ctx)
	})
	viewMenu.AddText("Reload (no cache)", keys.Combo("r", keys.ShiftKey, keys.CmdOrCtrlKey), func(_ *menu.CallbackData) {
		wruntime.WindowReloadApp(a.ctx)
	})
	addDevToolsItem(viewMenu, a)
	viewMenu.AddSeparator()
	viewMenu.AddText("Toggle Fullscreen", keys.Key("F11"), func(_ *menu.CallbackData) {
		if wruntime.WindowIsFullscreen(a.ctx) {
			wruntime.WindowUnfullscreen(a.ctx)
		} else {
			wruntime.WindowFullscreen(a.ctx)
		}
	})

	// Window (mac convention) ─────────────────────────────────────────────
	if goruntime.GOOS == "darwin" {
		winMenu := m.AddSubmenu("Window")
		winMenu.AddText("Minimize", keys.CmdOrCtrl("m"), func(_ *menu.CallbackData) {
			wruntime.WindowMinimise(a.ctx)
		})
	}

	// Help ────────────────────────────────────────────────────────────────
	helpMenu := m.AddSubmenu("Help")
	helpMenu.AddText("Documentation", nil, func(_ *menu.CallbackData) {
		_ = a.OpenExternal("https://github.com/SocialGouv/iterion/tree/main/docs")
	})
	helpMenu.AddText("GitHub", nil, func(_ *menu.CallbackData) {
		_ = a.OpenExternal("https://github.com/SocialGouv/iterion")
	})
	helpMenu.AddText("Report an issue", nil, func(_ *menu.CallbackData) {
		_ = a.OpenExternal("https://github.com/SocialGouv/iterion/issues/new")
	})
	helpMenu.AddSeparator()
	helpMenu.AddText("Check for Updates…", nil, func(_ *menu.CallbackData) {
		go func() {
			rel, err := a.CheckForUpdate()
			if err != nil {
				wruntime.EventsEmit(a.ctx, eventUpdateError, err.Error())
				return
			}
			if rel == nil {
				wruntime.EventsEmit(a.ctx, eventUpdateNone, nil)
				return
			}
			wruntime.EventsEmit(a.ctx, eventUpdateAvailable, rel)
		}()
	})
	helpMenu.AddText("About Iterion", nil, func(_ *menu.CallbackData) {
		wruntime.EventsEmit(a.ctx, eventMenuAbout)
	})

	return m
}
