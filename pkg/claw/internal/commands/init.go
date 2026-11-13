package commands

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const starterClawJSON = `{
  "permissions": {
    "defaultMode": "dontAsk"
  }
}
`

const gitignoreComment = "# Claw Code local artifacts"

var gitignoreEntries = []string{".claw/settings.local.json", ".claw/sessions/"}

// InitStatus describes the outcome of creating a single init artifact.
type InitStatus int

const (
	// StatusCreated means the artifact was newly created.
	StatusCreated InitStatus = iota
	// StatusUpdated means the artifact existed and was modified.
	StatusUpdated
	// StatusSkipped means the artifact already existed and was left unchanged.
	StatusSkipped
)

// Label returns a human-readable label for the status.
func (s InitStatus) Label() string {
	switch s {
	case StatusCreated:
		return "created"
	case StatusUpdated:
		return "updated"
	case StatusSkipped:
		return "skipped (already exists)"
	default:
		return "unknown"
	}
}

// InitArtifact records the name and outcome of a single init artifact.
type InitArtifact struct {
	Name   string
	Status InitStatus
}

// InitReport collects all artifacts created or skipped during init.
type InitReport struct {
	ProjectRoot string
	Artifacts   []InitArtifact
}

// Render formats the init report for human display.
func (r *InitReport) Render() string {
	var lines []string
	lines = append(lines, "Init")
	lines = append(lines, fmt.Sprintf("  Project          %s", r.ProjectRoot))
	for _, a := range r.Artifacts {
		lines = append(lines, fmt.Sprintf("  %-16s %s", a.Name, a.Status.Label()))
	}
	lines = append(lines, "  Next step        Review and tailor the generated guidance")
	return strings.Join(lines, "\n")
}

// InitializeRepo scaffolds a new project directory with .claw/, .claw.json,
// .gitignore entries, and a CLAUDE.md file.
func InitializeRepo(cwd string) (*InitReport, error) {
	var artifacts []InitArtifact

	clawDir := filepath.Join(cwd, ".claw")
	status, err := ensureDir(clawDir)
	if err != nil {
		return nil, err
	}
	artifacts = append(artifacts, InitArtifact{Name: ".claw/", Status: status})

	clawJSON := filepath.Join(cwd, ".claw.json")
	status, err = writeFileIfMissing(clawJSON, starterClawJSON)
	if err != nil {
		return nil, err
	}
	artifacts = append(artifacts, InitArtifact{Name: ".claw.json", Status: status})

	gitignorePath := filepath.Join(cwd, ".gitignore")
	status, err = ensureGitignoreEntries(gitignorePath)
	if err != nil {
		return nil, err
	}
	artifacts = append(artifacts, InitArtifact{Name: ".gitignore", Status: status})

	claudeMD := filepath.Join(cwd, "CLAUDE.md")
	content := RenderInitClaudeMD(cwd)
	status, err = writeFileIfMissing(claudeMD, content)
	if err != nil {
		return nil, err
	}
	artifacts = append(artifacts, InitArtifact{Name: "CLAUDE.md", Status: status})

	return &InitReport{
		ProjectRoot: cwd,
		Artifacts:   artifacts,
	}, nil
}

func ensureDir(path string) (InitStatus, error) {
	info, err := os.Stat(path)
	if err == nil && info.IsDir() {
		return StatusSkipped, nil
	}
	if err := os.MkdirAll(path, 0755); err != nil {
		return 0, err
	}
	return StatusCreated, nil
}

func writeFileIfMissing(path string, content string) (InitStatus, error) {
	if _, err := os.Stat(path); err == nil {
		return StatusSkipped, nil
	}
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		return 0, err
	}
	return StatusCreated, nil
}

func ensureGitignoreEntries(path string) (InitStatus, error) {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		lines := []string{gitignoreComment}
		lines = append(lines, gitignoreEntries...)
		if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0644); err != nil {
			return 0, err
		}
		return StatusCreated, nil
	}

	existing, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}

	lines := strings.Split(strings.TrimRight(string(existing), "\n"), "\n")
	changed := false

	hasComment := false
	for _, line := range lines {
		if line == gitignoreComment {
			hasComment = true
			break
		}
	}
	if !hasComment {
		lines = append(lines, gitignoreComment)
		changed = true
	}

	for _, entry := range gitignoreEntries {
		found := false
		for _, line := range lines {
			if line == entry {
				found = true
				break
			}
		}
		if !found {
			lines = append(lines, entry)
			changed = true
		}
	}

	if !changed {
		return StatusSkipped, nil
	}

	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0644); err != nil {
		return 0, err
	}
	return StatusUpdated, nil
}

// repoDetection holds markers about the detected project.
type repoDetection struct {
	rustWorkspace bool
	rustRoot      bool
	python        bool
	packageJSON   bool
	typescript    bool
	nextjs        bool
	react         bool
	vite          bool
	nest          bool
	srcDir        bool
	testsDir      bool
	rustDir       bool
}

// RenderInitClaudeMD generates CLAUDE.md content based on detected repo markers.
func RenderInitClaudeMD(cwd string) string {
	detection := detectRepo(cwd)
	var lines []string
	lines = append(lines, "# CLAUDE.md")
	lines = append(lines, "")
	lines = append(lines, "This file provides guidance to Claw Code (clawcode.dev) when working with code in this repository.")
	lines = append(lines, "")

	languages := detectedLanguages(&detection)
	frameworks := detectedFrameworks(&detection)
	lines = append(lines, "## Detected stack")
	if len(languages) == 0 {
		lines = append(lines, "- No specific language markers were detected yet; document the primary language and verification commands once the project structure settles.")
	} else {
		lines = append(lines, fmt.Sprintf("- Languages: %s.", strings.Join(languages, ", ")))
	}
	if len(frameworks) == 0 {
		lines = append(lines, "- Frameworks: none detected from the supported starter markers.")
	} else {
		lines = append(lines, fmt.Sprintf("- Frameworks/tooling markers: %s.", strings.Join(frameworks, ", ")))
	}
	lines = append(lines, "")

	verificationLines := verificationLines(cwd, &detection)
	if len(verificationLines) > 0 {
		lines = append(lines, "## Verification")
		lines = append(lines, verificationLines...)
		lines = append(lines, "")
	}

	structureLines := repositoryShapeLines(&detection)
	if len(structureLines) > 0 {
		lines = append(lines, "## Repository shape")
		lines = append(lines, structureLines...)
		lines = append(lines, "")
	}

	frameworkLines := frameworkNotes(&detection)
	if len(frameworkLines) > 0 {
		lines = append(lines, "## Framework notes")
		lines = append(lines, frameworkLines...)
		lines = append(lines, "")
	}

	lines = append(lines, "## Working agreement")
	lines = append(lines, "- Prefer small, reviewable changes and keep generated bootstrap files aligned with actual repo workflows.")
	lines = append(lines, "- Keep shared defaults in `.claw.json`; reserve `.claw/settings.local.json` for machine-local overrides.")
	lines = append(lines, "- Do not overwrite existing `CLAUDE.md` content automatically; update it intentionally when repo workflows change.")
	lines = append(lines, "")

	return strings.Join(lines, "\n")
}

func detectRepo(cwd string) repoDetection {
	pkgJSONContent := ""
	if data, err := os.ReadFile(filepath.Join(cwd, "package.json")); err == nil {
		pkgJSONContent = strings.ToLower(string(data))
	}

	return repoDetection{
		rustWorkspace: fileExists(filepath.Join(cwd, "rust", "Cargo.toml")),
		rustRoot:      fileExists(filepath.Join(cwd, "Cargo.toml")),
		python: fileExists(filepath.Join(cwd, "pyproject.toml")) ||
			fileExists(filepath.Join(cwd, "requirements.txt")) ||
			fileExists(filepath.Join(cwd, "setup.py")),
		packageJSON: fileExists(filepath.Join(cwd, "package.json")),
		typescript: fileExists(filepath.Join(cwd, "tsconfig.json")) ||
			strings.Contains(pkgJSONContent, "typescript"),
		nextjs:   strings.Contains(pkgJSONContent, `"next"`),
		react:    strings.Contains(pkgJSONContent, `"react"`),
		vite:     strings.Contains(pkgJSONContent, `"vite"`),
		nest:     strings.Contains(pkgJSONContent, "@nestjs"),
		srcDir:   dirExists(filepath.Join(cwd, "src")),
		testsDir: dirExists(filepath.Join(cwd, "tests")),
		rustDir:  dirExists(filepath.Join(cwd, "rust")),
	}
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func dirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

func detectedLanguages(d *repoDetection) []string {
	var languages []string
	if d.rustWorkspace || d.rustRoot {
		languages = append(languages, "Rust")
	}
	if d.python {
		languages = append(languages, "Python")
	}
	if d.typescript {
		languages = append(languages, "TypeScript")
	} else if d.packageJSON {
		languages = append(languages, "JavaScript/Node.js")
	}
	return languages
}

func detectedFrameworks(d *repoDetection) []string {
	var frameworks []string
	if d.nextjs {
		frameworks = append(frameworks, "Next.js")
	}
	if d.react {
		frameworks = append(frameworks, "React")
	}
	if d.vite {
		frameworks = append(frameworks, "Vite")
	}
	if d.nest {
		frameworks = append(frameworks, "NestJS")
	}
	return frameworks
}

func verificationLines(cwd string, d *repoDetection) []string {
	var lines []string
	if d.rustWorkspace {
		lines = append(lines, "- Run Rust verification from `rust/`: `cargo fmt`, `cargo clippy --workspace --all-targets -- -D warnings`, `cargo test --workspace`")
	} else if d.rustRoot {
		lines = append(lines, "- Run Rust verification from the repo root: `cargo fmt`, `cargo clippy --workspace --all-targets -- -D warnings`, `cargo test --workspace`")
	}
	if d.python {
		if fileExists(filepath.Join(cwd, "pyproject.toml")) {
			lines = append(lines, "- Run the Python project checks declared in `pyproject.toml` (for example: `pytest`, `ruff check`, and `mypy` when configured).")
		} else {
			lines = append(lines, "- Run the repo's Python test/lint commands before shipping changes.")
		}
	}
	if d.packageJSON {
		lines = append(lines, "- Run the JavaScript/TypeScript checks from `package.json` before shipping changes (`npm test`, `npm run lint`, `npm run build`, or the repo equivalent).")
	}
	if d.testsDir && d.srcDir {
		lines = append(lines, "- `src/` and `tests/` are both present; update both surfaces together when behavior changes.")
	}
	return lines
}

func repositoryShapeLines(d *repoDetection) []string {
	var lines []string
	if d.rustDir {
		lines = append(lines, "- `rust/` contains the Rust workspace and active CLI/runtime implementation.")
	}
	if d.srcDir {
		lines = append(lines, "- `src/` contains source files that should stay consistent with generated guidance and tests.")
	}
	if d.testsDir {
		lines = append(lines, "- `tests/` contains validation surfaces that should be reviewed alongside code changes.")
	}
	return lines
}

func frameworkNotes(d *repoDetection) []string {
	var lines []string
	if d.nextjs {
		lines = append(lines, "- Next.js detected: preserve routing/data-fetching conventions and verify production builds after changing app structure.")
	}
	if d.react && !d.nextjs {
		lines = append(lines, "- React detected: keep component behavior covered with focused tests and avoid unnecessary prop/API churn.")
	}
	if d.vite {
		lines = append(lines, "- Vite detected: validate the production bundle after changing build-sensitive configuration or imports.")
	}
	if d.nest {
		lines = append(lines, "- NestJS detected: keep module/provider boundaries explicit and verify controller/service wiring after refactors.")
	}
	return lines
}
