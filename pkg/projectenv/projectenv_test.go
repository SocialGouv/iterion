package projectenv

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/envtrust"
)

func TestSnapshotExpandsWithoutAShellWithoutMutatingProcess(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	if err := os.WriteFile(path, []byte("BASE_SUFFIX=${BASE}-tail\nPROJECT_ONLY='two words'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	before := os.Getenv("PROJECT_ONLY")
	env, err := Snapshot([]string{"HOME=/host/home", "BASE=head", "PATH=" + os.Getenv("PATH")}, path)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	got := toMap(env)
	if got["BASE_SUFFIX"] != "head-tail" || got["PROJECT_ONLY"] != "two words" {
		t.Fatalf("effective env = %#v", got)
	}
	if os.Getenv("PROJECT_ONLY") != before {
		t.Fatal("Snapshot mutated the parent process environment")
	}
}

func TestSnapshotRejectsReservedOverrides(t *testing.T) {
	isolateSnapshotEnv(t)
	path := filepath.Join(t.TempDir(), ".env")
	// PORT is reserved on this lane and in no plant class: its refusal is
	// the reserved check's own. HOME used to be this test's example, but it
	// is an execution-grade name now — its refusal is the class's, and the
	// reserved check stands behind it.
	if err := os.WriteFile(path, []byte("PORT=/tmp/other\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Snapshot([]string{"HOME=/host/home", "PATH=" + os.Getenv("PATH")}, path)
	if err == nil || !strings.Contains(err.Error(), "PORT") {
		t.Fatalf("reserved override error = %v", err)
	}
}

// The operator grade closes at this plant like the routing class does: the
// names the bot-var gate denies a stored record — the sandbox lifts, the
// auth and origin guards, the server's secrets — are refused here with the
// same one-line message, the operator's own value restored where they
// exported one. The H-10 review measured ITERION_SANDBOX_OVERRIDE=none and
// ITERION_DISABLE_AUTH=1 riding the run environment of every command the
// studio spawned; the grade refusal is the fix, and ITERION_HOME shows the
// refusal outranking the reserved-variable error it used to answer to.
func TestTheStudioSnapshotLeavesNoOperatorGradeNameInTheRunEnvironment(t *testing.T) {
	isolateSnapshotEnv(t)

	const operatorHome = "/host/iterion"
	base := []string{
		"HOME=/host/home", "PATH=" + os.Getenv("PATH"),
		"ITERION_HOME=" + operatorHome, // the operator's own posture
	}
	grades := []string{
		"ITERION_HOME",
		"ITERION_SANDBOX_OVERRIDE",
		"ITERION_SANDBOX_DEFAULT",
		"ITERION_DISABLE_AUTH",
		"ITERION_REQUIRE_ORIGIN",
		"ITERION_ALLOWED_ORIGINS",
		"ITERION_SECURITY_HEADERS",
		"ITERION_STUDIO_INSECURE_NONLOOPBACK",
		"ITERION_PI_TRUST_PROJECT",
		"ITERION_JWT_SECRET",
		"ITERION_BOOTSTRAP_ADMIN_PASSWORD",
		"ITERION_SERVER_TOKEN",
		"ITERION_COMPLETION_WEBHOOK_ALLOW_PRIVATE",
		"ITERION_DOTENV_ENDPOINTS",
	}
	var lines []string
	for _, name := range grades {
		lines = append(lines, name+"=planted-by-the-repo-under-review")
	}
	envPath := writeEnvFile(t, lines...)

	var env []string
	var snapErr error
	said := captureStderr(t, func() { env, snapErr = Snapshot(base, envPath) })
	if snapErr != nil {
		t.Fatalf("Snapshot: %v", snapErr)
	}
	if got, ok := envValue(env, "ITERION_HOME"); !ok || got != operatorHome {
		t.Errorf("ITERION_HOME = %q (present=%v), want the operator's own %q restored", got, ok, operatorHome)
	}
	for _, name := range grades[1:] {
		if got, ok := envValue(env, name); ok {
			t.Errorf("%s = %q in the run environment: the repository under review answered for the operator", name, got)
		}
	}
	if marker, ok := envValue(env, envtrust.EnvPlantedNames); ok {
		t.Errorf("the run environment carries a planted marker %q: nothing was planted, so nothing is distrusted", marker)
	}
	for _, name := range grades {
		if !strings.Contains(said, name) || !strings.Contains(said, envPath) {
			t.Errorf("the refusal does not name %s and the file — stderr said: %s", name, said)
		}
		if strings.Contains(said, envtrust.DotenvEndpointsOptIn+"=allow to let this file plant its routes") {
			t.Errorf("the operator-grade refusal offers the opt-in (stderr: %s): the opt-in widens routes, never the operator's posture", said)
		}
	}
}

// The command substitution a value spells is refused, not run: the studio
// snapshot was the lane whose sourcing shell EXECUTED `FOO=$(touch …)` on
// the host (the H-10 probe measured the marker file created, before the
// routing question was even asked). The parse-first loader decides before
// any shell exists — the command in the file never runs, the value does not
// exist, and the refusal names the file.
func TestTheStudioSnapshotRunsNothingACommandSubstitutionSpells(t *testing.T) {
	isolateSnapshotEnv(t)

	marker := filepath.Join(t.TempDir(), "substituted-ran")
	envPath := writeEnvFile(t,
		"FIRST=ok",
		"FOO=$(touch "+marker+")",
		"BAR=`touch "+marker+"`",
		"QUOTED=\"$(touch "+marker+")\"",
		"LAST=also-ok",
	)
	var env []string
	var snapErr error
	said := captureStderr(t, func() { env, snapErr = Snapshot([]string{"HOME=/host/home", "PATH=" + os.Getenv("PATH")}, envPath) })
	if snapErr != nil {
		t.Fatalf("Snapshot: %v", snapErr)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("the substitution marker exists: the file's command RAN on the host (stat err %v)", err)
	}
	for _, name := range []string{"FOO", "BAR", "QUOTED"} {
		if _, ok := envValue(env, name); ok {
			t.Errorf("%s reached the run environment: a refused value was applied", name)
		}
	}
	if got, ok := envValue(env, "FIRST"); !ok || got != "ok" {
		t.Errorf("FIRST = %q (present=%v): the refusals must not cost the file its ordinary plants", got, ok)
	}
	if got, ok := envValue(env, "LAST"); !ok || got != "also-ok" {
		t.Errorf("LAST = %q (present=%v): the refusals must not cost the file its ordinary plants", got, ok)
	}
	for _, name := range []string{"FOO", "BAR", "QUOTED"} {
		if !strings.Contains(said, name) || !strings.Contains(said, envPath) {
			t.Errorf("the substitution refusal does not name %s and the file — stderr said: %s", name, said)
		}
	}
}

// One parse, one plant set: an unquoted value with spaces is ONE value on
// this lane, exactly what the CLI's parser reads on the other one — the
// sourcing shell used to read `VAR=a b` as an assignment plus a command
// and lose the assignment with the command that never existed (the H-10
// probe measured the silent loss).
func TestTheStudioSnapshotPlantsWhatTheParserRead(t *testing.T) {
	isolateSnapshotEnv(t)

	envPath := writeEnvFile(t,
		"FIRST=ok",
		"SPACED=ssh -i /tmp/some-key",
		"LAST=also-ok",
	)
	env, err := Snapshot([]string{"HOME=/host/home", "PATH=" + os.Getenv("PATH")}, envPath)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if got, ok := envValue(env, "SPACED"); !ok || got != "ssh -i /tmp/some-key" {
		t.Errorf("SPACED = %q (present=%v), want the whole value the parser read", got, ok)
	}
	if got, ok := envValue(env, "FIRST"); !ok || got != "ok" {
		t.Errorf("FIRST = %q (present=%v)", got, ok)
	}
	if got, ok := envValue(env, "LAST"); !ok || got != "also-ok" {
		t.Errorf("LAST = %q (present=%v)", got, ok)
	}
}

// The legacy frame survives where it was wanted: `$VAR`/`${VAR}` expand on
// the base environment and on the file's own earlier assignments, and a
// single-quoted value stays literal — the sourcing shell used to give
// both, minus the execution the refusal removed. A dollar the file itself
// escaped stays literal too.
func TestTheStudioSnapshotStillExpandsTheWantedReferences(t *testing.T) {
	isolateSnapshotEnv(t)

	envPath := writeEnvFile(t,
		"BASE_SUFFIX=${BASE}-tail",
		"PROJECT_ONLY='two words'",
		"JOINED=$PROJECT_ONLY/tail",
		"LITERAL=\\$NOT_DEFINED_ANYWHERE",
		"ESCAPED=\\$HOME",
	)
	env, err := Snapshot([]string{"HOME=/host/home", "BASE=head", "PATH=" + os.Getenv("PATH")}, envPath)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	for key, want := range map[string]string{
		"BASE_SUFFIX":  "head-tail",
		"PROJECT_ONLY": "two words",
		"JOINED":       "two words/tail",
		"LITERAL":      "$NOT_DEFINED_ANYWHERE",
		"ESCAPED":      "$HOME",
	} {
		if got, ok := envValue(env, key); !ok || got != want {
			t.Errorf("%s = %q (present=%v), want %q", key, got, ok, want)
		}
	}
}

// isolateSnapshotEnv puts the operator's own environment under the test's
// control: the snapshot refuses against the environment the studio process
// INHERITED (the live process env here), and the provenance questions read
// the planted set.
func isolateSnapshotEnv(t *testing.T) {
	t.Helper()
	envtrust.ResetForTest()
	t.Cleanup(envtrust.ResetForTest)
	t.Setenv(envtrust.EnvPlantedNames, "")
	t.Setenv(envtrust.DotenvEndpointsOptIn, "")
}

// writeEnvFile writes the project `.env` and returns its path.
func writeEnvFile(t *testing.T, lines ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatalf("write env file: %v", err)
	}
	return path
}

// captureStderr runs fn with os.Stderr on a pipe and returns what it wrote:
// the snapshot's refusals go to stderr directly, where no log level —
// least of all one the file itself planted — reaches them.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stderr = w
	fn()
	os.Stderr = orig
	if err := w.Close(); err != nil {
		t.Fatalf("close the pipe: %v", err)
	}
	said, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read the pipe: %v", err)
	}
	if err := r.Close(); err != nil {
		t.Fatalf("close the read end: %v", err)
	}
	return string(said)
}

// envValue looks a key up in a returned environment slice.
func envValue(env []string, key string) (string, bool) {
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, key+"="); ok {
			return v, true
		}
	}
	return "", false
}

// The studio lane closes the same class at its own plant (the H-10 finding:
// this lane applied the project `.env`'s routes to every host-spawned
// command of a run, without a guard, a mark or a diagnostic). A routing name
// the file names does not reach the run environment: the operator's own
// value survives where they exported one, a name only the file carried does
// not exist at all, and every refusal names the file, the key and the
// opt-in — on stderr, where the log level the file itself may have planted
// a line above reaches nothing.
func TestTheStudioSnapshotLeavesNoRoutingNameInTheRunEnvironment(t *testing.T) {
	isolateSnapshotEnv(t)

	const operatorHost = "https://operator.example"
	base := []string{
		"HOME=/host/home", "PATH=" + os.Getenv("PATH"),
		"OPENAI_BASE_URL=" + operatorHost, // the operator's own route
	}
	// One route of every family the widened class answers for on this lane:
	// the tour-1 shapes, and the _URL/_URI/_ADDRS/_HOSTS/_DSN family the
	// H-10 review measured planted silently before it.
	routes := []string{
		"OPENAI_BASE_URL",
		"ITERION_OAUTH_FORFAIT_ANTHROPIC_TOKEN_URL",
		"AWS_ENDPOINT_URL_BEDROCK_RUNTIME",
		"ITERION_MONGO_URI",
		"ITERION_REDIS_SENTINEL_ADDRS",
		"ITERION_SANDBOX_MODEL_HOSTS",
		"SENTRY_DSN",
		"SEARXNG_URL",
		"HTTP_PROXY",
	}
	var lines []string
	for _, name := range routes {
		lines = append(lines, name+"=https://repo-under-review.example/planted")
	}
	envPath := writeEnvFile(t, lines...)

	var env []string
	var snapErr error
	said := captureStderr(t, func() { env, snapErr = Snapshot(base, envPath) })
	if snapErr != nil {
		t.Fatalf("Snapshot: %v", snapErr)
	}

	if got, ok := envValue(env, "OPENAI_BASE_URL"); !ok || got != operatorHost {
		t.Errorf("OPENAI_BASE_URL = %q (present=%v), want the operator's own %q restored: the file's line took their route", got, ok, operatorHost)
	}
	for _, name := range routes[1:] {
		if got, ok := envValue(env, name); ok {
			t.Errorf("%s = %q in the run environment: the repository under review chose where a connection goes", name, got)
		}
	}
	if marker, ok := envValue(env, envtrust.EnvPlantedNames); ok {
		t.Errorf("the run environment carries a planted marker %q: nothing was planted, so nothing is distrusted", marker)
	}
	for _, name := range routes {
		for _, want := range []string{name, envPath, envtrust.DotenvEndpointsOptIn + "=allow"} {
			if !strings.Contains(said, want) {
				t.Errorf("the refusal does not say %q — it said: %s", want, said)
			}
		}
	}
}

// The opt-in never crosses into the run environment, whoever said it: the
// file's line granted nothing before, and the operator's own export — which
// DID answer the studio process — answers the studio process only, not the
// runs launched with what the snapshot returns.
func TestTheStudioSnapshotNeverCarriesTheOptIn(t *testing.T) {
	isolateSnapshotEnv(t)

	const repoHost = "https://repo-under-review.example"
	// The file's own opt-in: granted nothing, and does not travel.
	envPath := writeEnvFile(t,
		"ITERION_DOTENV_ENDPOINTS=allow",
		"OPENAI_BASE_URL="+repoHost,
	)
	var env []string
	var snapErr error
	said := captureStderr(t, func() { env, snapErr = Snapshot([]string{"HOME=/host/home", "PATH=" + os.Getenv("PATH")}, envPath) })
	if snapErr != nil {
		t.Fatalf("Snapshot: %v", snapErr)
	}
	if v, ok := envValue(env, envtrust.DotenvEndpointsOptIn); ok {
		t.Errorf("the file's %s=%q crossed to the run environment: the file consented on the operator's behalf", envtrust.DotenvEndpointsOptIn, v)
	}
	if _, ok := envValue(env, "OPENAI_BASE_URL"); ok {
		t.Error("the route was applied: the file's own opt-in granted it")
	}
	for _, want := range []string{envtrust.DotenvEndpointsOptIn, envPath} {
		if !strings.Contains(said, want) {
			t.Errorf("the diagnostic does not say %q — it said: %s", want, said)
		}
	}

	// The operator's own export, inherited by the studio process — the
	// live process env, since workspace_studio passes os.Environ(): it
	// answers the refusals above (the routes plant, marked planted) and
	// still does not cross.
	t.Setenv(envtrust.DotenvEndpointsOptIn, "allow")
	optInBase := []string{"HOME=/host/home", "PATH=" + os.Getenv("PATH"), envtrust.DotenvEndpointsOptIn + "=allow"}
	routesPath := writeEnvFile(t, "OPENAI_BASE_URL="+repoHost)
	var env2 []string
	said2 := captureStderr(t, func() { env2, snapErr = Snapshot(optInBase, routesPath) })
	if snapErr != nil {
		t.Fatalf("Snapshot with the inherited opt-in: %v", snapErr)
	}
	if got, ok := envValue(env2, "OPENAI_BASE_URL"); !ok || got != repoHost {
		t.Errorf("OPENAI_BASE_URL = %q (present=%v), want the file's %q: the operator's own opt-in grants the laptop config", got, ok, repoHost)
	}
	marker, ok := envValue(env2, envtrust.EnvPlantedNames)
	if !ok || !strings.Contains(marker, "OPENAI_BASE_URL") {
		t.Errorf("the planted routes do not carry the mark (marker %q, present %v): a run would read them as the operator's own", marker, ok)
	}
	if v, ok := envValue(env2, envtrust.DotenvEndpointsOptIn); ok {
		t.Errorf("the operator's %s=%q crossed to the run environment: the runs answer their own opt-in where they launch", envtrust.DotenvEndpointsOptIn, v)
	}
	if !strings.Contains(said2, envtrust.DotenvEndpointsOptIn+" does not cross") {
		t.Errorf("the boundary the export stops at is not said — stderr: %s", said2)
	}
	if strings.Contains(said2, "OPENAI_BASE_URL") {
		t.Errorf("a granted plant was warned about: %s", said2)
	}
}

// What the file DOES plant keeps the provenance mark: the names travel in
// EnvPlantedNames inside the returned environment, merged onto the marker
// the studio itself inherited, so a run's own envtrust reads them planted —
// exactly what a CLI-loaded `.env` reads — instead of taking them for the
// operator's own.
func TestTheStudioSnapshotMarksWhatItPlants(t *testing.T) {
	isolateSnapshotEnv(t)

	base := []string{"HOME=/host/home", "PATH=" + os.Getenv("PATH"), envtrust.EnvPlantedNames + "=ITERION_HOME"}
	envPath := writeEnvFile(t, "OPENAI_API_KEY=sk-next-to-the-project")

	var env []string
	var snapErr error
	said := captureStderr(t, func() { env, snapErr = Snapshot(base, envPath) })
	if snapErr != nil {
		t.Fatalf("Snapshot: %v", snapErr)
	}
	if got, ok := envValue(env, "OPENAI_API_KEY"); !ok || got != "sk-next-to-the-project" {
		t.Errorf("OPENAI_API_KEY = %q (present=%v): a key next to a project is what the file is for", got, ok)
	}
	marker, ok := envValue(env, envtrust.EnvPlantedNames)
	if !ok {
		t.Fatal("the run environment carries no planted marker: the file's plant is unmarked")
	}
	for _, want := range []string{"ITERION_HOME", "OPENAI_API_KEY"} {
		if !strings.Contains(marker, want) {
			t.Errorf("the marker does not carry %s (marker: %s): the inherited mark would be lost, or the plant unmarked", want, marker)
		}
	}
	if said != "" {
		t.Errorf("an ordinary plant was warned about: %s", said)
	}
}

// The marker is what a loader records, never what a file declares: a file
// line naming ITERION_DOTENV_PLANTED is dropped and the studio's inherited
// marker travels on — one seeded marker would strip the operator's own
// environment of its authority in the run.
func TestTheStudioSnapshotNeverTakesTheMarkerFromTheFile(t *testing.T) {
	isolateSnapshotEnv(t)

	base := []string{"HOME=/host/home", "PATH=" + os.Getenv("PATH"), envtrust.EnvPlantedNames + "=ITERION_HOME"}
	envPath := writeEnvFile(t, envtrust.EnvPlantedNames+"=OPENAI_API_KEY,ANTHROPIC_API_KEY")

	var env []string
	var snapErr error
	said := captureStderr(t, func() { env, snapErr = Snapshot(base, envPath) })
	if snapErr != nil {
		t.Fatalf("Snapshot: %v", snapErr)
	}
	marker, ok := envValue(env, envtrust.EnvPlantedNames)
	if !ok || marker != "ITERION_HOME" {
		t.Errorf("the run marker = %q (present=%v), want the operator's own %q: the file seeded the planted set", marker, ok, "ITERION_HOME")
	}
	if said != "" {
		t.Errorf("the dropped marker line was warned about: %s", said)
	}
}

// The prompt expansion the round-6 review measured: `KEY=${BAR@P}` planted
// by the file made the expansion bash run the substitutions the OPERATOR's
// own BAR carried — the marker was created by the studio's own child shell,
// the bytes never spelled it. No shell exists on this lane now, and the
// belt refuses the operator form outright: KEY does not exist, the refusal
// names the file, and the operator's BAR rides untouched.
func TestTheStudioSnapshotRunsNothingAValueExpansionSpells(t *testing.T) {
	isolateSnapshotEnv(t)

	marker := filepath.Join(t.TempDir(), "prompt-expanded")
	base := []string{
		"HOME=/host/home",
		"PATH=" + os.Getenv("PATH"),
		"BAR=$(touch " + marker + ")", // the operator's own value
	}
	envPath := writeEnvFile(t, "KEY=${BAR@P}")
	var env []string
	var snapErr error
	said := captureStderr(t, func() { env, snapErr = Snapshot(base, envPath) })
	if snapErr != nil {
		t.Fatalf("Snapshot: %v", snapErr)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("the expansion marker exists: a substitution ran (stat err %v)", err)
	}
	if _, ok := envValue(env, "KEY"); ok {
		t.Errorf("KEY reached the run environment: an operator form was planted")
	}
	if got, ok := envValue(env, "BAR"); !ok || got != "$(touch "+marker+")" {
		t.Errorf("BAR = %q (present=%v), want the operator's own untouched", got, ok)
	}
	for _, want := range []string{"KEY", envPath} {
		if !strings.Contains(said, want) {
			t.Errorf("the refusal does not say %q — it said: %s", want, said)
		}
	}
}

// The execution grade closes at this plant like the routing class before
// it: BASH_ENV sourced a tool-node's bash, LD_PRELOAD preloaded the
// planter's object into every engine child, PATH and XDG_CONFIG_HOME moved
// what the children resolve — all planted silently before the round-6
// review measured it (every name below read plant-refused=false on the
// defect). Refused now, one predicate, the operator's own value restored
// where they exported one. GIT_SSH_COMMAND keeps reading as the routing
// class it has always been, and a name of both classes reads the routing
// line alone: the union, no duplicate message.
func TestTheStudioSnapshotLeavesNoExecutionGradeNameInTheRunEnvironment(t *testing.T) {
	isolateSnapshotEnv(t)

	const operatorPath = "/operator/bin"
	base := []string{
		"HOME=/host/home", "PATH=" + operatorPath, "XDG_CONFIG_HOME=/operator/xdg",
	}
	family := []string{
		"BASH_ENV", "ENV", "PS4", "SHELLOPTS", "BASHOPTS",
		"LD_PRELOAD", "LD_LIBRARY_PATH", "OPENSSL_CONF",
		"NODE_OPTIONS", "BUN_OPTIONS", "NPM_CONFIG_NODE_OPTIONS",
		"PATH", "IFS", "GCONV_PATH", "ZDOTDIR", "SHELL",
		"GOFLAGS", "GOENV", "GOTOOLCHAIN", "GOROOT", "GOAUTH",
		"SSH_ASKPASS", "EDITOR", "VISUAL", "TAR_OPTIONS",
		"XDG_CONFIG_HOME", "NIX_CONFIG", "HOME",
		"KUBECONFIG", "DOCKER_CONFIG", "SSL_CERT_FILE", "NODE_EXTRA_CA_CERTS",
		"GIT_ASKPASS", "GIT_EXEC_PATH", "VITALS_EMITTER_BIN", "CLAUDE_CONFIG_DIR",
		// The union witness: an execution prefix composed with a routing
		// suffix. Refused by the routing branch, so the refusal reads the
		// routing reason — one line, not two.
		"NIX_STORE_URL",
	}
	var lines []string
	for _, name := range family {
		lines = append(lines, name+"=/planted/by/the/repo-under-review")
	}
	envPath := writeEnvFile(t, lines...)

	var env []string
	var snapErr error
	said := captureStderr(t, func() { env, snapErr = Snapshot(base, envPath) })
	if snapErr != nil {
		t.Fatalf("Snapshot: %v", snapErr)
	}
	for _, name := range family {
		if got, ok := envValue(env, name); ok && name != "PATH" && name != "HOME" && name != "XDG_CONFIG_HOME" {
			t.Errorf("%s = %q in the run environment: the repository under review chose what iterion's children execute", name, got)
		}
	}
	if got, ok := envValue(env, "PATH"); !ok || got != operatorPath {
		t.Errorf("PATH = %q (present=%v), want the operator's own %q restored", got, ok, operatorPath)
	}
	if got, ok := envValue(env, "XDG_CONFIG_HOME"); !ok || got != "/operator/xdg" {
		t.Errorf("XDG_CONFIG_HOME = %q (present=%v), want the operator's own restored", got, ok)
	}
	if got, ok := envValue(env, "HOME"); !ok || got != "/host/home" {
		t.Errorf("HOME = %q (present=%v), want the operator's own restored", got, ok)
	}
	if marker, ok := envValue(env, envtrust.EnvPlantedNames); ok {
		t.Errorf("the run environment carries a planted marker %q: nothing was planted, so nothing is distrusted", marker)
	}
	for _, name := range family {
		if !strings.Contains(said, name) || !strings.Contains(said, envPath) {
			t.Errorf("the refusal does not name %s and the file — stderr said: %s", name, said)
		}
	}
	if !strings.Contains(said, "what iterion's children execute or trust") {
		t.Errorf("the execution refusal does not say why — stderr said: %s", said)
	}
	var askpassLine string
	for _, line := range strings.Split(said, "\n") {
		if strings.Contains(line, "GIT_ASKPASS comes from") {
			askpassLine = line
		}
	}
	if askpassLine == "" || !strings.Contains(askpassLine, "execute or trust") || strings.Contains(askpassLine, "plant its routes") {
		t.Errorf("GIT_ASKPASS must read as execution, its own line (got %q)", askpassLine)
	}
	if !strings.Contains(said, "NIX_STORE_URL") || !strings.Contains(said, "plant its routes") {
		t.Errorf("the routing/execution union must read the routing line alone — stderr said: %s", said)
	}
}

// The opt-in is the escape for the execution class too, and the one
// measured-legitimate form rides it: `PATH=$PATH:./bin` extends the
// operator's path on the lane where expansion exists. The plant carries
// the provenance mark, so the run reads it planted.
func TestTheStudioSnapshotOptInLetsTheMeasuredLegitimatePathThrough(t *testing.T) {
	isolateSnapshotEnv(t)

	// The grant is read from the environment the studio INHERITED (the live
	// process env here), like every opt-in answer.
	t.Setenv(envtrust.DotenvEndpointsOptIn, "allow")
	base := []string{"HOME=/host/home", "PATH=/operator/bin", envtrust.DotenvEndpointsOptIn + "=allow"}
	envPath := writeEnvFile(t, "PATH=$PATH:./bin")
	var env []string
	var snapErr error
	said := captureStderr(t, func() { env, snapErr = Snapshot(base, envPath) })
	if snapErr != nil {
		t.Fatalf("Snapshot: %v", snapErr)
	}
	if got, ok := envValue(env, "PATH"); !ok || got != "/operator/bin:./bin" {
		t.Errorf("PATH = %q (present=%v), want the operator's path extended by the file", got, ok)
	}
	marker, ok := envValue(env, envtrust.EnvPlantedNames)
	if !ok || !strings.Contains(marker, "PATH") {
		t.Errorf("the planted PATH carries no mark (marker %q, present %v)", marker, ok)
	}
	if v, ok := envValue(env, envtrust.DotenvEndpointsOptIn); ok {
		t.Errorf("the opt-in crossed into the run environment (%q): the export answers the studio, not the runs", v)
	}
	// The only thing said is the boundary the export stops at — no refusal
	// of the granted plant itself.
	if !strings.Contains(said, envtrust.DotenvEndpointsOptIn+" does not cross") {
		t.Errorf("the boundary the export stops at is not said — stderr: %s", said)
	}
	if strings.Contains(said, "PATH comes from") {
		t.Errorf("a granted plant was warned about: %s", said)
	}
}

// What the bash frame used to act on is bytes now: an operator form, a
// default form, a doubled dollar, a backslash before a non-special byte —
// the same bytes the CLI lane plants, the same bytes a sourcing shell kept
// before a non-special character. The lanes stopped diverging.
func TestTheStudioSnapshotExpansionKeepsEverythingElseLiteral(t *testing.T) {
	isolateSnapshotEnv(t)

	// (The operator form ${NOPE@P} is NOT here: the belt refuses it — that
	// is the expansion test above. What is here are the forms a shell would
	// act on but the belt does not refuse.)
	envPath := writeEnvFile(t,
		"DEFAULT_FORM=${NOPE:-fallback}",
		"ANSI=$'A'",
		"DOLLARS=a$$b",
		"BACKSLASH=a\\z b",
		"ESCAPED=\\$HOME",
	)
	env, err := Snapshot([]string{"HOME=/host/home", "PATH=" + os.Getenv("PATH")}, envPath)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	for key, want := range map[string]string{
		"DEFAULT_FORM": "${NOPE:-fallback}",
		"ANSI":         "$'A'",
		"DOLLARS":      "a$$b",
		"BACKSLASH":    `a\z b`,
		"ESCAPED":      "$HOME",
	} {
		if got, ok := envValue(env, key); !ok || got != want {
			t.Errorf("%s = %q (present=%v), want the literal %q", key, got, ok, want)
		}
	}
}

// An expansion can INTRODUCE a trigger the file never spelled: the base
// value a `$VAR` pulls in may carry `$(…)` bytes. The belt asks the
// EXPANDED value, so the reference is refused and the operator's own value
// rides — the trigger reaches no run either way.
func TestTheStudioSnapshotRefusesATriggerAnExpansionIntroduces(t *testing.T) {
	isolateSnapshotEnv(t)

	marker := filepath.Join(t.TempDir(), "introduced-ran")
	base := []string{
		"HOME=/host/home",
		"PATH=" + os.Getenv("PATH"),
		"BAR=$(touch " + marker + ")",
	}
	envPath := writeEnvFile(t, "KEY=prefix-$BAR-suffix")
	var env []string
	var snapErr error
	said := captureStderr(t, func() { env, snapErr = Snapshot(base, envPath) })
	if snapErr != nil {
		t.Fatalf("Snapshot: %v", snapErr)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("the introduced marker exists: a substitution ran (stat err %v)", err)
	}
	if _, ok := envValue(env, "KEY"); ok {
		t.Errorf("KEY reached the run environment: the trigger travelled")
	}
	if got, ok := envValue(env, "BAR"); !ok || got != "$(touch "+marker+")" {
		t.Errorf("BAR = %q (present=%v), want the operator's own untouched", got, ok)
	}
	for _, want := range []string{"KEY", envPath} {
		if !strings.Contains(said, want) {
			t.Errorf("the refusal does not say %q — it said: %s", want, said)
		}
	}
}
