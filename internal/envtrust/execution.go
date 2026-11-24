package envtrust

import "strings"

// Package-level file: the execution-grade name inventory — the generic
// host-environment names whose PLANT makes a child of iterion execute,
// load or trust what a dotenv file chose.
//
// The inventory is the execution portion of the devcontainer deny class
// (pkg/sandbox/devcontainer/spec.go deniedEnvNames / deniedEnvNamespaces,
// #2303), which answered this question first for a different planter: a
// devcontainer.json there, a project `.env` here — the same children on
// the receiving end. The H-10 round-6 review measured the gap: the three
// dotenv loaders planted BASH_ENV and LD_PRELOAD silently (a tool-node
// bash SOURCED the planted file; every engine child PRELOADED the planted
// shared object), because the plant gate asked only the routing shapes
// and the operator grade, and neither answers a generic execution name.
// The loader doctrine extends (ARB-122): the loader is the one site that
// knows it is planting, so the class closes at the plant — one predicate,
// the three lanes. The reads carry the same belt they always did.
//
// Unlike the operator grade, the opt-in (DotenvEndpointsOptIn) answers
// this class: these names have legitimate lives in an operator's own
// dotenv file, and the operator — not the nearest repository — decides.
// The refusal line names the export, like the routing class it sits
// beside; the union with the routing shapes is expressed in
// DotenvPlantRefused, and a name both classes answer reads ONE refusal,
// the routing one.
//
// Measured and left out, with the reason — the same membership test the
// devcontainer class states (does a process ITERION starts read it, not
// does the name look scary):
//
//   - PATH: a `.env` that extends the operator's PATH (the one legitimate
//     form, `PATH=$PATH:…`) is answered by the opt-in, and the expansion
//     that makes the extension real is the studio lane's (pkg/projectenv
//     expandReferences); the CLI and desktop lanes plant no expansion, so
//     a reference there plants the raw bytes — their loader refusing the
//     name instead of planting it broken is part of what this class fixes.
//   - the provider credential namespaces (AWS_, AZURE_, GOOGLE_, ANTHROPIC_,
//     GH_, GLAB_, OTEL_, SENTRY_, CLAUDE_, …) and the endpoint/region
//     switches (BEDROCK_REGION, GCLOUD_PROJECT, ENABLE_*_TELEMETRY_*): a
//     provider credential next to a project is what the file is FOR, and
//     a name the routing shapes answer (_URL, _ENDPOINT, _HOST, _DSN)
//     was already refused. The partition is held by
//     TestEveryDeniedEnvKeyReachesADotenvPlantClassOrAnExemption in
//     pkg/sandbox/devcontainer, where the full table lives.
var executionPrefixes = []string{
	// The dynamic loader: LD_PRELOAD / LD_AUDIT load the planter's shared
	// object into every process iterion forks; LD_LIBRARY_PATH moves the
	// libraries they all resolve.
	"LD_",
	// OpenSSL's configuration file loads a provider or engine module — the
	// planter's code inside every OpenSSL-linked child.
	"OPENSSL_",
	// A non-root bash imports a function from every BASH_FUNC_<name>%%: a
	// planted `git` function runs in place of git in every `bash -c`.
	"BASH_FUNC_",
	// The bun runtime the claude CLI is built on takes BUN_OPTIONS, where
	// --preload runs the planter's script inside the CLI's own process.
	"BUN_",
	// npm turns NPM_CONFIG_NODE_OPTIONS / NPM_CONFIG_SCRIPT_SHELL into the
	// options and the shell of every node child it spawns — `npx <server>`,
	// the documented way an MCP server starts, runs the planter's module in
	// that server's own process.
	"NPM_CONFIG_",
	// nix, which iterion's own provisioning drives: NIX_CONFIG carries a
	// whole nix.conf, and a `plugin-files` in it makes nix dlopen the file
	// it names on any invocation.
	"NIX_",
	// The XDG base directories are where the configuration and credential
	// stores of the programs iterion runs LIVE: git reads
	// $XDG_CONFIG_HOME/git/config (every `insteadOf` and helper), opencode's
	// login sits under $XDG_DATA_HOME. The whole family: a dir nobody reads
	// today is a dir the next CLI release reads (#2318).
	"XDG_",
}

// executionNames are the exact names outside those families. The match is
// case-insensitive (upper): Windows folds the case anyway (CanonicalName),
// and a lowercase spelling of a loader name asks the same question.
var executionNames = map[string]bool{
	// The shells the engine's children run: bash sources BASH_ENV before
	// any script, SHELLOPTS=xtrace runs a planted PS4's command
	// substitutions in a non-root bash, BASHOPTS is its twin; ENV is sh's
	// startup file, ZDOTDIR moves zsh's, SHELL is the shell the claude CLI
	// picks. PS4 spells what xtrace runs. IFS re-splits every unquoted
	// expansion those shells perform — a planted separator re-parses the
	// scripts the engine's tool nodes run.
	"BASH_ENV": true, "BASHOPTS": true, "SHELLOPTS": true, "PS4": true,
	"ENV": true, "ZDOTDIR": true, "SHELL": true, "IFS": true,
	// glibc's iconv modules, loaded by name on the first conversion.
	"GCONV_PATH": true,
	// The node runtime's options and module path: NODE_OPTIONS carries
	// --require, NODE_PATH the modules every require resolves.
	"NODE_OPTIONS": true, "NODE_PATH": true,
	// The interpreters a tool node runs. PYTHONPATH puts a directory on
	// every python's module path and its sitecustomize.py EXECUTES at
	// interpreter startup: the H-10 probe measured the marker file created
	// by `python3 -c "print(1)"` alone, no user code — the BASH_ENV form
	// for the python family. PYTHONHOME moves the standard library itself,
	// PYTHONSTARTUP names a file an interactive run sources. RUBYOPT and
	// PERL5OPT carry the interpreter's own switches (a -M/`use` loads the
	// planter's module), PERL5LIB its module path. JAVA_TOOL_OPTIONS and
	// JDK_JAVA_OPTIONS are read by every JVM on the host with no flag of
	// ours, and CLASSPATH is where its classes resolve from.
	"PYTHONPATH":        true,
	"PYTHONHOME":        true,
	"PYTHONSTARTUP":     true,
	"RUBYOPT":           true,
	"PERL5OPT":          true,
	"PERL5LIB":          true,
	"JAVA_TOOL_OPTIONS": true,
	"JDK_JAVA_OPTIONS":  true,
	"CLASSPATH":         true,
	// The interpreter the operator's cloud CLI runs its own commands with:
	// a planted one executes wherever a tool node drives `gcloud`.
	"CLOUDSDK_PYTHON": true,
	// The go command a bot's build step runs: GOFLAGS carries -toolexec,
	// GOENV names a file that sets every go variable the env leaves unset,
	// GOTOOLCHAIN downloads and runs another toolchain, GOROOT picks the one
	// in use, GOAUTH names a program run for module credentials.
	"GOFLAGS": true, "GOENV": true, "GOTOOLCHAIN": true, "GOROOT": true, "GOAUTH": true,
	// The programs a credential prompt or an editor launch runs: ssh's
	// askpass helper, the editor chain a `git commit` falls back to when
	// no message of its own is passed — the planter's program ran in the
	// H-10 probe's editor (`git commit --allow-empty` without -m, no TTY,
	// the editor created its marker) — and the pager git pipes diffs and
	// logs through when a TTY answers.
	"SSH_ASKPASS": true, "SSH_ASKPASS_REQUIRE": true,
	"EDITOR": true, "VISUAL": true,
	// GNU tar prepends TAR_OPTIONS to its own arguments, and
	// --checkpoint-action=exec= is one: the planter's program runs in each
	// tar step of a workspace populate or export.
	"TAR_OPTIONS": true,
	// The command search path and the home every configuration file above
	// resolves from when its own variable is unset — `~/.gitconfig` first.
	"PATH": true, "HOME": true,
	// The agents' config homes: a planted one hands the CLI the
	// repository's own settings — hooks and permission gates included.
	// opencode is one of the engine's own backends (pkg/backend/delegate
	// runs its CLI live), and its config file, config dir, inline config
	// content and permission bits are the same object the CODEX_HOME
	// admission answers for: the explicit names, not the XDG default
	// paths, are what a planted variable moves.
	// (CLAUDE_CONFIG_DIR is the claude CLI's, spelled exact here: the
	// dotenv question does not adopt the whole CLAUDE_ namespace.)
	"CODEX_HOME":              true,
	"KIMI_CODE_HOME":          true,
	"PI_CODING_AGENT_DIR":     true,
	"CLAUDE_CONFIG_DIR":       true,
	"OPENCODE_CONFIG":         true,
	"OPENCODE_CONFIG_DIR":     true,
	"OPENCODE_CONFIG_CONTENT": true,
	"OPENCODE_PERMISSION":     true,
	// The cluster and daemon an operator-authored deploy step talks to: a
	// kubeconfig's `exec` plugin runs on every kubectl invocation, a docker
	// config's `credsStore` names a docker-credential-<name> binary.
	// (DOCKER_HOST rides the routing shapes.)
	"KUBECONFIG": true, "DOCKER_CONFIG": true,
	// The binaries the claude CLI spawns by a path env alone: its
	// guest-vitals emitter, the agent SDK's native binary, a skill's
	// browser.
	"VITALS_EMITTER_BIN": true, "SDK_NATIVE_BIN": true, "DS_CHROMIUM_PATH": true,
	// git's credential prompt, its alternate binary tree, its editor chain
	// and its pager: the programs git itself runs. (The transport family —
	// GIT_SSH, GIT_PROXY_COMMAND, GIT_CONFIG_ — is the routing class's,
	// where it has been since that class closed; GIT_SSL_* ride git's own
	// TLS trust and are named in the devcontainer guard's exemptions.)
	"GIT_ASKPASS": true, "GIT_EXEC_PATH": true,
	"GIT_EDITOR": true, "GIT_SEQUENCE_EDITOR": true, "GIT_PAGER": true,
	// The TLS trust of the connections that carry credentials: node for the
	// CLIs, OpenSSL and Go's crypto/x509 for the runner, grpc-js (which
	// REPLACES its root set, where NODE_EXTRA_CA_CERTS appends), and the
	// bundles the drivers point at. AWS_CA_BUNDLE is the vendored AWS
	// SDK's own spelling — pkg/store/blob's S3 store boots
	// aws-sdk-go-v2's config on this process's env, and the vendored
	// env_config.go reads the name into the HTTP client's trust root. A
	// planted trust root is the planter vouching for their host as yours.
	"NODE_EXTRA_CA_CERTS":              true,
	"NODE_TLS_REJECT_UNAUTHORIZED":     true,
	"SSL_CERT_FILE":                    true,
	"SSL_CERT_DIR":                     true,
	"CURL_CA_BUNDLE":                   true,
	"REQUESTS_CA_BUNDLE":               true,
	"AWS_CA_BUNDLE":                    true,
	"GRPC_DEFAULT_SSL_ROOTS_FILE_PATH": true,
	"GRPC_SSL_CIPHER_SUITES":           true,
	// The unix socket the claude CLI's Anthropic client dials in place of
	// the API: a ROUTE, read by an external binary the Go-form grep cannot
	// see — the devcontainer table has named that reader since #2303, the
	// same blindness the routing class confessed when GIT_PROXY_COMMAND
	// joined on the git binary's evidence. A planted socket is the
	// planter answering the API's voice.
	"ANTHROPIC_UNIX_SOCKET": true,
}

// ExecutionGradeName reports whether a variable's NAME puts it in the
// execution class — the generic host-environment names whose plant makes a
// child of iterion execute, load or trust what the file chose. Asked by
// DotenvPlantRefused (one entry point per plant, three lanes); refused by
// default, answered by the operator's opt-in like the routing class. The
// match is case-insensitive.
func ExecutionGradeName(name string) bool {
	upper := strings.ToUpper(name)
	if executionNames[upper] {
		return true
	}
	for _, prefix := range executionPrefixes {
		if strings.HasPrefix(upper, prefix) {
			return true
		}
	}
	return false
}
