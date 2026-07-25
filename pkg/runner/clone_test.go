package runner

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

func TestInjectGitToken(t *testing.T) {
	cases := []struct{ in, tok, want string }{
		// https → oauth2 userinfo injected
		{"https://gitlab.example/grp/repo.git", "tok123", "https://oauth2:tok123@gitlab.example/grp/repo.git"},
		// no token → unchanged
		{"https://gitlab.example/grp/repo.git", "", "https://gitlab.example/grp/repo.git"},
		// non-https (scp-like / http) → unchanged, never carry a token in cleartext schemes
		{"git@github.com:grp/repo.git", "tok", "git@github.com:grp/repo.git"},
		{"http://insecure/repo.git", "tok", "http://insecure/repo.git"},
	}
	for _, c := range cases {
		if got := injectGitToken(c.in, c.tok); got != c.want {
			t.Errorf("injectGitToken(%q, %q) = %q; want %q", c.in, c.tok, got, c.want)
		}
	}
}

func TestValidateRepoTarget(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name             string
		repoURL, repoSHA string
		wantErr          bool
	}{
		// Valid: https + ssh URLs with public-IP-literal hosts (hermetic; no DNS).
		// (Use 8.8.8.8 — a public unicast IP — so ResolvePublicHost passes without
		// hitting the network, mirroring how httpdial accepts IP literals directly.)
		{"https public IP no ref", "https://8.8.8.8/org/repo.git", "", false},
		{"https public IP with sha", "https://8.8.8.8/org/repo.git", "a1b2c3d4e5f6", false},
		{"https public IP with branch ref", "https://8.8.8.8/grp/repo.git", "feature/x", false},
		{"https public IP with pull ref", "https://8.8.8.8/org/repo.git", "refs/pull/12/head", false},
		{"scp-like ssh public IP", "git@8.8.8.8:org/repo.git", "main", false},
		{"ssh url public IP", "ssh://git@8.8.8.8/org/repo.git", "main", false},
		// Injection: remote-helper transport in the URL → RCE vector.
		{"ext remote helper", "ext::sh -c 'id'", "main", true},
		{"transport marker", "fd::17", "main", true},
		// Injection: local-repo / cleartext transports git would honour.
		{"file url", "file:///etc/passwd", "main", true},
		{"git proto", "git://host/repo.git", "main", true},
		{"http cleartext", "http://host/repo.git", "main", true},
		// Empty / null URL.
		{"empty url", "", "main", true},
		{"null byte url", "https://h/r\x00.git", "main", true},
		// Injection: flag-shaped ref → `git fetch`/`checkout` option injection.
		{"flag ref upload-pack", "https://8.8.8.8/org/repo.git", "--upload-pack=/evil", true},
		{"flag ref dash", "https://8.8.8.8/org/repo.git", "-O/tmp/x", true},
		{"traversal ref", "https://8.8.8.8/org/repo.git", "a/../../b", true},
		{"null byte ref", "https://8.8.8.8/org/repo.git", "ma\x00in", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := validateRepoTarget(ctx, c.repoURL, c.repoSHA)
			if c.wantErr && err == nil {
				t.Fatalf("validateRepoTarget(%q, %q) = nil; want error", c.repoURL, c.repoSHA)
			}
			if !c.wantErr && err != nil {
				t.Fatalf("validateRepoTarget(%q, %q) = %v; want nil", c.repoURL, c.repoSHA, err)
			}
		})
	}
}

// TestValidateRepoTargetHostGuard covers the SSRF host-allowlist layer added
// on top of ValidateCloneSource/ValidateBranchName: a holder of a per-org
// `iwh_` webhook token must not be able to point the cloud runner at an
// internal host (loopback, RFC1918, link-local, cloud-metadata) and use it
// as an SSRF probe. Uses IP literals so the test is hermetic (no DNS).
func TestValidateRepoTargetHostGuard(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name         string
		repoURL      string
		allowPrivate bool
		wantErr      bool
	}{
		// Public unicast IPs always pass (with or without escape hatch).
		{"public IP https", "https://8.8.8.8/org/repo.git", false, false},
		{"public IP ssh url", "ssh://git@1.1.1.1/org/repo.git", false, false},
		{"public IP scp-like", "git@8.8.8.8:org/repo.git", false, false},
		// Internal/private hosts are rejected by default — the SSRF gap.
		{"loopback https", "https://127.0.0.1/org/repo.git", false, true},
		{"loopback ssh url", "ssh://git@127.0.0.1/org/repo.git", false, true},
		{"loopback scp-like", "git@127.0.0.1:org/repo.git", false, true},
		{"rfc1918 10.x", "https://10.0.0.5:8200/org/repo.git", false, true},
		{"rfc1918 192.168.x", "https://192.168.1.10/org/repo.git", false, true},
		{"link-local", "https://169.254.169.254/latest/meta-data/", false, true},
		// (IPv6 literal URLs like https://[::1]/... are already rejected one
		// layer up by ValidateCloneSource's `::` remote-helper guard, so they
		// never reach the host check — no case needed here.)
		// Escape hatch (ITERION_RUNNER_CLONE_ALLOW_PRIVATE=1) lets on-prem
		// deployments reach internal forges.
		{"loopback with allow_private", "https://127.0.0.1/org/repo.git", true, false},
		{"rfc1918 with allow_private", "https://10.0.0.5:8200/org/repo.git", true, false},
		{"scp-like with allow_private", "git@127.0.0.1:org/repo.git", true, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.allowPrivate {
				t.Setenv("ITERION_RUNNER_CLONE_ALLOW_PRIVATE", "1")
			} else {
				t.Setenv("ITERION_RUNNER_CLONE_ALLOW_PRIVATE", "")
			}
			_, err := validateRepoTarget(ctx, c.repoURL, "main")
			switch {
			case c.wantErr && err == nil:
				t.Fatalf("validateRepoTarget(%q, allowPrivate=%v) = nil; want error", c.repoURL, c.allowPrivate)
			case !c.wantErr && err != nil:
				t.Fatalf("validateRepoTarget(%q, allowPrivate=%v) = %v; want nil", c.repoURL, c.allowPrivate, err)
			case c.wantErr && err != nil:
				// Sanity: the rejection should name the host guard, not be a
				// generic ValidateCloneSource error — otherwise we are testing
				// the wrong layer.
				if !strings.Contains(err.Error(), "public address") {
					t.Fatalf("validateRepoTarget(%q) error = %v; want a host-guard rejection", c.repoURL, err)
				}
			}
		})
	}
}

// TestGitAuthorIdentity pins the committer identity seeded into a cloud
// clone: the neutral bot fallback (RFC 2606 .invalid domain so GitHub can
// never attribute it to a real account) and the per-deployment env override.
func TestGitAuthorIdentity(t *testing.T) {
	t.Setenv("ITERION_GIT_AUTHOR_NAME", "")
	t.Setenv("ITERION_GIT_AUTHOR_EMAIL", "")
	if got := gitAuthorName(); got != "iterion-runner[bot]" {
		t.Errorf("default author name = %q, want iterion-runner[bot]", got)
	}
	if got := gitAuthorEmail(); got != "iterion-runner@bot.iterion.invalid" {
		t.Errorf("default author email = %q, want the .invalid fallback", got)
	}

	t.Setenv("ITERION_GIT_AUTHOR_NAME", "  Custom Bot  ")
	t.Setenv("ITERION_GIT_AUTHOR_EMAIL", "bot@corp.example")
	if got := gitAuthorName(); got != "Custom Bot" {
		t.Errorf("override author name = %q, want trimmed Custom Bot", got)
	}
	if got := gitAuthorEmail(); got != "bot@corp.example" {
		t.Errorf("override author email = %q, want bot@corp.example", got)
	}
}

// TestDefaultGitOpTimeout pins the ITERION_RUNNER_GIT_TIMEOUT parsing
// contract: unset/invalid → the 15m default; a valid duration wins,
// including a non-positive one (which disables the bound).
func TestDefaultGitOpTimeout(t *testing.T) {
	cases := []struct {
		name, env string
		want      time.Duration
	}{
		{"unset", "", 15 * time.Minute},
		{"valid", "90s", 90 * time.Second},
		{"invalid falls back", "ninety-seconds", 15 * time.Minute},
		{"non-positive disables", "-1s", -1 * time.Second},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("ITERION_RUNNER_GIT_TIMEOUT", c.env)
			if got := defaultGitOpTimeout(); got != c.want {
				t.Errorf("defaultGitOpTimeout() with env %q = %v, want %v", c.env, got, c.want)
			}
		})
	}
}

// TestSeedRunScratchIgnore pins the local-exclude seeding: a fresh clone
// gets `.claude/` appended to .git/info/exclude (so a bot's `git add -A`
// never drags the mirrored skills into the review diff), and a non-repo
// dir is a silent no-op (best-effort contract).
func TestSeedRunScratchIgnore(t *testing.T) {
	r := &Runner{cfg: Config{Logger: iterlog.Nop()}}
	dir := t.TempDir()
	if err := r.runGit(context.Background(), dir, "", "init", "-q"); err != nil {
		t.Fatalf("git init: %v", err)
	}
	seedRunScratchIgnore(dir)
	got, err := os.ReadFile(filepath.Join(dir, ".git", "info", "exclude"))
	if err != nil {
		t.Fatalf("read exclude: %v", err)
	}
	if !strings.Contains(string(got), ".claude/") {
		t.Errorf("exclude does not ignore .claude/: %q", got)
	}

	// Non-repo dir: no panic, nothing created (OpenFile without O_CREATE).
	bare := t.TempDir()
	seedRunScratchIgnore(bare)
	if _, err := os.Stat(filepath.Join(bare, ".git")); !os.IsNotExist(err) {
		t.Errorf("seedRunScratchIgnore created .git in a non-repo dir")
	}
}

// TestRunGitRedactsToken proves an authed-clone token never leaks into a
// git error: both the shown args and the subprocess output are redacted
// to *** before the error is returned (and thus before it can be logged).
func TestRunGitRedactsToken(t *testing.T) {
	const tok = "sekret-token-123"
	r := &Runner{cfg: Config{Logger: iterlog.Nop()}}
	dir := t.TempDir()
	if err := r.runGit(context.Background(), dir, "", "init", "-q"); err != nil {
		t.Fatalf("git init: %v", err)
	}
	err := r.runGit(context.Background(), dir, tok, "rev-parse", "--verify", tok)
	if err == nil {
		t.Fatal("expected rev-parse of a bogus revision to fail")
	}
	msg := err.Error()
	if strings.Contains(msg, tok) {
		t.Fatalf("token leaked into git error: %q", msg)
	}
	if !strings.Contains(msg, "***") {
		t.Errorf("expected *** placeholder in redacted error, got %q", msg)
	}
}

// TestRunGitEnvExtraEnvReachesSubprocess proves runGitEnv's extraEnv
// entries land in the git subprocess environment and win over the
// baseline — the seam the clone-guard proxy rides via HTTPS_PROXY.
func TestRunGitEnvExtraEnvReachesSubprocess(t *testing.T) {
	r := &Runner{cfg: Config{Logger: iterlog.Nop()}}
	dir := t.TempDir()
	if err := r.runGit(context.Background(), dir, "", "init", "-q"); err != nil {
		t.Fatalf("git init: %v", err)
	}
	// Baseline: the repo resolves its own git dir.
	if err := r.runGitEnv(context.Background(), dir, "", nil, "rev-parse", "--git-dir"); err != nil {
		t.Fatalf("rev-parse without extraEnv: %v", err)
	}
	// GIT_DIR pointed at a nonexistent path must break resolution — only
	// possible if extraEnv actually reaches the subprocess.
	extra := []string{"GIT_DIR=" + filepath.Join(dir, "absent-git-dir")}
	if err := r.runGitEnv(context.Background(), dir, "", extra, "rev-parse", "--git-dir"); err == nil {
		t.Fatal("expected GIT_DIR override from extraEnv to fail rev-parse; extraEnv did not reach the subprocess")
	}
}

func TestExtractRepoHost(t *testing.T) {
	cases := []struct {
		name, in, want string
		wantErr        bool
	}{
		{"https with port", "https://gitlab.example:8443/grp/repo.git", "gitlab.example", false},
		{"https no port", "https://github.com/org/repo.git", "github.com", false},
		{"ssh url with user", "ssh://git@host.example/org/repo.git", "host.example", false},
		{"ssh url no user", "ssh://host.example/org/repo.git", "host.example", false},
		{"scp-like", "git@github.com:org/repo.git", "github.com", false},
		{"scp-like no user", "host.example:org/repo.git", "host.example", false},
		{"ipv6 https", "https://[2001:db8::1]/org/repo.git", "2001:db8::1", false},
		{"ipv4 literal https", "https://10.0.0.5:8200/x", "10.0.0.5", false},
		{"empty", "", "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := extractRepoHost(c.in)
			if c.wantErr {
				if err == nil {
					t.Fatalf("extractRepoHost(%q) = %q, nil; want error", c.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("extractRepoHost(%q) error = %v", c.in, err)
			}
			if got != c.want {
				t.Fatalf("extractRepoHost(%q) = %q; want %q", c.in, got, c.want)
			}
		})
	}
}
