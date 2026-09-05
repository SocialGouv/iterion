package server

import "testing"

// The Claude 5 family carries its level table from the claw registry:
// levels and default on Opus 5 and Sonnet 5, and the bare alias names the
// newest Opus. Whether the ultracode mode is offered on them is the model
// gate's business (ir.ModelSupportsUltracode), asserted where it lives.
func TestEffortCapabilities_Claude5Family(t *testing.T) {
	_, hs := newTestServer(t)

	opus := getEffortCaps(t, hs.URL, "claw", "claude-opus-5")
	if opus.Source != "claw-registry" {
		t.Errorf("Source=%q, want %q", opus.Source, "claw-registry")
	}
	if opus.Default != "high" {
		t.Errorf("opus-5 Default=%q, want %q", opus.Default, "high")
	}
	assertEffortLevels(t, opus.Supported,
		[]string{"low", "medium", "high", "xhigh", "max"}, // required
		nil, // forbidden
	)

	sonnet := getEffortCaps(t, hs.URL, "claw", "claude-sonnet-5")
	assertEffortLevels(t, sonnet.Supported,
		[]string{"low", "medium", "high", "xhigh", "max"}, // required
		[]string{"ultracode"},                             // forbidden
	)

	alias := getEffortCaps(t, hs.URL, "claw", "opus")
	assertEffortLevels(t, alias.Supported, []string{"low", "medium", "high", "xhigh", "max"}, nil)
}
