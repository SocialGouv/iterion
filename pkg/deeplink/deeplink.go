// Package deeplink builds the URLs that point a human at a page of the studio.
//
// The studio is served under a base path so that "/" can serve the product
// home instead: every studio route lives under StudioBase. That prefix is a
// single decision, and it is made here — six packages used to concatenate
// `base + "/runs/" + id` on their own, which is six places for the next prefix
// change to be applied in five.
//
// Callers keep their own policy about an EMPTY base: with base == "" these
// helpers return a root-relative path, which a same-origin consumer (the
// web-push service worker) resolves and an email cannot. Whether that is
// acceptable is the caller's call, exactly as it was before this package
// existed; nothing here silently turns a link into an empty string.
package deeplink

import (
	"net/url"
	"strings"
)

// StudioBase is the path prefix every studio route sits under. The studio's
// own copy of this value is STUDIO_BASE in studio/src/lib/scope.ts — the two
// must agree, and pkg/server/studio_legacy_redirect.go is what makes an
// already-published URL without the prefix still resolve.
const StudioBase = "/studio"

// Studio joins base, StudioBase and a studio-relative path ("/runs/abc").
func Studio(base, path string) string {
	return strings.TrimRight(base, "/") + StudioBase + path
}

// Path is the root-relative studio path for a studio route — what a
// same-origin redirect needs when it lands an operator back in the studio (an
// OAuth callback, a post-login bounce). Path("") is the studio root.
func Path(route string) string {
	return Studio("", route)
}

// Run is the studio page for one run.
func Run(base, runID string) string {
	return Studio(base, "/runs/"+url.PathEscape(runID))
}

// Runs is the studio's run list.
func Runs(base string) string {
	return Studio(base, "/runs")
}

// LegacyRun is the spelling Run had before the studio moved under StudioBase.
//
// It exists for READING, not for writing: URLs in this shape are already out
// in the world — on forge commit statuses of pull requests still in flight, in
// delivered mail, in posted pull-request comments — and a reader that only
// knows the current spelling would fail to recognise them. Nothing should emit
// it.
func LegacyRun(base, runID string) string {
	return strings.TrimRight(base, "/") + "/runs/" + url.PathEscape(runID)
}
