// The clone credential: which secret a repo-targeted cloud run clones and
// pushes with, and what its `sinks:` declaration grants (ADR-146).
//
// The runner keeps that token in the clone's own credential store so the
// agent's late `git push` re-reads a live one — a FILE the workspace holds,
// so the channel spends the `file` sink and nothing else. A declaration that
// withholds `file` asks for a credential this channel cannot serve: the run
// is refused, typed, before the clone.
//
// Detached from the package clause on purpose: pkg/runner's own doc lives in
// loop.go, and the generated package map reads the alphabetically FIRST
// file's package comment — a second one here would describe the whole runner
// as its clone credential (docs/references/map-packages.md).

package runner

import (
	"context"
	"fmt"
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
// credential comes from a declared secret that withholds the `file` sink.
//
// It runs BEFORE the clone (prepareRepoWorkspace, ahead of the transport
// checks and the first git subprocess): the grants are the compiled
// workflow's and the token is the sealed bundle's, both in hand before any
// workspace exists, so a refused run spends no clone, no workspace
// directory and no forge connection, and its token is never written to
// disk — neither into the clone URL git persists in .git/config nor into
// the credential store. There is nothing to take back.
//
// A secret that declares no `sinks:` grants every sink, `file` included,
// and is never refused here (secretsink.Grants reads the empty list that
// way); nor is one that grants `file`; nor a token that comes from no
// declaration, where there are no grants to read.
//
// The gate fails CLOSED: a run that holds a clone credential and reaches it
// with no compiled workflow has no grants to read, so it is refused rather
// than served on a declaration nobody saw. That shape is a programming error
// of the caller (prepareRepoWorkspace takes the run's workflow), never a
// legitimate run.
//
// The run DOCUMENT's own verdict (status, code, remedy) is the shared
// recorder of every pre-engine refusal (iterion#2925): here the delivery
// path reads the code, acks the refusal with its cause on the run's
// timeline and never redelivers it.
func refuseUngrantedCloneCredential(ctx context.Context, wf *ir.Workflow, runID string) error {
	name, _ := cloneCredentialToken(ctx)
	if name == "" {
		return nil
	}
	if wf == nil {
		return fmt.Errorf("runner: run %s: the run clones with %q and carries no compiled workflow to read its grants from — refusing rather than writing a credential whose declaration nobody read", runID, name)
	}
	s := wf.Secrets[name]
	if s == nil || s.Grants(secretsink.File) {
		return nil
	}
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
