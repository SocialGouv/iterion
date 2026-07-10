//go:build desktop && !linux

package main

// applyLinuxWebKitFixes is a no-op on macOS / Windows — the WebKitGTK DMABUF
// renderer issue is Linux-only (WKWebView / WebView2 don't have it).
func applyLinuxWebKitFixes() {}
