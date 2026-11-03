// Package detecttest stages the host pkg/backend/detect probes, for a witness
// whose verdict follows a host detection report: the provider keys it names
// and a claude_code CLI the test owns, never what the machine running the
// test happens to hold.
package detecttest

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/SocialGouv/iterion/internal/envtrust"
)

// familyVars are the variables a review family is read from
// (pkg/reviewtopology folds anthropic and zai into "claude", openai into
// "gpt"). Every one is cleared before the named keys are set.
var familyVars = []string{
	"ANTHROPIC_API_KEY", "ANTHROPIC_BASE_URL", "ANTHROPIC_AUTH_TOKEN",
	"ZAI_API_KEY", "OPENAI_API_KEY", "OPENAI_BASE_URL",
}

// claudeCLI is the staged claude_code CLI: installed, with no login. It
// answers the one question detection asks it (`claude auth status --json`)
// and refuses anything else loudly, so a path that starts driving the CLI
// fails on the stage instead of passing on an answer the stage never gave.
const claudeCLI = `#!/bin/sh
if [ "$*" = "auth status --json" ]; then
	printf '%s\n' '{"loggedIn":false,"authMethod":"none"}'
	exit 0
fi
printf 'detecttest: the staged claude CLI answers only "auth status --json", not: %s\n' "$*" >&2
exit 64
`

// StageClaudeCodeHost pins what detect.Detect sees to a host carrying the
// claude_code CLI and exactly the named provider keys: a scratch HOME and
// empty credential homes, so no forfait, codex login or OAuth bridge is
// found; every family variable cleared, then the named ones set to a literal
// witness value, so no host value is ever read or printed; and a claude
// executable the test owns first on PATH.
//
// The CLI is part of the stage because the claude slot's routes pin
// claude_code (#1221): whether a host serves the claude family depends on the
// CLI being installed, so a witness that leaves it to the machine is green on
// a workstation that has one and red on a runner that does not. Its ABSENCE
// cannot be staged on PATH — dropping a directory drops the tools beside it —
// so a CLI-less host is judged on a synthetic detect.Report, as
// pkg/reviewtopology's route witnesses do.
//
// The stage is asserted before the test goes on: a claude that does not
// resolve to the staged one fails here (a PATH a project .env planted makes
// the resolver refuse every bare name), rather than leaving the witness to
// judge another host.
func StageClaudeCodeHost(t testing.TB, keys ...string) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("CODEX_HOME", t.TempDir())
	t.Setenv("ITERION_BACKEND_PREFERENCE", "")
	for _, name := range familyVars {
		t.Setenv(name, "")
	}
	for _, name := range keys {
		t.Setenv(name, "sk-witness-not-a-real-key")
	}
	bin := t.TempDir()
	cli := filepath.Join(bin, "claude")
	if err := os.WriteFile(cli, []byte(claudeCLI), 0o755); err != nil {
		t.Fatalf("detecttest: write the staged claude CLI: %v", err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	if got, err := envtrust.LookPath("claude"); err != nil || got != cli {
		t.Fatalf("detecttest: claude resolves to %q (%v), want the staged %s — the witness would judge this machine's CLI, not the stage", got, err, cli)
	}
}
