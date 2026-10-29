package bots

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// The sandbox browser image is the one place a cloud rgaa-audit run can find a
// browser, and it ships Debian's Chromium — a binary Playwright's own resolution
// never finds. The image therefore names it once, as the environment variable
// the bot's `prepare` node reads when no `browser_path` var is given, and the
// engine launches exactly that file (`--browser-path`). Three artefacts, one
// contract: the apt package, the ENV line and the bot's reader must agree, or a
// run on the image probes « no browser » while a Chromium sits at /usr/bin.
func TestRgaaAuditSandboxBrowserImageNamesItsChromiumForTheBot(t *testing.T) {
	raw, err := os.ReadFile("../sandbox/browser/Dockerfile")
	if err != nil {
		t.Fatalf("read sandbox/browser/Dockerfile: %v", err)
	}
	dockerfile := string(raw)
	m := regexp.MustCompile(`(?m)^ENV ULTRA11Y_BROWSER_PATH=(\S+)$`).FindStringSubmatch(dockerfile)
	if m == nil {
		t.Fatal("sandbox/browser/Dockerfile declares no ENV ULTRA11Y_BROWSER_PATH — the rgaa-audit bot's rendered layer cannot find the image's Chromium")
	}
	if m[1] != "/usr/bin/chromium" {
		t.Fatalf("ULTRA11Y_BROWSER_PATH=%s, want Debian's /usr/bin/chromium (the `chromium` apt package the image installs)", m[1])
	}
	if !regexp.MustCompile(`(?m)^\s+chromium \\$`).MatchString(dockerfile) {
		t.Fatal("the image no longer installs the `chromium` apt package the ENV line points at")
	}
	// The bot's half of the contract: `prepare` reads that variable when the
	// operator named no browser, and hands it to the engine as --browser-path.
	wf := compilePlanPhaseBot(t, "rgaa-audit")
	node, ok := wf.Nodes["prepare"].(*ir.ToolNode)
	if !ok {
		t.Fatal("the rgaa-audit bot has no `prepare` tool node")
	}
	for _, want := range []string{"process.env.ULTRA11Y_BROWSER_PATH", "--browser-path="} {
		if !strings.Contains(node.Script, want) {
			t.Fatalf("the prepare node no longer carries %q — the image's ENV would be dead", want)
		}
	}
}
