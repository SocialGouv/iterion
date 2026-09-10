package botreplay

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/SocialGouv/iterion/pkg/botregistry"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

// repoRoot walks upward from the current working directory until it
// finds a go.mod, returning that directory. The replay test runs with
// CWD = pkg/botreplay, so examples/ lives a couple levels up; resolving
// the module root keeps the lookup robust regardless of where `go test`
// is invoked from.
func repoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, statErr := os.Stat(filepath.Join(dir, "go.mod")); statErr == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("botreplay: go.mod not found above %s", dir)
		}
		dir = parent
	}
}

// BotPath resolves a bot name to its workflow source file (a bundle's
// main.bot or a loose .bot) through the shared botregistry catalog — the
// same path resolution + kebab/snake name normalization the dispatcher
// and studio use, so botreplay discovers and validates bots through one
// catalog rather than a hardcoded layout assumption.
func BotPath(bot string) (string, error) {
	root, err := repoRoot()
	if err != nil {
		return "", err
	}
	return botregistry.ResolveBotPath(bot, botregistry.DefaultPaths(root))
}

// CompileBot parses and compiles a bot's main.bot into its IR Workflow.
func CompileBot(bot string) (*ir.Workflow, error) {
	path, err := BotPath(bot)
	if err != nil {
		return nil, err
	}
	if abs, err := filepath.Abs(path); err == nil {
		path = abs // an include resolves beside the file named in full
	}
	src, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("botreplay: read %s: %w", path, err)
	}
	pr := parser.Parse(path, string(src))
	if pr.File == nil {
		return nil, fmt.Errorf("botreplay: parse %s returned nil AST", path)
	}
	cr := ir.Compile(pr.File)
	if cr.HasErrors() {
		var msgs []string
		for _, d := range cr.Diagnostics {
			msgs = append(msgs, d.Error())
		}
		return nil, fmt.Errorf("botreplay: compile %s failed: %s", path, strings.Join(msgs, "; "))
	}
	return cr.Workflow, nil
}
