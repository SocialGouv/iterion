package runner

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/secretsink"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/retrypolicy"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
)

const (
	cloneCredTestToken = "ghs_live_installation_token"
	cloneCredTestRepo  = "https://github.com/iterion-sandbox/appy-demo.git"
)

// clonedWorkspace reproduces prepareRepoWorkspace's own shape at the moment
// the grant gate runs: a clone whose origin carried the token, with the
// credential store installed from it.
func clonedWorkspace(t *testing.T, r *Runner) string {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	if err := r.runGit(ctx, dir, "", "init", "-q"); err != nil {
		t.Fatalf("git init: %v", err)
	}
	if err := r.runGit(ctx, dir, "", "remote", "add", "origin", injectGitToken(cloneCredTestRepo, cloneCredTestToken)); err != nil {
		t.Fatalf("add tokenized remote: %v", err)
	}
	if err := r.installGitCredentialStore(ctx, dir, cloneCredTestRepo, cloneCredTestToken); err != nil {
		t.Fatalf("install credential store: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".git", gitCredentialFile)); err != nil {
		t.Fatalf("fixture has no credential store to judge: %v", err)
	}
	return dir
}

func cloneCredCtx(name string) context.Context {
	return secrets.WithCredentials(context.Background(), secrets.Credentials{
		Generic: map[string]string{name: cloneCredTestToken},
	})
}

// The residue ADR-146 recorded and did not govern: the runner writes the
// run's forge token into `<clone>/.git/iterion-credentials` — a file the
// agent can read and the pod copy keeps — whatever the secret's `sinks:`
// said. A declaration that withholds `file` asks for a credential this
// channel cannot serve, so the run is refused before a node runs, typed,
// and the credential is taken back from BOTH locations.
func TestTheCloneCredentialRefusesASecretThatWithholdsTheFileSink(t *testing.T) {
	r := &Runner{cfg: Config{Logger: iterlog.Nop()}}
	dir := clonedWorkspace(t, r)
	// The pod's copy of the clone exists on a copy-based driver: the
	// teardown must reach it too, never half the credential.
	fake := newWriteThroughRun()
	r.registerSandboxRun("run-1", fake)
	defer r.unregisterSandboxRun("run-1")

	// A store write torn between its two steps leaves the credential line
	// in the atomic-replace sibling: the teardown owes that location too.
	tornSibling := filepath.Join(dir, ".git", gitCredentialFile+".tmp")
	line, err := renderGitCredentialLine(cloneCredTestRepo, cloneCredTestToken)
	if err != nil {
		t.Fatalf("render the credential line: %v", err)
	}
	if err := os.WriteFile(tornSibling, []byte(line), 0o600); err != nil {
		t.Fatalf("plant the torn-write sibling: %v", err)
	}

	wf := &ir.Workflow{Secrets: map[string]*ir.Secret{
		"forge_token": {Name: "forge_token", Sinks: []string{secretsink.Prompt, secretsink.Egress}},
	}}

	err = r.refuseUngrantedCloneCredential(cloneCredCtx("forge_token"), wf, "run-1", dir)
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

	// The clone keeps no credential: a refused run's workspace survives in
	// the pod's workdir (executeRun arms its removal only once the clone is
	// accepted), so the file would hold a live token for the pod's life.
	if _, err := os.Stat(filepath.Join(dir, ".git", gitCredentialFile)); !os.IsNotExist(err) {
		t.Errorf("the credential store survived the refusal (stat err = %v)", err)
	}
	if _, err := os.Stat(tornSibling); !os.IsNotExist(err) {
		t.Errorf("the torn-write sibling survived the refusal (stat err = %v)", err)
	}
	// … and nowhere else in the clone either: .git/config (the tokenized
	// origin the install detached), and any location a future writer adds.
	if holders := filesHoldingToken(t, dir, cloneCredTestToken); len(holders) != 0 {
		t.Errorf("files under the refused clone still hold the token: %v", holders)
	}
	// … and neither does the pod's copy: the seam writes, so the erase is
	// an empty store, which resolves no credential.
	copied, seen := fake.workspaceFiles[".git/"+gitCredentialFile]
	if !seen {
		t.Fatalf("the sandbox workspace copy was never touched: %+v", fake.workspaceFiles)
	}
	if len(copied) != 0 {
		t.Errorf("the pod's credential copy = %q, want an empty store", copied)
	}
}

// filesHoldingToken walks the clone and names every file whose bytes
// contain token — the assertion that outlives the list of locations the
// remover knows today.
func filesHoldingToken(t *testing.T, root, token string) []string {
	t.Helper()
	var holders []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		if strings.Contains(string(b), token) {
			rel, _ := filepath.Rel(root, path)
			holders = append(holders, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk the clone: %v", err)
	}
	return holders
}

// The gate fails closed: a run that holds a clone credential and reaches
// the gate with no compiled workflow has no grants to read. That shape is a
// caller's programming error, never a legitimate run, and the answer is a
// loud refusal with the credential taken back — not a silent pass that
// keeps a credential whose declaration nobody read.
func TestTheCloneCredentialIsRefusedWhenThereIsNoWorkflowToReadItsGrantsFrom(t *testing.T) {
	r := &Runner{cfg: Config{Logger: iterlog.Nop()}}
	dir := clonedWorkspace(t, r)

	err := r.refuseUngrantedCloneCredential(cloneCredCtx("forge_token"), nil, "run-1", dir)
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
	if holders := filesHoldingToken(t, dir, cloneCredTestToken); len(holders) != 0 {
		t.Errorf("files under the refused clone still hold the token: %v", holders)
	}
}

// The other side of the gate: a declaration that grants `file`, one that
// declares no `sinks:` at all (every sink — today's behaviour), a token
// from a secret the workflow never declared, and a withheld declaration on
// a secret this run did NOT clone with, all proceed with the credential
// exactly where the runner put it.
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
			r := &Runner{cfg: Config{Logger: iterlog.Nop()}}
			dir := clonedWorkspace(t, r)
			fake := newWriteThroughRun()
			r.registerSandboxRun("run-1", fake)
			defer r.unregisterSandboxRun("run-1")

			if err := r.refuseUngrantedCloneCredential(cloneCredCtx(tc.cloned), &ir.Workflow{Secrets: tc.secrets}, "run-1", dir); err != nil {
				t.Fatalf("the run must proceed, got %v", err)
			}
			got, rerr := os.ReadFile(filepath.Join(dir, ".git", gitCredentialFile))
			if rerr != nil {
				t.Fatalf("the credential store must survive: %v", rerr)
			}
			if want := "https://oauth2:" + cloneCredTestToken + "@github.com\n"; string(got) != want {
				t.Errorf("credential line = %q, want %q", got, want)
			}
			if len(fake.workspaceFiles) != 0 {
				t.Errorf("the pod's copy was rewritten on an accepted run: %+v", fake.workspaceFiles)
			}
		})
	}
	// A run with no forge token at all reads no declaration and refuses
	// nothing (the clone ran credential-less).
	r := &Runner{cfg: Config{Logger: iterlog.Nop()}}
	wf := &ir.Workflow{Secrets: map[string]*ir.Secret{
		"forge_token": {Name: "forge_token", Sinks: []string{secretsink.Prompt, secretsink.Egress}},
	}}
	if err := r.refuseUngrantedCloneCredential(context.Background(), wf, "run-1", t.TempDir()); err != nil {
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
