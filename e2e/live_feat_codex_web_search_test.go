//go:build live

package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
)

// TestLive_Feat_CodexWebSearch proves that tools: [web_search] reaches the
// native hosted Codex tool, returns consulted URLs, emits a distinct
// WebSearch lifecycle, and remains compatible with readonly: true.
func TestLive_Feat_CodexWebSearch(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live test in short mode")
	}
	loadDotEnv(t)
	requireCLI(t, "codex")

	workspaceDir, err := os.MkdirTemp("", "iterion-feat-codex-web-search-*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	seedGitRepo(t, workspaceDir)
	sentinel := filepath.Join(workspaceDir, "codex-readonly-must-not-create")

	res := runBotLive(t, liveSpec{
		runIDBase:    "live-feat-codex-web-search",
		botFile:      "codex_web_search.bot",
		workspaceDir: workspaceDir,
		withWorkDir:  true,
		timeout:      8 * time.Minute,
	})

	assertNodesFinished(t, res.events, "researcher")
	assertSchemaValid(t, res.wf, res.events, "researcher")
	out, _ := lastNodeOutput(res.events, "researcher")
	urls, _ := out["source_urls"].([]any)
	if len(urls) == 0 {
		t.Fatalf("Codex returned no consulted source URLs: %v", out)
	}
	for _, raw := range urls {
		url, _ := raw.(string)
		if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
			t.Errorf("source URL is not absolute: %q", url)
		}
	}
	if succeeded, _ := out["write_succeeded"].(bool); succeeded {
		t.Errorf("Codex reported a successful write under readonly: true: %v", out)
	}
	if _, err := os.Stat(sentinel); err == nil {
		t.Fatalf("readonly Codex node created sentinel %s", sentinel)
	} else if !os.IsNotExist(err) {
		t.Fatalf("stat sentinel: %v", err)
	}

	var sawSearch bool
	for _, event := range res.events {
		if event.NodeID != "researcher" || (event.Type != store.EventToolStarted && event.Type != store.EventToolCalled) {
			continue
		}
		tool, _ := event.Data["tool"].(string)
		switch tool {
		case "WebSearch":
			sawSearch = true
		}
	}
	if !sawSearch {
		t.Fatal("run events contain no WebSearch tool lifecycle")
	}
}
