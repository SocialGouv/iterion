package runner

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// The regression: the clone URL carries the token so the initial fetch can
// authenticate, and git persists remote.origin.url verbatim — freezing a
// credential that lives ONE HOUR into .git/config. An app-building run takes
// several hours and pushes at the very end, so the run's last and most
// valuable action failed 403 with a dead token.
func TestInstallGitCredentialStore(t *testing.T) {
	const tok = "ghs_installation_token"
	const repoURL = "https://github.com/iterion-sandbox/appy-demo.git"
	r := &Runner{cfg: Config{Logger: iterlog.Nop()}}
	ctx := context.Background()
	dir := t.TempDir()
	if err := r.runGit(ctx, dir, "", "init", "-q"); err != nil {
		t.Fatalf("git init: %v", err)
	}
	// Reproduce the clone: an origin whose URL embeds the token.
	if err := r.runGit(ctx, dir, "", "remote", "add", "origin", injectGitToken(repoURL, tok)); err != nil {
		t.Fatalf("add tokenized remote: %v", err)
	}

	if err := r.installGitCredentialStore(ctx, dir, repoURL, tok); err != nil {
		t.Fatalf("install credential store: %v", err)
	}

	// 1. The token must be OUT of the persisted remote.
	cfg, err := os.ReadFile(filepath.Join(dir, ".git", "config"))
	if err != nil {
		t.Fatalf("read .git/config: %v", err)
	}
	if strings.Contains(string(cfg), tok) {
		t.Error("the token is still in .git/config — a later push would use this frozen copy")
	}

	// 2. The credential file carries it instead, readable only by the owner.
	path := filepath.Join(dir, ".git", gitCredentialFile)
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read credential file: %v", err)
	}
	if want := "https://oauth2:" + tok + "@github.com\n"; string(got) != want {
		t.Errorf("credential line = %q, want %q", got, want)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("credential file mode = %o, want 600", perm)
	}

	// 3. git must actually be pointed at that file.
	if !strings.Contains(string(cfg), "store --file="+path) {
		t.Errorf(".git/config does not wire the credential helper to %s:\n%s", path, cfg)
	}
}

// Rewriting must be atomic and in place, so a `git push` racing the refresh
// never reads a torn line — and so the NEW token is what git picks up.
func TestWriteGitCredentialsReplacesInPlace(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "creds")
	const repoURL = "https://github.com/o/r.git"

	if err := writeGitCredentials(path, repoURL, "first"); err != nil {
		t.Fatalf("initial write: %v", err)
	}
	if err := writeGitCredentials(path, repoURL, "second"); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "https://oauth2:second@github.com\n" {
		t.Errorf("after refresh the file must hold ONLY the new token, got %q", got)
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Error("the temp file must not survive the rename")
	}
}

func TestWriteGitCredentialsRejectsUnusableURL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "creds")
	// Fail loudly: silently writing no credential would surface hours later
	// as an unexplained 403 at push time.
	if err := writeGitCredentials(path, "not-a-url", "tok"); err == nil {
		t.Fatal("want an error for a URL with no scheme/host")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("no credential file should be created for an unusable URL")
	}
}

// A run with no forge token must not get a credential helper at all.
func TestInstallGitCredentialStoreNoTokenIsNoop(t *testing.T) {
	r := &Runner{cfg: Config{Logger: iterlog.Nop()}}
	dir := t.TempDir()
	if err := r.installGitCredentialStore(context.Background(), dir, "https://github.com/o/r.git", ""); err != nil {
		t.Fatalf("no-token install must be a no-op, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".git", gitCredentialFile)); !os.IsNotExist(err) {
		t.Error("no credential file should exist without a token")
	}
}
