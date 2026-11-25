package runview

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/store"
)

const mergeCloneTestToken = "glpat-merge-clone-secret"

func mergeCloneTestHeader() string {
	return "AUTHORIZATION: Basic " + base64.StdEncoding.EncodeToString([]byte("oauth2:"+mergeCloneTestToken))
}

// The merge clone's git calls authenticate to the forge; the credential must
// reach git through its environment, never argv, which any local user reads
// in /proc/<pid>/cmdline while the command runs, and it is bound to the
// forge origin.
func TestMergeGitCmdCarriesCredentialInEnvNotArgv(t *testing.T) {
	header := mergeCloneTestHeader()
	cmd, err := newMergeGitCmd(context.Background(), t.TempDir(), "https://forge.example/o/r.git", mergeCloneTestToken,
		"-c", "http.followRedirects=false", "fetch", "--quiet", "origin", "main")
	if err != nil {
		t.Fatalf("newMergeGitCmd: %v", err)
	}
	for _, a := range cmd.Args {
		if strings.Contains(a, mergeCloneTestToken) || strings.Contains(a, header) || strings.Contains(strings.ToLower(a), "extraheader") {
			t.Errorf("argv element %q carries the forge credential; argv = %q", a, cmd.Args)
		}
	}
	if !slices.Contains(cmd.Args, "fetch") {
		t.Errorf("argv %q lost the caller's command", cmd.Args)
	}
	key := ""
	for _, kv := range cmd.Env {
		if v, ok := strings.CutPrefix(kv, "GIT_CONFIG_VALUE_"); ok && strings.HasSuffix(v, "="+header) {
			key = "GIT_CONFIG_KEY_" + strings.SplitN(v, "=", 2)[0] + "=http.https://forge.example/.extraHeader"
		}
	}
	if key == "" || !slices.Contains(cmd.Env, key) {
		t.Errorf("env carries no extraHeader entry bound to the forge origin with the forge header (key %q)", key)
	}
}

func TestMergeGitCmdWithoutTokenAddsNoHeader(t *testing.T) {
	cmd, err := newMergeGitCmd(context.Background(), "", "https://forge.example/o/r.git", "", "status")
	if err != nil {
		t.Fatalf("newMergeGitCmd: %v", err)
	}
	for _, kv := range cmd.Env {
		if strings.Contains(kv, "AUTHORIZATION") {
			t.Errorf("env entry %q: a token-less merge git call must carry no forge header", kv)
		}
	}
}

// The guarantee, through the production path: a repo-targeted merge clones,
// fetches and pushes with the forge token, and no git process it starts
// carries the credential in argv — each authenticated call carries it in its
// environment instead. A PATH-front git records every invocation's argv and
// its GIT_CONFIG_* entries, then hands over to the real git; an operator
// insteadOf maps the run's https forge URL onto the local bare forge.
func TestRepoTargetedMergeGitCarriesCredentialInEnvNotArgv(t *testing.T) {
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not on PATH")
	}
	bareURL, storageBranch, storageSHA := seedForge(t)
	const repoURL = "https://127.0.0.1/o/r.git"
	svc, runID := seedRepoTargetedRun(t, t.TempDir(), repoURL, storageBranch, storageSHA)
	svc.forgeToken = func(context.Context, *store.Run) (string, error) { return mergeCloneTestToken, nil }
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "url."+bareURL+".insteadOf")
	t.Setenv("GIT_CONFIG_VALUE_0", repoURL)

	tmp := t.TempDir()
	bin := filepath.Join(tmp, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatalf("mkdir the recording git's dir: %v", err)
	}
	logPath := filepath.Join(tmp, "git.log")
	script := fmt.Sprintf("#!/bin/sh\nprintf '%%s\\t%%s\\n' \"$*\" \"$(env | grep '^GIT_CONFIG_' | tr '\\n' ' ')\" >> %s\nexec %s \"$@\"\n",
		strconv.Quote(logPath), strconv.Quote(realGit))
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o700); err != nil {
		t.Fatalf("write the recording git: %v", err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	res, mergeErr := svc.PerformMergeCtx(context.Background(), runID, MergeRequest{Strategy: store.MergeStrategyMerge})

	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read the recording git's log: %v", err)
	}
	header := mergeCloneTestHeader()
	b64 := strings.TrimPrefix(header, "AUTHORIZATION: Basic ")
	authed := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n") {
		argv, env, _ := strings.Cut(line, "\t")
		if strings.Contains(argv, mergeCloneTestToken) || strings.Contains(argv, b64) || strings.Contains(strings.ToLower(argv), "extraheader") {
			t.Errorf("git argv carries the forge credential: %q", argv)
		}
		for _, verb := range []string{"clone", "fetch", "push"} {
			if strings.Contains(" "+argv+" ", " "+verb+" ") && strings.Contains(env, "=http.https://127.0.0.1/.extraHeader") && strings.Contains(env, "="+header) {
				authed[verb] = true
			}
		}
	}
	for _, verb := range []string{"clone", "fetch", "push"} {
		if !authed[verb] {
			t.Errorf("no git %s carried the forge header, bound to the forge origin, in its environment; invocations:\n%s", verb, raw)
		}
	}
	if mergeErr != nil {
		t.Fatalf("PerformMergeCtx: %v", mergeErr)
	}
	if res.MergeStatus != store.MergeStatusMerged {
		t.Fatalf("merge status = %q, want merged", res.MergeStatus)
	}
}
