package cli

import (
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

//go:embed templates/pr_refine_single_model_delegate.iter
var defaultWorkflow []byte

const workflowFileName = "pr_refine_single_model_delegate.iter"

const envExample = `# Iterion environment configuration
# Copy this file to .env and fill in optional settings.

# ── Delegation ────────────────────────────────────────────
# The example workflow delegates to claude-code.
# Authenticate with: claude login
# No API key is needed here — the claude CLI manages its own auth.

# Optional: override the workspace directory.
# PROJECT_DIR=.
`

var gitignoreEntries = []string{".iterion/", ".env"}

// InitOptions holds the configuration for the init command.
type InitOptions struct {
	Dir string // target directory (default: ".")
}

// InitResult holds the outcome for JSON output.
type InitResult struct {
	Dir          string   `json:"dir"`
	FilesCreated []string `json:"files_created"`
	FilesSkipped []string `json:"files_skipped"`
}

// RunInit initializes a directory with an example workflow and environment config.
func RunInit(opts InitOptions, p *Printer) error {
	dir := opts.Dir
	if dir == "" {
		dir = "."
	}

	// Ensure target directory exists.
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("cannot create directory %q: %w", dir, err)
	}

	absDir, err := filepath.Abs(dir)
	if err != nil {
		return fmt.Errorf("cannot resolve directory: %w", err)
	}

	var created, skipped []string

	// Write workflow file.
	wfStatus, err := writeIfAbsent(filepath.Join(dir, workflowFileName), defaultWorkflow)
	if err != nil {
		return err
	}
	trackFile(&created, &skipped, workflowFileName, wfStatus)

	// Write .env.example.
	envStatus, err := writeIfAbsent(filepath.Join(dir, ".env.example"), []byte(envExample))
	if err != nil {
		return err
	}
	trackFile(&created, &skipped, ".env.example", envStatus)

	// Update .gitignore.
	giStatus, err := updateGitignore(dir, gitignoreEntries)
	if err != nil {
		return err
	}
	trackFile(&created, &skipped, ".gitignore", giStatus)

	// Output.
	if p.Format == OutputJSON {
		p.JSON(InitResult{
			Dir:          absDir,
			FilesCreated: created,
			FilesSkipped: skipped,
		})
		return nil
	}

	p.Header("Init")
	p.KV("Directory", absDir)
	p.Blank()

	for _, f := range created {
		p.Line("  + %s", f)
	}
	for _, f := range skipped {
		p.Line("  ~ %s (already exists, skipped)", f)
	}

	p.Blank()
	p.Line("  Next steps:")
	p.Line("    1. Install Claude Code CLI (if not already):")
	p.Line("         npm install -g @anthropic-ai/claude-code")
	p.Line("    2. Authenticate:")
	p.Line("         claude login")
	p.Line("    3. Run the example workflow:")
	p.Line("         iterion run %s \\", workflowFileName)
	p.Line("           --var pr_title=\"Fix auth middleware\" \\")
	p.Line("           --var review_rules=\"No SQL injection, all errors handled\" \\")
	p.Line("           --var compliance_rules=\"OWASP top 10\"")
	p.Blank()

	return nil
}

// fileStatus indicates what happened when writing a file.
type fileStatus int

const (
	fileCreated fileStatus = iota
	fileSkipped
	fileUpdated
)

func trackFile(created, skipped *[]string, name string, status fileStatus) {
	switch status {
	case fileCreated, fileUpdated:
		*created = append(*created, name)
	case fileSkipped:
		*skipped = append(*skipped, name)
	}
}

// writeIfAbsent writes data to path only if the file does not already exist.
func writeIfAbsent(path string, data []byte) (fileStatus, error) {
	if _, err := os.Stat(path); err == nil {
		return fileSkipped, nil
	}
	if err := os.WriteFile(path, data, 0644); err != nil {
		return 0, fmt.Errorf("cannot write %s: %w", filepath.Base(path), err)
	}
	return fileCreated, nil
}

// updateGitignore ensures the given entries exist in the .gitignore file.
// Creates the file if it doesn't exist; appends missing entries otherwise.
func updateGitignore(dir string, entries []string) (fileStatus, error) {
	path := filepath.Join(dir, ".gitignore")

	existing, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return 0, fmt.Errorf("cannot read .gitignore: %w", err)
	}

	isNew := os.IsNotExist(err)

	// Build set of existing lines.
	lines := strings.Split(string(existing), "\n")
	have := make(map[string]bool)
	for _, l := range lines {
		have[strings.TrimSpace(l)] = true
	}

	// Determine which entries to add.
	var toAdd []string
	for _, e := range entries {
		if !have[e] {
			toAdd = append(toAdd, e)
		}
	}

	if len(toAdd) == 0 {
		return fileSkipped, nil
	}

	// Append missing entries.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return 0, fmt.Errorf("cannot open .gitignore: %w", err)
	}
	defer f.Close()

	// Ensure we start on a new line if the file doesn't end with one.
	if len(existing) > 0 && existing[len(existing)-1] != '\n' {
		fmt.Fprintln(f)
	}

	for _, e := range toAdd {
		fmt.Fprintln(f, e)
	}

	if isNew {
		return fileCreated, nil
	}
	return fileUpdated, nil
}
