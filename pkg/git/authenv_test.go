package git

import (
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"testing"
)

const (
	authEnvTestToken  = "glpat-authenv-test-token"
	authEnvTestRemote = "https://forge.example/o/r.git"
)

func wantAuthHeader() string {
	return "AUTHORIZATION: Basic " + base64.StdEncoding.EncodeToString([]byte("oauth2:"+authEnvTestToken))
}

// gitConfigUnder asks the real git binary what one config key resolves to
// under env, so the test checks the entries git reads, not their spelling.
func gitConfigUnder(t *testing.T, env []string, query ...string) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	if len(query) == 1 {
		query = []string{"--get-all", query[0]}
	}
	cmd := exec.Command("git", NoAutoMaintenance(append([]string{"config"}, query...)...)...)
	cmd.Dir = t.TempDir()
	cmd.Env = append(env, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "HOME="+t.TempDir())
	out, err := cmd.Output()
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 1 {
		return "" // git config's answer for an unset key
	}
	if err != nil {
		t.Fatalf("git config %q: %v", query, err)
	}
	return strings.TrimSpace(string(out))
}

// gitHeaderFor asks the real git binary which extraHeader it applies to a
// request for url under env (urlmatch, as git and git-lfs resolve it).
func gitHeaderFor(t *testing.T, env []string, url string) string {
	t.Helper()
	return gitConfigUnder(t, env, "--get-urlmatch", "http.extraHeader", url)
}

func TestAuthEnvMergesWithOperatorCount(t *testing.T) {
	operator := []string{
		"PATH=/usr/bin:/bin",
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=url.https://mirror.example/.insteadOf",
		"GIT_CONFIG_VALUE_0=https://forge.example/",
	}
	env, err := AuthEnv(operator, authEnvTestRemote, authEnvTestToken)
	if err != nil {
		t.Fatalf("AuthEnv: %v", err)
	}
	if got := gitConfigUnder(t, env, "url.https://mirror.example/.insteadOf"); got != "https://forge.example/" {
		t.Errorf("operator insteadOf = %q after AuthEnv, want it preserved", got)
	}
	if got := gitHeaderFor(t, env, authEnvTestRemote); got != wantAuthHeader() {
		t.Errorf("extraHeader for the forge = %q, want %q", got, wantAuthHeader())
	}
	if !slices.Contains(env, "GIT_CONFIG_COUNT=2") {
		t.Errorf("env %q: want GIT_CONFIG_COUNT=2 after one operator entry", env)
	}
}

func TestAuthEnvFreshCount(t *testing.T) {
	env, err := AuthEnv([]string{"PATH=/usr/bin:/bin"}, authEnvTestRemote, authEnvTestToken)
	if err != nil {
		t.Fatalf("AuthEnv: %v", err)
	}
	if got := gitHeaderFor(t, env, authEnvTestRemote); got != wantAuthHeader() {
		t.Errorf("extraHeader for the forge = %q, want %q", got, wantAuthHeader())
	}
	if !slices.Contains(env, "GIT_CONFIG_KEY_0=http.https://forge.example/.extraHeader") || !slices.Contains(env, "GIT_CONFIG_COUNT=1") {
		t.Errorf("env %q: want the header at index 0 and a count of 1", env)
	}
}

func TestAuthEnvReadsLastCount(t *testing.T) {
	env, err := AuthEnv([]string{"GIT_CONFIG_COUNT=junk", "GIT_CONFIG_COUNT=3"}, authEnvTestRemote, authEnvTestToken)
	if err != nil {
		t.Fatalf("AuthEnv: %v", err)
	}
	if !slices.Contains(env, "GIT_CONFIG_KEY_3=http.https://forge.example/.extraHeader") || env[len(env)-1] != "GIT_CONFIG_COUNT=4" {
		t.Errorf("env %q: want the header at index 3 (the count the child sees) and a final count of 4", env)
	}
}

// AuthEnv reads GIT_CONFIG_COUNT as git does (strtoul: leading whitespace
// and a sign skipped, no trailing characters), so it refuses exactly the
// counts git refuses — a blank count it took for 0 would overwrite the
// operator's entry 0 — and accepts the ones git accepts. Each case is
// checked against the real git as well.
func TestAuthEnvRefusesMalformedCount(t *testing.T) {
	for _, raw := range []string{"two", "-1", "1x", "1 ", " ", "\t"} {
		if env, err := AuthEnv([]string{"GIT_CONFIG_COUNT=" + raw}, authEnvTestRemote, authEnvTestToken); err == nil {
			t.Errorf("GIT_CONFIG_COUNT=%q: AuthEnv returned %q and no error, want a refusal", raw, env)
		}
		if gitAcceptsCount(t, raw) {
			t.Errorf("GIT_CONFIG_COUNT=%q: git accepts it, so AuthEnv must not refuse it", raw)
		}
	}
}

func TestAuthEnvAcceptsTheCountsGitAccepts(t *testing.T) {
	for _, raw := range []string{" 1", "\t1", "+1", "01"} {
		env, err := AuthEnv([]string{"GIT_CONFIG_COUNT=" + raw, "GIT_CONFIG_KEY_0=core.foo", "GIT_CONFIG_VALUE_0=bar"}, authEnvTestRemote, authEnvTestToken)
		if err != nil {
			t.Errorf("GIT_CONFIG_COUNT=%q: AuthEnv: %v, want the header at index 1", raw, err)
			continue
		}
		if !slices.Contains(env, "GIT_CONFIG_KEY_1=http.https://forge.example/.extraHeader") {
			t.Errorf("GIT_CONFIG_COUNT=%q: env %q, want the header at index 1", raw, env)
		}
		if got := gitConfigUnder(t, env, "core.foo"); got != "bar" {
			t.Errorf("GIT_CONFIG_COUNT=%q: operator core.foo = %q after AuthEnv, want it preserved", raw, got)
		}
		if !gitAcceptsCount(t, raw) {
			t.Errorf("GIT_CONFIG_COUNT=%q: git refuses it, so AuthEnv must refuse it too", raw)
		}
	}
}

// gitAcceptsCount reports whether the real git reads raw as a valid
// GIT_CONFIG_COUNT (with one entry 0 behind it).
func gitAcceptsCount(t *testing.T, raw string) bool {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	cmd := exec.Command("git", NoAutoMaintenance("config", "--get-all", "core.foo")...)
	cmd.Dir = t.TempDir()
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_COUNT=" + raw, "GIT_CONFIG_KEY_0=core.foo", "GIT_CONFIG_VALUE_0=bar"}
	out, err := cmd.CombinedOutput()
	if strings.Contains(string(out), "GIT_CONFIG_COUNT") { // "bogus count", "too many entries"
		return false
	}
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 1 {
			return true // a valid count of 0: core.foo unset
		}
		t.Fatalf("git config with GIT_CONFIG_COUNT=%q: %v\n%s", raw, err, out)
	}
	return true
}

// The header is bound to the forge origin: git resolves it for the forge's
// URLs only, never for another host (an LFS storage host, a foreign
// lfs.url) nor another port, and a remote that is not http(s) gets no
// header at all — git-lfs reads http.* config for the https requests it
// makes on behalf of an ssh remote.
func TestAuthEnvScopesTheHeaderToTheForgeOrigin(t *testing.T) {
	env, err := AuthEnv(nil, "https://forge.example:8443/o/r.git", authEnvTestToken)
	if err != nil {
		t.Fatalf("AuthEnv: %v", err)
	}
	if got := gitHeaderFor(t, env, "https://forge.example:8443/o/r.git/info/lfs/objects/batch"); got != wantAuthHeader() {
		t.Errorf("extraHeader for the forge's LFS API = %q, want %q", got, wantAuthHeader())
	}
	for _, other := range []string{
		"https://storage.example/presigned/obj",
		"https://forge.example/o/r.git",
		"https://forge.example:9443/o/r.git",
		"http://forge.example:8443/o/r.git",
	} {
		if got := gitHeaderFor(t, env, other); got != "" {
			t.Errorf("extraHeader for %s = %q, want none: only the forge origin receives the credential", other, got)
		}
	}
	for _, remote := range []string{"ssh://git@forge.example/o/r.git", "git@forge.example:o/r.git", "file:///srv/o/r.git", "/srv/o/r.git"} {
		in := []string{"PATH=/usr/bin:/bin"}
		env, err := AuthEnv(in, remote, authEnvTestToken)
		if err != nil {
			t.Errorf("AuthEnv(%q): %v, want env unchanged", remote, err)
			continue
		}
		if !slices.Equal(env, in) {
			t.Errorf("AuthEnv(%q) = %q, want env unchanged: a non-http(s) remote carries no header", remote, env)
		}
	}
	for _, bad := range []string{"https://", "https://forge.example:bad/o/r.git"} {
		if env, err := AuthEnv(nil, bad, authEnvTestToken); err == nil {
			t.Errorf("AuthEnv(%q) = %q and no error, want a refusal: the credential has no origin to bind to", bad, env)
		} else if strings.Contains(err.Error(), bad) {
			t.Errorf("AuthEnv(%q) error %q quotes the URL", bad, err)
		}
	}
}

func TestAuthEnvEmptyTokenLeavesEnvAlone(t *testing.T) {
	in := []string{"PATH=/usr/bin:/bin", "GIT_CONFIG_COUNT=1"}
	env, err := AuthEnv(in, authEnvTestRemote, "")
	if err != nil {
		t.Fatalf("AuthEnv: %v", err)
	}
	if !slices.Equal(env, in) {
		t.Errorf("AuthEnv(env, \"\") = %q, want env unchanged", env)
	}
}

// git sends the header AuthEnv configures on the wire to the forge, and to
// no other origin: two stand-ins on the same address, different ports,
// record the Authorization of the requests a real `git ls-remote` makes.
func TestAuthEnvHeaderReachesTheForge(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	var (
		mu   sync.Mutex
		seen []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Header.Get("Authorization"))
		mu.Unlock()
		http.Error(w, "stand-in forge", http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	var otherSeen []string
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		otherSeen = append(otherSeen, r.Header.Get("Authorization"))
		mu.Unlock()
		http.Error(w, "another host", http.StatusNotFound)
	}))
	t.Cleanup(other.Close)

	base := []string{"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_TERMINAL_PROMPT=0", "NO_PROXY=127.0.0.1", "no_proxy=127.0.0.1"}
	env, err := AuthEnv(base, srv.URL+"/o/r.git", authEnvTestToken)
	if err != nil {
		t.Fatalf("AuthEnv: %v", err)
	}
	for _, target := range []string{srv.URL, other.URL} {
		cmd := exec.Command("git", NoAutoMaintenance("ls-remote", target+"/o/r.git")...)
		cmd.Dir = t.TempDir()
		cmd.Env = env
		_ = cmd.Run() // both stand-ins answer 404; only the requests they saw matter
	}

	mu.Lock()
	defer mu.Unlock()
	if len(seen) == 0 {
		t.Fatal("git never reached the stand-in forge")
	}
	if seen[0] != strings.TrimPrefix(wantAuthHeader(), "AUTHORIZATION: ") {
		t.Errorf("first request's Authorization = %q, want %q", seen[0], strings.TrimPrefix(wantAuthHeader(), "AUTHORIZATION: "))
	}
	if len(otherSeen) == 0 {
		t.Fatal("git never reached the second host")
	}
	for _, a := range otherSeen {
		if a != "" {
			t.Errorf("a host other than the forge received Authorization %q", a)
		}
	}
}
