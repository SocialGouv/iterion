//go:build linux

package kubernetes

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"github.com/SocialGouv/iterion/pkg/backend/delegate/claudesdk"
	"github.com/SocialGouv/iterion/pkg/backend/delegate/pisdk"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/sandbox"
)

func TestNativeExecTransportCallerInteractions(t *testing.T) {
	isolateTransportConfig(t)
	host, guest := nativeExecTransport(t)
	t.Setenv(execTransportPrefix+"HOST_HELPER", host)
	t.Setenv(execTransportPrefix+"GUEST_HELPER", guest)
	cfg, err := loadExecTransportConfig()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	witness := filepath.Join(dir, "client-config")
	client := filepath.Join(dir, "kubectl")
	t.Setenv("CLIENT_WITNESS_PATH", witness)
	t.Setenv("KUBECONFIG", "operator-config")
	script := `#!/bin/sh
printf '%s' "$KUBECONFIG" > "$CLIENT_WITNESS_PATH"
for target; do :; done
exec /usr/bin/env -i PATH=/bin:/usr/bin NATIVE_ONLY="$POD_VALUE" KUBECONFIG=pod-config "$target"
`
	if err := os.WriteFile(client, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	r := testRun()
	r.driver.kubectl = Kubectl(client)
	r.driver.transport = cfg
	for _, pod := range []string{"pod-first", "pod-second"} {
		t.Setenv("POD_VALUE", pod)
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		cmd := r.Command(ctx, []string{"/bin/sh", "-c", `printf '%s|%s|%s|' "$SECRET" "$KUBECONFIG" "$NATIVE_ONLY"; cat`}, sandbox.ExecOpts{Env: map[string]string{"SECRET": "private-fixture", "KUBECONFIG": "target-config"}, Stdin: strings.NewReader("user-stdin")})
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		stdout, err := cmd.Output()
		cancel()
		want := "private-fixture|target-config|" + pod + "|user-stdin"
		if err != nil || string(stdout) != want || stderr.Len() != 0 {
			t.Fatalf("native result=%q stderr=%q err=%v want=%q", stdout, stderr.String(), err, want)
		}
		got, err := os.ReadFile(witness)
		if err != nil || string(got) != "operator-config" {
			t.Fatalf("host config=%q err=%v", got, err)
		}
		for _, arg := range cmd.Args {
			if strings.Contains(arg, "private-fixture") {
				t.Fatal("target environment leaked into host argv")
			}
		}
	}
	t.Run("pi-close-only", func(t *testing.T) {
		original, err := os.ReadFile(client)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = os.WriteFile(client, original, 0700) }()
		sdkDir := t.TempDir()
		pluginPID := filepath.Join(sdkDir, "plugin-pid")
		targetReady := filepath.Join(sdkDir, "target-ready")
		t.Setenv("PLUGIN_PID", pluginPID)
		script := `#!/bin/sh
sleep 60 </dev/null >/dev/null 2>&1 &
printf '%s' "$!" > "$PLUGIN_PID"
for target; do :; done
exec /usr/bin/env -i PATH=/bin:/usr/bin "$target"
`
		if err = os.WriteFile(client, []byte(script), 0700); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
		defer cancel()
		sdk := pisdk.NewClient(pisdk.ClientOptions{PreserveCommandCancellation: sandbox.OwnsCommandCancellation(r), Spawn: func(c context.Context, _ []string) *exec.Cmd {
			cmd := r.Command(c, []string{"/bin/sh", "-c", `printf ready > "$1"; exec sleep 60`, "sh", targetReady}, sandbox.ExecOpts{Env: map[string]string{"X": "fixture"}, KeepStdinOpen: true})
			// Force the mount supervisor to see cancellation before the Go host can
			// run its signal handler. This used to kill the host before group cleanup.
			originalCancel := cmd.Cancel
			cmd.Cancel = func() error {
				raw, e := os.ReadFile(pluginPID)
				if e != nil {
					return e
				}
				plugin, e := strconv.Atoi(string(raw))
				if e != nil {
					return e
				}
				parent := func(pid int) (int, error) {
					raw, e := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
					if e != nil {
						return 0, e
					}
					fields := strings.Fields(string(raw)[strings.LastIndex(string(raw), ")")+1:])
					return strconv.Atoi(fields[1])
				}
				target, e := parent(plugin)
				if e != nil {
					return e
				}
				host, e := parent(target)
				if e != nil {
					return e
				}
				t.Logf("schedule witness plugin=%d target=%d host=%d", plugin, target, host)
				if e = syscall.Kill(host, syscall.SIGSTOP); e != nil {
					return e
				}
				defer func() { _ = syscall.Kill(host, syscall.SIGCONT) }()
				e = originalCancel()
				time.Sleep(100 * time.Millisecond) // force supervisor delivery before host handler can run
				return e
			}
			return cmd
		}})
		if err = sdk.Start(ctx); err != nil {
			t.Fatal(err)
		}
		defer sdk.Close()
		deadline := time.Now().Add(5 * time.Second)
		for {
			if _, e := os.Stat(targetReady); e == nil {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("target never ready")
			}
			time.Sleep(5 * time.Millisecond)
		}
		raw, err := os.ReadFile(pluginPID)
		if err != nil {
			t.Fatal(err)
		}
		pid, err := strconv.Atoi(string(raw))
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = syscall.Kill(pid, syscall.SIGKILL) }()
		_ = sdk.Close()
		if ctx.Err() != nil {
			t.Fatal("Close relied on context deadline")
		}
		deadline = time.Now().Add(time.Second)
		for {
			raw, e := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
			if os.IsNotExist(e) || strings.Contains(string(raw), ") Z ") {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("SDK cancellation left client plugin live: pid=%d stat=%q", pid, raw)
			}
			time.Sleep(10 * time.Millisecond)
		}
	})
	t.Run("pi-plain-kubectl-cancel", func(t *testing.T) {
		original, err := os.ReadFile(client)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = os.WriteFile(client, original, 0700) }()
		sdkDir := t.TempDir()
		pluginPID := filepath.Join(sdkDir, "plugin-pid")
		targetReady := filepath.Join(sdkDir, "target-ready")
		t.Setenv("PLUGIN_PID", pluginPID)
		script := `#!/bin/sh
sleep 60 </dev/null >/dev/null 2>&1 &
printf '%s' "$!" > "$PLUGIN_PID"
while [ "$1" != -- ]; do shift; done
shift
exec "$@"
`
		if err = os.WriteFile(client, []byte(script), 0700); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
		defer cancel()
		sdk := pisdk.NewClient(pisdk.ClientOptions{PreserveCommandCancellation: sandbox.OwnsCommandCancellation(r), Spawn: func(c context.Context, _ []string) *exec.Cmd {
			return r.Command(c, []string{"/bin/sh", "-c", `printf ready > "$1"; cat >/dev/null`, "sh", targetReady}, sandbox.ExecOpts{KeepStdinOpen: true})
		}})
		if err = sdk.Start(ctx); err != nil {
			t.Fatal(err)
		}
		defer sdk.Close()
		deadline := time.Now().Add(5 * time.Second)
		for {
			if _, e := os.Stat(targetReady); e == nil {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("target never ready")
			}
			time.Sleep(5 * time.Millisecond)
		}
		raw, err := os.ReadFile(pluginPID)
		if err != nil {
			t.Fatal(err)
		}
		pid, err := strconv.Atoi(string(raw))
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = syscall.Kill(pid, syscall.SIGKILL) }()
		cancel()
		_ = sdk.Close()
		deadline = time.Now().Add(time.Second)
		for {
			raw, e := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
			if os.IsNotExist(e) || strings.Contains(string(raw), ") Z ") {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("SDK cancellation left client plugin live: pid=%d stat=%q", pid, raw)
			}
			time.Sleep(10 * time.Millisecond)
		}
	})
	t.Run("claude-close-only", func(t *testing.T) {
		original, err := os.ReadFile(client)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = os.WriteFile(client, original, 0700) }()
		sdkDir := t.TempDir()
		pluginPID := filepath.Join(sdkDir, "plugin-pid")
		targetReady := filepath.Join(sdkDir, "target-ready")
		t.Setenv("PLUGIN_PID", pluginPID)
		script := `#!/bin/sh
sleep 60 </dev/null >/dev/null 2>&1 &
printf '%s' "$!" > "$PLUGIN_PID"
for target; do :; done
exec /usr/bin/env -i PATH=/bin:/usr/bin "$target"
`
		if err = os.WriteFile(client, []byte(script), 0700); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
		defer cancel()
		sdk := claudesdk.NewSession(claudesdk.WithPreservedCommandCancellation(sandbox.OwnsCommandCancellation(r)), claudesdk.WithCommandBuilder(func(c context.Context, _ string, _ []string, _ string, _ map[string]string, _ bool) *exec.Cmd {
			return r.Command(c, []string{"/bin/sh", "-c", `printf ready > "$1"; exec sleep 60`, "sh", targetReady}, sandbox.ExecOpts{Env: map[string]string{"X": "fixture"}, KeepStdinOpen: true})
		}))
		if err = sdk.Send(ctx, "no model"); err != nil {
			t.Fatal(err)
		}
		defer sdk.Close()

		deadline := time.Now().Add(5 * time.Second)
		for {
			if _, e := os.Stat(targetReady); e == nil {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("target never ready")
			}
			time.Sleep(5 * time.Millisecond)
		}
		raw, err := os.ReadFile(pluginPID)
		if err != nil {
			t.Fatal(err)
		}
		pid, err := strconv.Atoi(string(raw))
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = syscall.Kill(pid, syscall.SIGKILL) }()
		_ = sdk.Close()
		if ctx.Err() != nil {
			t.Fatal("Close relied on context deadline")
		}
		deadline = time.Now().Add(time.Second)
		for {
			raw, e := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
			if os.IsNotExist(e) || strings.Contains(string(raw), ") Z ") {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("SDK cancellation left client plugin live: pid=%d stat=%q", pid, raw)
			}
			time.Sleep(10 * time.Millisecond)
		}
	})
	t.Run("pi-spawner-preserves-client-cancel", func(t *testing.T) {
		original, err := os.ReadFile(client)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = os.WriteFile(client, original, 0700) }()
		sdkDir := t.TempDir()
		pluginPID := filepath.Join(sdkDir, "plugin-pid")
		targetReady := filepath.Join(sdkDir, "target-ready")
		t.Setenv("PLUGIN_PID", pluginPID)
		script := `#!/bin/sh
sleep 60 </dev/null >/dev/null 2>&1 &
printf '%s' "$!" > "$PLUGIN_PID"
for target; do :; done
exec /usr/bin/env -i PATH=/bin:/usr/bin "$target"
`
		if err = os.WriteFile(client, []byte(script), 0700); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
		defer cancel()
		sdk := pisdk.NewClient(pisdk.ClientOptions{PreserveCommandCancellation: sandbox.OwnsCommandCancellation(r), Spawn: func(c context.Context, _ []string) *exec.Cmd {
			return r.Command(c, []string{"/bin/sh", "-c", `printf ready > "$1"; cat >/dev/null`, "sh", targetReady}, sandbox.ExecOpts{Env: map[string]string{"X": "fixture"}, KeepStdinOpen: true})
		}})
		if err = sdk.Start(ctx); err != nil {
			t.Fatal(err)
		}
		defer sdk.Close()
		deadline := time.Now().Add(5 * time.Second)
		for {
			if _, e := os.Stat(targetReady); e == nil {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("target never ready")
			}
			time.Sleep(5 * time.Millisecond)
		}
		raw, err := os.ReadFile(pluginPID)
		if err != nil {
			t.Fatal(err)
		}
		pid, err := strconv.Atoi(string(raw))
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = syscall.Kill(pid, syscall.SIGKILL) }()
		cancel()
		_ = sdk.Close()
		deadline = time.Now().Add(time.Second)
		for {
			raw, e := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
			if os.IsNotExist(e) || strings.Contains(string(raw), ") Z ") {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("SDK cancellation left client plugin live: pid=%d stat=%q", pid, raw)
			}
			time.Sleep(10 * time.Millisecond)
		}
	})
	t.Run("claude-builder-preserves-client-cancel", func(t *testing.T) {
		original, err := os.ReadFile(client)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = os.WriteFile(client, original, 0700) }()
		sdkDir := t.TempDir()
		pluginPID := filepath.Join(sdkDir, "plugin-pid")
		targetReady := filepath.Join(sdkDir, "target-ready")
		t.Setenv("PLUGIN_PID", pluginPID)
		script := `#!/bin/sh
sleep 60 </dev/null >/dev/null 2>&1 &
printf '%s' "$!" > "$PLUGIN_PID"
for target; do :; done
exec /usr/bin/env -i PATH=/bin:/usr/bin "$target"
`
		if err = os.WriteFile(client, []byte(script), 0700); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
		defer cancel()
		done := make(chan error, 1)
		go func() {
			_, err := claudesdk.Prompt(ctx, "no model", claudesdk.WithPreservedCommandCancellation(sandbox.OwnsCommandCancellation(r)), claudesdk.WithCommandBuilder(func(c context.Context, _ string, _ []string, _ string, _ map[string]string, _ bool) *exec.Cmd {
				return r.Command(c, []string{"/bin/sh", "-c", `printf ready > "$1"; exec sleep 60`, "sh", targetReady}, sandbox.ExecOpts{Env: map[string]string{"X": "fixture"}, KeepStdinOpen: true})
			}))
			done <- err
		}()

		deadline := time.Now().Add(5 * time.Second)
		for {
			if _, e := os.Stat(targetReady); e == nil {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("target never ready")
			}
			time.Sleep(5 * time.Millisecond)
		}
		raw, err := os.ReadFile(pluginPID)
		if err != nil {
			t.Fatal(err)
		}
		pid, err := strconv.Atoi(string(raw))
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = syscall.Kill(pid, syscall.SIGKILL) }()
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("Claude Prompt did not finish")
		}
		deadline = time.Now().Add(time.Second)
		for {
			raw, e := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
			if os.IsNotExist(e) || strings.Contains(string(raw), ") Z ") {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("SDK cancellation left client plugin live: pid=%d stat=%q", pid, raw)
			}
			time.Sleep(10 * time.Millisecond)
		}
	})
	t.Run("unmarked-spawn-keeps-group-hardening", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		pidfile := filepath.Join(t.TempDir(), "pid")
		sdk := pisdk.NewClient(pisdk.ClientOptions{Spawn: func(c context.Context, _ []string) *exec.Cmd {
			return exec.CommandContext(c, "/bin/sh", "-c", `sleep 60 </dev/null >/dev/null 2>&1 & printf '%s' $! > "$1"; cat >/dev/null`, "sh", pidfile)
		}})
		if err := sdk.Start(ctx); err != nil {
			t.Fatal(err)
		}
		defer sdk.Close()
		var pid int
		deadline := time.Now().Add(3 * time.Second)
		for {
			raw, e := os.ReadFile(pidfile)
			if e == nil {
				pid, _ = strconv.Atoi(string(raw))
				if pid > 0 {
					break
				}
			}
			if time.Now().After(deadline) {
				t.Fatal("no child pid")
			}
			time.Sleep(5 * time.Millisecond)
		}
		defer func() { _ = syscall.Kill(pid, syscall.SIGKILL) }()
		_ = sdk.Close()
		deadline = time.Now().Add(time.Second)
		for {
			raw, e := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
			if os.IsNotExist(e) || strings.Contains(string(raw), ") Z ") {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("unmarked custom Spawn lost existing group cleanup pid=%d", pid)
			}
			time.Sleep(10 * time.Millisecond)
		}
	})
	t.Run("tool-completion-wrapper", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()
		completion := sandbox.NewCompletion("r10")
		key, value := completion.SessionEnv()
		result, err := r.Exec(ctx, completion.Wrap([]string{"/bin/sh", "-c", "printf 'tool-output'; exit 19"}), sandbox.ExecOpts{Env: map[string]string{key: value}, WorkDir: dir})
		clean, status, ended := completion.Settle(result.Stdout)
		if err != nil || result.ExitCode != 19 || !ended || status != 19 || string(clean) != "tool-output" {
			t.Fatalf("completion output=%q clean=%q ended=%v status=%d exit=%d err=%v", result.Stdout, clean, ended, status, result.ExitCode, err)
		}
	})
	t.Run("oversized-script-exec", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()
		script := "#" + strings.Repeat("x", sandbox.MaxInlineArgBytes+50) + "\nprintf '%s|%s' \"$FOO\" \"$PWD\""
		result, err := r.Exec(ctx, []string{"/bin/sh", "-c", script}, sandbox.ExecOpts{Env: map[string]string{"FOO": "script-value"}, WorkDir: dir})
		if err != nil || result.ExitCode != 0 || string(result.Stdout) != "script-value|"+dir || len(result.Stderr) != 0 {
			t.Fatalf("script stdout=%q stderr=%q exit=%d err=%v", result.Stdout, result.Stderr, result.ExitCode, err)
		}
	})
	t.Run("binary-full-duplex", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()
		payload := bytes.Repeat([]byte{0, 1, 255, '\n', 'x'}, 500000)
		out, err := r.Command(ctx, []string{"/bin/cat"}, sandbox.ExecOpts{Env: map[string]string{"X": "fixture"}, Stdin: bytes.NewReader(payload)}).Output()
		if err != nil || !bytes.Equal(out, payload) {
			t.Fatalf("lost stdin/output: sent=%d got=%d err=%v", len(payload), len(out), err)
		}
	})
	t.Run("nil-stdin-eof", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
		defer cancel()
		out, err := r.Command(ctx, []string{"/bin/sh", "-c", "if read x; then exit 91; fi; printf EOF"}, sandbox.ExecOpts{Env: map[string]string{"X": "fixture"}}).Output()
		if err != nil || string(out) != "EOF" {
			t.Fatalf("nil stdin=%q err=%v", out, err)
		}
	})
	t.Run("large-both-streams", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()
		result, err := r.Exec(ctx, []string{"/bin/sh", "-c", "head -c 1048576 /dev/zero; head -c 1048576 /dev/zero >&2; exit 17"}, sandbox.ExecOpts{Env: map[string]string{"X": "fixture"}})
		if err != nil || result.ExitCode != 17 || len(result.Stdout) != 1048576 || len(result.Stderr) != 1048576 {
			t.Fatalf("streams stdout=%d stderr=%d exit=%d err=%v", len(result.Stdout), len(result.Stderr), result.ExitCode, err)
		}
	})
	t.Run("controlled-cancel-local-descendants", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
		defer cancel()
		c := r.Command(ctx, []string{"/bin/sh", "-c", "sleep 60 & printf '%s %s\n' $$ $!; wait"}, sandbox.ExecOpts{Env: map[string]string{"X": "fixture"}, KeepStdinOpen: true})
		input, err := c.StdinPipe()
		if err != nil {
			t.Fatal(err)
		}
		defer input.Close()
		output, err := c.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		if err = c.Start(); err != nil {
			t.Fatal(err)
		}
		line, err := bufio.NewReader(output).ReadString('\n')
		if err != nil {
			cancel()
			_ = c.Wait()
			t.Fatal(err)
		}
		fields := strings.Fields(line)
		if len(fields) != 2 {
			cancel()
			_ = c.Wait()
			t.Fatalf("pid response=%q", line)
		}
		started := time.Now()
		cancel()
		err = c.Wait()
		if err == nil || time.Since(started) > 3*time.Second {
			t.Fatalf("cancel err=%v duration=%s", err, time.Since(started))
		}
		for _, field := range fields {
			pid, e := strconv.Atoi(field)
			if e != nil {
				t.Fatal(e)
			}
			t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
			deadline := time.Now().Add(time.Second)
			for {
				raw, e := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
				if os.IsNotExist(e) || strings.Contains(string(raw), ") Z ") {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("live descendant pid=%d stat=%q", pid, raw)
				}
				time.Sleep(10 * time.Millisecond)
			}
		}
	})
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	result, execErr := r.Exec(ctx, []string{"/bin/sh", "-c", `printf '%s|%s' "$X" "$PWD"; exit 7`}, sandbox.ExecOpts{Env: map[string]string{"X": "exec-output"}, WorkDir: dir})
	if execErr != nil || result.ExitCode != 7 || string(result.Stdout) != "exec-output|"+dir || len(result.Stderr) != 0 {
		t.Fatalf("native Exec result=%+v err=%v", result, execErr)
	}
	cmd := r.Command(ctx, []string{"/bin/sh", "-c", `IFS= read -r line; printf 'reply:%s' "$line"`}, sandbox.ExecOpts{Env: map[string]string{"X": "fixture"}, KeepStdinOpen: true})
	input, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if _, err = input.Write([]byte("ping\n")); err != nil {
		t.Fatal(err)
	}
	// The target exits after a line; its caller deliberately retains stdin.
	if err = cmd.Wait(); err != nil || stdout.String() != "reply:ping" || stderr.Len() != 0 {
		t.Fatalf("open-stdin result=%q stderr=%q err=%v", stdout.String(), stderr.String(), err)
	}
	t.Run("client-success-without-ready-is-refusal", func(t *testing.T) {
		if err := os.WriteFile(client, []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
		defer cancel()
		result, err := r.Exec(ctx, []string{"/bin/true"}, sandbox.ExecOpts{Env: map[string]string{"X": "fixture"}})
		if err == nil && result.ExitCode == 0 {
			t.Fatalf("false success: %+v", result)
		}
		if len(result.Stdout) != 0 {
			t.Fatalf("control leaked stdout=%q", result.Stdout)
		}
	})

}
