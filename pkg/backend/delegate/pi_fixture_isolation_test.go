package delegate

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestPiBinaryFixtureOwnsAgentDirectories(t *testing.T) {
	// Resolve an explicitly supplied binary without requiring pi on this
	// machine. These are configuration assertions, not a protocol smoke test.
	bin := filepath.Join(t.TempDir(), "pi")
	t.Setenv("ITERION_PI_BIN", bin)
	operator := t.TempDir()
	native := t.TempDir()
	t.Setenv("ITERION_PI_AGENT_DIR", operator)
	t.Setenv("PI_CODING_AGENT_DIR", native)
	for _, dir := range []string{operator, native} {
		if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(`{"extensions":["operator-extension"]}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var owned string
	t.Run("scenario", func(t *testing.T) {
		if got := requirePiBinary(t); got != bin {
			t.Fatalf("binary = %q, want explicit %q", got, bin)
		}
		owned = os.Getenv("ITERION_PI_AGENT_DIR")
		if owned == "" || owned == operator || owned == native {
			t.Fatalf("fixture still reads operator configuration: %q", owned)
		}
		if got := os.Getenv("PI_CODING_AGENT_DIR"); got != owned {
			t.Fatalf("direct pi reads %q, iterion reads %q", got, owned)
		}
		if got := piAgentDir(); got != owned {
			t.Fatalf("ambient context reads %q, want fixture %q", got, owned)
		}
		if got := piResolveEnv(context.Background())["PI_CODING_AGENT_DIR"]; got != owned {
			t.Fatalf("backend pins %q, want fixture %q", got, owned)
		}
		entries, err := os.ReadDir(owned)
		if err != nil || len(entries) != 0 {
			t.Fatalf("fixture directory is not empty: entries=%v err=%v", entries, err)
		}
	})
	if os.Getenv("ITERION_PI_AGENT_DIR") != operator || os.Getenv("PI_CODING_AGENT_DIR") != native {
		t.Fatal("scenario configuration leaked into the next test")
	}
	if owned != "" {
		if _, err := os.Stat(owned); !os.IsNotExist(err) {
			t.Fatalf("scenario did not clean its directory: %v", err)
		}
	}
	for _, dir := range []string{operator, native} {
		data, err := os.ReadFile(filepath.Join(dir, "settings.json"))
		if err != nil || string(data) != `{"extensions":["operator-extension"]}` {
			t.Fatalf("operator configuration changed: %q, %v", data, err)
		}
	}
}

// Run the whole real-binary family under two owned operator profiles. Each
// profile's extension writes a canary if any scenario reads that profile,
// even when pi would otherwise continue successfully after loading it.
func TestPiLiveWitnessesOwnAgentConfiguration(t *testing.T) {
	bin := requirePiBinary(t)
	var dirs, markers []string
	for range 2 {
		dir := t.TempDir()
		marker := filepath.Join(dir, "operator-extension-read")
		extDir := filepath.Join(dir, "extensions")
		if err := os.Mkdir(extDir, 0o700); err != nil {
			t.Fatal(err)
		}
		source := "import { writeFileSync } from \"node:fs\";\nwriteFileSync(" + strconv.Quote(marker) + ", \"loaded\");\nexport default function () {}\n"
		if err := os.WriteFile(filepath.Join(extDir, "canary.ts"), []byte(source), 0o600); err != nil {
			t.Fatal(err)
		}
		// Prove the canary loads in this actual pi version before using its
		// absence as evidence. This control uses only the scripted provider.
		runNativePiFixture(t, bin, "PI_CODING_AGENT_DIR="+dir)
		if data, err := os.ReadFile(marker); err != nil || string(data) != "loaded" {
			t.Fatalf("native pi did not load the planted operator extension: %q, %v", data, err)
		}
		if err := os.Remove(marker); err != nil {
			t.Fatal(err)
		}
		dirs = append(dirs, dir)
		markers = append(markers, marker)
	}
	out, err := fixtureTestOutput(t, []string{"-test.run=^TestPi(Smoke|RPCLive)", "-test.count=1", "-test.v"}, func(cmd *exec.Cmd) {
		cmd.Env = append(os.Environ(), "ITERION_PI_AGENT_DIR="+dirs[0], "PI_CODING_AGENT_DIR="+dirs[1])
	})
	if err != nil {
		t.Fatalf("a real pi fixture fails with operator configuration present: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "--- PASS: TestPiSmokeNativeAgentDirectory") || !strings.Contains(string(out), "--- PASS: TestPiRPCLiveAskUser") {
		t.Fatalf("child did not exercise both native and RPC sessions:\n%s", out)
	}
	for _, marker := range markers {
		if _, err := os.Stat(marker); !os.IsNotExist(err) {
			t.Fatalf("a real pi fixture reads an operator agent directory: marker=%s err=%v", marker, err)
		}
	}
}

// This direct session inherits the fixture environment instead of the
// backend's ResolveEnv projection or a command-specific agent-directory pin.
func TestPiSmokeNativeAgentDirectory(t *testing.T) {
	runNativePiFixture(t, requirePiBinary(t))
}

func runNativePiFixture(t *testing.T, bin string, extraEnv ...string) {
	t.Helper()
	out, err := fixtureProcessOutput(t, bin, []string{"--mode", "json", "--no-approve", "-e", mockProviderPath(t), "--model", "mock/scripted"}, func(cmd *exec.Cmd) {
		cmd.Dir = t.TempDir()
		cmd.Stdin = strings.NewReader("reply with the scripted fixture answer")
		cmd.Env = append(os.Environ(), "ITERION_PI_MOCK_TEXT=fixture-owned", "ITERION_PI_MOCK_TOOL=", "ITERION_PI_MOCK_STOP=stop", "ITERION_PI_MOCK_ERROR=")
		cmd.Env = append(cmd.Env, extraEnv...)
	})
	if err != nil || !strings.Contains(string(out), "fixture-owned") {
		t.Fatalf("native fixture session: %v\n%s", err, out)
	}
}
