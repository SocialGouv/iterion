package bots

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/SocialGouv/iterion/internal/gittest"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// commitProbeResult is commit_probe_output.
type commitProbeResult struct {
	SHA    string `json:"sha"`
	Moved  bool   `json:"moved"`
	Reason string `json:"reason"`
}

// runCommitProbe runs commit_probe in ws with the forge token at tokenPath.
func runCommitProbe(t *testing.T, ws, prepared, reported, tokenPath string, env []string) (commitProbeResult, string) {
	t.Helper()
	body := toolScript(t, "dep-update-guard/main.bot", "commit_probe")
	for ref, value := range map[string]string{"{{vars.workspace_dir}}": ws, "{{input.head_sha}}": prepared,
		"{{input.sha}}": reported, "{{secrets.forge_token.path}}": tokenPath} {
		encoded, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		body = strings.ReplaceAll(body, ref, string(encoded))
	}
	if strings.Contains(body, "{{") {
		t.Fatal("unsubstituted commit probe input")
	}
	path := filepath.Join(t.TempDir(), "probe.py")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("python3", "-I", path)
	cmd.Env = env
	raw, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("probe: %v: %s", err, raw)
	}
	var got commitProbeResult
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("probe JSON: %v: %s", err, raw)
	}
	if got.Reason == "" {
		t.Fatal("probe omitted its reason")
	}
	return got, string(raw)
}

// TestDepUpdateGuardCommitProbeReadsGit: a new commit counts only when git
// says so — the workspace HEAD, ONE commit whose only parent is the audited
// commit, held by the remote branch — and the agent's report, when it names
// a commit id, names that same commit.
//
// Mutations (main.bot `commit_probe`): drop the remote == local HEAD
// comparison (the unpushed case certifies a commit the PR does not carry);
// restore the report-first exit on an empty report (the pushed-but-
// unreported case is refused again); refuse a report that names no commit
// id instead of setting it aside (the odd-report case); test only that the
// audited commit is an ancestor (the someone-else's-commit cases).
func TestDepUpdateGuardCommitProbeReadsGit(t *testing.T) {
	read := func(t *testing.T, ws, prepared, reported string, extra ...string) commitProbeResult {
		t.Helper()
		got, _ := runCommitProbe(t, ws, prepared, reported, "", append(gittest.Env(), extra...))
		return got
	}
	origin := filepath.Join(t.TempDir(), "origin.git")
	ws := t.TempDir()
	ancestor := gittest.InitRepo(t, ws)
	gittest.Run(t, ws, "init", "-q", "--bare", origin)
	gittest.Run(t, origin, "config", "maintenance.auto", "false")
	gittest.Run(t, ws, "remote", "add", "origin", origin)
	gittest.Run(t, ws, "commit", "--allow-empty", "-m", "dependency bump")
	prepared := strings.TrimSpace(gittest.Run(t, ws, "rev-parse", "HEAD"))
	gittest.Run(t, ws, "push", "-q", "origin", "HEAD:refs/heads/main")
	for _, reported := range []string{"", "none", "HEAD", "--all", strings.Repeat("f", 40), prepared, prepared[:7], prepared[:12], strings.ToUpper(prepared), prepared + "\n", ancestor} {
		t.Run("unmoved/"+reported, func(t *testing.T) {
			if got := read(t, ws, prepared, reported); got.Moved || got.SHA != "" {
				t.Fatalf("an unmoved head was certified as a new commit: %+v", got)
			}
		})
	}
	gittest.Run(t, ws, "commit", "--allow-empty", "-m", "alignment")
	aligned := strings.TrimSpace(gittest.Run(t, ws, "rev-parse", "HEAD"))
	t.Run("committed but not pushed: no evidence", func(t *testing.T) {
		for _, reported := range []string{"", aligned} {
			if got := read(t, ws, prepared, reported); got.Moved || got.SHA != "" || !strings.Contains(got.Reason, "no push") {
				t.Fatalf("a commit the remote does not hold was certified (report %q): %+v", reported, got)
			}
		}
	})
	gittest.Run(t, ws, "push", "-q", "origin", "HEAD:refs/heads/main")
	for _, reported := range []string{aligned, aligned[:7], aligned[:12], strings.ToUpper(aligned), aligned + "\n"} {
		t.Run("new-head/"+reported, func(t *testing.T) {
			if got := read(t, ws, prepared, reported); !got.Moved || got.SHA != aligned {
				t.Fatalf("git's new head was not resolved to its full identity: %+v", got)
			}
		})
	}
	// The agent pushed and left its report empty: git is the evidence, so
	// the verdict is pinned to the commit the run's own push carried.
	t.Run("pushed, report empty: the head git and the remote agree on", func(t *testing.T) {
		if got := read(t, ws, prepared, ""); !got.Moved || got.SHA != aligned {
			t.Fatalf("a pushed alignment with no report was not certified: %+v", got)
		}
	})
	// A report with no commit id in it says nothing git cannot: refusing on
	// it pinned the verdict to the head the run's own push moved away from,
	// and the server refused that as "head moved".
	for _, reported := range []string{"N/A", "unknown", "pushed", "commit abc1234"} {
		t.Run("pushed, a report naming no commit id is set aside/"+reported, func(t *testing.T) {
			got := read(t, ws, prepared, reported)
			if !got.Moved || got.SHA != aligned || !strings.Contains(got.Reason, "set aside") {
				t.Fatalf("a pushed alignment with the report %q was not certified on the git evidence: %+v", reported, got)
			}
		})
	}
	foreign := t.TempDir()
	foreignHead := gittest.InitRepo(t, foreign)
	t.Run("ambient repository redirection is ignored", func(t *testing.T) {
		got := read(t, ws, prepared, aligned, "GIT_DIR="+filepath.Join(foreign, ".git"), "GIT_WORK_TREE="+foreign, "GIT_OBJECT_DIRECTORY="+filepath.Join(foreign, ".git", "objects"))
		if !got.Moved || got.SHA != aligned {
			t.Fatalf("ambient git variables redirected the proof: %+v", got)
		}
	})
	t.Run("foreign report is not evidence", func(t *testing.T) {
		if got := read(t, ws, prepared, foreignHead); got.Moved || got.SHA != "" {
			t.Fatalf("a foreign commit was accepted: %+v", got)
		}
	})
	gittest.Run(t, ws, "commit", "--allow-empty", "-m", "later commit")
	t.Run("a new but non-tip commit is not the published revision", func(t *testing.T) {
		if got := read(t, ws, prepared, aligned); got.Moved || got.SHA != "" {
			t.Fatalf("an intermediate commit was accepted as the tip: %+v", got)
		}
	})
	// The remote holds a commit the workspace never made: someone pushed in
	// between, so this run's commit is not what the pull request carries.
	sibling := strings.TrimSpace(gittest.Run(t, ws, "commit-tree", "-p", prepared, "-m", "someone else", prepared+"^{tree}"))
	gittest.Run(t, ws, "push", "-q", "--force", "origin", sibling+":refs/heads/main")
	gittest.Run(t, ws, "reset", "-q", "--hard", aligned)
	t.Run("the remote holds another commit: no evidence", func(t *testing.T) {
		if got := read(t, ws, prepared, ""); got.Moved || got.SHA != "" || !strings.Contains(got.Reason, "no push") {
			t.Fatalf("a head the remote does not hold was certified: %+v", got)
		}
	})
	gittest.Run(t, ws, "switch", "--orphan", "unrelated")
	gittest.Run(t, ws, "commit", "--allow-empty", "-m", "unrelated root")
	unrelated := strings.TrimSpace(gittest.Run(t, ws, "rev-parse", "HEAD"))
	gittest.Run(t, ws, "push", "-q", "origin", "HEAD:refs/heads/unrelated")
	t.Run("an unrelated head is not this run's alignment", func(t *testing.T) {
		if got := read(t, ws, prepared, unrelated); got.Moved || got.SHA != "" {
			t.Fatalf("an unrelated head was certified: %+v", got)
		}
	})
	t.Run("an unreadable workspace proves nothing", func(t *testing.T) {
		if got := read(t, t.TempDir(), prepared, aligned); got.Moved || got.SHA != "" {
			t.Fatalf("an unreadable repository certified a commit: %+v", got)
		}
	})

	// The alignment is ONE commit on the audited head. Anything else that
	// descends from it and that the remote holds — a commit someone else
	// pushed meanwhile, pulled or rebased under the alignment, a merge of
	// it, a second commit — would ride on a verdict that never saw it. Each
	// case is pushed, so only the one-commit rule can refuse it.
	for _, tc := range []struct {
		name  string
		build func(t *testing.T, ws, audited, foreign string)
	}{
		{"the alignment rebased onto a commit someone else pushed", func(t *testing.T, ws, _, foreign string) {
			gittest.Run(t, ws, "reset", "-q", "--hard", foreign)
			gittest.Run(t, ws, "commit", "--allow-empty", "-m", "alignment, rebased")
		}},
		{"the alignment with a commit someone else pushed merged in", func(t *testing.T, ws, _, foreign string) {
			gittest.Run(t, ws, "commit", "--allow-empty", "-m", "alignment")
			gittest.Run(t, ws, "merge", "-q", "--no-edit", "--no-ff", foreign)
		}},
		{"two commits on the audited head", func(t *testing.T, ws, _, _ string) {
			gittest.Run(t, ws, "commit", "--allow-empty", "-m", "alignment")
			gittest.Run(t, ws, "commit", "--allow-empty", "-m", "and more")
		}},
	} {
		t.Run("pushed, not one commit on the audited head: "+tc.name, func(t *testing.T) {
			origin := filepath.Join(t.TempDir(), "origin.git")
			ws := t.TempDir()
			gittest.InitRepo(t, ws)
			gittest.Run(t, ws, "init", "-q", "--bare", origin)
			gittest.Run(t, origin, "config", "maintenance.auto", "false")
			gittest.Run(t, ws, "remote", "add", "origin", origin)
			gittest.Run(t, ws, "commit", "--allow-empty", "-m", "dependency bump")
			audited := strings.TrimSpace(gittest.Run(t, ws, "rev-parse", "HEAD"))
			foreign := strings.TrimSpace(gittest.Run(t, ws, "commit-tree", "-p", audited, "-m", "someone else", audited+"^{tree}"))
			tc.build(t, ws, audited, foreign)
			gittest.Run(t, ws, "push", "-q", "origin", "HEAD:refs/heads/main")
			if got := read(t, ws, audited, ""); got.Moved || got.SHA != "" || !strings.Contains(got.Reason, "ONE commit") {
				t.Fatalf("a head that is not one commit on the audited one was certified: %+v", got)
			}
		})
	}
}

// TestDepUpdateGuardCommitProbeTokenTravelsByEnvironment: the push evidence
// needs a credentialed read of the remote, and the forge token reaches git
// through its ENVIRONMENT — never its argv, never the probe's output. The
// remote is a TLS server that records what git sends; a git shim records
// every argv.
//
// Mutation (main.bot `commit_probe`): pass the header as `-c
// http.extraHeader=…` on the command line — the argv assertion reddens; drop
// the header — the server assertion reddens.
func TestDepUpdateGuardCommitProbeTokenTravelsByEnvironment(t *testing.T) {
	const token = "ghs_probeSecretToken1234567890"
	var mu sync.Mutex
	var auth []string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		auth = append(auth, r.Header.Get("Authorization"))
		mu.Unlock()
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	ws := t.TempDir()
	gittest.InitRepo(t, ws)
	prepared := gittest.Run(t, ws, "rev-parse", "HEAD")
	gittest.Run(t, ws, "commit", "--allow-empty", "-m", "alignment")
	gittest.Run(t, ws, "remote", "add", "origin", srv.URL+"/acme/widgets.git")
	tokenPath := filepath.Join(t.TempDir(), "forge_token")
	if err := os.WriteFile(tokenPath, []byte(token+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	real, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	shim, argv := t.TempDir(), filepath.Join(t.TempDir(), "argv.log")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> " + argv + "\nexec " + real + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(shim, "git"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	var env []string
	for _, kv := range gittest.Env() {
		name := strings.ToUpper(strings.SplitN(kv, "=", 2)[0])
		if strings.HasSuffix(name, "_PROXY") || name == "PATH" {
			continue
		}
		env = append(env, kv)
	}
	env = append(env, "PATH="+shim+string(os.PathListSeparator)+os.Getenv("PATH"), "GIT_SSL_NO_VERIFY=1")

	got, out := runCommitProbe(t, ws, prepared, "", tokenPath, env)
	if got.Moved {
		t.Fatalf("a remote that refused every read certified a push: %+v", got)
	}
	mu.Lock()
	seen := slices.Clone(auth)
	mu.Unlock()
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("oauth2:"+token))
	if !slices.Contains(seen, want) {
		t.Errorf("the remote never received the token as an Authorization header (saw %q)", seen)
	}
	logged, err := os.ReadFile(argv)
	if err != nil {
		t.Fatalf("the git shim recorded nothing: %v", err)
	}
	if !strings.Contains(string(logged), "ls-remote") {
		t.Fatalf("the shim did not see the push-evidence read:\n%s", logged)
	}
	encoded := base64.StdEncoding.EncodeToString([]byte("oauth2:" + token))
	if strings.Contains(string(logged), token) || strings.Contains(string(logged), encoded) {
		t.Errorf("the forge token reached git's argv:\n%s", logged)
	}
	if strings.Contains(out, token) || strings.Contains(out, encoded) {
		t.Errorf("the forge token reached the probe's output: %s", out)
	}
}

func TestDepUpdateGuardCommitEvidenceReachesEveryConsumer(t *testing.T) {
	wf := ir.Compile(parseBotUnit("dep-update-guard/main.bot").File).Workflow
	if wf == nil {
		t.Fatal("workflow did not compile")
	}
	if _, ok := wf.Nodes["commit_probe"].(*ir.ToolNode); !ok {
		t.Fatal("commit evidence must be a deterministic tool")
	}
	var commitEdges, probeEdges int
	for _, edge := range wf.Edges {
		mappings := map[string]string{}
		for _, m := range edge.With {
			mappings[m.Key] = strings.Trim(m.Raw, `"`)
		}
		if edge.From == "commit" {
			commitEdges++
			if edge.To != "commit_probe" || mappings["sha"] != "{{outputs.commit.sha}}" || mappings["head_sha"] != "{{outputs.prepare.head_sha}}" {
				t.Fatalf("commit bypasses or misfeeds its proof: %+v", edge)
			}
		}
		if edge.From == "commit_probe" {
			probeEdges++
			if edge.To != "commit_check" {
				t.Fatalf("proof bypasses commit verdict: %+v", edge)
			}
		}
		if edge.From == "feedback_health" && edge.To == "arm_automerge" && mappings["committed_sha"] != "{{outputs.commit_probe.sha}}" {
			t.Fatal("arming reads an unverified commit report")
		}
		// The revision arming vouches for is the one post_feedback pinned
		// the verdict to — after a repin, the head the green was carried
		// onto. Mutation: map prepare.head_sha again -> a repinned run
		// refuses to arm the head it just gated.
		if edge.From == "feedback_health" && edge.To == "arm_automerge" && mappings["audited_sha"] != "{{outputs.post_feedback.pinned_sha}}" {
			t.Fatalf("arming vouches for %q, not the revision post_feedback pinned", mappings["audited_sha"])
		}
	}
	if commitEdges != 1 || probeEdges != 1 {
		t.Fatalf("commit/probe edges = %d/%d", commitEdges, probeEdges)
	}
}
