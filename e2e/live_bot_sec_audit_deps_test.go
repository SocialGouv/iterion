//go:build live

package e2e

import (
	"os"
	"testing"
	"time"
)

// TestLive_Bot_SecAuditDeps runs the sec-audit-deps bot (Depsy) against an
// npm lockfile pinning lodash@4.17.4 — a version with published advisories
// (e.g. prototype pollution). Depsy enumerates deps, runs SCA heuristics
// (trivy fs over the lockfile) in sandbox-sec, and LLM-reviews the
// findings into a report.
//
// Reliability invariants: enumerate_deps fires with a non-empty deps list
// and llm_review writes a findings report. (Board posting is unavailable
// in a sandboxed run — findings land in the report.) The quality panel
// grades the CVE findings + value.
//
// Requires: claude CLI + OpenAI + docker w/ iterion-sandbox-sec:edge.
// Expected: ~20-45 min.
func TestLive_Bot_SecAuditDeps(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live test in short mode")
	}
	loadDotEnv(t)
	requireCLI(t, "claude")
	requireOpenAI(t)
	requireDockerImage(t, "ghcr.io/socialgouv/iterion-sandbox-sec:edge")

	workspaceDir, err := os.MkdirTemp("", "iterion-sec-audit-deps-*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Logf("Workspace (persists): %s", workspaceDir)

	seedGitRepo(t, workspaceDir)
	writeWorkspaceFiles(t, workspaceDir, map[string]string{
		"package.json": "{\n  \"name\": \"fixture\",\n  \"version\": \"1.0.0\",\n  \"dependencies\": { \"lodash\": \"4.17.4\" }\n}\n",
		"package-lock.json": `{
  "name": "fixture",
  "version": "1.0.0",
  "lockfileVersion": 3,
  "requires": true,
  "packages": {
    "": { "name": "fixture", "version": "1.0.0", "dependencies": { "lodash": "4.17.4" } },
    "node_modules/lodash": {
      "version": "4.17.4",
      "resolved": "https://registry.npmjs.org/lodash/-/lodash-4.17.4.tgz",
      "integrity": "sha1-eCA6TRwyiuHYbcpkYONptX9AVa4="
    }
  }
}
`,
	})
	gitCommitAll(t, workspaceDir, "chore: seed npm lockfile with CVE-flagged lodash")

	vars := map[string]any{
		"severity_threshold": "low",
		"scope_notes":        "Audit the npm lockfile for known-CVE dependencies.",
	}
	res := runBotLive(t, liveSpec{
		runIDBase:    "live-sec-audit-deps",
		bundleDir:    "../bots/sec-audit-deps",
		workspaceDir: workspaceDir,
		vars:         vars,
		inputs:       vars,
		timeout:      45 * time.Minute,
		withWorkDir:  true,
	})

	assertNodesFinished(t, res.events, "enumerate_deps")
	assertOutputFieldsNonEmpty(t, res.events, "enumerate_deps", "deps")
	if lr, ok := lastNodeOutput(res.events, "llm_review"); ok {
		if p, _ := lr["report_path"].(string); p == "" {
			t.Errorf("expected llm_review to write a findings report (report_path)")
		}
	}

	assessQuality(t, res, qualityInput{
		kind:          "bot",
		name:          "sec-audit-deps",
		persona:       "Depsy",
		primaryFamily: "anthropic",
		task:          "SCA an npm lockfile pinning lodash@4.17.4 (known advisories); enumerate deps, scan, report CVEs.",
		workProduct:   secWorkProduct(res, "enumerate_deps", "llm_review"),
	})
}
