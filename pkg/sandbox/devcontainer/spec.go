package devcontainer

import (
	"maps"
	"slices"
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
	// collision because it is the spec-canonical field), then screen the
	// winners: a refused containerEnv value removes the name rather than
	// letting the remoteEnv value it overrode ride in its place. Both
	// maps may be nil — the zero-len cases are no-ops. Nothing denied or
	// malformed leaves this function: it is the one seam where
	// repo-authored env becomes spec.Env, so it is the one place the
	// removal has to live. A malformed name would fail the docker
	// driver's --env guard and kill the run outright, while the
	// kubernetes driver drops it silently — refusing here makes both
	// drivers agree, with the removal named in the event.
	repoEnv := make(map[string]string, len(f.RemoteEnv)+len(f.ContainerEnv))
	maps.Copy(repoEnv, f.RemoteEnv)
	maps.Copy(repoEnv, f.ContainerEnv)
	if len(repoEnv) > 0 {
		spec.Env = make(map[string]string, len(repoEnv))
		for k, v := range repoEnv {
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

	// Report what spec.Env goes without — every name the repository
	// wrote that the final map lacks, so the report never names a
	// repository value that rides in the container — sorted so event
	// payloads and test expectations are stable regardless of map
	// iteration order.
	denied := make([]string, 0, len(repoEnv))
	for k := range repoEnv {
		if _, kept := spec.Env[k]; !kept {
			denied = append(denied, k)
		}
	}
	slices.Sort(denied)
	return spec, denied
}

// usableRepoEnvEntry reports whether a repo-authored env entry may
// enter spec.Env: outside the deny class ([DeniedEnvKey]), and
// well-formed enough that every driver would have accepted it — the
// same characters the docker --env guard refuses, plus an empty name.
func usableRepoEnvEntry(name, value string) bool {
	if !UsableEnvName(name) {
		return false
	}
	if strings.ContainsAny(value, "\n\r\x00") {
		return false
	}
	return !DeniedEnvKey(name)
}

// UsableEnvName reports whether name is one a driver could pass to a
// container and a process inside it could resolve: non-empty, and free of
// the characters that make it un-passable (`=` splits the pair, a newline
// or a CR would inject a second `--env` flag, a NUL truncates it).
//
// It is the one predicate both readers of repository-authored env share:
// the devcontainer seam, which refuses such an entry outright, and the
// image env screen, which cannot neutralise a name no driver could name
// and no process could read (#2317).
func UsableEnvName(name string) bool {
	return name != "" && !strings.ContainsAny(name, "=\n\r\x00")
}

// DeniedEnvKey reports whether an env key from a target repository's
// devcontainer.json is in the deny class — names a reviewed repo must
// not plant into the sandbox environment (#2303). The container's env is
// the env of every process iterion runs there for the operator — the
// in-container claw runner, the agent CLIs and the commands they spawn,
// git — and a variable iterion does not set for such a process keeps the
// planted value: the runner is forwarded a host variable only when the
// host has one. So:
//
//   - a planted *_BASE_URL (and its sibling spellings OPENAI_API_BASE,
//     ANTHROPIC_UNIX_SOCKET, AZURE_OPENAI_ENDPOINT) re-routes every model
//     call to the planter's collector, *_API_KEY / *_TOKEN / *_SECRET
//     leak whatever the backends inject, *_PROXY / NO_PROXY re-routes
//     everything else;
//   - a planted cloud-provider switch (CLAUDE_CODE_USE_BEDROCK, _VERTEX,
//     _FOUNDRY, …), which the claude CLI reads before any token, with the
//     planter's AWS, Google or Azure credentials sends every prompt the
//     CLI sends to the planter's cloud account; the planter's credentials
//     alone do it under a switch, or a bedrock/vertex model, the operator
//     chose;
//   - the agents' and the runner's configuration namespaces and config
//     homes are the operator's settings: a planted settings file or
//     session script speaks with the operator's ambient credentials, and
//     a planted telemetry destination receives what the CLI records;
//   - git's configuration is the operator's too: a planted variable
//     rewrites its URLs, runs its transport or credential helper, points
//     it at other objects or another repository, or names its commits;
//   - the home those configurations are READ from is the same channel under
//     another name: HOME and the XDG base directories under it. A planted
//     HOME or XDG_CONFIG_HOME hands git the repository's own gitconfig —
//     `insteadOf` rewrites every URL, and `credential.helper` is RUN and
//     handed the operator's token (reproduced on #2316, Jo 2026-10-12) —
//     plus the agents' settings and hooks when no forfait mount pins their
//     config dir, and $HOME/.npmrc; XDG_DATA_HOME moves the credential
//     store an agent logs into. The runtime's HOME pin is not the answer:
//     it yields to a value already set, it takes HOME alone, and it exists
//     only where host_state binds a home at all — so the refusal lives
//     here, with the gitconfig variables it re-enters by (#2318);
//   - the rest execute the planter's code or redirect the run's own
//     tooling before any prompt is read: the dynamic loader and the
//     modules glibc and OpenSSL load, the node and bun runtimes, the shell
//     the claude CLI picks and bash's, sh's and zsh's startup variables,
//     its imported functions and its config-from-the-environment, PATH,
//     `go build -toolexec`, tar's own options, and the TLS trust of the
//     connections that carry credentials.
//
// All three forms match case-insensitively. That is a MARGIN, not a claim
// about a consumer: an environment name is case-sensitive to the program
// that reads it — measured, a lower-case `bash_env` is sourced by no bash —
// and the exceptions are deliberate on the consumer's side, curl picking up
// a lower-case `http_proxy` (measured: `Uses proxy env variable http_proxy`)
// and the claude CLI reading both spellings of the gcloud project. Refusing
// every spelling costs a repository nothing it could not write in the
// workflow's own `sandbox.env:`, and it is one rule rather than a twin per
// row — so no entry needs a lower-case twin of its own.
//
// Where the class STOPS, and why — the claude CLI carries Anthropic's own
// 192-name list of environment that executes code (its `f$` set), which is
// the reference table this class was swept against. The class takes the
// names that steer a process ITERION starts for the operator; it
// deliberately leaves out:
//
//   - the toolchain variables NO process iterion starts reads — compilers
//     and their flags, build systems, language runtimes, version managers
//     (CC, CFLAGS, MAVEN_OPTS, RUSTC*, PYTHON*, GEM_HOME, JAVA_HOME,
//     PYENV_ROOT, …). PYTHON* is the measured one, and it is measured
//     because the family DOES carry code channels — a `sitecustomize.py`
//     on PYTHONPATH, a stdlib under PYTHONHOME, a `usercustomize.py` under
//     PYTHONUSERBASE all run at interpreter startup: iterion's own python
//     tool node runs `python3 -I`, whose isolated mode ignores the whole
//     family (measured both ways on a built image), so what is left is the
//     bare `python3` of an operator-authored command — the repository's own
//     build. They configure builds of the repository's own code,
//     which already runs in that container (its postCreateCommand, its
//     tests), and a devcontainer that sets them is doing its job. That
//     question, not the family, is the membership test: GOFLAGS, PATH and
//     NIX_* are toolchain variables too, and they are denied because
//     iterion's own nodes and provisioning run under them;
//   - what needs a terminal nobody has there (PAGER, MANPAGER, LESSOPEN,
//     BROWSER, PROMPT_COMMAND);
//   - the *_HOME of a toolchain (JAVA_HOME, ANT_HOME, GEM_HOME, PYENV_ROOT):
//     HOME is an exact name here, never a suffix, so a repository still
//     points its own build at its own toolchain.
//
// A key outside the class still configures the repo's own sandbox — the
// denylist removes trust, it does not review content.
func DeniedEnvKey(name string) bool {
	return screenEnvName(name) != notDenied
}

// ScreenImageEnvName classifies a name an IMAGE bakes in: denied says it is
// in the class above, neutralize says the runtime overrides it with an empty
// value rather than only reporting it (#2317).
//
// It is the one reader of the class's disposition flag, so the screen of an
// image reads the SAME inventory the devcontainer screen reads — a second
// list of "the credential ones" would be a copy, and a copy drifts.
//
// PRECEDENCE, when a name is caught by more than one guard: the exact names
// first, then the suffixes, then the namespaces, and within the namespaces
// the FIRST entry that matches. The order is the rule, not an accident, and
// it tilts toward neutralising — `ITERION_SOMETHING_TOKEN` is caught by the
// reported-only `ITERION_` namespace and by the neutralised `_TOKEN`
// suffix, and the suffix answers. The last clause is what lets a NARROWER
// namespace refine a family: `GIT_CONFIG_KEY_` is listed before `GIT_` and
// answers for its own members, so the family keeps its answer for the
// rest.
func ScreenImageEnvName(name string) (denied, neutralize bool) {
	switch screenEnvName(name) {
	case neutralizeInImage:
		return true, true
	case reportedOnly:
		return true, false
	default:
		return false, false
	}
}

// screenEnvName walks the three forms of the class and returns the
// disposition of the first guard that catches name — the single walk both
// [DeniedEnvKey] and [ScreenImageEnvName] answer from.
//
// All three forms read the UPPER-cased name, so case is nowhere a
// membership rule: a table that matched its exact names as written applied
// the case rule at one row (`gcloud_project`) and left every sibling
// spelling — `openai_api_base`, `imds_endpoint`, `bash_env` — answering
// "outside the class". It is a margin and [DeniedEnvKey] says what it costs
// the repository channel, where a lower-case spelling of an exact name is
// dropped from `containerEnv` like its upper-case twin.
func screenEnvName(name string) imageEnvDisposition {
	upper := strings.ToUpper(name)
	if d, exact := deniedEnvNames[upper]; exact {
		return d
	}
	for _, guard := range deniedEnvSuffixes {
		if strings.HasSuffix(upper, guard.pattern) {
			return guard.image
		}
	}
	for _, guard := range deniedEnvNamespaces {
		if strings.HasPrefix(upper, guard.pattern) {
			return guard.image
		}
	}
	return notDenied
}

// imageEnvDisposition is what the runtime does about a name of this class
// that an IMAGE bakes into its `ENV`. The question only arises for images: a
// devcontainer.json entry is dropped in [ToSandboxSpec] before it ever
// becomes spec.Env, while an image's ENV is already inside the artefact and
// can only be OVERRIDDEN at start (`--env NAME=`) — ARB-112 on #2317.
//
// The zero value is [notDenied], so a map lookup that misses answers
// "outside the class" instead of naming a disposition the class never gave.
type imageEnvDisposition uint8

const (
	// notDenied is the zero value: the name is not in the class at all.
	notDenied imageEnvDisposition = iota

	// reportedOnly tells the operator and leaves the image's value in
	// place. It is the answer wherever an EMPTY value would break an image
	// doing its job, would be read as presence rather than as a value, or
	// where nothing a process iterion starts reads the name at all: the
	// command search path, the loader's and the runtimes' library paths,
	// the shells and the startup files they do read, the toolchains, the
	// homes every configuration is resolved from, the TLS bundles an image
	// may legally bake for its own tooling, and the names of binaries a CLI
	// spawns.
	// iterion's own published images set three of these on purpose (`PATH`,
	// `GOFLAGS`, `ITERION_VERSION`/`_COMMIT`/`_BOTS_PATH`), which is the
	// measurement that killed screening the class as a whole.
	reportedOnly

	// neutralizeInImage overrides the name with an EMPTY value in the
	// container, and reports it. It is the answer for two sub-classes. The
	// first chooses WHERE a model call goes and WHAT credential it carries
	// — the endpoint, region, project and identity names, the credential
	// suffixes, and the provider/agent namespaces. The second leads to
	// CODE, or to a configuration a program EXECUTES, at the startup of a
	// process iterion starts there: the loader, the shells' own startup
	// channels, node's and npm's and bun's options, glibc's and OpenSSL's
	// modules, git's config-from-the-environment, tar's options. A member
	// of the second re-plants everything the first empties, which is why
	// they share an answer (ARB-119).
	//
	// Three measurements decide a row, on a disposable image through the
	// program's own startup path: the planted value RUNS, the empty value
	// is inert, and no image iterion publishes bakes the name. Those
	// consumers all read a value (a truthiness, a URL, a key, an option
	// string), so an empty one is inert; none is set by an image iterion
	// publishes, so no working image loses anything. The operator's own channels still win: the workflow's
	// `sandbox.env:`, the launch environment and the driver's own flags are
	// applied on top, and the host-side allow knob lets a named variable
	// through (see pkg/runtime's image env screen).
	neutralizeInImage
)

// envGuard is one pattern of the class — a suffix or a namespace prefix —
// carrying the [imageEnvDisposition] every name it catches inherits. The
// flag rides on the ENTRY so there is one inventory, not two.
type envGuard struct {
	pattern string
	image   imageEnvDisposition
}

// deniedEnvSuffixes route a call or carry a credential under any prefix.
// Every one of them is the credential/routing sub-class by definition, so
// all five neutralise: an empty endpoint, key, token, secret or proxy is
// inert, where the planted value is the whole attack.
var deniedEnvSuffixes = []envGuard{
	{"_BASE_URL", neutralizeInImage},
	{"_API_KEY", neutralizeInImage},
	{"_TOKEN", neutralizeInImage},
	{"_SECRET", neutralizeInImage},
	{"_PROXY", neutralizeInImage},
}

// deniedEnvNamespaces are the configuration namespaces of the agents
// iterion runs for the operator, of the SDKs their providers
// authenticate through, and of the code loaders that start them.
//
// The agent and provider namespaces neutralise: they name a route and a
// credential, nothing iterion publishes bakes one, and an empty value is
// inert to every consumer. The loader, toolchain and home namespaces are
// reported only — `ITERION_` because iterion's own images set
// `ITERION_VERSION`/`_COMMIT`/`_BOTS_PATH`, the rest because an empty
// loader path, gitconfig or XDG dir breaks an image doing its job.
var deniedEnvNamespaces = []envGuard{
	// The in-container runner: every knob and switch it is forwarded; the
	// HOOK_* payload its settings hooks — the permission gate among them —
	// are told about; and claw-code-go's own CLAWD_ namespace, which CLAW_
	// does not prefix: the payload a plugin tool is given (CLAWD_TOOL_NAME,
	// CLAWD_TOOL_INPUT, CLAWD_PLUGIN_*) and its telemetry knobs.
	{"ITERION_", reportedOnly},
	{"CLAW_", neutralizeInImage},
	{"CLAWD_", neutralizeInImage},
	{"HOOK_", neutralizeInImage},
	// The claude CLI: its cloud-provider switches and their auth skips,
	// its shell override, session-environment script, config dir and
	// certificate store, its telemetry switch; its remote mode's agent
	// proxy and token file; the ANT_ twins of its telemetry variables.
	{"CLAUDE_", neutralizeInImage},
	{"CCR_", neutralizeInImage},
	{"ANT_OTEL_", neutralizeInImage},
	{"ANT_CLAUDE_CODE_", neutralizeInImage},
	// The Anthropic client of the claude CLI: ANTHROPIC_UNIX_SOCKET, the
	// Foundry resource and the Vertex project a call is spent in, and
	// ANTHROPIC_CUSTOM_HEADERS, which claw applies too — last, so it rides
	// a funded route.
	{"ANTHROPIC_", neutralizeInImage},
	// opencode: its config file, config dir, inline config, permissions.
	// (Its credential STORE is under XDG_DATA_HOME, refused by XDG_ below.)
	{"OPENCODE_", neutralizeInImage},
	// The AWS SDK chain Bedrock authenticates through: credentials,
	// profiles and their files, the container and instance credential
	// endpoints, the region, AWS_ENDPOINT_URL_<SERVICE>.
	{"AWS_", neutralizeInImage},
	// azidentity and Azure OpenAI: credentials, authority host, endpoint.
	{"AZURE_", neutralizeInImage},
	// Google ADC, which Vertex authenticates through: the service-account
	// file, project and region, the metadata server, gcloud's credential
	// dir — and the claude CLI's per-model Vertex region.
	{"GOOGLE_", neutralizeInImage},
	{"GCE_METADATA_", neutralizeInImage},
	{"CLOUDSDK_", neutralizeInImage},
	{"VERTEX_REGION_", neutralizeInImage},
	// Where an OpenTelemetry exporter ships what it records: the claude
	// CLI's telemetry carries the prompts once the operator opts in.
	{"OTEL_", neutralizeInImage},
	// Error tracking: the iterion binary is its own in-container runner and
	// calls errtrack.Init for every command but the permission hook, so a
	// planted SENTRY_DSN is not a redirection of the operator's reporting —
	// it switches an outbound channel ON (pkg/errtrack).
	{"SENTRY_", neutralizeInImage},
	// The forge CLIs a tool node runs for the operator: their config dir
	// chooses the identity `gh`/`glab` speak with, GH_HOST the API they
	// speak to.
	{"GH_", neutralizeInImage},
	{"GLAB_", neutralizeInImage},
	// nix, which iterion's own provisioning step drives (devbox install, run
	// as the container's post-create): NIX_CONFIG carries a whole nix.conf,
	// and a `plugin-files` in it makes nix dlopen the file it names on any
	// invocation, before any build — the repository's shared object would
	// load into iterion's own step. NIX_SSL_CERT_FILE (nix does not honour
	// SSL_CERT_FILE) and the store's NIX_PATH/NIX_REMOTE ride along.
	{"NIX_", reportedOnly},
	// git's configuration by the road that needs NO file and no HOME:
	// GIT_CONFIG_COUNT (an exact row below) with GIT_CONFIG_KEY_<n> and
	// GIT_CONFIG_VALUE_<n> is `git -c` spelled in the environment, so a
	// planted `core.sshCommand` or `credential.helper` is a program git
	// RUNS — measured on a built image: git fetch executed the planter's
	// program with the operator's remote on its argv, and `--env
	// GIT_CONFIG_COUNT=` leaves `git config --get core.sshCommand`
	// answering nothing. Listed BEFORE `GIT_`, because the first matching
	// namespace answers.
	{"GIT_CONFIG_KEY_", neutralizeInImage},
	{"GIT_CONFIG_VALUE_", neutralizeInImage},
	// git and its forge siblings: its config (GIT_CONFIG_*), transport (GIT_SSH_COMMAND,
	// GIT_PROXY_COMMAND, GIT_EXEC_PATH, GIT_ALLOW_PROTOCOL), credential
	// prompt (GIT_ASKPASS) and TLS trust, the repository and object store
	// it acts on, its trace files, its identity.
	{"GIT_", reportedOnly},
	// Every member of this one is a shell FUNCTION definition, which bash
	// imports into every NON-interactive shell — the shape of a tool node
	// and of an agent's Bash tool — so a planted `BASH_FUNC_git%%` runs in
	// place of git there. Nothing legitimate bakes one and an empty
	// definition defines nothing: measured on a built image, the planted
	// function answered `git --version` and `--env BASH_FUNC_git%%=`
	// restored git's own.
	{"BASH_FUNC_", neutralizeInImage},
	// The FAMILIES of the loader, of glibc's and OpenSSL's module paths,
	// of the bun runtime and of npm's config stay reported: LD_LIBRARY_PATH
	// is how a CUDA-style image finds its own runtime, OPENSSL_MODULES and
	// BUN_INSTALL name directories an image legally bakes, NPM_CONFIG_PREFIX
	// the global root it installs into. The members that reach CODE —
	// LD_PRELOAD, LD_AUDIT, OPENSSL_CONF, BUN_OPTIONS,
	// NPM_CONFIG_NODE_OPTIONS, NPM_CONFIG_SCRIPT_SHELL — are exact rows of
	// [deniedEnvNames], which is consulted first.
	{"LD_", reportedOnly},
	{"OPENSSL_", reportedOnly},
	{"BUN_", reportedOnly},
	{"NPM_CONFIG_", reportedOnly},
	// The XDG base directories, which are where the configuration and the
	// credential stores of the programs iterion runs there LIVE: git reads
	// $XDG_CONFIG_HOME/git/config (every `insteadOf` and helper of the
	// GIT_CONFIG_* entries, by another road), and opencode's login sits
	// under $XDG_DATA_HOME (pkg/backend/detect's openCodeAuthPath). The
	// whole family, not the two names seen planted: a dir nobody reads
	// today is a dir the next CLI release reads (#2318).
	{"XDG_", reportedOnly},
}

// deniedEnvNames are the exact names outside the families above, each with
// what the runtime does when an IMAGE bakes it in. Consulted FIRST, so an
// exact name overrides the disposition its namespace would give it, and
// keyed by the UPPER-cased name: the lookup upper-cases what it is asked,
// so every entry answers for every spelling and no row needs a twin.
//
// The endpoint, region, project and managed-identity names neutralise, and
// so do the three channels that re-export a neutralised name. The rest are
// reported only, and three groups earn that answer rather than
// inherit it: `PATH`, `HOME`, `GOROOT` and the shells, where an empty value
// breaks the container outright; `KUBECONFIG`, `DOCKER_HOST` and
// `DOCKER_CONFIG`, where emptying would send the client to its DEFAULT —
// the operator's own `~/.kube/config` and docker socket under `host_state`,
// which is worse than the planted path; and the TLS bundles and
// binary-path names, which an image may legally bake for its own tooling
// and whose consumers read presence as much as value.
var deniedEnvNames = map[string]imageEnvDisposition{
	// The three channels that RESTORE a neutralised name, which is why
	// they neutralise rather than report: bash reads BASH_ENV for every
	// NON-interactive shell — the shape of a tool node (`bash -c`) and of
	// an agent's Bash tool — so a planted script re-exports every name
	// this screen just emptied; `NODE_OPTIONS=--require=…` runs a planted
	// module inside the agent CLI's own process, which is node; and
	// NODE_PATH resolves a bare `require` from the planter's directory.
	// Empty is their default — measured on built images: `bash -c` and
	// `node -e` run normally with all three empty, where the planted
	// values otherwise come back (#2317, ARB-119).
	"BASH_ENV": neutralizeInImage, "NODE_OPTIONS": neutralizeInImage, "NODE_PATH": neutralizeInImage,
	// The rest of that sub-class: a name that leads to CODE, or to a
	// configuration a program EXECUTES, at the startup of a process
	// iterion starts in the container — and whose empty value is inert,
	// measured on a disposable image through the program's own startup
	// path, all of them at once (#2317, round 3).
	//
	//   - the loader runs the planter's shared object in EVERY process:
	//     LD_PRELOAD's and LD_AUDIT's constructors ran in pid 1 and
	//     re-exported the four credential names this screen had just
	//     emptied, to bash, to node and to an npm-started MCP server
	//     alike. LD_LIBRARY_PATH keeps the family's answer: a CUDA-style
	//     image finds its own runtime through it;
	//   - GCONV_PATH is glibc's: a planted gconv-modules is dlopened by
	//     the charset conversion python does at interpreter startup —
	//     through `python3 -I`, the argv of iterion's OWN python tool
	//     node, which ignores every PYTHON* variable;
	//   - OPENSSL_CONF is a configuration file that dlopens the provider
	//     module it names: the planter's code ran in curl and in the
	//     openssl CLI;
	//   - SHELLOPTS=xtrace makes bash run the command substitutions of a
	//     planted PS4 once per traced command, in every `bash -c`;
	//   - npm turns NPM_CONFIG_NODE_OPTIONS into the NODE_OPTIONS of
	//     every node child it spawns — `npm run` and `npm exec`, the
	//     documented way an MCP server starts — which undoes NODE_OPTIONS
	//     above, and NPM_CONFIG_SCRIPT_SHELL makes the planter's program
	//     the shell those scripts run under;
	//   - the bun runtime the claude CLI is built on reads BUN_OPTIONS:
	//     `--preload` ran the planter's module inside the CLI's own
	//     process (measured on `claude --version` in the default image);
	//   - GNU tar prepends TAR_OPTIONS to its own arguments, and
	//     --checkpoint-action=exec= is one: the planter's program ran in
	//     a tar ITERION starts — the scratch bank and its restore, the
	//     kubernetes workspace populate and export;
	//   - GIT_CONFIG_COUNT is the head of the no-file config road above.
	//
	// The control for all of them, measured in the same image: with every
	// one emptied, bash, sh, node, npm, npx, git, python3, the claude CLI,
	// tar, openssl, curl and perl all run normally.
	"LD_PRELOAD": neutralizeInImage, "LD_AUDIT": neutralizeInImage,
	"GCONV_PATH": neutralizeInImage, "OPENSSL_CONF": neutralizeInImage,
	"SHELLOPTS": neutralizeInImage, "PS4": neutralizeInImage,
	"NPM_CONFIG_NODE_OPTIONS": neutralizeInImage, "NPM_CONFIG_SCRIPT_SHELL": neutralizeInImage,
	"BUN_OPTIONS": neutralizeInImage, "TAR_OPTIONS": neutralizeInImage,
	"GIT_CONFIG_COUNT": neutralizeInImage,
	// The shell the claude CLI picks, bash's other option set, sh's and
	// zsh's startup files, the command search path, `go build -toolexec`,
	// ssh's askpass helper. An image that bakes one of these is doing its
	// job and an empty value breaks it — or the name is read as presence,
	// or nothing reads it at the startup of a process iterion starts:
	// `ENV` is the measured one, which neither dash (the default image's
	// /bin/sh) nor bash-as-sh sources for a NON-interactive `sh -c`.
	"SHELL": reportedOnly, "BASHOPTS": reportedOnly,
	"ENV": reportedOnly, "ZDOTDIR": reportedOnly, "PATH": reportedOnly, "GOFLAGS": reportedOnly,
	"SSH_ASKPASS": reportedOnly, "SSH_ASKPASS_REQUIRE": reportedOnly,
	// git's editor chain is GIT_EDITOR (denied by GIT_), core.editor,
	// VISUAL, EDITOR: the last two run the planter's program in a `git
	// commit` an agent makes with no message of its own.
	"EDITOR": reportedOnly, "VISUAL": reportedOnly,
	// The go command a bot's build step runs: GOENV names a file that sets
	// every go variable the env leaves unset — GOFLAGS included, so it
	// re-opens that one — GOTOOLCHAIN downloads and runs another toolchain,
	// GOROOT picks the one in use, GOAUTH names a program run for module
	// credentials. (GOPROXY and GOPRIVATE stay out: they point the
	// repository's own dependency fetch, which a devcontainer legitimately
	// configures.)
	"GOENV": reportedOnly, "GOTOOLCHAIN": reportedOnly, "GOROOT": reportedOnly, "GOAUTH": reportedOnly,
	// The cluster and daemon an operator-authored deploy step talks to, and
	// the credential programs they run: a kubeconfig's `exec` plugin runs on
	// every kubectl invocation, a docker config's `credsStore` names a
	// docker-credential-<name> binary.
	"KUBECONFIG": reportedOnly, "DOCKER_HOST": reportedOnly, "DOCKER_CONFIG": reportedOnly,
	// The agents' config homes (CLAUDE_CONFIG_DIR rides CLAUDE_).
	"CODEX_HOME": reportedOnly, "KIMI_CODE_HOME": reportedOnly, "PI_CODING_AGENT_DIR": reportedOnly,
	// HOME itself: every file above is resolved from it when its own
	// variable is unset — `~/.gitconfig` first. The EXACT name only: the
	// *_HOME of a toolchain (JAVA_HOME, ANT_HOME) configures the
	// repository's own build, which is a devcontainer doing its job.
	"HOME": reportedOnly,
	// Endpoints, projects and regions off the suffix rule; the claude
	// CLI's agent proxy and beta tracing.
	"OPENAI_API_BASE": neutralizeInImage, "BEDROCK_REGION": neutralizeInImage, "CLOUD_ML_REGION": neutralizeInImage,
	// The claude CLI reads BOTH spellings of the gcloud project, and ONE
	// entry answers for both: the names are matched case-insensitively,
	// like the suffixes and the namespaces.
	"GCLOUD_PROJECT":  neutralizeInImage,
	"AGENT_PROXY_URL": neutralizeInImage, "BETA_TRACING_ENDPOINT": neutralizeInImage,
	"ENABLE_BETA_TRACING_DETAILED": neutralizeInImage, "ENABLE_ENHANCED_TELEMETRY_BETA": neutralizeInImage,
	// The endpoint the claude CLI falls back to when no ANTHROPIC_BASE_URL
	// is set — the usual case on a forfait run, where it then IS the API
	// base — and reads first at its session-ingress origin check; and the
	// binaries it spawns by a path env alone names: its guest-vitals
	// emitter, the agent SDK's native binary, a skill's browser.
	"SESSION_INGRESS_URL": neutralizeInImage,
	"VITALS_EMITTER_BIN":  reportedOnly, "SDK_NATIVE_BIN": reportedOnly, "DS_CHROMIUM_PATH": reportedOnly,
	// Azure's managed-identity chain, which azidentity's
	// DefaultAzureCredential authenticates a foundry node through when no
	// Azure key is set: these name the server that mints its token, and
	// the Arc leg reads a file that server's answer names. Off the AZURE_
	// prefix, so one entry each.
	"IDENTITY_ENDPOINT": neutralizeInImage, "IDENTITY_HEADER": neutralizeInImage, "IDENTITY_SERVER_THUMBPRINT": neutralizeInImage,
	"IMDS_ENDPOINT": neutralizeInImage, "MSI_ENDPOINT": neutralizeInImage, "DEFAULT_IDENTITY_CLIENT_ID": neutralizeInImage,
	"MSAL_FORCE_REGION": neutralizeInImage,
	// The TLS trust of the connections that carry credentials: node for the
	// CLIs, OpenSSL and Go's crypto/x509 for the runner, grpc-js (which
	// REPLACES its root set, where NODE_EXTRA_CA_CERTS appends), and the
	// bundles the drivers themselves point at the per-run CA in inspection
	// mode (sandbox/docker caInjection) — that set, not every runtime's:
	// another language's bundle configures the repository's own tooling.
	"NODE_EXTRA_CA_CERTS": reportedOnly, "NODE_TLS_REJECT_UNAUTHORIZED": reportedOnly,
	"SSL_CERT_FILE": reportedOnly, "SSL_CERT_DIR": reportedOnly,
	"CURL_CA_BUNDLE": reportedOnly, "REQUESTS_CA_BUNDLE": reportedOnly,
	// grpc-js REPLACES its root set from this one, where NODE_EXTRA_CA_CERTS
	// only appends, and takes its cipher list from its twin.
	"GRPC_DEFAULT_SSL_ROOTS_FILE_PATH": reportedOnly, "GRPC_SSL_CIPHER_SUITES": reportedOnly,
}
