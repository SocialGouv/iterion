package operatormcp

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// isolateHome points HOME at an empty dir so tests never read the
// developer's real ~/.iterion/cli-auth.json, and clears the env-only
// remote config path.
func isolateHome(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ITERION_REMOTE_URL", "")
	t.Setenv("ITERION_REMOTE_TOKEN", "")
	t.Setenv("ITERION_TOKEN", "")
}

func newTestServer(t *testing.T) *Server {
	t.Helper()
	return &Server{StoreDir: t.TempDir(), WorkDir: t.TempDir()}
}

func TestToolsRegistry(t *testing.T) {
	s := newTestServer(t)
	tools := s.Tools()
	if len(tools) == 0 {
		t.Fatal("no tools registered")
	}
	seen := map[string]bool{}
	var hasLocal, hasBoard, hasRemote bool
	for i, tool := range tools {
		if tool.Name == "" || tool.Description == "" || tool.handler == nil {
			t.Fatalf("tool %d is incomplete: %+v", i, tool)
		}
		if seen[tool.Name] {
			t.Fatalf("duplicate tool name %q", tool.Name)
		}
		seen[tool.Name] = true
		if !json.Valid(tool.InputSchema) {
			t.Fatalf("tool %s has invalid JSON schema", tool.Name)
		}
		if i > 0 && tools[i-1].Name >= tool.Name {
			t.Fatalf("tools not sorted: %s >= %s", tools[i-1].Name, tool.Name)
		}
		switch {
		case strings.HasPrefix(tool.Name, localBoardPrefix):
			hasBoard = true
		case strings.HasPrefix(tool.Name, "local_"):
			hasLocal = true
		case strings.HasPrefix(tool.Name, "remote_"):
			hasRemote = true
		default:
			t.Fatalf("tool %s has neither local_ nor remote_ prefix", tool.Name)
		}
	}
	if !hasLocal || !hasBoard || !hasRemote {
		t.Fatalf("missing a family: local=%v board=%v remote=%v", hasLocal, hasBoard, hasRemote)
	}
}

func TestToolsReadOnlyFiltering(t *testing.T) {
	s := &Server{StoreDir: t.TempDir(), WorkDir: t.TempDir(), ReadOnly: true}
	names := map[string]bool{}
	for _, tool := range s.Tools() {
		if !tool.ReadOnly && !tool.ListedInReadOnly {
			t.Fatalf("mutating tool %s exposed in read-only mode", tool.Name)
		}
		names[tool.Name] = true
	}
	for _, gone := range []string{"local_run", "local_resume", "local_run_cancel", "local_answer", "local_board_create_issue", "remote_runs_launch", "remote_issue_create"} {
		if names[gone] {
			t.Fatalf("tool %s should be hidden in read-only mode", gone)
		}
	}
	// The escape hatch stays listed (its handler enforces GET-only).
	for _, kept := range []string{"remote_api", "local_runs_list", "local_board_list_issues", "remote_status"} {
		if !names[kept] {
			t.Fatalf("tool %s should stay listed in read-only mode", kept)
		}
	}
}

func TestToolsFamilyFiltering(t *testing.T) {
	local := &Server{StoreDir: t.TempDir(), WorkDir: t.TempDir(), Only: FamilyLocal}
	for _, tool := range local.Tools() {
		if strings.HasPrefix(tool.Name, "remote_") {
			t.Fatalf("remote tool %s exposed with --only local", tool.Name)
		}
	}
	remote := &Server{StoreDir: t.TempDir(), WorkDir: t.TempDir(), Only: FamilyRemote}
	for _, tool := range remote.Tools() {
		if strings.HasPrefix(tool.Name, "local_") {
			t.Fatalf("local tool %s exposed with --only remote", tool.Name)
		}
	}
	if len(remote.Tools()) == 0 || len(local.Tools()) == 0 {
		t.Fatal("family filtering emptied the registry")
	}
}

func TestParseFamily(t *testing.T) {
	for in, want := range map[string]Family{"": FamilyAll, "local": FamilyLocal, "REMOTE": FamilyRemote} {
		got, err := ParseFamily(in)
		if err != nil || got != want {
			t.Fatalf("ParseFamily(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	if _, err := ParseFamily("bogus"); err == nil {
		t.Fatal("ParseFamily(bogus) should error")
	}
}

func TestCallUnknownTool(t *testing.T) {
	s := newTestServer(t)
	_, err := s.Call(context.Background(), "nope", nil)
	var unknown *ErrUnknownTool
	if !errors.As(err, &unknown) {
		t.Fatalf("want ErrUnknownTool, got %v", err)
	}

	// A mutating tool hidden by read-only mode is unknown too — the
	// gate is structural, not advisory.
	ro := &Server{StoreDir: t.TempDir(), WorkDir: t.TempDir(), ReadOnly: true}
	if _, err := ro.Call(context.Background(), "local_run", nil); !errors.As(err, &unknown) {
		t.Fatalf("read-only Call(local_run) should be unknown, got %v", err)
	}
}

func TestReadOnlyAnnotationIsTruthful(t *testing.T) {
	s := newTestServer(t)
	for _, tool := range s.Tools() {
		if tool.Name == "remote_api" {
			if tool.ReadOnly {
				t.Fatal("remote_api can mutate — its ReadOnly flag (the readOnlyHint source) must be false")
			}
			if !tool.ListedInReadOnly {
				t.Fatal("remote_api must stay listed in read-only mode (handler enforces GET-only)")
			}
			return
		}
	}
	t.Fatal("remote_api not found")
}

func TestReadOnlyModeCreatesNothingOnDisk(t *testing.T) {
	dir := t.TempDir()
	storeDir := filepath.Join(dir, "absent-store")
	s := &Server{StoreDir: storeDir, WorkDir: dir, ReadOnly: true}

	for _, name := range []string{"local_runs_list", "local_board_list_issues"} {
		res, err := s.Call(context.Background(), name, json.RawMessage(`{}`))
		if err != nil {
			t.Fatalf("Call(%s): %v", name, err)
		}
		if !res.IsError {
			t.Fatalf("%s on an absent store should error in read-only mode: %+v", name, res)
		}
		if !strings.Contains(res.Content[0].Text, "read-only mode") {
			t.Fatalf("%s should explain the read-only refusal: %s", name, res.Content[0].Text)
		}
	}
	if _, err := os.Stat(storeDir); !os.IsNotExist(err) {
		t.Fatalf("read-only mode created the store directory (stat err: %v)", err)
	}
}

func TestCallReportsToolErrorsInBand(t *testing.T) {
	s := newTestServer(t)
	res, err := s.Call(context.Background(), "local_run_get", json.RawMessage(`{"run_id":"missing-run"}`))
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if !res.IsError {
		t.Fatalf("want isError=true for a missing run, got %+v", res)
	}
	if len(res.Content) != 1 || res.Content[0].Text == "" {
		t.Fatalf("want one non-empty text block, got %+v", res.Content)
	}
}
