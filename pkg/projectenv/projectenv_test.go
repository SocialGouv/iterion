package projectenv

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/envtrust"
)

func TestSnapshotEvaluatesShellWithoutMutatingProcess(t *testing.T) {
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
	path := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(path, []byte("ITERION_HOME=/tmp/other\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Snapshot([]string{"HOME=/host/home", "ITERION_HOME=/host/iterion", "PATH=" + os.Getenv("PATH")}, path)
	if err == nil || !strings.Contains(err.Error(), "ITERION_HOME") {
		t.Fatalf("reserved override error = %v", err)
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
