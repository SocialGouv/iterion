package dispatcher

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/internal/hometest"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

func newTestLogger() *iterlog.Logger {
	return iterlog.New(iterlog.LevelInfo, &bytes.Buffer{})
}

func TestHookRunScript(t *testing.T) {
	dir := t.TempDir()
	h := &Hook{Script: "echo hello > out.txt"}
	if err := h.Run(context.Background(), newTestLogger(), "test", dir, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "out.txt"))
	if err != nil {
		t.Fatalf("expected out.txt: %v", err)
	}
	if strings.TrimSpace(string(data)) != "hello" {
		t.Fatalf("unexpected content: %q", data)
	}
}

func TestHookRunNil(t *testing.T) {
	var h *Hook
	if err := h.Run(context.Background(), newTestLogger(), "noop", t.TempDir(), nil); err != nil {
		t.Fatalf("nil hook should be a no-op, got %v", err)
	}
}

func TestHookRunCommandFails(t *testing.T) {
	h := &Hook{Script: "exit 7"}
	err := h.Run(context.Background(), newTestLogger(), "fail", t.TempDir(), nil)
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestHookRunTimeout(t *testing.T) {
	h := &Hook{Script: "sleep 30", TimeoutMS: 100}
	start := time.Now()
	err := h.Run(context.Background(), newTestLogger(), "slow", t.TempDir(), nil)
	if err == nil || !strings.Contains(err.Error(), "timeout") {
		t.Fatalf("expected timeout error, got %v", err)
	}
	// cmd.WaitDelay (2s) bounds the wait when the orphaned grandchild
	// keeps stdout/stderr pipes open after `sh` is killed. 5s leaves
	// headroom for slow CI without masking a regression that
	// re-introduces the unbounded wait.
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("timeout took too long: %s", d)
	}
}

func TestHookRunUsesEnv(t *testing.T) {
	dir := t.TempDir()
	h := &Hook{Script: "echo $ITERION_TEST_VAR > out.txt"}
	if err := h.Run(context.Background(), newTestLogger(), "env", dir, []string{"ITERION_TEST_VAR=greenlight"}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "out.txt"))
	if strings.TrimSpace(string(data)) != "greenlight" {
		t.Fatalf("env not propagated: %q", data)
	}
}

// Every hook a test here spawns runs its login shell under the HOME TestMain
// made, never the operator's: that one's profile is what the shell would
// source (#2231).
func TestHookRunLoginShellReadsNoOperatorProfile(t *testing.T) {
	dir := t.TempDir()
	h := &Hook{Script: `printf '%s' "$HOME" > home.txt`}
	if err := h.Run(context.Background(), newTestLogger(), "home", dir, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "home.txt"))
	if err != nil {
		t.Fatalf("read the HOME the hook saw: %v", err)
	}
	hometest.RequireNoOperatorProfile(t, string(data))
}

func TestHooksValidate(t *testing.T) {
	cases := []struct {
		name    string
		h       Hooks
		wantErr bool
	}{
		{"empty ok", Hooks{}, false},
		{"script only", Hooks{AfterCreate: &Hook{Script: "echo ok"}}, false},
		{"path only", Hooks{AfterCreate: &Hook{Path: "/bin/true"}}, false},
		{"both set", Hooks{BeforeRun: &Hook{Script: "x", Path: "/bin/true"}}, true},
		{"neither set", Hooks{AfterRun: &Hook{}}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.h.Validate()
			if (err != nil) != tc.wantErr {
				t.Fatalf("want err=%v, got %v", tc.wantErr, err)
			}
		})
	}
}
