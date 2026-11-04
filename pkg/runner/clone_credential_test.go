package runner

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	dslast "github.com/SocialGouv/iterion/pkg/dsl/ast"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/parser/parsertest"
	"github.com/SocialGouv/iterion/pkg/dsl/secretsink"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/retrypolicy"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
)

const cloneCredTestToken = "ghs_live_installation_token"

func cloneCredCtx(name string) context.Context {
	return secrets.WithCredentials(context.Background(), secrets.Credentials{
		Generic: map[string]string{name: cloneCredTestToken},
	})
}

// The residue ADR-146 recorded and did not govern: the runner writes the
// run's forge token into `<clone>/.git/iterion-credentials` — a file the
// agent can read and the pod copy keeps — whatever the secret's `sinks:`
// said. A declaration that withholds `file` asks for a credential this
// channel cannot serve, so the run is refused, typed, with a verdict that
// names the secret, its grants and both remedies.
func TestTheCloneCredentialRefusesASecretThatWithholdsTheFileSink(t *testing.T) {
	wf := &ir.Workflow{Secrets: map[string]*ir.Secret{
		"forge_token": {Name: "forge_token", Sinks: []string{secretsink.Prompt, secretsink.Egress}},
	}}

	err := refuseUngrantedCloneCredential(cloneCredCtx("forge_token"), wf, "run-1")
	if err == nil {
		t.Fatal("a forge_token granting [prompt, egress] must refuse the run: the clone credential is a file the workspace holds")
	}
	// Typed, with the code the delivery path reads to ack instead of
	// redelivering a verdict eight times (retrypolicy: deterministic).
	if code := runtimeCodeOf(err); code != runtime.ErrCodeCapabilityUnsupported {
		t.Errorf("runtime code = %q, want %q", code, runtime.ErrCodeCapabilityUnsupported)
	}
	// … and the disposition the delivery path reads from it: deterministic,
	// so the refusal is acked with its cause on the run's timeline instead
	// of being redelivered eight times to reach the same verdict.
	if code := refusalCode(err); !retrypolicy.IsDeterministic(code) {
		t.Errorf("refusal code %q must be deterministic, or every delivery pays a pod and a clone to be told the same thing", code)
	}
	unservable, ok := runtime.IsSecretGrantUnservable(err)
	if !ok {
		t.Fatalf("the refusal must carry runtime.SecretGrantUnservableError as its cause, got %T: %v", err, err)
	}
	if got := unservable.Secret(); got != "forge_token" {
		t.Errorf("refused secret = %q, want forge_token", got)
	}
	if len(unservable.Secrets) != 1 || strings.Join(unservable.Secrets[0].Sinks, ",") != "prompt,egress" {
		t.Fatalf("the refusal must name the declaration's grants, got %+v", unservable.Secrets)
	}
	// The verdict this gate carries is "one sink the run spends is
	// withheld", not the launch gate's "no granted sink can be served":
	// the cause says which, and its Paths pair with THAT verdict — the one
	// channel spending the withheld sink, never an entry for a sink the
	// declaration does not grant.
	refused := unservable.Secrets[0]
	if refused.Withheld != secretsink.File {
		t.Errorf("withheld sink = %q, want %q", refused.Withheld, secretsink.File)
	}
	if len(refused.Paths) != 1 || !strings.Contains(refused.Paths[0], gitCredentialFile) {
		t.Errorf("the cause's paths must hold the one channel spending the withheld sink, got %q", refused.Paths)
	}
	text := err.Error()
	for _, want := range []string{"forge_token", secretsink.File, gitCredentialFile, `withhold the "file" sink`} {
		if !strings.Contains(text, want) {
			t.Errorf("the refusal must say %q: %s", want, text)
		}
	}
	if strings.Contains(text, "none of which this run can serve") {
		t.Errorf("the refusal asserts the launch gate's verdict, which is false here (this run serves prompt and egress): %s", text)
	}
	// Both remedies, never one picked for the operator.
	hint := runtime.RemedyOf(err).Hint
	for _, want := range []string{"`file` sink", "gitlab_token"} {
		if !strings.Contains(hint, want) {
			t.Errorf("the remedy must name %q: %s", want, hint)
		}
	}
	if strings.Contains(text+hint, cloneCredTestToken) {
		t.Error("the refusal carries the token itself")
	}

}

// The gate fails closed: a run that holds a clone credential and reaches
// the gate with no compiled workflow has no grants to read. That shape is a
// caller's programming error, never a legitimate run, and the answer is a
// loud refusal — not a silent pass that writes a credential whose
// declaration nobody read.
func TestTheCloneCredentialIsRefusedWhenThereIsNoWorkflowToReadItsGrantsFrom(t *testing.T) {
	err := refuseUngrantedCloneCredential(cloneCredCtx("forge_token"), nil, "run-1")
	if err == nil {
		t.Fatal("a clone credential with no workflow to read its grants from must refuse the run, not pass it")
	}
	for _, want := range []string{"run-1", "forge_token", "no compiled workflow"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal must name %q: %v", want, err)
		}
	}
	if strings.Contains(err.Error(), cloneCredTestToken) {
		t.Error("the refusal carries the token itself")
	}
}

// The other side of the gate: a declaration that grants `file`, one that
// declares no `sinks:` at all (every sink — today's behaviour), a token
// from a secret the workflow never declared, and a withheld declaration on
// a secret this run did NOT clone with, all proceed to the clone.
func TestTheCloneCredentialIsKeptWhenFileIsGranted(t *testing.T) {
	cases := []struct {
		name    string
		cloned  string
		secrets map[string]*ir.Secret
	}{
		{
			name:   "file granted beside prompt",
			cloned: "forge_token",
			secrets: map[string]*ir.Secret{
				"forge_token": {Name: "forge_token", Sinks: []string{secretsink.Prompt, secretsink.File}},
			},
		},
		{
			name:   "no sinks declared grants every sink",
			cloned: "forge_token",
			secrets: map[string]*ir.Secret{
				"forge_token": {Name: "forge_token"},
			},
		},
		{
			name:    "the token comes from no declaration",
			cloned:  "github_token",
			secrets: map[string]*ir.Secret{},
		},
		{
			name:   "the withheld declaration is another secret",
			cloned: "gitlab_token",
			secrets: map[string]*ir.Secret{
				"gitlab_token": {Name: "gitlab_token", Sinks: []string{secretsink.File}},
				"db_password":  {Name: "db_password", Sinks: []string{secretsink.Command}},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := refuseUngrantedCloneCredential(cloneCredCtx(tc.cloned), &ir.Workflow{Secrets: tc.secrets}, "run-1"); err != nil {
				t.Fatalf("the run must proceed, got %v", err)
			}
		})
	}
	// A run with no forge token at all reads no declaration and refuses
	// nothing (the clone ran credential-less).
	wf := &ir.Workflow{Secrets: map[string]*ir.Secret{
		"forge_token": {Name: "forge_token", Sinks: []string{secretsink.Prompt, secretsink.Egress}},
	}}
	if err := refuseUngrantedCloneCredential(context.Background(), wf, "run-1"); err != nil {
		t.Fatalf("a run carrying no clone credential must proceed, got %v", err)
	}
}

// The precedence IS the behaviour of the one list five readers share: the
// clone's token pick, the two host-side pushes, the rotation's store ref
// and the grant gate all agree on which secret a run clones with only
// because they read the same order. A run carrying several of the names
// clones with the highest one that holds a VALUE — a blank value is no
// credential, so the next name is the clone's secret.
func TestTheCloneCredentialIsTheHighestNameTheRunCarriesAValueFor(t *testing.T) {
	ctxOf := func(values map[string]string) context.Context {
		return secrets.WithCredentials(context.Background(), secrets.Credentials{Generic: values})
	}
	cases := []struct {
		name   string
		values map[string]string
		want   string
	}{
		{"forge_token outranks gitlab_token and github_token", map[string]string{
			"forge_token": "tok-forge", "gitlab_token": "tok-gitlab", "github_token": "tok-github",
		}, "forge_token"},
		{"gitlab_token outranks github_token", map[string]string{
			"gitlab_token": "tok-gitlab", "github_token": "tok-github",
		}, "gitlab_token"},
		{"a blank forge_token is no credential: gitlab_token clones", map[string]string{
			"forge_token": "  ", "gitlab_token": "tok-gitlab", "github_token": "tok-github",
		}, "gitlab_token"},
		{"github_token alone", map[string]string{"github_token": "tok-github"}, "github_token"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			name, token := cloneCredentialToken(ctxOf(tc.values))
			if name != tc.want {
				t.Fatalf("clone credential = %q, want %q", name, tc.want)
			}
			if token != tc.values[tc.want] {
				t.Errorf("clone token = %q, want %s's value %q", token, tc.want, tc.values[tc.want])
			}
		})
	}
	if name, token := cloneCredentialToken(ctxOf(map[string]string{"forge_token": "", "other": "x"})); name != "" || token != "" {
		t.Errorf("a run with only blank or foreign names has no clone credential, got %q/%q", name, token)
	}
}

// The rotation must refresh the secret the clone AUTHENTICATED with. Two
// independent walks over the name precedence — the clone's over the values,
// the refresher's over the store refs — answered differently the moment a
// run carried a snapshot-only forge_token beside a ref'd gitlab_token: the
// rotation then rewrote the clone's credential store every five minutes
// with a token git had never cloned with, from a declaration whose grants
// the gate above never read.
func TestTheCloneCredentialRotationFollowsTheSecretTheCloneUsed(t *testing.T) {
	sealer, err := secrets.NewAESGCMSealerFromBase64("MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY=")
	if err != nil {
		t.Fatal(err)
	}
	r := &Runner{cfg: Config{
		Logger:         iterlog.Nop(),
		Sealer:         sealer,
		GenericSecrets: secrets.NewMemoryGenericSecretStore(),
	}}
	withCreds := func(values, refs map[string]string) context.Context {
		return secrets.WithCredentials(store.WithTenant(context.Background(), "team-1"), secrets.Credentials{
			Generic: values, GenericRefs: refs,
		})
	}

	// The clone used gitlab_token's value: its id is the one to refresh,
	// not forge_token's, which this run holds a ref for but never cloned
	// with.
	ctx := withCreds(
		map[string]string{"gitlab_token": "cloned-with-this"},
		map[string]string{"forge_token": "id-forge", "gitlab_token": "id-gitlab"},
	)
	if got := r.gitCredentialSecretRef(ctx); got != "id-gitlab" {
		t.Errorf("rotation ref = %q, want id-gitlab (the secret the clone authenticated with)", got)
	}

	// The clone's secret is snapshot-only: nothing to refresh. Refreshing
	// the OTHER secret's record would overwrite the clone's credential with
	// a different secret's value.
	ctx = withCreds(
		map[string]string{"forge_token": "cloned-with-this"},
		map[string]string{"gitlab_token": "id-gitlab"},
	)
	if got := r.gitCredentialSecretRef(ctx); got != "" {
		t.Errorf("rotation ref = %q, want \"\" — the clone's own secret carries no store ref", got)
	}

	// No token at all: nothing to rotate.
	if got := r.gitCredentialSecretRef(withCreds(nil, map[string]string{"forge_token": "id-forge"})); got != "" {
		t.Errorf("rotation ref = %q, want \"\" for a run with no clone credential", got)
	}
}

// standInForge stands in for the forge a repo-targeted run clones from, on
// both transports the runner clones over. https reaches a loopback listener
// that counts the connections it is offered and closes each one, so a clone
// that reaches it fails fast, after being counted. ssh reaches a stand-in
// ssh program (GIT_SSH_COMMAND) that records each invocation and serves the test's
// own repository through `git upload-pack`, so an ssh clone succeeds.
type standInForge struct {
	ln     net.Listener
	dials  atomic.Int64
	sshLog string
}

func newStandInForge(t *testing.T) *standInForge {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	tmp := t.TempDir()
	origin := filepath.Join(tmp, "origin.git")
	gitOut(t, tmp, "init", "--quiet", "--bare", "--initial-branch=main", origin)
	seed := filepath.Join(tmp, "seed")
	gitOut(t, tmp, "clone", "--quiet", origin, seed)
	gitOut(t, seed, "config", "user.email", "t@test.invalid")
	gitOut(t, seed, "config", "user.name", "t")
	gitOut(t, seed, "commit", "--quiet", "--allow-empty", "-m", "baseline")
	gitOut(t, seed, "push", "--quiet", "origin", "HEAD:main")

	f := &standInForge{sshLog: filepath.Join(tmp, "ssh.log")}
	ssh := filepath.Join(tmp, "ssh")
	script := fmt.Sprintf("#!/bin/sh\necho \"$*\" >> %s\nexec git upload-pack %s\n", strconv.Quote(f.sshLog), strconv.Quote(origin))
	if err := os.WriteFile(ssh, []byte(script), 0o700); err != nil {
		t.Fatalf("write the stand-in ssh: %v", err)
	}
	// GIT_SSH_COMMAND outranks GIT_SSH, so the stand-in takes that one; the
	// variant keeps git from probing the program with `-G`.
	t.Setenv("GIT_SSH_COMMAND", "'"+ssh+"'")
	t.Setenv("GIT_SSH_VARIANT", "ssh")

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen for the stand-in forge: %v", err)
	}
	f.ln = ln
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			f.dials.Add(1)
			_ = c.Close()
		}
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		<-done
	})
	return f
}

// url is the forge's clone URL over transport ("https" or "ssh").
func (f *standInForge) url(transport string) string {
	if transport == "ssh" {
		return fmt.Sprintf("ssh://git@%s/o/r.git", f.ln.Addr())
	}
	return fmt.Sprintf("https://%s/o/r.git", f.ln.Addr())
}

// attempts is every clone or fetch the forge saw, on either transport.
func (f *standInForge) attempts(t *testing.T) int64 {
	t.Helper()
	n := f.dials.Load()
	b, err := os.ReadFile(f.sshLog)
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("read the stand-in ssh log: %v", err)
	}
	return n + int64(strings.Count(string(b), "\n"))
}

// forgeRunner is a runner that can reach the stand-in forge: the
// private-forge escape hatch lets the transport checks and the clone-guard
// proxy reach a loopback address, and the SSRF pin writes a hosts file this
// test owns. It returns the runner's work dir, which holds every workspace
// the runner prepares.
func forgeRunner(t *testing.T) (*Runner, string) {
	t.Helper()
	t.Setenv("ITERION_RUNNER_CLONE_ALLOW_PRIVATE", "1")
	t.Setenv("ITERION_SANDBOX_DEFAULT", "none")
	hosts := filepath.Join(t.TempDir(), "hosts")
	if err := os.WriteFile(hosts, []byte("127.0.0.1 localhost\n"), 0o600); err != nil {
		t.Fatalf("write the test's hosts file: %v", err)
	}
	prevHosts := runnerHostsFile
	runnerHostsFile = hosts
	t.Cleanup(func() { runnerHostsFile = prevHosts })

	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	workDir := t.TempDir()
	return &Runner{cfg: Config{Store: st, WorkDir: workDir, Logger: iterlog.Nop()}}, workDir
}

// cloneCredWorkflow declares the run's forge_token with sinks (none: every
// sink).
func cloneCredWorkflow(sinks ...string) *ir.Workflow {
	return &ir.Workflow{Secrets: map[string]*ir.Secret{
		"forge_token": {Name: "forge_token", Sinks: sinks},
	}}
}

const cloneGateRunID = "run-clone-gate"

// The gate sits BEFORE the clone, on every transport: a run whose forge
// secret withholds the `file` sink is refused with nothing spent on it — no
// clone attempt, no workspace directory, so the token reaches neither the
// clone URL git persists in .git/config nor the credential store. Driven
// through prepareRepoWorkspace itself; each granted control proves the
// stand-in forge sees a clone when one happens on that transport, so the
// refused case's zero is a measurement, not a blind spot.
func TestAWithheldCloneCredentialIsRefusedBeforeTheClone(t *testing.T) {
	for _, transport := range []string{"https", "ssh"} {
		t.Run(transport+"/withheld: refused with no clone spent", func(t *testing.T) {
			forge := newStandInForge(t)
			r, workDir := forgeRunner(t)
			msg := &queue.RunMessage{RunID: cloneGateRunID, RepoURL: forge.url(transport), RepoSHA: "main"}
			_, _, err := r.prepareRepoWorkspace(cloneCredCtx("forge_token"), cloneCredWorkflow(secretsink.Prompt, secretsink.Egress), msg)
			if n := forge.attempts(t); n != 0 {
				t.Errorf("the refused run reached the forge %d time(s) over %s: the grant gate runs after the clone", n, transport)
			}
			entries, rerr := os.ReadDir(workDir)
			if rerr != nil {
				t.Fatalf("read the work dir: %v", rerr)
			}
			if len(entries) != 0 {
				t.Errorf("the refused run spent a workspace (%s holds %d entr(ies), first %q): the grant gate runs after the workspace is prepared", workDir, len(entries), entries[0].Name())
			}
			if _, ok := runtime.IsSecretGrantUnservable(err); !ok {
				t.Fatalf("prepareRepoWorkspace = %v, want the typed grant refusal (cause runtime.SecretGrantUnservableError) the delivery path reads with errors.As", err)
			}
			if code := runtimeCodeOf(err); code != runtime.ErrCodeCapabilityUnsupported {
				t.Errorf("runtime code = %q, want %q", code, runtime.ErrCodeCapabilityUnsupported)
			}
		})
		t.Run(transport+"/granted: the clone reaches the forge", func(t *testing.T) {
			forge := newStandInForge(t)
			r, workDir := forgeRunner(t)
			msg := &queue.RunMessage{RunID: cloneGateRunID, RepoURL: forge.url(transport), RepoSHA: "main"}
			_, _, err := r.prepareRepoWorkspace(cloneCredCtx("forge_token"), cloneCredWorkflow(secretsink.Prompt, secretsink.File), msg)
			if _, ok := runtime.IsSecretGrantUnservable(err); ok {
				t.Fatalf("a forge_token granting `file` was refused by the grant gate: %v", err)
			}
			if n := forge.attempts(t); n == 0 {
				t.Errorf("the granted run never reached the stand-in forge over %s (err = %v): the refused case's zero proves nothing", transport, err)
			}
			if _, serr := os.Stat(filepath.Join(workDir, "repos")); serr != nil {
				t.Errorf("the granted run prepared no workspace parent under %s: %v", workDir, serr)
			}
		})
	}
}

// The refusal reaches the delivery path typed: executeRun hands the gate's
// verdict on with its cause reachable, so the delivery classifies it as a
// deterministic failure and acks it — a redelivery would reach the same
// bundle and the same verdict, and spend a pod to be told so. Driven
// through executeRun from the run's compiled IR, the workflow the run
// executes.
func TestAWithheldCloneCredentialIsAckedByTheDeliveryNotRedelivered(t *testing.T) {
	forge := newStandInForge(t)
	r, workDir := forgeRunner(t)
	pr := parsertest.Parse("main.bot", "secrets:\n  forge_token:\n    sinks: [prompt, egress]\n\nworkflow main:\n  entry: done\n")
	body, err := dslast.MarshalFile(pr.File)
	if err != nil {
		t.Fatalf("marshal the workflow: %v", err)
	}
	ctx := secrets.WithCredentials(store.WithIdentity(context.Background(), "team-1", "u1"), secrets.Credentials{
		Generic: map[string]string{"forge_token": cloneCredTestToken},
	})
	msg := &queue.RunMessage{RunID: cloneGateRunID, TenantID: "team-1", OwnerID: "u1", WorkflowName: "main", IRCompiled: body,
		RepoURL: forge.url("https"), RepoSHA: "main"}
	execErr := r.executeRun(ctx, msg, nil, nil)
	if _, ok := runtime.IsSecretGrantUnservable(execErr); !ok {
		t.Fatalf("executeRun = %v, want the typed grant refusal: its cause no longer reaches the delivery path", execErr)
	}
	if code := refusalCode(execErr); code != store.FailureCapabilityUnsupported {
		t.Errorf("refusalCode = %q, want %q: the run's timeline would name no code", code, store.FailureCapabilityUnsupported)
	}
	if got := classifyExecResult(execErr, msg.RunID); got.action != actionAck || got.finalStatus != "deterministic_failure" {
		t.Errorf("the delivery classifies the refusal as %s/%v, want deterministic_failure/ack: it would be redelivered", got.finalStatus, got.action)
	}
	if n := forge.attempts(t); n != 0 {
		t.Errorf("the refused run reached the forge %d time(s)", n)
	}
	if _, serr := os.Stat(filepath.Join(workDir, "repos")); !os.IsNotExist(serr) {
		t.Errorf("the refused run prepared a workspace under %s (stat: %v)", workDir, serr)
	}
}

// Only an accepted workspace keeps the run's clone. A tree an earlier
// attempt left on this pod is wiped before the verdict, so a refusal does
// not leave it holding the credential it was cloned with; and a preparation
// that fails after the clone removes the clone it made.
func TestAFailedPreparationLeavesNoCloneBehind(t *testing.T) {
	t.Run("a refused run wipes the tree an earlier attempt left", func(t *testing.T) {
		forge := newStandInForge(t)
		r, workDir := forgeRunner(t)
		stale := filepath.Join(workDir, "repos", cloneGateRunID)
		if err := os.MkdirAll(filepath.Join(stale, ".git"), 0o755); err != nil {
			t.Fatalf("stage the earlier attempt's tree: %v", err)
		}
		if err := os.WriteFile(filepath.Join(stale, ".git", gitCredentialFile), []byte("https://oauth2:"+cloneCredTestToken+"@forge.invalid\n"), 0o600); err != nil {
			t.Fatalf("stage the earlier attempt's credential store: %v", err)
		}
		msg := &queue.RunMessage{RunID: cloneGateRunID, RepoURL: forge.url("https"), RepoSHA: "main"}
		_, _, err := r.prepareRepoWorkspace(cloneCredCtx("forge_token"), cloneCredWorkflow(secretsink.Prompt, secretsink.Egress), msg)
		if _, ok := runtime.IsSecretGrantUnservable(err); !ok {
			t.Fatalf("prepareRepoWorkspace = %v, want the typed grant refusal", err)
		}
		if _, serr := os.Stat(stale); !os.IsNotExist(serr) {
			t.Errorf("the refused run left the earlier attempt's tree %s in place (stat: %v): its credential store keeps the token", stale, serr)
		}
	})
	t.Run("a preparation that fails after the clone removes it", func(t *testing.T) {
		forge := newStandInForge(t)
		r, workDir := forgeRunner(t)
		// The clone and the fetch succeed over ssh; the admitted commit is
		// one the forge never served, so the preparation fails after both.
		msg := &queue.RunMessage{RunID: cloneGateRunID, RepoURL: forge.url("ssh"), RepoSHA: "main",
			RepoSHAExpected: strings.Repeat("de", 20)}
		_, _, err := r.prepareRepoWorkspace(cloneCredCtx("forge_token"), cloneCredWorkflow(), msg)
		if err == nil || !strings.Contains(err.Error(), "admitted for") {
			t.Fatalf("prepareRepoWorkspace = %v, want the fetched-commit refusal that follows the clone", err)
		}
		if n := forge.attempts(t); n < 2 {
			t.Fatalf("the stand-in forge served %d transfer(s), want the clone and the fetch: the failure is not after the clone", n)
		}
		clone := filepath.Join(workDir, "repos", cloneGateRunID)
		if _, serr := os.Stat(clone); !os.IsNotExist(serr) {
			t.Errorf("the failed preparation left its clone %s behind (stat: %v)", clone, serr)
		}
	})
}
