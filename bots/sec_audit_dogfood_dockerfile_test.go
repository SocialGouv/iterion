package bots

import (
	"os"
	"testing"
)

func TestSecAuditDogfoodDockerfileIsGone(t *testing.T) {
	// The dogfood augmentation carried the ONLY pin (2.0.12) while the
	// published image floated on latest — two sources of truth for one
	// binary. The pin moved into the published image; the copy must go.
	if _, err := os.Stat("sec-audit-source/sandbox/sec/Dockerfile"); !os.IsNotExist(err) {
		t.Fatalf("bots/sec-audit-source/sandbox/sec/Dockerfile still exists — the dogfood pin was merged into sandbox/sec/Dockerfile (#2350), keeping both re-opens the drift")
	}
}
