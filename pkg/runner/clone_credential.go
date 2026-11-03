// The clone credential: which secret a repo-targeted cloud run clones and
// pushes with, and what its `sinks:` declaration grants (ADR-146).
//
// The runner keeps that token in the clone's own credential store so the
// agent's late `git push` re-reads a live one — a FILE the workspace holds,
// so the channel spends the `file` sink and nothing else. A declaration that
// withholds `file` asks for a credential this channel cannot serve: the run
// is refused, typed, and the credential it was given is taken back.
//
// Detached from the package clause on purpose: pkg/runner's own doc lives in
// loop.go, and the generated package map reads the alphabetically FIRST
// file's package comment — a second one here would describe the whole runner
// as its clone credential (docs/references/map-packages.md).

package runner

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/secretsink"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/secrets"
)

// cloneCredentialNames is the name precedence a run's forge token is read
// by, highest first. One list for every reader — the clone's token pick,
// the host-side pushes, the mid-run rotation's store ref and the grant gate
// below — so the secret whose grants are judged, the secret the rotation
// refreshes and the secret git authenticates with are the SAME secret: two
// independent walks over these names answer differently as soon as a run
// carries two of them.
var cloneCredentialNames = []string{"forge_token", "gitlab_token", "github_token"}

// cloneCredentialToken returns the name of the declared secret this run's
// git operations authenticate with and its token, both empty when the run
// carries none. The VALUE decides, not the store ref: a run can carry a
// snapshot-only forge_token beside a ref'd gitlab_token, and it is the
// value git spends.
func cloneCredentialToken(ctx context.Context) (name, token string) {
	creds, ok := secrets.CredentialsFromContext(ctx)
	if !ok {
		return "", ""
	}
	for _, candidate := range cloneCredentialNames {
		if tok := creds.GenericSecret(candidate); strings.TrimSpace(tok) != "" {
			return candidate, tok
		}
	}
	return "", ""
}

// refuseUngrantedCloneCredential refuses a repo-targeted run whose clone
// credential comes from a declared secret that withholds the `file` sink,
// and erases the credential the run would have held.
//
// It fires where the credential has just been materialised rather than
// before the clone: the clone itself spends the token on one fetch and
// keeps nothing, while what the declaration withholds is the credential
// FILE the workspace keeps for the rest of the run. So the verdict sits
// beside the write it is about, and takes it back — both locations, through
// removeCloneCredential — before the run returns.
//
// A secret that declares no `sinks:` grants every sink, `file` included,
// and is never refused here (secretsink.Grants reads the empty list that
// way); nor is one that grants `file`; nor a token that comes from no
// declaration, where there are no grants to read.
//
// The gate fails CLOSED: a run that holds a clone credential and reaches it
// with no compiled workflow has no grants to read, so it is refused — and
// the credential taken back — rather than served on a declaration nobody
// saw. That shape is a programming error of the caller (prepareRepoWorkspace
// takes the run's workflow), never a legitimate run.
//
// The run DOCUMENT's own verdict (status, code, remedy) is the shared
// recorder of every pre-engine refusal (iterion#2925): here the delivery
// path reads the code, acks the refusal with its cause on the run's
// timeline and never redelivers it.
func (r *Runner) refuseUngrantedCloneCredential(ctx context.Context, wf *ir.Workflow, runID, dir string) error {
	name, _ := cloneCredentialToken(ctx)
	if name == "" {
		return nil
	}
	if wf == nil {
		r.removeCloneCredential(runID, dir)
		return fmt.Errorf("runner: run %s: the clone credential %q was materialised with no compiled workflow to read its grants from — refusing rather than keeping a credential whose declaration nobody read", runID, name)
	}
	s := wf.Secrets[name]
	if s == nil || s.Grants(secretsink.File) {
		return nil
	}
	r.removeCloneCredential(runID, dir)
	granted := strings.Join(s.Sinks, ", ")
	return &runtime.RuntimeError{
		Code: runtime.ErrCodeCapabilityUnsupported,
		Message: fmt.Sprintf("this repo-targeted run clones with secret %q, whose grants [%s] withhold the %q sink the clone credential needs",
			name, granted, secretsink.File),
		Hint: fmt.Sprintf("grant %q the `file` sink (`sinks: [%s, %s]`) so the run may keep its clone credential in the workspace, or keep the value out of the workspace by declaring it under a name outside the clone precedence (%s) and leaving the run's forge token to a declaration that grants `file` — docs/secrets.md, « sinks »",
			name, granted, secretsink.File, strings.Join(cloneCredentialNames, ", ")),
		Cause: &runtime.SecretGrantUnservableError{Secrets: []runtime.UnservableSecret{{
			Secret:   name,
			Sinks:    append([]string(nil), s.Sinks...),
			Withheld: secretsink.File,
			Paths: []string{"the cloud runner writes this run's forge token into " +
				filepath.Join(".git", gitCredentialFile) + " of the clone, and rewrites it on every rotation, so the agent's late `git push` re-reads a live token — the clone channel spends no other sink"},
		}}},
	}
}

// removeCloneCredential erases the clone token from both locations a run
// holds it in: the clone's own credential store, and the copy a copy-based
// sandbox keeps in step with it when one is LIVE. One remover for the pair
// the two writers (writeGitCredentials, writeThroughSandboxGitCredential)
// keep in step, so no caller can erase half of the credential.
//
// At the grant gate the second half finds nothing to erase: the gate runs
// in prepareRepoWorkspace, before the run's sandbox starts, so no copy
// exists yet and the write-through answers "no sandbox". The store is the
// location the refusal takes back; the pair is covered so a later caller
// with a live sandbox is covered too.
//
// A refused run's clone dir SURVIVES — executeRun arms its removal only
// once the workspace is accepted — so the file would otherwise sit in the
// pod's workdir with a live token for the pod's life.
//
// Best-effort and loud: the run is refused either way, and a location that
// kept the token is named in the log rather than passed over.
func (r *Runner) removeCloneCredential(runID, dir string) {
	path := filepath.Join(dir, ".git", gitCredentialFile)
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		r.cfg.Logger.Warn("runner: run %s: the refused clone credential could not be removed from %s: %v", runID, path, err)
	}
	// writeGitCredentials replaces atomically through this sibling; a write
	// torn between the two steps leaves the token there.
	if err := os.Remove(path + ".tmp"); err != nil && !os.IsNotExist(err) {
		r.cfg.Logger.Warn("runner: run %s: the refused clone credential could not be removed from %s: %v", runID, path+".tmp", err)
	}
	if wrote, err := r.refreshSandboxGitCredential(runID, nil); err != nil {
		r.cfg.Logger.Warn("runner: run %s: the refused clone credential could not be erased from the sandbox workspace copy: %v", runID, err)
	} else if wrote {
		r.cfg.Logger.Info("runner: run %s: the refused clone credential was erased from the sandbox workspace copy", runID)
	}
}
