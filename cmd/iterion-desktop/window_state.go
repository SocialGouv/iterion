//go:build desktop

package main

import (
	"context"

	wruntime "github.com/wailsapp/wails/v2/pkg/runtime"
)

// applyWindowState restores the window's size/position from persisted
// state, defending against the multi-monitor "monitor went away" case
// (we centre the window if its previous origin is off-screen).
func applyWindowState(ctx context.Context, ws WindowState) {
	if ws.Width > 0 && ws.Height > 0 {
		wruntime.WindowSetSize(ctx, ws.Width, ws.Height)
	}
	if !isOffScreen(ctx, ws.X, ws.Y, ws.Width, ws.Height) {
		wruntime.WindowSetPosition(ctx, ws.X, ws.Y)
	} else {
		wruntime.WindowCenter(ctx)
	}
	if ws.Maximised {
		wruntime.WindowMaximise(ctx)
	}
	if ws.Fullscreen {
		wruntime.WindowFullscreen(ctx)
	}
}

// readWindowState samples the current window geometry. Called from
// onBeforeClose so we capture it before Wails tears down.
func readWindowState(ctx context.Context, prev WindowState) WindowState {
	w, h := wruntime.WindowGetSize(ctx)
	x, y := wruntime.WindowGetPosition(ctx)
	out := WindowState{
		Width:      w,
		Height:     h,
		X:          x,
		Y:          y,
		Maximised:  wruntime.WindowIsMaximised(ctx),
		Fullscreen: wruntime.WindowIsFullscreen(ctx),
	}
	// Sanity check: 0×0 happens on some WMs during teardown — keep prev.
	if out.Width <= 0 || out.Height <= 0 {
		return prev
	}
	return out
}

// isOffScreen returns true if the rectangle does not intersect any
// connected screen. We default to "off-screen" when enumeration fails so
// the caller falls back to WindowCenter rather than restoring possibly
// stale coordinates from a now-disconnected monitor.
func isOffScreen(ctx context.Context, x, y, w, h int) bool {
	if w <= 0 || h <= 0 {
		return true
	}
	screens, err := wruntime.ScreenGetAll(ctx)
	if err != nil || len(screens) == 0 {
		return true
	}
	for _, s := range screens {
		if rectIntersects(x, y, w, h, 0, 0, s.Size.Width, s.Size.Height) {
			return false
		}
	}
	return true
}

func rectIntersects(ax, ay, aw, ah, bx, by, bw, bh int) bool {
	return ax < bx+bw && ax+aw > bx && ay < by+bh && ay+ah > by
}
