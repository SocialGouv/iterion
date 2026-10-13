package bots

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"
)

// The ultra11y engine is pinned in ONE authored place, bots/rgaa-audit/ultra11y.pin,
// and copied mechanically into the places that cannot read it: the bot's own
// var defaults (what `prepare` fingerprints a candidate against), the runner
// image's ARG (what it pre-installs) and the install-time lockfile (asserted by
// TestRgaaAuditEnginePinFileIsWellFormed). A copy that drifts is a bot that
// fingerprints one engine and installs another; this test holds them together.
func TestRgaaAuditBotDefaultsMatchThePin(t *testing.T) {
	pin := rgaaReadPin(t)
	wf := compilePlanPhaseBot(t, "rgaa-audit")
	for name, want := range map[string]string{
		"engine_version": pin.Version,
		"engine_sha256":  pin.SHA256,
		"engine_size":    fmt.Sprint(pin.Size),
	} {
		v, ok := wf.Vars[name]
		if !ok || !v.HasDefault {
			t.Fatalf("rgaa-audit declares no default for var %q", name)
		}
		if got := fmt.Sprint(v.Default); got != want {
			t.Fatalf("rgaa-audit var %s defaults to %q, the pin file says %q", name, got, want)
		}
	}

	raw, err := os.ReadFile("../Dockerfile.runner-devbox")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^ARG ULTRA11Y_VERSION=(\S+)$`).FindStringSubmatch(string(raw))
	if m == nil {
		t.Fatal("Dockerfile.runner-devbox declares no ARG ULTRA11Y_VERSION — the runner image no longer pre-installs the engine")
	}
	if m[1] != pin.Version {
		t.Fatalf("Dockerfile.runner-devbox pre-installs ultra11y %s, the pin file says %s", m[1], pin.Version)
	}
	// The image installs from the committed lockfile, which the root build
	// context excludes with scripts/ unless re-included.
	ignore, err := os.ReadFile("../.dockerignore")
	if err != nil {
		t.Fatal(err)
	}
	exclude := strings.Index(string(ignore), "\nscripts/\n")
	if exclude < 0 {
		t.Fatal(".dockerignore no longer excludes scripts/ — this test guards the re-inclusion that follows it")
	}
	for _, line := range []string{"!scripts/ultra11y-engine/", "!scripts/ultra11y-engine/package.json", "!scripts/ultra11y-engine/package-lock.json"} {
		at := strings.Index(string(ignore), "\n"+line+"\n")
		if at < 0 {
			t.Fatalf(".dockerignore lacks %q: the runner image's COPY of the engine lockfile would fail", line)
		}
		// A negation only re-includes what an EARLIER pattern excluded.
		if at < exclude {
			t.Fatalf(".dockerignore re-includes %q before it excludes scripts/: the later exclusion wins and the COPY fails", line)
		}
	}
}
