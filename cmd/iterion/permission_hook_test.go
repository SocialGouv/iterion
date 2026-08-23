package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestIsPermissionHookInvocation(t *testing.T) {
	orig := os.Args
	t.Cleanup(func() { os.Args = orig })

	cases := []struct {
		args []string
		want bool
	}{
		{[]string{"iterion", "__permission-hook"}, true},
		{[]string{"iterion", "__permission-hook", "--backend", "grok"}, true},
		{[]string{"iterion", "--json", "__permission-hook"}, true},
		{[]string{"iterion", "run", "x.bot"}, false},
		{[]string{"iterion", "__mcp-board"}, false},
		{[]string{"iterion", "help", "__permission-hook"}, false},
		{[]string{"iterion"}, false},
	}
	for _, tc := range cases {
		os.Args = tc.args
		if got := isPermissionHookInvocation(); got != tc.want {
			t.Errorf("args=%v got %v, want %v", tc.args, got, tc.want)
		}
	}
}

// TestPermissionHookDoesNotLoadWorkspaceDotEnv pins R6fa6d2: the hook
// process is spawned with cwd = the gated workspace, so loadDotEnvFromCwd
// would read an agent-writable `.env`. The skip must happen for the hook
// argv and must NOT happen for an ordinary command (the `.env` fixture
// is otherwise a no-op and the test would not catch a broken skip).
func TestPermissionHookDoesNotLoadWorkspaceDotEnv(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("PERMISSION_HOOK_SENTINEL=loaded\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PERMISSION_HOOK_SENTINEL", "")
	os.Unsetenv("PERMISSION_HOOK_SENTINEL")

	orig := os.Args
	t.Cleanup(func() { os.Args = orig })

	os.Args = []string{"iterion", "__permission-hook", "--backend", "kimi"}
	if !isPermissionHookInvocation() {
		loadDotEnvFromCwd()
	}
	if _, ok := os.LookupEnv("PERMISSION_HOOK_SENTINEL"); ok {
		t.Fatal("permission hook loaded the workspace .env")
	}

	os.Args = []string{"iterion", "version"}
	if !isPermissionHookInvocation() {
		loadDotEnvFromCwd()
	}
	if os.Getenv("PERMISSION_HOOK_SENTINEL") != "loaded" {
		t.Fatal("ordinary commands must still load .env; otherwise the skip is unobservable")
	}
}
