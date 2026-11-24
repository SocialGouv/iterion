package bots

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// The rgaa sandbox image class (#2632, decision (a)) is the one place a
// SANDBOXED rgaa-audit run finds its two hard requirements without a per-run
// install: Chromium (inherited from the browser base, which names it
// ULTRA11Y_BROWSER_PATH — the contract
// TestRgaaAuditSandboxBrowserImageNamesItsChromiumForTheBot pins) and the
// ultra11y engine itself, installed globally at the ONE version
// bots/rgaa-audit/ultra11y.pin declares, so the bot's npm-global resolution
// arm (`npm root -g`) finds an engine whose fingerprint matches instead of
// paying `npm install ultra11y@<pin>` on every run. A drift between the
// Dockerfile's pin and the bot's pin is a refused engine at run time —
// this test is where it is refused at review time.
func TestTheRgaaSandboxImageCarriesThePinnedUltra11yOnChromium(t *testing.T) {
	raw, err := os.ReadFile("../sandbox/rgaa/Dockerfile")
	if err != nil {
		t.Fatalf("read sandbox/rgaa/Dockerfile: %v — the rgaa image class was declared but its manifest is gone", err)
	}
	dockerfile := string(raw)

	// Chromium arrives through the base: the rgaa variant must build FROM the
	// browser base, or the rendered layer probes « no browser ».
	m := regexp.MustCompile(`(?m)^ARG BASE=(\S+)$`).FindStringSubmatch(dockerfile)
	if m == nil || !strings.Contains(m[1], "iterion-sandbox-browser-base") {
		t.Fatalf("the rgaa image does not build FROM the browser base (ARG BASE=%v) — Chromium and ULTRA11Y_BROWSER_PATH live there", m)
	}

	// The engine, globally installed, at the bot's pin. Three artefacts must
	// agree: ultra11y.pin (the bot's fingerprint), the ARG default below, and
	// the renovate comment that keeps them moving together.
	pin, err := os.ReadFile("rgaa-audit/ultra11y.pin")
	if err != nil {
		t.Fatalf("read rgaa-audit/ultra11y.pin: %v", err)
	}
	want := regexp.MustCompile(`(?m)^version=(\S+)$`).FindStringSubmatch(string(pin))
	if want == nil {
		t.Fatal("rgaa-audit/ultra11y.pin carries no version= line — the pin this test holds the image to is unreadable")
	}
	got := regexp.MustCompile(`(?m)^ARG ULTRA11Y_VERSION=(\S+)$`).FindStringSubmatch(dockerfile)
	if got == nil {
		t.Fatal("sandbox/rgaa/Dockerfile declares no ARG ULTRA11Y_VERSION — the image bakes no engine, and the run pays the per-run npm install it exists to remove")
	}
	if got[1] != want[1] {
		t.Fatalf("the image bakes ultra11y %s but the bot pins %s (rgaa-audit/ultra11y.pin): the bot fingerprints the engine and would REFUSE the image's", got[1], want[1])
	}
	if !regexp.MustCompile(`npm install -g "?ultra11y@\$\{?ULTRA11Y_VERSION\}?`).MatchString(dockerfile) {
		t.Fatal("the image does not `npm install -g ultra11y@${ULTRA11Y_VERSION}` — the bot's npm-global resolution arm would find nothing")
	}
}
