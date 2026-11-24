package runner

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// git runs a credential.helper value through `sh -c`, and the clone dir
// carries the caller-chosen run id: shell syntax in that id must reach the
// helper as a literal path, and the helper must still answer with the token.
func TestInstallGitCredentialStoreQuotesPathForShell(t *testing.T) {
	const tok = "tok-shell-quote"
	wantAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte("oauth2:"+tok))
	for name, runID := range map[string]string{
		"command-substitution":              "x$(touch${IFS}PWNED)",
		"command-substitution-single-quote": "x$(touch${IFS}PWNED)'q",
	} {
		t.Run(name, func(t *testing.T) {
			// A scratch global config keeps the operator's credential
			// helpers out: git would ask them before the run's store.
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
			t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(home, ".gitconfig"))
			t.Setenv("NO_PROXY", "127.0.0.1,localhost")
			t.Setenv("no_proxy", "127.0.0.1,localhost")
			var mu sync.Mutex
			var sawAuth bool
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				mu.Lock()
				if req.Header.Get("Authorization") == wantAuth {
					sawAuth = true
				}
				mu.Unlock()
				w.Header().Set("WWW-Authenticate", `Basic realm="forge"`)
				http.Error(w, "auth", http.StatusUnauthorized)
			}))
			t.Cleanup(srv.Close)
			repoURL := srv.URL + "/o/r.git"

			r := &Runner{cfg: Config{Logger: iterlog.Nop()}}
			ctx := context.Background()
			dir := filepath.Join(t.TempDir(), "repos", runID)
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := r.runGit(ctx, dir, "", "init", "-q"); err != nil {
				t.Fatalf("git init: %v", err)
			}
			if err := r.installGitCredentialStore(ctx, dir, repoURL, tok); err != nil {
				t.Fatalf("install credential store: %v", err)
			}
			// The forge refuses everything: ls-remote fails, after git asked
			// the helper for the credential.
			_, lsErr := r.runGitOutEnv(ctx, dir, "", nil, "ls-remote", repoURL)

			for _, d := range []string{dir, filepath.Dir(dir)} {
				if _, err := os.Stat(filepath.Join(d, "PWNED")); err == nil {
					t.Fatalf("the run id was executed as shell by the credential helper: %s/PWNED exists (ls-remote: %v)", d, lsErr)
				}
			}
			mu.Lock()
			defer mu.Unlock()
			if !sawAuth {
				t.Errorf("the credential helper did not supply the token to the forge (ls-remote: %v)", lsErr)
			}
		})
	}
}
