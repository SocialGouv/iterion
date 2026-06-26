package runner

import (
	"context"
	"strings"
	"testing"
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
