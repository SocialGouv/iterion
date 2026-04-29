package cli_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/cli"
)

func TestRunInit_CreatesFiles(t *testing.T) {
	dir := t.TempDir()
	p := &cli.Printer{W: &bytes.Buffer{}, Format: cli.OutputHuman}

	if err := cli.RunInit(cli.InitOptions{Dir: dir}, p); err != nil {
		t.Fatal(err)
	}

	// Verify workflow file exists and is non-empty.
	data, err := os.ReadFile(filepath.Join(dir, "pr_refine_single_model_backend.iter"))
	if err != nil {
		t.Fatal("workflow file not created:", err)
	}
	if len(data) == 0 {
		t.Fatal("workflow file is empty")
	}
	if !strings.Contains(string(data), "workflow pr_refine_single_model_backend") {
		t.Error("workflow file missing expected content")
	}

	// Verify .env.example exists.
	data, err = os.ReadFile(filepath.Join(dir, ".env.example"))
	if err != nil {
		t.Fatal(".env.example not created:", err)
	}
	if !strings.Contains(string(data), "claude login") {
		t.Error(".env.example missing claude login instructions")
	}

	// Verify .gitignore exists with expected entries.
	data, err = os.ReadFile(filepath.Join(dir, ".gitignore"))
	if err != nil {
		t.Fatal(".gitignore not created:", err)
	}
	content := string(data)
	if !strings.Contains(content, ".iterion/") {
		t.Error(".gitignore missing .iterion/ entry")
	}
	if !strings.Contains(content, ".env") {
		t.Error(".gitignore missing .env entry")
	}
}

func TestRunInit_Idempotent(t *testing.T) {
	dir := t.TempDir()
	p := &cli.Printer{W: &bytes.Buffer{}, Format: cli.OutputHuman}

	// First run creates files.
	if err := cli.RunInit(cli.InitOptions{Dir: dir}, p); err != nil {
		t.Fatal(err)
	}

	// Record contents.
	wf1, _ := os.ReadFile(filepath.Join(dir, "pr_refine_single_model_backend.iter"))
	env1, _ := os.ReadFile(filepath.Join(dir, ".env.example"))

	// Second run should skip everything.
	buf := &bytes.Buffer{}
	p2 := &cli.Printer{W: buf, Format: cli.OutputHuman}
	if err := cli.RunInit(cli.InitOptions{Dir: dir}, p2); err != nil {
		t.Fatal(err)
	}

	output := buf.String()
	if !strings.Contains(output, "already exists, skipped") {
		t.Error("second run should report files as skipped")
	}

	// Verify contents unchanged.
	wf2, _ := os.ReadFile(filepath.Join(dir, "pr_refine_single_model_backend.iter"))
	env2, _ := os.ReadFile(filepath.Join(dir, ".env.example"))

	if !bytes.Equal(wf1, wf2) {
		t.Error("workflow file was modified on second run")
	}
	if !bytes.Equal(env1, env2) {
		t.Error(".env.example was modified on second run")
	}
}

func TestRunInit_ExistingGitignore(t *testing.T) {
	dir := t.TempDir()

	// Create a .gitignore with one of the entries already present.
	existing := "node_modules/\n.iterion/\n"
	if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte(existing), 0644); err != nil {
		t.Fatal(err)
	}

	p := &cli.Printer{W: &bytes.Buffer{}, Format: cli.OutputHuman}
	if err := cli.RunInit(cli.InitOptions{Dir: dir}, p); err != nil {
		t.Fatal(err)
	}

	data, _ := os.ReadFile(filepath.Join(dir, ".gitignore"))
	content := string(data)

	// Should have appended .env but not duplicated .iterion/.
	if strings.Count(content, ".iterion/") != 1 {
		t.Errorf(".iterion/ duplicated in .gitignore: %q", content)
	}
	if !strings.Contains(content, ".env") {
		t.Error(".gitignore missing .env entry after update")
	}
}

func TestRunInit_CreatesSubdir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "my-project")
	p := &cli.Printer{W: &bytes.Buffer{}, Format: cli.OutputHuman}

	if err := cli.RunInit(cli.InitOptions{Dir: dir}, p); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(filepath.Join(dir, "pr_refine_single_model_backend.iter")); err != nil {
		t.Error("workflow file not created in subdirectory:", err)
	}
}

func TestRunInit_JSONOutput(t *testing.T) {
	dir := t.TempDir()
	buf := &bytes.Buffer{}
	p := &cli.Printer{W: buf, Format: cli.OutputJSON}

	if err := cli.RunInit(cli.InitOptions{Dir: dir}, p); err != nil {
		t.Fatal(err)
	}

	var result cli.InitResult
	if err := json.Unmarshal(buf.Bytes(), &result); err != nil {
		t.Fatal("invalid JSON output:", err)
	}

	if len(result.FilesCreated) != 3 {
		t.Errorf("expected 3 files created, got %d: %v", len(result.FilesCreated), result.FilesCreated)
	}
	if result.Dir == "" {
		t.Error("dir should not be empty in JSON output")
	}
}
