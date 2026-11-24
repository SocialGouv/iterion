package tool

import (
	"crypto/rand"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	gitlib "github.com/SocialGouv/iterion/pkg/git"
)

// The cloud runner writes the run's LIVE forge token, in plaintext, to
// `<clone>/.git/iterion-credentials` and points git at it with
// `credential.helper store --file=…` (pkg/runner/loop_gitws.go). The clone
// root IS the claw workspace, so every workspace reader has to refuse that
// file: `.git-credentials`, which the exclusion already named, is git's
// DEFAULT store and a repo-targeted run never writes it — the runner's own
// file is the one credential file such a run is guaranteed to have.
//
// Each guard that refuses it gets its own scenario. They cover each other on
// the ordinary input (the early path check hides the resolved-path check, the
// walk's ignoredSearchDir hides both), so a single scenario would leave the
// others unproven: the read tool checks the raw path and the resolved path,
// grep checks the raw path, the resolved search path, and every entry it
// walks.

// cloneCredentialFixture writes a credential-store line holding a random
// token into <workspace>/.git/<name>, and returns the token. Random, because
// "the token did not reach the output" must not be an assertion a value
// shared with the rest of the fixture can satisfy by accident.
func cloneCredentialFixture(t *testing.T, workspace, name string) string {
	t.Helper()
	raw := make([]byte, 20)
	if _, err := rand.Read(raw); err != nil {
		t.Fatalf("random token: %v", err)
	}
	token := "iteriontest" + hex.EncodeToString(raw)
	gitDir := filepath.Join(workspace, ".git")
	if err := os.MkdirAll(gitDir, 0o700); err != nil {
		t.Fatal(err)
	}
	line := "https://oauth2:" + token + "@forge.example.test\n"
	if err := os.WriteFile(filepath.Join(gitDir, name), []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	return token
}

func TestWorkspaceReadFile_RefusesTheClonesCredentialStore(t *testing.T) {
	t.Run("named directly", func(t *testing.T) {
		// The atomic replace writes the token to a sibling before renaming it
		// into place: `<name>.tmp` on the runner host (writeGitCredentials),
		// `<name>.tmp.$$` inside the sandbox (pkg/sandbox/execwrite.go). A
		// replace that fails leaves that sibling on disk with the token in it.
		for _, name := range []string{
			gitlib.CredentialStoreFile,
			gitlib.CredentialStoreFile + ".tmp",
			gitlib.CredentialStoreFile + ".tmp.4242",
		} {
			workspace := t.TempDir()
			token := cloneCredentialFixture(t, workspace, name)
			rel := ".git/" + name
			out, err := executeWorkspaceReadFile(map[string]any{"path": rel}, NewReadScope(workspace))
			if err == nil || !strings.Contains(err.Error(), "credential or secret file") {
				t.Errorf("read_file(%q) error = %v, want the credential exclusion", rel, err)
			}
			if strings.Contains(out, token) {
				t.Errorf("read_file(%q) served the clone's forge token", rel)
			}
		}
	})

	t.Run("through a symlink inside the workspace", func(t *testing.T) {
		// A bot can write the link itself: the raw path is innocuous and only
		// the RESOLVED path names the credential.
		workspace := t.TempDir()
		token := cloneCredentialFixture(t, workspace, gitlib.CredentialStoreFile)
		if err := os.Symlink(filepath.Join(workspace, ".git", gitlib.CredentialStoreFile), filepath.Join(workspace, "notes.txt")); err != nil {
			t.Fatal(err)
		}
		out, err := executeWorkspaceReadFile(map[string]any{"path": "notes.txt"}, NewReadScope(workspace))
		if err == nil || !strings.Contains(err.Error(), "credential or secret file") {
			t.Errorf("read_file through a symlink: error = %v, want the credential exclusion", err)
		}
		if strings.Contains(out, token) {
			t.Errorf("read_file through a symlink served the clone's forge token")
		}
	})
}

func TestWorkspaceGrep_NeverServesTheClonesCredentialStore(t *testing.T) {
	t.Run("named directly", func(t *testing.T) {
		workspace := t.TempDir()
		token := cloneCredentialFixture(t, workspace, gitlib.CredentialStoreFile)
		rel := ".git/" + gitlib.CredentialStoreFile
		out, err := executeWorkspaceGrep(t.Context(), map[string]any{"pattern": "oauth2", "path": rel}, NewReadScope(workspace))
		if err == nil || !strings.Contains(err.Error(), "excluded as a credential or secret file") {
			t.Errorf("grep(%q) error = %v, want the credential exclusion", rel, err)
		}
		if strings.Contains(out, token) {
			t.Errorf("grep(%q) served the clone's forge token", rel)
		}
	})

	t.Run("through a symlink inside the workspace", func(t *testing.T) {
		// Reaches the guard on the RESOLVED search path, which the raw-path
		// refusal above never lets run.
		workspace := t.TempDir()
		token := cloneCredentialFixture(t, workspace, gitlib.CredentialStoreFile)
		if err := os.Symlink(filepath.Join(workspace, ".git", gitlib.CredentialStoreFile), filepath.Join(workspace, "notes.txt")); err != nil {
			t.Fatal(err)
		}
		out, err := executeWorkspaceGrep(t.Context(), map[string]any{"pattern": "oauth2", "path": "notes.txt"}, NewReadScope(workspace))
		if err == nil || !strings.Contains(err.Error(), "protected credential file") {
			t.Errorf("grep through a symlink: error = %v, want the protected-credential refusal", err)
		}
		if strings.Contains(out, token) {
			t.Errorf("grep through a symlink served the clone's forge token")
		}
	})

	t.Run("while walking the .git directory", func(t *testing.T) {
		// ignoredSearchDir only skips `.git` for a walk that STARTS above it;
		// a search rooted AT `.git` walks in, and the per-entry filter is the
		// only thing between the walk and the credential. The sibling file
		// proves the walk really ran, so a green here cannot come from the
		// whole search being refused.
		workspace := t.TempDir()
		token := cloneCredentialFixture(t, workspace, gitlib.CredentialStoreFile)
		if err := os.WriteFile(filepath.Join(workspace, ".git", "description"), []byte("oauth2 is mentioned here too\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		out, err := executeWorkspaceGrep(t.Context(), map[string]any{"pattern": "oauth2", "path": ".git"}, NewReadScope(workspace))
		if err != nil {
			t.Fatalf("grep(.git): %v", err)
		}
		if !strings.Contains(out, "description") {
			t.Errorf("grep(.git) never reached the walk: out = %q", out)
		}
		if strings.Contains(out, token) {
			t.Errorf("grep(.git) served the clone's forge token")
		}
	})
}

// glob cannot name the file: it requires a directory and refuses anything
// else before the credential check. Its reachable guard is the per-entry
// filter of the walk, so a glob rooted AT `.git` must not list the
// credential — the name alone tells an agent the store is there to read by
// another route.
func TestWorkspaceGlob_NeverListsTheClonesCredentialStore(t *testing.T) {
	workspace := t.TempDir()
	cloneCredentialFixture(t, workspace, gitlib.CredentialStoreFile)
	// The sibling proves the walk really ran, so a green here cannot come
	// from the whole search being refused.
	if err := os.WriteFile(filepath.Join(workspace, ".git", "description"), []byte("not a secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := executeWorkspaceGlob(t.Context(), map[string]any{"path": ".git", "pattern": "*"}, NewReadScope(workspace))
	if err != nil {
		t.Fatalf("glob(.git): %v", err)
	}
	if !strings.Contains(out, "description") {
		t.Errorf("glob(.git) never reached the walk: out = %q", out)
	}
	if strings.Contains(out, gitlib.CredentialStoreFile) {
		t.Errorf("glob(.git) listed the clone's credential store: out = %q", out)
	}
}
