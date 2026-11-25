package runner

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/store"
)

// lfsForge is a private https forge for prepareRepoWorkspace: git smart-HTTP
// (git http-backend) and an LFS batch API, both behind Basic
// oauth2:<cloneCredTestToken>, with a real git-lfs configured as the clone's
// filter through a scratch HOME. Branch main carries a plain baseline;
// branch feat adds one LFS object (big.bin). With storage set, the batch API
// hands out download hrefs on a SEPARATE TLS host — the shape of a forge's
// object storage (a presigned URL that carries its own authorization) —
// which records every Authorization it receives.
type lfsForge struct {
	url      string // the repository URL: <forge>/o/r.git
	seed     string // a working clone of the forge's repository
	content  []byte // big.bin's smudged content
	mu       sync.Mutex
	forgeLog []string
	storage  []string // Authorization values the storage host received
}

func (f *lfsForge) logs() (forge, storage []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.forgeLog...), append([]string(nil), f.storage...)
}

func newLFSForge(t *testing.T, featOnMain, storage bool) *lfsForge {
	t.Helper()
	if _, err := exec.LookPath("git-lfs"); err != nil {
		t.Skip("git-lfs not on PATH")
	}
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not on PATH")
	}
	tmp := t.TempDir()
	root := filepath.Join(tmp, "srv")
	origin := filepath.Join(root, "o", "r.git")
	if err := os.MkdirAll(filepath.Dir(origin), 0o755); err != nil {
		t.Fatalf("mkdir the forge root: %v", err)
	}
	gitOut(t, tmp, "init", "--quiet", "--bare", "--initial-branch=main", origin)
	f := &lfsForge{seed: filepath.Join(tmp, "seed"), content: make([]byte, 2048)}
	gitOut(t, tmp, "clone", "--quiet", origin, f.seed)
	gitOut(t, f.seed, "config", "user.email", "t@test.invalid")
	gitOut(t, f.seed, "config", "user.name", "t")
	writeTestFile(t, filepath.Join(f.seed, "README"), "baseline\n")
	gitOut(t, f.seed, "add", "README")
	gitOut(t, f.seed, "commit", "--quiet", "-m", "baseline")
	gitOut(t, f.seed, "push", "--quiet", "origin", "HEAD:main")
	gitOut(t, f.seed, "checkout", "--quiet", "-b", "feat")
	if _, err := rand.Read(f.content); err != nil {
		t.Fatalf("random LFS content: %v", err)
	}
	sum := sha256.Sum256(f.content)
	oid := hex.EncodeToString(sum[:])
	writeTestFile(t, filepath.Join(f.seed, ".gitattributes"), "*.bin filter=lfs diff=lfs merge=lfs -text\n")
	writeTestFile(t, filepath.Join(f.seed, "big.bin"), fmt.Sprintf("version https://git-lfs.github.com/spec/v1\noid sha256:%s\nsize %d\n", oid, len(f.content)))
	gitOut(t, f.seed, "add", ".gitattributes", "big.bin")
	gitOut(t, f.seed, "commit", "--quiet", "-m", "lfs object")
	gitOut(t, f.seed, "push", "--quiet", "origin", "HEAD:feat")
	if featOnMain {
		gitOut(t, f.seed, "push", "--quiet", "--force", "origin", "HEAD:main")
	}

	wantAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte("oauth2:"+cloneCredTestToken))
	storageURL := ""
	var certs [][]byte
	if storage {
		st := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			f.mu.Lock()
			f.storage = append(f.storage, strings.Join(req.Header.Values("Authorization"), " | "))
			f.mu.Unlock()
			_, _ = w.Write(f.content)
		}))
		t.Cleanup(st.Close)
		storageURL = st.URL
		certs = append(certs, st.Certificate().Raw)
	}
	var forgeURL string
	backend := &cgi.Handler{Path: realGit, Args: []string{"http-backend"}, Env: []string{"GIT_PROJECT_ROOT=" + root, "GIT_HTTP_EXPORT_ALL=1"}}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		ok := req.Header.Get("Authorization") == wantAuth
		f.mu.Lock()
		f.forgeLog = append(f.forgeLog, fmt.Sprintf("%s %s authed=%v", req.Method, req.URL.Path, ok))
		f.mu.Unlock()
		if !ok {
			w.Header().Set("WWW-Authenticate", `Basic realm="forge"`)
			http.Error(w, "auth required", http.StatusUnauthorized)
			return
		}
		switch {
		case strings.HasSuffix(req.URL.Path, "/info/lfs/objects/batch"):
			download := map[string]any{"href": forgeURL + "/lfs-obj/" + oid, "header": map[string]string{"Authorization": wantAuth}}
			if storageURL != "" {
				download = map[string]any{"href": storageURL + "/presigned/" + oid + "?X-Amz-Signature=stand-in"}
			}
			w.Header().Set("Content-Type", "application/vnd.git-lfs+json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"transfer": "basic",
				"objects": []any{map[string]any{
					"oid": oid, "size": len(f.content), "authenticated": true,
					"actions": map[string]any{"download": download},
				}},
			})
		case strings.HasPrefix(req.URL.Path, "/lfs-obj/"):
			_, _ = w.Write(f.content)
		default:
			backend.ServeHTTP(w, req)
		}
	}))
	t.Cleanup(srv.Close)
	forgeURL = srv.URL
	f.url = srv.URL + "/o/r.git"
	certs = append(certs, srv.Certificate().Raw)

	var bundle []byte
	for _, c := range certs {
		bundle = append(bundle, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c})...)
	}
	ca := filepath.Join(tmp, "ca.pem")
	if err := os.WriteFile(ca, bundle, 0o600); err != nil {
		t.Fatalf("write the stand-ins' CA bundle: %v", err)
	}
	home := filepath.Join(tmp, "home")
	if err := os.Mkdir(home, 0o755); err != nil {
		t.Fatalf("mkdir the scratch HOME: %v", err)
	}
	writeTestFile(t, filepath.Join(home, ".gitconfig"),
		"[filter \"lfs\"]\n\tclean = git-lfs clean -- %f\n\tsmudge = git-lfs smudge -- %f\n\tprocess = git-lfs filter-process\n\trequired = true\n")
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(home, ".gitconfig"))
	t.Setenv("GIT_SSL_CAINFO", ca)
	t.Setenv("NO_PROXY", "127.0.0.1,localhost")
	t.Setenv("no_proxy", "127.0.0.1,localhost")
	t.Setenv("GIT_CONFIG_PARAMETERS", "")
	t.Setenv("GIT_CONFIG_COUNT", "0")
	return f
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// assertSmudged fails unless big.bin in dir is the LFS object, not its
// pointer.
func (f *lfsForge) assertSmudged(t *testing.T, dir string) {
	t.Helper()
	got, err := os.ReadFile(filepath.Join(dir, "big.bin"))
	if err != nil {
		t.Fatalf("read big.bin in the prepared workspace: %v", err)
	}
	if string(got) != string(f.content) {
		forge, _ := f.logs()
		t.Fatalf("big.bin is not the smudged LFS object (len %d, pointer=%v); the forge saw:\n%s",
			len(got), strings.HasPrefix(string(got), "version https://git-lfs"), strings.Join(forge, "\n"))
	}
}

// git-lfs smudges an object the run's ref carries during the checkout that
// follows the ref's fetch. git-lfs authenticates from git's credential
// helpers and its http.* config, never from the process that runs the
// clone, so the run's credential store must already be wired when that
// checkout runs: a private forge refuses an anonymous batch request.
func TestTheRunRefSmudgesLFSObjectsBehindForgeAuth(t *testing.T) {
	r, _ := forgeRunner(t)
	f := newLFSForge(t, false, false)
	msg := &queue.RunMessage{RunID: "run-lfs-ref", RepoURL: f.url, RepoSHA: "feat"}
	dir, _, err := r.prepareRepoWorkspace(cloneCredCtx("forge_token"), cloneCredWorkflow(), msg)
	if err != nil {
		forge, _ := f.logs()
		t.Fatalf("prepareRepoWorkspace on a private ref carrying an LFS object: %v\nthe forge saw:\n%s", err, strings.Join(forge, "\n"))
	}
	f.assertSmudged(t, dir)
}

// A re-executed run restores the chain an earlier attempt banked; the
// fast-forward onto it smudges the LFS objects the chain carries, through
// the same credential store.
func TestTheRestoredChainSmudgesLFSObjectsBehindForgeAuth(t *testing.T) {
	r, _ := forgeRunner(t)
	f := newLFSForge(t, false, false)
	msg := &queue.RunMessage{RunID: "run-lfs-restore", TenantID: "team-a", RepoURL: f.url, RepoSHA: "main", Resume: &queue.ResumeSpec{}}
	ctx := store.WithIdentity(cloneCredCtx("forge_token"), "team-a", "")
	if err := r.cfg.Store.SaveRun(ctx, &store.Run{ID: msg.RunID, TenantID: msg.TenantID, FinalBranch: "feat",
		FinalCommit: gitOut(t, f.seed, "rev-parse", "HEAD")}); err != nil {
		t.Fatalf("seed the banked run: %v", err)
	}
	dir, _, err := r.prepareRepoWorkspace(ctx, cloneCredWorkflow(), msg)
	if err != nil {
		t.Fatalf("prepareRepoWorkspace on a re-execution: %v", err)
	}
	var restored []string
	_ = r.cfg.Store.ScanEvents(ctx, msg.RunID, func(e *store.Event) bool {
		if e.Type == store.EventRunWorkspaceBankRestored {
			restored = append(restored, fmt.Sprint(e.Data))
		}
		return true
	})
	if len(restored) != 1 || !strings.Contains(restored[0], "restored:true") {
		t.Errorf("bank restore events = %q, want one with restored:true", restored)
	}
	f.assertSmudged(t, dir)
}

// The forge credential goes to the forge origin only: git-lfs downloads an
// object from the storage host the batch API names (a presigned URL that
// carries its own authorization), and that host must never receive the
// forge token — not from the clone's header, not from the store.
func TestTheLFSStorageHostNeverSeesTheForgeCredential(t *testing.T) {
	r, _ := forgeRunner(t)
	f := newLFSForge(t, true, true)
	msg := &queue.RunMessage{RunID: "run-lfs-storage", RepoURL: f.url}
	dir, _, err := r.prepareRepoWorkspace(cloneCredCtx("forge_token"), cloneCredWorkflow(), msg)
	if err != nil {
		t.Fatalf("prepareRepoWorkspace: %v", err)
	}
	f.assertSmudged(t, dir)
	_, storage := f.logs()
	if len(storage) == 0 {
		t.Fatal("git-lfs never reached the storage host")
	}
	for _, a := range storage {
		if a != "" {
			t.Errorf("the LFS storage host received Authorization %q; only the forge origin may receive the credential", a)
		}
	}
}

// An ssh remote authenticates through ssh; git-lfs, spawned by that clone,
// reads http.* config for the https requests it makes to the LFS API and
// storage hosts, so the forge header must not ride the environment at all.
func TestAnSSHRemoteCarriesNoForgeHeader(t *testing.T) {
	for _, remote := range []string{"ssh://git@forge.example/o/r.git", "git@forge.example:o/r.git"} {
		cmd, _, err := newGitCmd(context.Background(), "", cloneCredTestToken, remote, nil, "clone", remote, t.TempDir())
		if err != nil {
			t.Fatalf("newGitCmd(%q): %v", remote, err)
		}
		for _, kv := range cmd.Env {
			if strings.Contains(kv, "AUTHORIZATION") || strings.Contains(strings.ToLower(kv), "extraheader") {
				t.Errorf("remote %q: env entry %q carries the forge header", remote, kv)
			}
		}
	}
	cmd, _, err := newGitCmd(context.Background(), "", cloneCredTestToken, "https://forge.example/o/r.git", nil, "clone")
	if err != nil {
		t.Fatalf("newGitCmd(https): %v", err)
	}
	scoped := false
	for _, kv := range cmd.Env {
		if strings.HasPrefix(kv, "GIT_CONFIG_KEY_") && strings.HasSuffix(kv, "=http.https://forge.example/.extraHeader") {
			scoped = true
		}
	}
	if !scoped {
		t.Error("https remote: the env carries no extraHeader bound to the forge origin")
	}
}
