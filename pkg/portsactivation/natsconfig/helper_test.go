package natsconfig

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	natsclient "github.com/nats-io/nats.go"
)

func TestNATSConfigHelperProcess(t *testing.T) {
	if slices.Contains(os.Args, "--nats-config-stall-test") {
		if os.WriteFile("private.conf", []byte("fixture source bytes"), 0o600) != nil {
			os.Exit(1)
		}
		for {
			time.Sleep(time.Second)
		}
	}
	if !slices.Contains(os.Args, "--nats-config-helper-test") {
		return
	}
	if err := RunHelper(os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(0)
}

func parseFixture(ctx context.Context, s Sources) (*Result, error) {
	return parseWithExecutable(ctx, os.Args[0], []string{"-test.run=^TestNATSConfigHelperProcess$", "--", "--nats-config-helper-test"}, s)
}

func TestNATSConfigSourcesConfineIncludes(t *testing.T) {
	for name, sources := range map[string]Sources{
		"absolute entry":       {Entry: "/tmp/main.conf", Files: map[string]string{"/tmp/main.conf": "port: 4222"}},
		"parent entry":         {Entry: "../main.conf", Files: map[string]string{"../main.conf": "port: 4222"}},
		"missing entry":        {Entry: "main.conf", Files: map[string]string{"other.conf": "port: 4222"}},
		"absolute include":     {Entry: "main.conf", Files: map[string]string{"main.conf": "include \"/etc/passwd\""}},
		"parent include":       {Entry: "main.conf", Files: map[string]string{"main.conf": "include \"../private.conf\""}},
		"missing include":      {Entry: "main.conf", Files: map[string]string{"main.conf": "include \"private.conf\""}},
		"inline include":       {Entry: "main.conf", Files: map[string]string{"main.conf": "port: 4222; include \"private.conf\""}},
		"noncanonical include": {Entry: "main.conf", Files: map[string]string{"main.conf": "INCLUDE \"private.conf\""}},
		"include variable":     {Entry: "main.conf", Files: map[string]string{"main.conf": "include $NATS_FILE"}},
		"cycle":                {Entry: "main.conf", Files: map[string]string{"main.conf": "include \"second.conf\"", "second.conf": "include \"main.conf\""}},
		"oversized sources":    {Entry: "main.conf", Files: map[string]string{"main.conf": strings.Repeat("x", MaxSourceBytes+1)}},
	} {
		t.Run(name, func(t *testing.T) {
			if sources.Validate() == nil {
				t.Fatal("unsafe source bundle accepted")
			}
		})
	}
}

func TestNATSConfigUsesLocalVariablesAndIncludedDefaults(t *testing.T) {
	sources := Sources{Entry: "main.conf", Files: map[string]string{
		"main.conf":     "PORT = 4223\nport: $PORT\ninclude \"accounts.conf\"\n",
		"accounts.conf": "accounts { WORK { default_permissions { publish: [\"safe.>\"] }; users: [{user: runner, password: fixture-secret}] } }",
	}}
	result, err := parseFixture(t.Context(), sources)
	if err != nil {
		t.Fatal(err)
	}
	if result.ParserVersion != ParserVersion || len(result.Digest) != 71 || !strings.HasPrefix(result.Digest, "sha256:") ||
		string(result.Config["port"]) != "4223" || !slices.Contains(result.Variables, "PORT") {
		t.Fatal("upstream configuration resolution or variable evidence was lost")
	}
	var accounts map[string]struct {
		Defaults struct {
			Publish []string `json:"publish"`
		} `json:"default_permissions"`
	}
	if err := json.Unmarshal(result.Config["accounts"], &accounts); err != nil || len(accounts["WORK"].Defaults.Publish) != 1 || accounts["WORK"].Defaults.Publish[0] != "safe.>" {
		t.Fatal("included permission defaults were lost")
	}
	sources.Files["accounts.conf"] = strings.ReplaceAll(sources.Files["accounts.conf"], "safe.>", "other.>")
	changed, err := parseFixture(t.Context(), sources)
	if err != nil || changed.Digest == result.Digest {
		t.Fatal("changed effective permissions kept the same digest")
	}
}

func TestNATSConfigBoundsRepeatedIncludeExpansion(t *testing.T) {
	sources := Sources{Entry: "0.conf", Files: make(map[string]string)}
	for i := range 20 {
		name := fmt.Sprintf("%d.conf", i)
		if i == 19 {
			sources.Files[name] = "port: 4222"
			continue
		}
		sources.Files[name] = strings.Repeat(fmt.Sprintf("include \"%d.conf\"\n", i+1), 2)
	}
	if sources.Validate() == nil {
		t.Fatal("exponential include expansion accepted")
	}
}

func TestNATSConfigDoesNotInheritEnvironmentOrLeakParseErrors(t *testing.T) {
	const secret = "must-not-leak-from-parent"
	t.Setenv("NATS_AUTHORITY_EXTERNAL_SECRET", secret)
	sources := Sources{Entry: "main.conf", Files: map[string]string{"main.conf": "password: $NATS_AUTHORITY_EXTERNAL_SECRET"}}
	if _, err := parseFixture(t.Context(), sources); err == nil || strings.Contains(err.Error(), secret) {
		t.Fatal("external environment was accepted or exposed")
	}
	sources.Files["main.conf"] = "password: [\"" + secret + "\""
	if _, err := parseFixture(t.Context(), sources); err == nil || strings.Contains(err.Error(), secret) {
		t.Fatal("upstream parse error exposed source text")
	}
	// Normal server environment remains untouched throughout both subprocesses.
	if os.Getenv("NATS_AUTHORITY_EXTERNAL_SECRET") != secret {
		t.Fatal("parser changed the parent environment")
	}
}

func TestNATSConfigCanceledHelperCleansScratch(t *testing.T) {
	scratch := t.TempDir()
	t.Setenv("TMPDIR", scratch)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := parseFixture(ctx, Sources{Entry: "main.conf", Files: map[string]string{"main.conf": "port: 4222"}}); err == nil {
		t.Fatal("canceled helper succeeded")
	}
	entries, err := os.ReadDir(scratch)
	if err != nil || len(entries) != 0 {
		t.Fatal("canceled helper left source scratch behind")
	}
	// Kill a process after it has actually written private source bytes.
	// The child cannot run its cleanup defer; the parent must remove them.
	liveCtx, liveCancel := context.WithCancel(t.Context())
	defer liveCancel()
	done := make(chan error, 1)
	go func() {
		_, err := parseWithExecutable(liveCtx, os.Args[0], []string{"-test.run=^TestNATSConfigHelperProcess$", "--", "--nats-config-stall-test"},
			Sources{Entry: "main.conf", Files: map[string]string{"main.conf": "port: 4222"}})
		done <- err
	}()
	wrote := false
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		files, _ := filepath.Glob(filepath.Join(scratch, "iterion-nats-authority-*", "private.conf"))
		if len(files) == 1 {
			wrote = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	liveCancel()
	if err := <-done; err == nil || !wrote {
		t.Fatal("did not exercise cancellation after the helper wrote source bytes")
	}
	entries, err = os.ReadDir(scratch)
	if err != nil || len(entries) != 0 {
		t.Fatal("killed helper left private source bytes behind")
	}
}

func TestNATSConfigOutputLimitAppliesToPipeCopy(t *testing.T) {
	var output boundedBuffer
	// A pipe exposes Read, not a string reader's WriteTo fast path. io.Copy
	// must not discover a promoted bytes.Buffer.ReadFrom that bypasses Write.
	reader := struct{ io.Reader }{strings.NewReader(strings.Repeat("x", MaxMessageBytes+1))}
	if _, err := io.Copy(&output, reader); err == nil || len(output.Bytes()) > MaxMessageBytes {
		t.Fatal("subprocess pipe copy bypassed the output size limit")
	}
}

func TestNATSConfigProductionHelperBypassesDotEnv(t *testing.T) {
	binary := os.Getenv("ITERION_TEST_CURRENT_BINARY")
	if binary == "" {
		if os.Getenv("ITERION_TEST_REQUIRED") == "1" {
			t.Fatal("production helper test requires ITERION_TEST_CURRENT_BINARY")
		}
		t.Skip("ITERION_TEST_CURRENT_BINARY not set")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("NATS_AUTHORITY_AMBIENT_VARIABLE=unexpected\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(Sources{Entry: "main.conf", Files: map[string]string{"main.conf": "port: 4222"}})
	cmd := exec.CommandContext(t.Context(), binary, HelperCommand)
	cmd.Env, cmd.Dir, cmd.Stdin = []string{}, dir, bytes.NewReader(body)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("production helper failed with ambient .env: %v", err)
	}
	var result Result
	if json.Unmarshal(out, &result) != nil || string(result.Config["port"]) != "4222" {
		t.Fatal("production helper did not return the isolated parse result")
	}
}

func TestNATSConfigDigestMatchesBroker(t *testing.T) {
	binary := os.Getenv("ITERION_TEST_NATS_SERVER")
	if binary == "" {
		if os.Getenv("ITERION_TEST_REQUIRED") == "1" {
			t.Fatal("broker digest test requires ITERION_TEST_NATS_SERVER from the pinned image")
		}
		t.Skip("ITERION_TEST_NATS_SERVER not set")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()
	clientListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	clientPort := clientListener.Addr().(*net.TCPAddr).Port
	_ = clientListener.Close()
	sources := Sources{Entry: "main.conf", Files: map[string]string{
		"main.conf": fmt.Sprintf("listen: 127.0.0.1:%d\nhttp: 127.0.0.1:%d\njetstream: true\ninclude \"auth.conf\"\n", clientPort, port),
		"auth.conf": "accounts { SYS {users: [{user: sys, password: fixture}]}; WORK {users: [{user: runner, password: fixture}]} }\nsystem_account: SYS\n",
	}}
	parsed, err := parseFixture(t.Context(), sources)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	for name, content := range sources.Files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.CommandContext(t.Context(), binary, "-c", filepath.Join(dir, sources.Entry))
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	client := &http.Client{Timeout: 200 * time.Millisecond}
	for phase := range 2 {
		if phase == 1 {
			sources.Files["auth.conf"] = strings.ReplaceAll(sources.Files["auth.conf"], "user: runner, password: fixture", "user: runner, password: changed-fixture")
			changed, err := parseFixture(t.Context(), sources)
			if err != nil || changed.Digest == parsed.Digest {
				t.Fatal("reload fixture did not change the parsed digest")
			}
			parsed = changed
			if err := os.WriteFile(filepath.Join(dir, "auth.conf"), []byte(sources.Files["auth.conf"]), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := exec.CommandContext(t.Context(), binary, "--signal", fmt.Sprintf("reload=%d", cmd.Process.Pid)).Run(); err != nil {
				t.Fatalf("reload disposable broker: %v", err)
			}
		}
		profile, err := LoadProfile(parsed)
		if err != nil || profile.ConfigDigest != parsed.Digest || profile.SystemAccount != "SYS" || len(profile.Principals) != 2 {
			t.Fatalf("loaded broker configuration did not yield the supported authorization profile: %+v %v", profile, err)
		}
		matched := false
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			response, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/varz", port))
			if err == nil {
				var varz struct {
					Version string `json:"version"`
					Digest  string `json:"config_digest"`
				}
				err := json.NewDecoder(response.Body).Decode(&varz)
				_ = response.Body.Close()
				if err != nil || varz.Version != ParserVersion {
					t.Fatalf("broker/parser identity mismatch: version=%s, error=%v", varz.Version, err)
				}
				if varz.Digest == parsed.Digest {
					matched = true
					break
				}
			}
			time.Sleep(10 * time.Millisecond)
		}
		if !matched {
			t.Fatal("disposable pinned NATS broker did not expose the loaded configuration digest")
		}
		// Exercise the same authenticated system-account request path that
		// the authority uses; the HTTP fixture alone would not prove it.
		nc, err := natsclient.Connect(fmt.Sprintf("nats://127.0.0.1:%d", clientPort), natsclient.UserInfo("sys", "fixture"))
		if err != nil {
			t.Fatal(err)
		}
		msg, err := nc.Request("$SYS.REQ.SERVER."+nc.ConnectedServerId()+".VARZ", nil, time.Second)
		nc.Close()
		if err != nil {
			t.Fatalf("system VARZ request failed: %v", err)
		}
		var system struct {
			Data struct {
				Digest string `json:"config_digest"`
			} `json:"data"`
		}
		if json.Unmarshal(msg.Data, &system) != nil || system.Data.Digest != parsed.Digest {
			t.Fatal("system VARZ disagrees with the parsed loaded configuration")
		}
	}
}
