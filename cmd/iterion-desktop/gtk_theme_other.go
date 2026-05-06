//go:build desktop && !linux

package main

// applyLinuxSystemTheme is a Linux-only concern. The macOS WebView and the
// Windows WebView2 control follow the OS appearance natively without
// any Wails-side intervention.
func applyLinuxSystemTheme() {}
