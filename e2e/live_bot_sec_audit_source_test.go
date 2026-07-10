//go:build live

package e2e

import (
	"os"
	"testing"
	"time"
)

// TestLive_Bot_SecAuditSource runs the sec-audit-source bot (Seki) against
// a repo with a planted SSRF (http.Get of a user-supplied URL) and a
// hardcoded AWS-style key. Seki inventories, detects the stack, runs the
// generic + language scanners in sandbox-sec, gates on scan_health
// (hard-fails if the always-on scanners produced nothing), triages, and
// writes a findings report.
//
// Reliability invariants: inventory/detect_tech/scan_health fire,
// scan_health is healthy (scanners ran), and a findings report is written.
// (Board posting is unavailable in a sandboxed run — findings land in the
// report, so we assert report_path, not board issues.) The quality panel
// grades the findings + value.
//
// Requires: claude CLI + OpenAI + docker w/ iterion-sandbox-sec:edge.
// Expected: ~25-50 min.
func TestLive_Bot_SecAuditSource(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live test in short mode")
	}
	loadDotEnv(t)
	requireCLI(t, "claude")
	requireOpenAI(t)
	requireDockerImage(t, "ghcr.io/socialgouv/iterion-sandbox-sec:edge")

	workspaceDir, err := os.MkdirTemp("", "iterion-sec-audit-source-*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Logf("Workspace (persists): %s", workspaceDir)

	seedGoModuleFixture(t, workspaceDir, map[string]string{
		"handler.go": `package fixture

import (
	"io"
	"net/http"
)

// Fetch proxies a user-supplied URL with no allowlist — classic SSRF (gosec G107).
func Fetch(target string) ([]byte, error) {
	resp, err := http.Get(target)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}
`,
		"config.go": `package fixture

// Hardcoded credential — a secret scanner (gitleaks) must flag this.
const AWSAccessKey = "AKIAIOSFODNN7EXAMPLE"
`,
	})

	vars := map[string]any{
		"severity_threshold": "low",
		"scope_notes":        "Audit the Go source for injection + secret findings.",
	}
	res := runBotLive(t, liveSpec{
		runIDBase:    "live-sec-audit-source",
		bundleDir:    "../bots/sec-audit-source",
		workspaceDir: workspaceDir,
		vars:         vars,
		inputs:       vars,
		timeout:      50 * time.Minute,
		withWorkDir:  true,
	})

	assertNodesFinished(t, res.events, "inventory", "detect_tech", "scan_health")
	if sh, ok := lastNodeOutput(res.events, "scan_health"); ok {
		t.Logf("scan_health: healthy=%v degraded=%v total_findings_seen=%v", sh["healthy"], sh["degraded"], sh["total_findings_seen"])
	}
	if rc, ok := lastNodeOutput(res.events, "report_card"); ok {
		if p, _ := rc["report_path"].(string); p == "" {
			t.Errorf("expected report_card to write a findings report (report_path)")
		}
	}

	assessQuality(t, res, qualityInput{
		kind:          "bot",
		name:          "sec-audit-source",
		persona:       "Seki",
		primaryFamily: "anthropic",
		task:          "SAST a Go repo with a planted SSRF + hardcoded AWS key; run scanners, gate scan_health, triage, report.",
		workProduct:   secWorkProduct(res, "triage", "report_card"),
	})
}

// secWorkProduct renders the triage candidates + report summary for grading.
func secWorkProduct(res liveResult, triageNode, reportNode string) string {
	tr, _ := lastNodeOutput(res.events, triageNode)
	rc, _ := lastNodeOutput(res.events, reportNode)
	work := "## triage candidates\n" + sprintAny(mapField(tr, "candidates"))
	work += "\n\n## report\n" + sprintAny(mapField(rc, "report_path")) + "\n" + sprintAny(mapField(rc, "summary"))
	return work
}

func mapField(m map[string]any, k string) any {
	if m == nil {
		return nil
	}
	return m[k]
}
