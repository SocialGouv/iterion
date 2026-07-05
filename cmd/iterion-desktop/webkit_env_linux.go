//go:build desktop && linux

package main

import (
	"log"
	"os"
)

// applyLinuxWebKitFixes sets WebKitGTK environment defaults that make the
// embedded webview render reliably across GPUs — it MUST run before Wails
// boots GTK/WebKit (WebKit reads these at web-view creation and propagates them
// to any renderer/GPU subprocess it spawns).
//
// The DMABUF renderer (WebKitGTK 2.36+) fails to composite on a wide range of
// Linux setups — NVIDIA and hybrid-GPU laptops, some Intel/Mesa drivers,
// Wayland-on-NVIDIA, and many VMs — leaving the window black with only the
// native GTK menu bar visible. Disabling it falls back to a
// universally-compatible renderer at negligible cost, so we set it as the
// default rather than shipping an app that is black on common hardware. A value
// the user already exported is respected (e.g. to force the DMABUF path back
// on, or to swap in WEBKIT_DISABLE_COMPOSITING_MODE for an exotic GPU).
func applyLinuxWebKitFixes() {
	if _, set := os.LookupEnv("WEBKIT_DISABLE_DMABUF_RENDERER"); !set {
		_ = os.Setenv("WEBKIT_DISABLE_DMABUF_RENDERER", "1")
		log.Printf("desktop: WEBKIT_DISABLE_DMABUF_RENDERER=1 (reliable webview rendering across GPUs; export your own value to override)")
	}
}
