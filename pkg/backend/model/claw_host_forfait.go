package model

import (
	"context"
	"crypto/rand"
	"fmt"
	"os"
	"path"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/internal/envtrust"
	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/sandbox"
	"github.com/SocialGouv/iterion/pkg/secrets"
)

// hostCodexForfaitSandboxRoot is where a sandboxed claw node's copy of the
// HOST's ChatGPT forfait lands in the container, one directory per dispatch.
// Not secrets.CodexSandboxConfigDir: that one holds the run's own forfait.
const hostCodexForfaitSandboxRoot = "/tmp/iterion-codex-host"

// removeHostCodexForfaitScript removes one dispatch's copy: the file
// sandbox.WriteFileExec wrote (and its writers' temp files, auth.json.tmp.<pid>,
// should a write have stopped half-way), then the directory if nothing else is
// left in it. Never recursive: what the node wrote there itself stays, and the
// exit status is the copy's removal alone.
const removeHostCodexForfaitScript = `rm -f -- "$1/auth.json" "$1"/auth.json.tmp.* && { rmdir -- "$1" 2>/dev/null || :; }`

// hostCodexForfait returns what a sandboxed claw node on model is handed of
// the HOST's ChatGPT forfait, when the in-process path spends that forfait —
// or nil.
//
// In process, ITERION_OPENAI_USE_OAUTH=1 orders the disk factory to spend the
// codex home's ChatGPT login over OPENAI_API_KEY, and ResolveWithContext
// reaches that factory when the run holds no OpenAI instrument of its own: no
// key (its default one, or one pinned for the route) and no ChatGPT forfait
// that serves. This is that predicate, on the same inputs: the file the
// factory reads (secrets.CodexAuthJSONPath), parsed the same way. The file is
// read and parsed once, and what is returned is built from that parse: the
// access token and the account (secrets.CodexAccessTokenLogin), all claw
// spends.
//
// Only the operator's explicit order ships the forfait into a sandbox. With
// the switch unset, a host holding no OPENAI_API_KEY spends its forfait in
// process too, but the operator never asked for it to leave the host. And a
// switch or a codex home a project `.env` planted decides what this process
// spends, but does not speak for the operator: the forfait stays home, and
// refused says why.
func hostCodexForfait(ctx context.Context, model string) (payload []byte, refused string) {
	if os.Getenv("ITERION_OPENAI_USE_OAUTH") != "1" || !openAIOAuthAllowed() {
		return nil, ""
	}
	if provider, _, err := ParseModelSpec(model); err != nil || provider != "openai" {
		return nil, ""
	}
	creds, _ := secrets.CredentialsFromContext(ctx)
	if creds.AmbientRefused {
		return nil, "the run refuses the ambient environment — the host's ChatGPT login is the pod's instrument, not this run's"
	}
	if creds.APIKeyForRoute(secrets.ProviderOpenAI) != "" || OpenAIForfaitServes(creds) {
		return nil, ""
	}
	authPath := secrets.CodexAuthJSONPath()
	if authPath == "" {
		return nil, ""
	}
	data, err := os.ReadFile(authPath)
	if err != nil {
		return nil, ""
	}
	view, err := secrets.ParseCodexView(data)
	if err != nil || !view.IsChatGPTMode() {
		return nil, ""
	}
	if envtrust.Planted("ITERION_OPENAI_USE_OAUTH") {
		return nil, "ITERION_OPENAI_USE_OAUTH=1 was set by a project .env, which does not speak for the operator — export it in your shell to send it"
	}
	if secrets.CodexHomePlanted() {
		return nil, "the codex home it would come from was chosen by a project .env, which does not speak for the operator"
	}
	payload, err = secrets.CodexAccessTokenLogin(view)
	if err != nil {
		return nil, fmt.Sprintf("its login could not be copied: %v", err)
	}
	return payload, ""
}

// deliverHostCodexForfait gives a sandboxed claw node the host's ChatGPT
// forfait when the in-process path spends it (hostCodexForfait): it writes the
// login's access token and account into a directory of the container's own,
// through the exec seam the runner's mid-run forfait refresh uses, and returns
// the env that points the node's runner at it, forcing the OAuth path over the
// OPENAI_API_KEY that crosses beside. release removes the copy once the runner
// is done.
//
// Delivered at the dispatch rather than with the sandbox, so the decision and
// what is delivered come from one read of the operator's live codex home: no
// dispatch finds the switch saying one thing and the container holding
// another, and a node gets the host's token as it is when the node starts.
//
// Every command the runner starts, its bash tool's included, inherits that env
// and can read the copy: an access token that expires, with no refresh token,
// id token or API key beside it, so the copy renews nothing. It does not keep
// the login out of the container: under host_state's default bind the whole
// ~/.codex is there already, and host_state: none is what keeps it out.
//
// One directory per dispatch: nodes of a run dispatch concurrently into the
// same container, and a shared file would be rewritten under a runner reading
// it.
//
// A task whose commands execute on the host (delegate.Task.Hostless: the noop
// driver, however wrapped) gets nothing: its runner inherits the host's own
// environment and codex home.
func deliverHostCodexForfait(ctx context.Context, task delegate.Task, logger *iterlog.Logger) (env map[string]string, release func(), err error) {
	release = func() {}
	if task.Hostless() {
		return nil, release, nil
	}
	payload, refused := hostCodexForfait(ctx, task.Model)
	if refused != "" && logger != nil {
		logger.Warn("[%s#%d/claw] the host's ChatGPT forfait is not written into the sandbox: %s", task.NodeID, task.Iteration, refused)
	}
	if payload == nil {
		return nil, release, nil
	}
	run := task.Sandbox
	dir := path.Join(hostCodexForfaitSandboxRoot, rand.Text())
	if err := sandbox.WriteFileExec(ctx, run, path.Join(dir, "auth.json"), payload); err != nil {
		removeHostCodexForfaitCopy(ctx, run, dir, logger)
		return nil, release, fmt.Errorf("claw backend: deliver the host's ChatGPT forfait into the sandbox (ITERION_OPENAI_USE_OAUTH=1): %w", err)
	}
	release = func() { removeHostCodexForfaitCopy(ctx, run, dir, logger) }
	return map[string]string{
		"CODEX_HOME":               dir,
		"ITERION_OPENAI_USE_OAUTH": "1",
	}, release, nil
}

// removeHostCodexForfaitCopy removes one dispatch's copy, on a context of its
// own: the node's may already be cancelled. A copy left behind dies with the
// container; the failure is logged, never the node's.
func removeHostCodexForfaitCopy(ctx context.Context, run sandbox.Run, dir string, logger *iterlog.Logger) {
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	res, err := run.Exec(cctx, []string{"sh", "-c", removeHostCodexForfaitScript, "sh", dir}, sandbox.ExecOpts{})
	if err == nil && res.ExitCode != 0 {
		err = fmt.Errorf("exited %d: %s", res.ExitCode, strings.TrimSpace(string(res.Stderr)))
	}
	if err != nil && logger != nil {
		logger.Warn("claw backend: remove the host's ChatGPT forfait copy %s from the sandbox: %v", dir, err)
	}
}
