package claudesdk

import (
	"context"
	"crypto/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Exercise the public APIs through spawnProcess. The builder owns the host
// transport environment; WithEnv is a separate workload channel, never a
// replacement for the returned command's driver-only data.
func TestCommandBuilderPreservesDriverEnvironment(t *testing.T) {
	for _, mode := range []string{"prompt", "hooked-prompt", "session"} {
		t.Run(mode, func(t *testing.T) {
			const key = "ITERION_TEST_SDK_DRIVER_NONCE"
			const workload = "ITERION_TEST_SDK_WORKLOAD"
			for _, k := range []string{key, workload} {
				t.Setenv(k, "")
				if err := os.Unsetenv(k); err != nil {
					t.Fatal(err)
				}
				if _, ok := os.LookupEnv(k); ok {
					t.Fatalf("%s must be absent ambient", k)
				}
			}
			nonce := rand.Text()
			dir := t.TempDir()
			path := filepath.Join(dir, "receipt")
			calls := 0
			builder := func(ctx context.Context, _ string, args []string, cwd string, env map[string]string, open bool) *exec.Cmd {
				calls++
				if env[workload] != "remote-workload" || cwd != "/container/only" || open != (mode != "prompt") {
					t.Errorf("builder inputs: env=%v cwd=%q open=%v", env, cwd, open)
				}
				script := `printf '%s\n%s\n%s\n' "$ITERION_TEST_SDK_DRIVER_NONCE" "$ITERION_TEST_SDK_WORKLOAD" "$2" > "$1"
printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"result":"done","session_id":"env-test"}'
cat > "$1.stdin"`
				cmd := exec.CommandContext(ctx, "sh", "-c", script, "sdk-transport", path, env[workload])
				cmd.Env = append(os.Environ(), key+"="+nonce)
				return cmd
			}
			opts := []Option{WithCommandBuilder(builder), WithEnv(workload, "remote-workload"), WithCwd("/container/only")}
			if mode == "hooked-prompt" {
				opts = append(opts, WithHook(HookPreToolUse, denyBash(t)))
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if mode == "session" {
				s := NewSession(opts...)
				t.Cleanup(func() { _ = s.Close() })
				if err := s.Send(ctx, "driver-sdk-input"); err != nil {
					t.Fatal(err)
				}
				result := false
				for msg, err := range s.Stream(ctx) {
					if err != nil {
						t.Fatal(err)
					}
					if _, ok := msg.(*ResultMessage); ok {
						result = true
						break
					}
				}
				if !result {
					t.Fatal("session missing result")
				}
				if err := s.Close(); err != nil {
					t.Fatal(err)
				}
			} else if _, err := Prompt(ctx, "driver-sdk-input", opts...); err != nil {
				t.Fatal(err)
			}
			if calls != 1 {
				t.Fatalf("builder calls=%d", calls)
			}
			body, err := os.ReadFile(path)
			if err != nil || string(body) != nonce+"\n\nremote-workload\n" {
				t.Fatalf("real child lost driver environment or mixed channels: receipt=%q err=%v", body, err)
			}
			if mode != "prompt" {
				body, err := os.ReadFile(path + ".stdin")
				if err != nil || !strings.Contains(string(body), "driver-sdk-input") {
					t.Fatalf("stdin=%q err=%v", body, err)
				}
			}
		})
	}
}
