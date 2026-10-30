package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/envtrust"
)

// TestReloadIterionEnvFile_ClearsCommentedKey reproduces the dogfood
// case: the launching shell pre-loaded OPENAI_API_KEY (via direnv on a
// symlinked ~/.iterion/env → repo/.env), and the user later commented
// the line out in the file. Reload must clear the var even though
// applyDotenvFile didn't track it at boot ("shell wins" skipped it).
func TestReloadIterionEnvFile_ClearsCommentedKey(t *testing.T) {
	dir := t.TempDir()
	envPath := filepath.Join(dir, "env")

	// Simulate the boot state: env file has the key active.
	if err := os.WriteFile(envPath, []byte("OPENAI_API_KEY=initial-from-file\n"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	t.Setenv("ITERION_ENV_FILE", envPath)

	// Reset package globals between tests.
	dotenvMu.Lock()
	dotenvAppliedKeys = nil
	dotenvMu.Unlock()

	// Simulate the shell pre-loading the key (e.g. direnv).
	t.Setenv("OPENAI_API_KEY", "shell-loaded-value")

	// Boot-time load: shell wins, the file value is skipped, key is NOT
	// tracked. This is the case my naive reload couldn't recover from.
	loadIterionEnvFile()
	if got := os.Getenv("OPENAI_API_KEY"); got != "shell-loaded-value" {
		t.Fatalf("after boot load: env = %q, want shell-loaded-value", got)
	}
	dotenvMu.Lock()
	tracked := append([]string(nil), dotenvAppliedKeys...)
	dotenvMu.Unlock()
	for _, k := range tracked {
		if k == "OPENAI_API_KEY" {
			t.Fatalf("OPENAI_API_KEY should NOT be tracked (shell-set), but got %v", tracked)
		}
	}

	// User comments the line out.
	if err := os.WriteFile(envPath, []byte("# OPENAI_API_KEY=initial-from-file\n"), 0o600); err != nil {
		t.Fatalf("rewrite: %v", err)
	}

	// Refresh must clear the key — the new reload semantics treat the
	// file as the source of truth.
	ReloadIterionEnvFile()
	if got, present := os.LookupEnv("OPENAI_API_KEY"); present {
		t.Fatalf("after reload: OPENAI_API_KEY should be unset, got %q", got)
	}
}

// TestReloadIterionEnvFile_PreservesShellOnlyKeys verifies that a key
// the shell set which does NOT appear anywhere in the file (active or
// commented) is left alone on reload — the shell still owns it.
func TestReloadIterionEnvFile_PreservesShellOnlyKeys(t *testing.T) {
	dir := t.TempDir()
	envPath := filepath.Join(dir, "env")
	if err := os.WriteFile(envPath, []byte("# nothing here\n"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	t.Setenv("ITERION_ENV_FILE", envPath)
	dotenvMu.Lock()
	dotenvAppliedKeys = nil
	dotenvMu.Unlock()

	t.Setenv("MY_SHELL_VAR", "shell-only")
	loadIterionEnvFile()

	ReloadIterionEnvFile()
	if got := os.Getenv("MY_SHELL_VAR"); got != "shell-only" {
		t.Errorf("shell-only key was wrongly unset on reload: got %q", got)
	}
}

// TestReloadIterionEnvFile_RereadsActiveKey verifies that a value
// changed in the file is picked up by reload.
func TestReloadIterionEnvFile_RereadsActiveKey(t *testing.T) {
	dir := t.TempDir()
	envPath := filepath.Join(dir, "env")
	if err := os.WriteFile(envPath, []byte("MY_KEY=first\n"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	t.Setenv("ITERION_ENV_FILE", envPath)
	os.Unsetenv("MY_KEY")
	dotenvMu.Lock()
	dotenvAppliedKeys = nil
	dotenvMu.Unlock()

	loadIterionEnvFile()
	if got := os.Getenv("MY_KEY"); got != "first" {
		t.Fatalf("initial: env = %q, want first", got)
	}

	if err := os.WriteFile(envPath, []byte("MY_KEY=second\n"), 0o600); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	ReloadIterionEnvFile()
	if got := os.Getenv("MY_KEY"); got != "second" {
		t.Errorf("after reload: env = %q, want second", got)
	}
}

// The desktop env-file loader closes the same routing class as the CLI and
// the studio lanes: a route the file names is not applied — whatever the
// path resolves to, since a direnv symlink can point this very file at a
// PROJECT's `.env` — and the refusal names the file, the key and the
// opt-in on stderr. The operator's own credentials keep flowing: what IS
// applied here is not marked planted, because the file sits in the
// operator's home and its values ARE the operator's own — the mark exists
// for files that sit in a repository.
func TestTheDesktopEnvFileLeavesNoRoutingNameInTheEnvironment(t *testing.T) {
	envtrust.ResetForTest()
	t.Cleanup(envtrust.ResetForTest)
	t.Setenv(envtrust.EnvPlantedNames, "")
	t.Setenv(envtrust.DotenvEndpointsOptIn, "")
	os.Unsetenv("SEARXNG_URL")
	os.Unsetenv("AWS_ENDPOINT_URL_BEDROCK_RUNTIME")
	t.Cleanup(func() {
		os.Unsetenv("SEARXNG_URL")
		os.Unsetenv("AWS_ENDPOINT_URL_BEDROCK_RUNTIME")
	})

	dir := t.TempDir()
	envPath := filepath.Join(dir, "env")
	if err := os.WriteFile(envPath, []byte(strings.Join([]string{
		"SEARXNG_URL=https://repo-under-review.example/search",
		"AWS_ENDPOINT_URL_BEDROCK_RUNTIME=https://repo-under-review.example/bedrock",
		"OPENAI_API_KEY=sk-the-operator-owns-it",
	}, "\n")+"\n"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	t.Setenv("ITERION_ENV_FILE", envPath)
	dotenvMu.Lock()
	dotenvAppliedKeys = nil
	dotenvMu.Unlock()

	said := captureStderr(t, loadIterionEnvFile)

	for _, name := range []string{"SEARXNG_URL", "AWS_ENDPOINT_URL_BEDROCK_RUNTIME"} {
		if got, present := os.LookupEnv(name); present {
			t.Errorf("%s = %q: the file chose where a connection goes", name, got)
		}
		for _, want := range []string{name, envPath, envtrust.DotenvEndpointsOptIn + "=allow"} {
			if !strings.Contains(said, want) {
				t.Errorf("the refusal does not say %q — it said: %s", want, said)
			}
		}
	}
	// The operator's own credentials keep flowing, unmarked.
	if got := os.Getenv("OPENAI_API_KEY"); got != "sk-the-operator-owns-it" {
		t.Errorf("OPENAI_API_KEY = %q: the keys stashed in the operator's own file are what this loader exists for", got)
	}
	if envtrust.Planted("OPENAI_API_KEY") {
		t.Error("the operator's own key reads as planted: the mark is for repository files, not the home")
	}
}

// captureStderr runs fn with os.Stderr on a pipe and returns what it wrote.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stderr = w
	fn()
	os.Stderr = orig
	if err := w.Close(); err != nil {
		t.Fatalf("close the pipe: %v", err)
	}
	said, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read the pipe: %v", err)
	}
	if err := r.Close(); err != nil {
		t.Fatalf("close the read end: %v", err)
	}
	return string(said)
}

// The substitution refusal on the desktop lane, like on the CLI and studio
// ones: a value spelling command substitution does not exist after the
// load, and the refusal names the file. Nothing here evaluates values, but
// the trigger does not travel either — the H-10 review measured the studio
// lane RUNNING `$(touch …)` from the file, and the same bytes must not be
// handed to a run on this lane either.
func TestTheDesktopEnvFileRunsNothingACommandSubstitutionSpells(t *testing.T) {
	envtrust.ResetForTest()
	t.Cleanup(envtrust.ResetForTest)
	t.Setenv(envtrust.EnvPlantedNames, "")
	envtrust.ResetForTest()

	dir := t.TempDir()
	envPath := filepath.Join(dir, "env")
	marker := filepath.Join(dir, "substituted-ran")
	content := "FIRST=ok\n" +
		"FOO=$(touch " + marker + ")\n" +
		"BAR=`touch " + marker + "`\n" +
		"LAST=also-ok\n"
	if err := os.WriteFile(envPath, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ITERION_ENV_FILE", envPath)

	// Reset package globals between tests, and leave the witness names
	// unset (t.Setenv cannot express "absent").
	dotenvMu.Lock()
	dotenvAppliedKeys = nil
	dotenvMu.Unlock()
	for _, name := range []string{"FIRST", "FOO", "BAR", "LAST"} {
		t.Setenv(name, "")
		os.Unsetenv(name)
	}

	var said string
	{
		orig := os.Stderr
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatalf("pipe: %v", err)
		}
		os.Stderr = w
		loadIterionEnvFile()
		os.Stderr = orig
		_ = w.Close()
		b, _ := io.ReadAll(r)
		_ = r.Close()
		said = string(b)
	}

	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("the substitution marker exists: the file's command RAN (stat err %v)", err)
	}
	for _, name := range []string{"FOO", "BAR"} {
		if _, set := os.LookupEnv(name); set {
			t.Errorf("%s reached the environment: a refused value was applied", name)
		}
		if !strings.Contains(said, name) || !strings.Contains(said, envPath) {
			t.Errorf("the substitution refusal does not name %s and the file — it said: %s", name, said)
		}
	}
	if got := os.Getenv("FIRST"); got != "ok" {
		t.Errorf("FIRST = %q: the refusals must not cost the file its ordinary plants", got)
	}
	if got := os.Getenv("LAST"); got != "also-ok" {
		t.Errorf("LAST = %q: the refusals must not cost the file its ordinary plants", got)
	}
}
