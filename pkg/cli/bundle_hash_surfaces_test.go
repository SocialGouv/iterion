package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/SocialGouv/iterion/pkg/runview"
)

// TestPromotedMainBotHashesLikeItsBundleOnEverySurface: the workflow hash
// a run stores at launch is the BUNDLE's — main.bot with its prompts/ and
// presets/ — whichever surface opened it: the CLI on the bare main.bot
// (promoted by openBundleOrFile), the resolver runview promotes the same
// file through, and the resume that re-opens the recorded bundle path. The three
// agree, so a run launched on one surface resumes on another without
// `--force`; and the hash moves with a prompt file, which is why a bare
// compile could not stand in for it.
func TestPromotedMainBotHashesLikeItsBundleOnEverySurface(t *testing.T) {
	inTempWorkspace(t)
	p, _ := testPrinter()
	// Pinned model and backend: the compile refuses C018 on a
	// credential-less host, and the test measures the hash, not the host.
	if err := BotsCreate(BotsCreateOptions{Slug: "mf", Template: "multi-file", Model: "anthropic/claude-opus-4-8", Backend: "claude_code"}, p); err != nil {
		t.Fatalf("BotsCreate: %v", err)
	}
	mainBot := filepath.Join("bots", "mf", "main.bot")
	hashOf := func(surface string, open func() (string, error)) string {
		t.Helper()
		h, err := open()
		if err != nil {
			t.Fatalf("%s: %v", surface, err)
		}
		if h == "" {
			t.Fatalf("%s: empty hash", surface)
		}
		return h
	}
	cli := hashOf("cli on the bare main.bot", func() (string, error) {
		opened, _, _, cleanup, err := openBundleOrFile(mainBot)
		if err != nil {
			return "", err
		}
		defer func() { _ = cleanup() }()
		_, h, err := runview.CompileBundleWorkflow(opened.IterPath, opened)
		return h, err
	})
	// The launch compile itself (Service.Launch → compileForLaunch) is held
	// to the bundle's hash in pkg/runview's own test; this is the exported
	// resolver it promotes through.
	promoted := hashOf("runview.ResolveBundleFromFilePath on the bare main.bot", func() (string, error) {
		b, err := runview.ResolveBundleFromFilePath(mainBot)
		if err != nil {
			return "", err
		}
		if b == nil {
			t.Fatal("runview did not promote the bare main.bot to its bundle")
		}
		_, h, err := runview.CompileBundleWorkflow(b.IterPath, b)
		return h, err
	})
	resume := hashOf("resume on the recorded bundle path", func() (string, error) {
		opened, cleanup, err := openResumeBundle(filepath.Join("bots", "mf"))
		if err != nil {
			return "", err
		}
		defer func() { _ = cleanup() }()
		_, h, err := runview.CompileBundleWorkflow(opened.IterPath, opened)
		return h, err
	})
	// The shared helper every path-driven surface compiles through (the
	// dispatcher's engine path, rewind, the export).
	pathHelper := hashOf("runview.CompileWorkflowPath on the bare main.bot", func() (string, error) {
		_, h, _, err := runview.CompileWorkflowPath(mainBot)
		return h, err
	})
	if cli != promoted || cli != resume || cli != pathHelper {
		t.Errorf("the surfaces disagree: cli=%s resolver=%s resume=%s path=%s", cli, promoted, resume, pathHelper)
	}
	// The hash covers the bundle's prompts: editing one moves it.
	mission := filepath.Join("bots", "mf", "prompts", "mission.md")
	if err := os.WriteFile(mission, []byte("A different mission.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if edited := hashOf("cli after a prompt edit", func() (string, error) {
		opened, _, _, cleanup, err := openBundleOrFile(mainBot)
		if err != nil {
			return "", err
		}
		defer func() { _ = cleanup() }()
		_, h, err := runview.CompileBundleWorkflow(opened.IterPath, opened)
		return h, err
	}); edited == cli {
		t.Errorf("the hash did not move with prompts/mission.md; a bundle edit would resume silently")
	}
}
