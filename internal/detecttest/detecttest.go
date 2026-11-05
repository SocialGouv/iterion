// Package detecttest stages the host pkg/backend/detect probes, for a witness
// whose verdict follows a host detection report: the provider keys it names
// and a claude_code CLI the test owns, never what the machine running the
// test happens to hold.
package detecttest

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/SocialGouv/iterion/internal/envtrust"
	"github.com/SocialGouv/iterion/pkg/backend/detect"
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

// providerVars are every variable pkg/backend/detect reads a provider
// credential, endpoint or credential home from. IsolateProviders clears or
// redirects all of them.
var providerVars = []string{
	"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_BASE_URL",
	"OPENAI_API_KEY", "OPENAI_BASE_URL", "ITERION_OPENAI_USE_OAUTH",
	"ZAI_API_KEY", "MOONSHOT_API_KEY", "XAI_API_KEY",
	"AZURE_OPENAI_API_KEY", "AZURE_OPENAI_ENDPOINT",
	"AWS_REGION", "AWS_DEFAULT_REGION", "GOOGLE_CLOUD_PROJECT",
}

// IsolateProviders switches host provider detection off for the whole test
// binary: every provider variable unset, and the credential homes the
// detector reads (CLAUDE_CONFIG_DIR, CODEX_HOME, XDG_DATA_HOME) pointed at
// one empty, read-only scratch directory — read-only so the claude CLI the
// detector probes (`claude auth status`) cannot leave its own files in it.
// Without it, a fixture whose structured answer falls to the last-resort
// extraction (extractStructuredViaClaw) reaches whatever provider the machine
// running the test holds — a scripted test that passes or fails, and spends,
// by host. Call it from TestMain, around m.Run, in a file the `live` build
// excludes: the live layer runs on the operator's credentials by design. A
// test that stages a provider sets its own with t.Setenv.
func IsolateProviders() (restore func()) {
	saved := map[string]*string{}
	keep := func(name string) {
		if v, ok := os.LookupEnv(name); ok {
			saved[name] = &v
		} else {
			saved[name] = nil
		}
	}
	for _, name := range providerVars {
		keep(name)
		if err := os.Unsetenv(name); err != nil {
			panic(err)
		}
	}
	scratch, err := os.MkdirTemp("", "detecttest-no-provider-")
	if err != nil {
		panic(err)
	}
	if err := os.Chmod(scratch, 0o555); err != nil {
		panic(err)
	}
	for _, name := range []string{"CLAUDE_CONFIG_DIR", "CODEX_HOME", "XDG_DATA_HOME"} {
		keep(name)
		if err := os.Setenv(name, scratch); err != nil {
			panic(err)
		}
	}
	return func() {
		for name, v := range saved {
			if v == nil {
				_ = os.Unsetenv(name)
			} else {
				_ = os.Setenv(name, *v)
			}
		}
		rmErr := os.Chmod(scratch, 0o700)
		if rmErr == nil {
			rmErr = os.Remove(scratch)
		}
		if rmErr != nil {
			fmt.Fprintf(os.Stderr, "detecttest: the provider scratch dir %s stays behind: %v\n", scratch, rmErr)
		}
	}
}

// RequireNoProvider fails the test unless the provider variables hand claw
// no model to spend — the pick extractStructuredViaClaw recovers with: the
// witness that the package's TestMain installed IsolateProviders. It reads
// detect.Providers, which probes no backend CLI.
func RequireNoProvider(t testing.TB) {
	t.Helper()
	if pick := detect.SuggestedModel(detect.BackendClaw, detect.Providers()); pick != "" {
		t.Fatalf("detecttest: this test binary still detects a provider claw would spend (%q) — its TestMain must call IsolateProviders", pick)
	}
}
