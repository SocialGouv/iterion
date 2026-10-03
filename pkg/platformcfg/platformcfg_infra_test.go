package platformcfg

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/store"
)

// TestInfraNamesCoverTheEnforcementSwitches: botVarsInfraExact exists so a
// stored bot var can never become an infra override. The two project-trust
// switches are the ones that matter most — each lifts a refusal that stops an
// agent CLI from executing code out of the repository under review — and the
// two binary pins decide which binary runs.
func TestInfraNamesCoverTheEnforcementSwitches(t *testing.T) {
	for _, name := range []string{
		"ITERION_PI_TRUST_PROJECT",
		"ITERION_OPENCODE_TRUST_PROJECT",
		"ITERION_PI_BIN",
		"ITERION_OPENCODE_BIN",
	} {
		if !botVarsInfraExact[name] {
			t.Errorf("%s is writable as a bot var; it decides what runs or whether a refusal holds", name)
		}
		if botVarNameOK(name) {
			t.Errorf("%s passes botVarNameOK; the infra list is not consulted", name)
		}
	}
}

// guardNames lift or weaken a guard, configure the process rather than a
// run, name an outside endpoint or identity, or are written by the engine
// for a child — read from what each one does. Each must stay refused: the
// classification test alone would accept one moved to writableBotVars.
var guardNames = []string{
	// Browser-facing guards.
	"ITERION_ALLOWED_ORIGINS", "ITERION_REQUIRE_ORIGIN", "ITERION_REQUIRE_WS_ORIGIN",
	"ITERION_SECURITY_HEADERS", "ITERION_LEGACY_REFRESH_COOKIE", "ITERION_CANONICAL_REDIRECT",
	"ITERION_STUDIO_INSECURE_NONLOOPBACK",
	// Private-network reach and host allowlists.
	"ITERION_COMPLETION_WEBHOOK_ALLOW_PRIVATE", "ITERION_CONNECTOR_ALLOW_PRIVATE",
	"ITERION_LLM_ENDPOINT_ALLOW_PRIVATE", "ITERION_OPENAI_COMPATIBLE_RESOLVED",
	"ITERION_RUNNER_CLONE_ALLOW_PRIVATE", "ITERION_WEBHOOK_FORGE_HOSTS",
	// Code and configuration loaded from the repository or the host.
	"ITERION_MCP_AUTOLOAD", "ITERION_MCP_HEALTHCHECK", "ITERION_SKIP_MCP_HEALTH", "ITERION_PI_MCP_SERVERS",
	"ITERION_MCP_EXPAND_UNTRUSTED_ENV",
	"ITERION_PLUGINS_ENABLE", "ITERION_PLUGINS_DISABLE", "ITERION_PLUGIN_FIRECRAWL_API_URL",
	"ITERION_CLAUDE_CODE_SETTING_SOURCES", "ITERION_CLAUDE_CODE_STRICT_MCP",
	"ITERION_CLAUDE_CODE_DISALLOW_ORCHESTRATION_TOOLS", "ITERION_PI_NO_CONTEXT_FILES",
	"ITERION_CLAW_SLASH_COMMANDS", "ITERION_WEB_SEARCH", "ITERION_CONNECTOR_PROJECT_CATALOG",
	"ITERION_REPO_DEVBOX", "ITERION_SKILLS", "ITERION_BOTS_PATH", "ITERION_PI_AGENT_DIR",
	"ITERION_ASSISTANT_PYTHON", "ITERION_MARKETPLACE_SEED_PATHS", "ITERION_RTK_BIN", "ITERION_CODEINDEX_BIN",
	// The permission gate's mode, and the classifier whose Allow skips tool_policy.
	"ITERION_PERMISSION", "ITERION_PI_PERMISSION", "ITERION_LLM_CLASSIFIER_MODEL",
	// Defaults an unpinned node inherits: its backend, and hermetic auto-memory.
	"ITERION_DEFAULT_BACKEND", "ITERION_BACKEND_PREFERENCE", "ITERION_AUTO_MEMORY",
	// Bounds on work that spends.
	"ITERION_NODE_MAX_RETRIES", "ITERION_OUTPUT_CORRECTION_BUDGET",
	"ITERION_CLAUDE_CODE_NO_PROGRESS_TIMEOUT", "ITERION_CLAUDE_CODE_MAX_TOOL_ERRORS",
	// Spend and quota ceilings and brakes.
	"ITERION_FORBID_SUBSCRIPTION_OAUTH", "ITERION_OPENAI_USE_OAUTH", "ITERION_LOOP_BUDGET_GUARD",
	"ITERION_CLOUD_MAX_COST_USD", "ITERION_CLOUD_MAX_DURATION", "ITERION_CLOUD_MAX_ITERATIONS",
	"ITERION_CLOUD_MAX_PARALLEL_BRANCHES", "ITERION_CLOUD_RETRY_MAX_ATTEMPTS", "ITERION_CLOUD_RETRY_MAX_WAIT",
	"ITERION_MAX_COST_PER_DAY_USD", "ITERION_MAX_CONCURRENT_PIPELINES",
	"ITERION_ORG_DEFAULT_MONTHLY_COST_CAP_USD", "ITERION_ORG_DEFAULT_MONTHLY_RUN_QUOTA",
	"ITERION_ORG_DEFAULT_MAX_CONCURRENT_RUNS", "ITERION_ORG_DEFAULT_LAUNCH_RATE_PER_MIN",
	"ITERION_FORFAIT_CAP_PCT", "ITERION_BUDGET_EXIT_GRACE", "ITERION_PLAN_REVIEW",
	"ITERION_RETRY_CIRCUIT_THRESHOLD", "ITERION_RETRY_CIRCUIT_COOLDOWN", "ITERION_ROUTE_COOLDOWN",
	"ITERION_MODEL_SPECS", "ITERION_BOARD_LAUNCH_ATTEMPTS",
	"ITERION_MEMORY_QUOTA_ORG_TOTAL", "ITERION_MEMORY_MAX_DOC",
	// Bounds on what untrusted input may cost.
	"ITERION_BUNDLE_MAX_BYTES", "ITERION_BUNDLE_MAX_ENTRIES", "ITERION_CLAW_SLASH_COMMAND_MAX_BYTES",
	"ITERION_ASSISTANT_AUTHORING_MAX_TOTAL_BYTES", "ITERION_ASSISTANT_EDITOR_MAX_SOURCE",
	// Workspace safety.
	"ITERION_WORKSPACE_TRACK", "ITERION_WORKSPACE_CHECKPOINT", "ITERION_WORKSPACE_MAX_FILE_MB",
	"ITERION_PRUNE_MIRROR_IN_CHECKOUT", "ITERION_WORKTREE_POOL_MAX", "ITERION_SCRATCH_RETENTION",
	// Trust roots, host files, filesystem exposure.
	"ITERION_JAVA_TRUSTSTORE", "ITERION_ENV_FILE", "ITERION_BROWSE_ROOT", "ITERION_HOME",
	// Rollout levers.
	"ITERION_EXECUTION_CONTEXT_POLICY", "ITERION_RELIABILITY_MODE", "ITERION_OUTCOME_ROUTER",
	"ITERION_DISPATCH_VIA_SERVICE", "ITERION_RUNS_DETACHED", "ITERION_MODE", "ITERION_BOARD_CLAIM_REAPER",
	// Sessions, run ownership, the process.
	"ITERION_REFRESH_TTL", "ITERION_LOCK_TTL", "ITERION_HEARTBEAT_INTERVAL", "ITERION_RUNNER_EPOCH",
	"ITERION_RUNNER_WORKDIR", "ITERION_DESKTOP", "ITERION_DESKTOP_ATTACH_DAEMON", "ITERION_LOG_LEVEL",
	"ITERION_PROJECT_STORE",
	// Endpoints and identities presented outside.
	"ITERION_ALERTS_WEBHOOK_URL", "ITERION_PROMETHEUS_ADDR", "ITERION_REMOTE_URL", "ITERION_SERVER_URL",
	"ITERION_FORGE_GITHUB_APP_ID", "ITERION_GIT_AUTHOR_EMAIL", "ITERION_LLM_USER_AGENT",
	"ITERION_CODEX_VERSION", "ITERION_FORGE_BRAND_AVATAR",
	// Written by the engine for a child process.
	"ITERION_ARTIFACT_FILES_DIR", "ITERION_TREE_NOISE", "ITERION_STORE_DIR", "ITERION_RUN_STORE_DIR",
	"ITERION_RUN_CAPS", "ITERION_BOARD_CAPS", "ITERION_SOURCE_ISSUE_ID", "ITERION_FACADE_SLOT",
	"ITERION_FORFAIT_SUPPRESSED", "ITERION_CODEX_HOST_VERSION", "ITERION_SHARD_INDEX",
	"ITERION_PI_CTRL", "ITERION_PI_NODE_ID", "ITERION_RUN_ID", "ITERION_PARENT_RUN_ID",
	"ITERION_WORKSPACE", "ITERION_ISSUE_ID", "ITERION_SCHEDULE", "ITERION_TENANT",
	"ITERION_RUN_SHELL_MAX_LIFETIME", "ITERION_PI_RUN_ID", "ITERION_TOOL_NAME", "ITERION_TOOL_INPUT",
	"ITERION_DOTENV_PLANTED",
	// Credential-shaped by a whole segment, one witness per word.
	"ITERION_GH_PAT", "ITERION_GH_PATS", "ITERION_REVIEW_BEARER", "ITERION_MAIL_PASS",
	"ITERION_DB_PASSWD", "ITERION_DB_PASSPHRASE", "ITERION_VAULT_PASSCODE", "ITERION_WEB_HTPASSWD",
	"ITERION_DB_PW", "ITERION_DB_PWD", "ITERION_API_CRED", "ITERION_GH_CREDS", "ITERION_SESSION_COOKIE",
	"ITERION_LOGIN_COOKIES", "ITERION_GH_JWT", "ITERION_SIGN_PEM", "ITERION_CURL_NETRC", "ITERION_MFA_OTP",
	"ITERION_HOOK_HMAC", "ITERION_HOOK_SIGNATURE", "ITERION_ERRORS_DSN",
	"ITERION_PROXY_AUTH", "ITERION_REVIEW_AUTHORIZATION", "ITERION_GITLAB_OAUTH", "ITERION_BASICAUTH",
	"ITERION_REVIEW_BEARERS", "ITERION_PG_PASSFILE", "ITERION_X_USERPASS", "ITERION_GH_JWTS", "ITERION_TLS_PEMS",
	"ITERION_MFA_OTPS", "ITERION_MFA_TOTP", "ITERION_MFA_HOTP", "ITERION_HOOK_HMACS", "ITERION_WEB_SESSIONID",
	"ITERION_SLACK_WEBHOOK_URL",
	// Covered by a namespace rather than by name.
	"ITERION_AUTH_TRUSTED_AUTO_LINK_PROVIDERS", "ITERION_PLATFORM_FACADE_DEFAULT",
}

func TestGuardNamesAreNeverBotVars(t *testing.T) {
	for _, name := range guardNames {
		if botVarNameOK(name) {
			t.Errorf("%s is writable as a bot var; a stored value must never set it", name)
		}
	}
}

// Why a stored bot var may carry an engine name.
const (
	whyModel    = "a model default read through the overlay by design; it picks a model, never whether a guard runs"
	whyLiveness = "a bound on WAITING — a silent stream, a process's shutdown, a settle, a stalled orchestration call: nothing is spent while it runs"
	whyRetry    = "the usage-window retry policy's machine-wide default, which a bot or a schedule overrides itself and the ITERION_CLOUD_RETRY_* ceilings cap when the operator sets them"
	whyMode     = "a run-behaviour default that node, workflow and run settings override"
	whyNotEnv   = "not an env var: a NATS stream name"
	whyHarness  = "read only by a test harness, never by the server or the runner"
)

// writableBotVars is every engine ITERION_ name a stored bot var may carry,
// with the reason. Together with the infra lists it classifies every name
// the production source reads; TestEveryEngineEnvNameIsClassified keeps it
// that way, so a new switch cannot land without somebody deciding which
// side it belongs on.
var writableBotVars = map[string]string{
	"ITERION_CONFLICT_RESOLVER_MODEL":    whyModel,
	"ITERION_DEFAULT_SESSIONBOARD_MODEL": whyModel,
	"ITERION_DEFAULT_SUPERVISOR_MODEL":   whyModel,
	"ITERION_VERIFIED_ACTION_MODEL":      whyModel,

	"ITERION_COMPRESS":                     whyMode,
	"ITERION_CLAW_COMPACT_PRESERVE_RECENT": whyMode,
	"ITERION_CLAW_COMPACT_THRESHOLD_RATIO": whyMode,
	"ITERION_SUPERVISORS":                  "whether a bot's declared supervisors spawn, under the run-level --supervisors override",
	"ITERION_SESSION_BOARD":                "opts the session board in; lifts no guard",
	"ITERION_CLAUDE_CODE_THINKING_DISPLAY": "how a claude_code session renders its thinking; lifts no guard",
	"ITERION_MCP_CACHE_TTL":                "freshness of cached MCP tool listings, keyed by the server config's hash",
	"ITERION_PI_MODE":                      "the pi transport; print refuses a node that declares a permission gate, so it lifts none",
	"ITERION_PI_OFFLINE":                   "pi's catalogue refresh inside a sandbox; the sandbox egress policy still applies",
	"ITERION_AUTO_RESUME":                  "read only by the local CLI's run loop, which installs no bot-var overlay",
	"ITERION_RETRY_JITTER":                 whyRetry,
	"ITERION_RETRY_MAX_ATTEMPTS":           whyRetry,
	"ITERION_RETRY_MAX_WAIT":               whyRetry,
	"ITERION_RETRY_USAGE_WINDOW":           whyRetry,

	"ITERION_BRANCH_CANCEL_GRACE":               whyLiveness,
	"ITERION_CLAUDE_CODE_CLOSE_GRACE":           whyLiveness,
	"ITERION_CLAUDE_CODE_CLOSE_TERM":            whyLiveness,
	"ITERION_CLAUDE_CODE_ORCH_RECOVERY_TIMEOUT": whyLiveness,
	"ITERION_CLAUDE_CODE_ORCH_STALL_TIMEOUT":    whyLiveness,
	"ITERION_CLAUDE_CODE_STREAM_COLD_TIMEOUT":   whyLiveness,
	"ITERION_CLAUDE_CODE_STREAM_IDLE_TIMEOUT":   whyLiveness,
	"ITERION_CLAW_STREAM_COLD_TIMEOUT":          whyLiveness,
	"ITERION_CLAW_STREAM_IDLE_TIMEOUT":          whyLiveness,
	"ITERION_PI_SETTLE_GRACE":                   whyLiveness,
	"ITERION_PI_STREAM_COLD_TIMEOUT":            whyLiveness,
	"ITERION_PI_STREAM_IDLE_TIMEOUT":            whyLiveness,
	"ITERION_PI_MCP_CONNECT_TIMEOUT_MS":         whyLiveness,

	"ITERION_EVENTS":   whyNotEnv,
	"ITERION_RUNS":     whyNotEnv,
	"ITERION_RUNS_DLQ": whyNotEnv,

	"ITERION_PROCTEST_SETTLE":     whyHarness,
	"ITERION_LIVE_JUDGE_MODELS":   whyHarness,
	"ITERION_LIVE_LEDGER_OFFLINE": whyHarness,
	"ITERION_LIVE_LEDGER_PATH":    whyHarness,
	"ITERION_LIVE_LEDGER_TARGET":  whyHarness,
}

// buildOutputs are the gitignored trees a local build writes under pkg: the
// dispatch copies of the catalog bots (their knobs are the bots' side, read
// by TestEveryShippedBotKnobIsABotVar) and the studio bundle. Reading them
// would make the scan depend on whether `task test` ran first.
var buildOutputs = []string{"../../pkg/cli/templates/dispatch_bots", "../../pkg/server/static"}

// engineEnvNames maps every ITERION_ name the engine spells to the files that
// spell it: the string literals of its Go source and of the vendored claw it
// links — a name, or the head of an assignment such as "ITERION_SHARD_INDEX=%d"
// — and every name written in the manifests and scripts it embeds under pkg
// (plugin manifests, the pi extension). The deny lists themselves are cut out
// of platformcfg.go first: a name they spell is not a name the engine reads.
func engineEnvNames(t *testing.T) map[string][]string {
	t.Helper()
	literal := regexp.MustCompile(`"(ITERION_[A-Z0-9_]+)["=]`)
	bare := regexp.MustCompile(`\bITERION_[A-Z0-9_]*[A-Z0-9]\b`)
	seen := map[string][]string{}
	note := func(name, path string) {
		seen[name] = append(seen[name], strings.TrimPrefix(filepath.ToSlash(path), "../../"))
	}
	walk := func(dir string, exts []string, read func(path, src string)) {
		err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if d.Name() == "testdata" || d.Name() == "node_modules" || slices.Contains(buildOutputs, filepath.ToSlash(path)) {
					return filepath.SkipDir
				}
				return nil
			}
			if !d.Type().IsRegular() || strings.HasSuffix(path, "_test.go") || !slices.Contains(exts, filepath.Ext(path)) {
				return nil
			}
			src, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			read(path, string(src))
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", dir, err)
		}
	}
	for _, dir := range []string{"../../pkg", "../../cmd", "../../internal", "../../vendor/github.com/SocialGouv/claw-code-go"} {
		walk(dir, []string{".go"}, func(path, src string) {
			if filepath.ToSlash(path) == "../../pkg/platformcfg/platformcfg.go" {
				src = withoutDenyLists(t, src)
			}
			for _, m := range literal.FindAllStringSubmatch(src, -1) {
				note(m[1], path)
			}
		})
	}
	walk("../../pkg", []string{".yaml", ".yml", ".js", ".mjs", ".cjs", ".json"}, func(path, src string) {
		for _, name := range bare.FindAllString(src, -1) {
			note(name, path)
		}
	})
	// Each anchor is found in the tree it proves the walk reads — not
	// anywhere, which a refused name spelled by a new list would satisfy.
	for anchor, tree := range map[string]string{
		"ITERION_PERMISSION":  "pkg/",
		"ITERION_SHARD_INDEX": "cmd/",
		"ITERION_TOOL_INPUT":  "vendor/github.com/SocialGouv/claw-code-go/",
		"ITERION_RTK_BIN":     "pkg/plugin/builtin/",
		"ITERION_PI_RUN_ID":   "pkg/backend/delegate/piext/asset/",
	} {
		if !slices.ContainsFunc(seen[anchor], func(p string) bool { return strings.HasPrefix(p, tree) }) {
			t.Fatalf("the walk did not find %s under %s (found in %v) — it is not reading what the engine ships", anchor, tree, seen[anchor])
		}
	}
	return seen
}

// The build outputs the scan skips must be gitignored WHOLE — a `<dir>/*`
// line. A file force-added there would still be skipped: these directories
// hold build outputs only.
func TestTheScanSkipsOnlyGitignoredBuildOutputs(t *testing.T) {
	raw, err := os.ReadFile("../../.gitignore")
	if err != nil {
		t.Fatalf("read .gitignore: %v", err)
	}
	lines := strings.Split(string(raw), "\n")
	for _, dir := range buildOutputs {
		whole := strings.TrimPrefix(dir, "../../") + "/*"
		if !slices.ContainsFunc(lines, func(line string) bool { return strings.TrimSpace(line) == whole }) {
			t.Errorf("the scan skips %s but .gitignore has no %q line ignoring it whole", dir, whole)
		}
	}
}

// withoutDenyLists blanks every botVars… declaration of platformcfg.go: a
// name the lists spell is not a name the engine reads, and counting it would
// let every refused name find itself. Parsed, not pattern-matched, so a new
// list — however it is written — is blanked too.
func withoutDenyLists(t *testing.T, src string) string {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "platformcfg.go", src, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse platformcfg.go: %v", err)
	}
	out := []byte(src)
	cut := 0
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.VAR {
			continue
		}
		for _, spec := range gen.Specs {
			vs := spec.(*ast.ValueSpec)
			if !slices.ContainsFunc(vs.Names, func(n *ast.Ident) bool { return strings.HasPrefix(n.Name, "botVars") }) {
				continue
			}
			for i := fset.Position(vs.Pos()).Offset; i < fset.Position(vs.End()).Offset; i++ {
				if out[i] != '\n' {
					out[i] = ' '
				}
			}
			cut++
		}
	}
	if cut < 2 {
		t.Fatalf("blanked %d botVars declarations in platformcfg.go, want the prefix and exact lists at least", cut)
	}
	return string(out)
}

func TestEveryEngineEnvNameIsClassified(t *testing.T) {
	seen := engineEnvNames(t)
	names := make([]string, 0, len(seen))
	for n := range seen {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		_, listed := writableBotVars[name]
		switch refused := !botVarNameOK(name); {
		case refused && listed:
			t.Errorf("%s is refused as a bot var and listed in writableBotVars — drop one", name)
		case !refused && !listed:
			t.Errorf("%s (%s) is writable as a bot var and unclassified: if it lifts or weakens a guard, configures the process, names an endpoint or identity, or is written by the engine for a child, refuse it (botVarsInfraExact/botVarsInfraPrefixes); otherwise list it in writableBotVars with the reason", name, seen[name][0])
		}
	}
	for name := range writableBotVars {
		if _, ok := seen[name]; !ok {
			t.Errorf("writableBotVars lists %s, which the engine's source no longer spells — drop it", name)
		}
	}
}

// knobsReadFromTheEnv are shipped bots' reads of names that are not knobs —
// infrastructure, or a value the engine writes for a tool's shell — which a
// stored value may not set.
var knobsReadFromTheEnv = map[string]string{
	"ITERION_DISPATCHER_PORT":    "the dispatcher's own port, read from the pod env to reach it",
	"ITERION_TREE_NOISE":         "the engine's tree-noise pathspecs, read by a tool's shell from the env the engine writes for it",
	"ITERION_ARTIFACT_FILES_DIR": "the run's files directory, read by a tool's shell from the env the engine writes for it",
}

// shippedBotRoots hold every bot iterion ships: the catalog, the examples,
// the scaffold templates and the default dispatch bot embedded in the binary.
var shippedBotRoots = []string{"../../bots", "../../examples", "../../pkg/botscaffold/templates", "../../pkg/cli/templates/dispatch_bots_default.bot"}

// The other side of the rule: a knob a shipped bot reads through
// ${ITERION_*} is what bot vars exist to tune, so the deny list must not
// swallow one — a prefix too wide refused ITERION_FORGE_REPORT_EFFORT.
func TestEveryShippedBotKnobIsABotVar(t *testing.T) {
	// Braced or bare: the DSL fields expand $NAME through the overlay too.
	knob := regexp.MustCompile(`\$\{?(ITERION_[A-Z0-9_]*[A-Z0-9])`)
	found := 0
	read := map[string]bool{}
	for _, dir := range shippedBotRoots {
		err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".bot") && !strings.HasSuffix(path, ".bot.tmpl") {
				return nil
			}
			src, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			for _, m := range knob.FindAllStringSubmatch(string(src), -1) {
				found++
				name := m[1]
				read[name] = true
				if _, infra := knobsReadFromTheEnv[name]; infra || botVarNameOK(name) {
					continue
				}
				t.Errorf("%s reads $%s, which bot vars refuse: narrow the infra list, or list it in knobsReadFromTheEnv with the reason", strings.TrimPrefix(path, "../../"), name)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", dir, err)
		}
	}
	if found < 100 {
		t.Fatalf("found %d ${ITERION_*} reads in the shipped bots — the walk is not reading them", found)
	}
	for name := range knobsReadFromTheEnv {
		if botVarNameOK(name) {
			t.Errorf("knobsReadFromTheEnv lists %s, which bot vars accept — drop it", name)
		}
		if !read[name] {
			t.Errorf("knobsReadFromTheEnv lists %s, which no shipped bot reads — drop it", name)
		}
	}
}

// Every name the engine treats as secret — store.IsSecretEnvName, which
// redacts a run's launch env — is refused as a bot var, so a value the engine
// hides never transits the non-secret bot-var surface. Checked over every
// real name (the engine's and the shipped bots' knobs), so a word added to
// either definition is caught. The one departure is an AUTHOR segment, a
// shipped bot's knob.
func TestCredentialWordsAgreeWithTheEnginesSecretNames(t *testing.T) {
	names := map[string]bool{}
	for name := range engineEnvNames(t) {
		names[name] = true
	}
	knob := regexp.MustCompile(`\$\{?(ITERION_[A-Z0-9_]*[A-Z0-9])`)
	for _, root := range shippedBotRoots {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".bot") && !strings.HasSuffix(path, ".bot.tmpl") {
				return err
			}
			src, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			for _, m := range knob.FindAllStringSubmatch(string(src), -1) {
				names[m[1]] = true
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", root, err)
		}
	}
	for _, probe := range []string{"TOKEN", "KEY", "SECRET", "PASSWORD", "CREDENTIAL", "AUTH", "OAUTH", "AUTHORIZATION"} {
		names["ITERION_X_"+probe] = true
	}
	for name := range names {
		if !store.IsSecretEnvName(name) || slices.Contains(strings.Split(name, "_"), "AUTHOR") {
			continue
		}
		if botVarNameOK(name) {
			t.Errorf("%s is secret to the engine and writable as a bot var", name)
		}
	}
	if !botVarNameOK("ITERION_SEC_PATCH_EFFORT_AUTHOR") {
		t.Error("ITERION_SEC_PATCH_EFFORT_AUTHOR is a shipped bot's knob and must stay writable")
	}
}
