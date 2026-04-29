package context

import (
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/SocialGouv/claw-code-go/internal/config"
)

// Assembler collects and caches project context for injection into the system prompt.
type Assembler struct {
	WorkDir string

	mu          sync.Mutex
	memCache    string
	memMtimes   map[string]int64
	frontmatter *config.FrontmatterConfig
}

// NewAssembler creates an Assembler for the given working directory.
func NewAssembler(workDir string) *Assembler {
	return &Assembler{WorkDir: workDir}
}

// Assemble returns a formatted context block combining environment info, git status,
// and CLAUDE.md memory files. Any individual failure is silently skipped.
func (a *Assembler) Assemble() string {
	var sections []string

	if info := SystemInfo(a.WorkDir); info != "" {
		sections = append(sections, "# Environment\n\n"+info)
	}

	if git := GitStatus(a.WorkDir); git != "" {
		sections = append(sections, "# Git Status\n\n"+git)
	}

	if mem := a.loadMemory(); mem != "" {
		sections = append(sections, "# Project Instructions (CLAUDE.md)\n\n"+mem)
	}

	if len(sections) == 0 {
		return ""
	}
	return strings.Join(sections, "\n\n")
}

// loadMemory returns cached CLAUDE.md content, re-reading only when files change.
func (a *Assembler) loadMemory() string {
	a.mu.Lock()
	defer a.mu.Unlock()

	current := MemoryFileMtimes(a.WorkDir)
	if !mtimesEqual(current, a.memMtimes) {
		a.memCache = LoadMemoryFiles(a.WorkDir)
		a.memMtimes = current
	}
	return a.memCache
}

// LoadFrontmatter reads the primary CLAUDE.md in the work directory and parses
// any YAML frontmatter config overrides. Returns nil if no frontmatter found.
func (a *Assembler) LoadFrontmatter() *config.FrontmatterConfig {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.frontmatter != nil {
		return a.frontmatter
	}
	data, err := os.ReadFile(filepath.Join(a.WorkDir, "CLAUDE.md"))
	if err != nil {
		return nil
	}
	fm, _, err := config.ParseFrontmatter(data)
	if err != nil {
		return nil
	}
	if fm.Model != nil || fm.PermissionMode != nil || len(fm.AllowedTools) > 0 {
		a.frontmatter = &fm
	}
	return a.frontmatter
}

// Frontmatter returns the parsed frontmatter config from CLAUDE.md, if any.
func (a *Assembler) Frontmatter() *config.FrontmatterConfig {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.frontmatter
}

func mtimesEqual(a, b map[string]int64) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}
