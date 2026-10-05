package config

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"
)

const scrubHelperEnv = "ITERION_SCRUB_TEST_HELPER"

// TestScrubHelperProcess is not a test: TestScrubPlatformSecretsLeavesNothingAChildCanRead
// runs the test binary as a fresh process whose BOOT environment holds the
// platform's secrets, and this is that process.
func TestScrubHelperProcess(t *testing.T) {
	if os.Getenv(scrubHelperEnv) != "1" {
		t.Skip("helper process only")
	}
	if err := ScrubPlatformSecrets(); err != nil {
		fmt.Println("SCRUB-ERROR", err)
		os.Exit(0)
	}
	for _, name := range PlatformSecretEnv {
		if _, ok := os.LookupEnv(name); ok {
			fmt.Println("STILL-SET", name)
		}
	}
	// A tool child, built the way the executor builds one.
	child := exec.Command("sh", "-c", "env; cat /proc/$PPID/environ")
	child.Env = os.Environ()
	out, _ := child.CombinedOutput()
	fmt.Println("CHILD-READ-BEGIN")
	fmt.Println(string(out))
	fmt.Println("CHILD-READ-END")
	os.Exit(0)
}

// Once a cloud process has booted, its own credentials are in none of the
// places a workflow, a shell or a child can read: not its environment, not the
// boot environment /proc serves.
func TestScrubPlatformSecretsLeavesNothingAChildCanRead(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("/proc/<pid>/environ is Linux-only")
	}
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	sentinel := "scrub-sentinel-" + hex.EncodeToString(b)
	cmd := exec.Command(os.Args[0], "-test.run=^TestScrubHelperProcess$", "-test.count=1")
	// Built by hand: no value of the host's own environment reaches the
	// helper, so nothing real can surface in this test's output.
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir(), scrubHelperEnv + "=1"}
	for _, name := range PlatformSecretEnv {
		cmd.Env = append(cmd.Env, name+"="+sentinel)
	}
	raw, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("helper process: %v", err)
	}
	out := string(raw)
	if strings.Contains(out, "SCRUB-ERROR") {
		t.Fatalf("ScrubPlatformSecrets failed in the helper: %s", strings.SplitN(out, "\n", 2)[0])
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "STILL-SET ") {
			t.Errorf("%s is still in the process environment", strings.TrimPrefix(line, "STILL-SET "))
		}
	}
	if !strings.Contains(out, "CHILD-READ-BEGIN") {
		t.Fatal("the helper never tried the child read: the test proves nothing")
	}
	if n := strings.Count(out, sentinel); n > 0 {
		t.Fatalf("a child of the same user read the platform's secrets back from /proc (%d occurrence(s))", n)
	}
}

// The names prod sets, each pinned here with what it carries: the scrub test
// above iterates PlatformSecretEnv, so a name dropped from the list would
// take its own check with it.
func TestPlatformSecretEnvCarriesTheNamesProdSets(t *testing.T) {
	have := map[string]bool{}
	for _, n := range PlatformSecretEnv {
		have[n] = true
	}
	for name, what := range map[string]string{
		"ITERION_SECRETS_KEY":                  "the key every stored secret is sealed with",
		"ITERION_SECRETS_KEYS":                 "the whole rotation ring of that key, every past key included",
		"ITERION_SECRETS_KEY_ID":               "the id of the current sealing key",
		"ITERION_JWT_SECRET":                   "the signing secret of every session",
		"ITERION_MONGO_URI":                    "a connection string carrying its own credentials",
		"ITERION_NATS_URL":                     "a connection string carrying its own credentials",
		"ITERION_REDIS_URL":                    "a connection string carrying its own credentials",
		"ITERION_FORGE_GITHUB_APP_PRIVATE_KEY": "the App key every forge token is minted from",
		"ITERION_COMPLETION_WEBHOOK_SECRET":    "what authenticates a run's completion callback",
		"SENTRY_DSN":                           "a write credential for the deployment's error tracker",
		"OTEL_EXPORTER_OTLP_HEADERS":           "where a collector's auth goes",
		"OTEL_EXPORTER_OTLP_TRACES_HEADERS":    "where a collector's auth goes",
		"ITERION_BOOTSTRAP_ADMIN_EMAIL":        "the super-admin's identity",
	} {
		if !have[name] {
			t.Errorf("%s is not scrubbed: it carries %s", name, what)
		}
	}
}
