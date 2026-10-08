package devcontainer

import (
	"slices"
	"sort"
	"strings"

	"github.com/SocialGouv/iterion/pkg/sandbox"
)

// ToSandboxSpec converts a parsed devcontainer.json into the
// driver-agnostic [sandbox.Spec] iterion's runtime consumes, and
// returns the names of the env keys it refused to copy — see
// [DeniedEnvKey].
//
// Field mapping:
//
//	image           -> Spec.Image
//	build           -> Spec.Build (Phase 2 deferred at driver layer)
//	containerEnv    -> Spec.Env (deny-class keys removed, issue #2303)
//	remoteEnv       -> merged into Spec.Env (containerEnv wins on collision)
//	mounts          -> Spec.Mounts
//	remoteUser      -> Spec.User (preferred)
//	containerUser   -> Spec.User (fallback when remoteUser empty)
//	workspaceFolder -> Spec.WorkspaceFolder
//	postCreateCommand -> Spec.PostCreate (joined to a shell snippet)
//
// The returned Spec has Mode=ModeAuto so callers know it came from
// devcontainer.json reading (vs ModeInline which signals an in-DSL
// block). Network is left nil — iterion handles network policy via
// its own DSL fields, not via devcontainer (which has no equivalent).
func ToSandboxSpec(f *File) (sandbox.Spec, []string) {
	if f == nil {
		return sandbox.Spec{}, nil
	}
	spec := sandbox.Spec{
		Mode:            sandbox.ModeAuto,
		Image:           f.Image,
		WorkspaceFolder: f.WorkspaceFolder,
	}
	if f.Build != nil {
		spec.Build = &sandbox.Build{
			Dockerfile: f.Build.Dockerfile,
			Context:    f.Build.Context,
			Args:       f.Build.Args,
		}
	}

	// Merge remoteEnv first, then containerEnv (containerEnv wins on
	// collision because it is the spec-canonical field). Both maps may
	// be nil — append to a fresh map and the zero-len cases are no-ops.
	// Nothing denied or malformed leaves this function: it is the one
	// seam where repo-authored env becomes spec.Env, so it is the one
	// place the removal has to live. A malformed name would fail the
	// docker driver's --env guard and kill the run outright, while the
	// kubernetes driver drops it silently — refusing here makes both
	// drivers agree, with the removal named in the event.
	if len(f.RemoteEnv) > 0 || len(f.ContainerEnv) > 0 {
		spec.Env = make(map[string]string, len(f.RemoteEnv)+len(f.ContainerEnv))
		for k, v := range f.RemoteEnv {
			if usableRepoEnvEntry(k, v) {
				spec.Env[k] = v
			}
		}
		for k, v := range f.ContainerEnv {
			if usableRepoEnvEntry(k, v) {
				spec.Env[k] = v
			}
		}
	}

	if len(f.Mounts) > 0 {
		spec.Mounts = append([]string(nil), f.Mounts...)
	}

	switch {
	case f.RemoteUser != "":
		spec.User = f.RemoteUser
	case f.ContainerUser != "":
		spec.User = f.ContainerUser
	}

	if !f.PostCreateCommand.Empty() {
		spec.PostCreate = strings.TrimSpace(f.PostCreateCommand.AsShell())
	}

	// Report what was removed — once per name even when both env maps
	// carried it — sorted so event payloads and test expectations are
	// stable regardless of map iteration order.
	denied := make([]string, 0, len(f.RemoteEnv)+len(f.ContainerEnv))
	for _, env := range []map[string]string{f.RemoteEnv, f.ContainerEnv} {
		for k := range env {
			if !usableRepoEnvEntry(k, env[k]) {
				denied = append(denied, k)
			}
		}
	}
	sort.Strings(denied)
	denied = slices.Compact(denied)
	return spec, denied
}

// usableRepoEnvEntry reports whether a repo-authored env entry may
// enter spec.Env: outside the deny class ([DeniedEnvKey]), and
// well-formed enough that every driver would have accepted it — the
// same characters the docker --env guard refuses, plus an empty name.
func usableRepoEnvEntry(name, value string) bool {
	if name == "" || strings.ContainsAny(name, "=\n\r\x00") {
		return false
	}
	if strings.ContainsAny(value, "\n\r\x00") {
		return false
	}
	return !DeniedEnvKey(name)
}

// DeniedEnvKey reports whether an env key from a target repository's
// devcontainer.json is in the deny class — names a reviewed repo must
// not plant into the sandbox environment (#2303). The container's env
// is what the LLM backends forward verbatim, so a planted
// *_BASE_URL (and its sibling spellings OPENAI_API_BASE /
// AZURE_OPENAI_ENDPOINT) re-routes every model call to the planter's
// collector, a planted *_API_KEY/*_TOKEN/*_SECRET leaks whatever the
// backends inject, *_PROXY/NO_PROXY re-routes everything else, and
// the exact names execute the planter's code or redirect the run's
// own tooling before any prompt is read: LD_PRELOAD, NODE_OPTIONS,
// PATH, BASH_ENV, ENV (shell startup files), GOFLAGS (-toolexec runs
// at every go build), GIT_CONFIG_COUNT / GIT_CONFIG_GLOBAL /
// GIT_CONFIG_SYSTEM and GIT_ASKPASS (a planted gitconfig rewrites
// every git URL, an askpass helper intercepts git credentials — the
// GIT_CONFIG_KEY_n/GIT_CONFIG_VALUE_n pair variables are not denied
// because the runtime's own GIT_CONFIG_COUNT seeding overwrites
// whatever a planted pair activates), CLAUDE_CONFIG_DIR / CODEX_HOME
// (a planted settings.json speaks with the operator's ambient
// credentials), ANTHROPIC_CUSTOM_HEADERS (the codebase itself
// neutralises it on funded routes because a foreign credential rides
// them).
//
// The suffixes match case-insensitively (http_proxy routes as much as
// HTTP_PROXY); the exact names are the load-bearing uppercase forms.
// A key outside the class still configures the repo's own sandbox —
// the denylist removes trust, it does not review content.
func DeniedEnvKey(name string) bool {
	switch name {
	case "LD_PRELOAD", "NODE_OPTIONS", "PATH",
		"BASH_ENV", "ENV", "GIT_CONFIG_COUNT",
		"GIT_CONFIG_GLOBAL", "GIT_CONFIG_SYSTEM", "GIT_ASKPASS", "GOFLAGS",
		"CLAUDE_CONFIG_DIR", "CODEX_HOME",
		"ANTHROPIC_CUSTOM_HEADERS", "AZURE_OPENAI_ENDPOINT", "OPENAI_API_BASE":
		return true
	}
	upper := strings.ToUpper(name)
	return strings.HasSuffix(upper, "_BASE_URL") ||
		strings.HasSuffix(upper, "_API_KEY") ||
		strings.HasSuffix(upper, "_TOKEN") ||
		strings.HasSuffix(upper, "_SECRET") ||
		strings.HasSuffix(upper, "_PROXY")
}
