package bots

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// #2350 — the published sec image (sandbox/sec/Dockerfile) installed
// `deepsec@latest` behind a best-effort `|| echo`, and the Layer 5 sanity
// step only LOGGED deepsec's presence, so the shipped :edge ran whatever
// npm served on the last base rebuild — across the 2.3.x release that
// changed `init` semantics. The fix is one exact pin with a renovate
// annotation, and a sanity step that FAILS on an absent or drifted
// deepsec, naming the version it found. The local-dogfood augmentation
// Dockerfile (bots/sec-audit-source/sandbox/sec/Dockerfile) that used to
// carry the only pin is deleted; the pin lives in the published image.
//
// Before the fix this test is red twice over: the ARG reads `latest`
// (not an exact version) and the sanity block carries no `exit 1`.
const secImageDockerfile = "../sandbox/sec/Dockerfile"

var deepsecPinArgRe = regexp.MustCompile(`(?m)^ARG DEEPSEC_VERSION=(\S+)\s*$`)

func TestSecImagePinsDeepsecExactly(t *testing.T) {
	body, err := os.ReadFile(secImageDockerfile)
	if err != nil {
		t.Fatalf("read %s: %v", secImageDockerfile, err)
	}
	src := string(body)

	m := deepsecPinArgRe.FindStringSubmatch(src)
	if m == nil {
		t.Fatalf("%s declares no ARG DEEPSEC_VERSION — the deepsec install is unpinned", secImageDockerfile)
	}
	pin := m[1]
	if pin == "latest" || strings.ContainsAny(pin, "^~><*") || pin == "" {
		t.Fatalf("DEEPSEC_VERSION=%q is not an exact pin: an unpinned install serves whatever npm has on rebuild day (#2350)", pin)
	}

	// The pin needs renovate's annotation or it rots in place — the same
	// file already does this for its other npm pins.
	argIdx := strings.Index(src, m[0])
	if !strings.Contains(src[max(0, argIdx-200):argIdx], "renovate: datasource=npm depName=deepsec") {
		t.Fatalf("ARG DEEPSEC_VERSION has no renovate annotation (# renovate: datasource=npm depName=deepsec) — nothing will propose its bumps")
	}

	// The pin file's readers: the coverage test header measures its fixture
	// doubles against the pinned producer; a bump without re-reading the
	// three cited sites degrades coverage to UNKNOWN on every run.
	if !strings.Contains(src, "bots/sec_audit_deepsec_coverage_test.go") {
		t.Fatalf("the DEEPSEC_VERSION rationale no longer names the coverage test that judges a bump")
	}
}

func TestSecImageSanityFailsOnAnUnpinnedDeepsec(t *testing.T) {
	body, err := os.ReadFile(secImageDockerfile)
	if err != nil {
		t.Fatalf("read %s: %v", secImageDockerfile, err)
	}
	src := string(body)

	// The sanity step must compare the INSTALLED version against the pin
	// and fail the build on either failure mode — absent or drifted —
	// naming what it found. A log-only probe is the #2350 façade.
	for _, marker := range []string{
		`DEEPSEC_FOUND="$(deepsec --version`,
		`[ "$DEEPSEC_FOUND" != "${DEEPSEC_VERSION}" ]`,
		"deepsec: ABSENT — expected the pinned ${DEEPSEC_VERSION}",
		"deepsec: version drift — the image pinned ${DEEPSEC_VERSION}",
	} {
		if !strings.Contains(src, marker) {
			t.Fatalf("the Layer 5 sanity step lost %q — the pin must be ASSERTED (fail), not logged (#2350)", marker)
		}
	}
	// Both refusal branches must exit non-zero; a bare `test` would fail
	// silently into the wrong error message, a missing exit ships anyway.
	if got := strings.Count(src[strings.Index(src, "DEEPSEC_FOUND"):], "exit 1"); got < 2 {
		t.Fatalf("the deepsec sanity refusal must exit 1 on BOTH the absent and the drifted branch, found %d exit 1", got)
	}
}

func TestSecAuditDogfoodDockerfileIsGone(t *testing.T) {
	// The dogfood augmentation carried the ONLY pin (2.0.12) while the
	// published image floated on latest — two sources of truth for one
	// binary. The pin moved into the published image; the copy must go.
	if _, err := os.Stat("../bots/sec-audit-source/sandbox/sec/Dockerfile"); !os.IsNotExist(err) {
		t.Fatalf("bots/sec-audit-source/sandbox/sec/Dockerfile still exists — the dogfood pin was merged into sandbox/sec/Dockerfile (#2350), keeping both re-opens the drift")
	}
}
