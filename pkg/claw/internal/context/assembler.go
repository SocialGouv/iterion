package context

import (
	"os"
	"strings"
	"sync"
)

// AssembleOptions selects which context sections Assemble emits. The zero
// value disables everything — construct via DefaultAssembleOptions (all on)
// and flip sections off, or use NewAssembler which defaults to all-on.
type AssembleOptions struct {
	Environment         bool
	GitStatus           bool
	ProjectInstructions bool
	// AutoMemory injects the persistent MEMORY.md section (path +
	// maintenance instructions + current content).
	AutoMemory bool
	// AutoMemoryDir overrides where that section reads MEMORY.md from.
	// Empty keeps the default derived from workDir. Set it when the host
	// owns the memory location — a workDir that changes per session (a
	// worktree, a container) otherwise fingerprints to a fresh, empty
	// directory every run.
	AutoMemoryDir string
	// Memory configures CLAUDE.md discovery (walk-up, imports, size cap and
	// which scopes are read) when ProjectInstructions is enabled.
	Memory MemoryOptions
}

// DefaultAssembleOptions returns the all-on default (Claude Code parity).
func DefaultAssembleOptions() AssembleOptions {
	return AssembleOptions{
		Environment:         true,
		GitStatus:           true,
		ProjectInstructions: true,
		AutoMemory:          true,
		Memory:              DefaultMemoryOptions(),
	}
}

// Assembler collects and caches project context for injection into the system prompt.
type Assembler struct {
	WorkDir string

	opts AssembleOptions

	mu            sync.Mutex
	memCache      string
	memKey        memoryCacheKey   // what memCache was built for
	memLoaded     bool             // memCache holds a load (memKey is meaningful)
	memMtimes     map[string]int64 // files actually read on the last load (incl. imports)
	memCandidates map[string]int64 // discovery candidates present on the last check
}

// memoryCacheKey is everything the memory section is a function of besides the
// files on disk: where it is built for and how it is scoped. The file state is
// tracked by mtime; this is the rest. It is compared whole, so a MemoryOptions
// field added later is in the key without anyone remembering to put it there —
// and one that cannot be compared (a slice, a map) stops this compiling rather
// than silently dropping out of it.
//
// The mtime check alone cannot tell two scopes apart: the same files can carry
// different labels (HOME is an ancestor of workDir, so ~/.claude/CLAUDE.md is
// the user's file under one scope and an ancestor's under another).
type memoryCacheKey struct {
	workDir string
	memory  MemoryOptions
}

// NewAssembler creates an Assembler for the given working directory with all
// context sections enabled.
func NewAssembler(workDir string) *Assembler {
	return NewAssemblerWithOptions(workDir, DefaultAssembleOptions())
}

// NewAssemblerWithOptions creates an Assembler emitting only the sections
// enabled in opts.
func NewAssemblerWithOptions(workDir string, opts AssembleOptions) *Assembler {
	return &Assembler{WorkDir: workDir, opts: opts}
}

// Assemble returns a formatted context block combining environment info, git status,
// and CLAUDE.md memory files. Any individual failure is silently skipped.
func (a *Assembler) Assemble() string {
	var sections []string

	if a.opts.Environment {
		if info := SystemInfo(a.WorkDir); info != "" {
			sections = append(sections, "# Environment\n\n"+info)
		}
	}

	if a.opts.GitStatus {
		if git := GitStatus(a.WorkDir); git != "" {
			sections = append(sections, "# Git Status\n\n"+git)
		}
	}

	if a.opts.ProjectInstructions {
		if mem := a.loadMemory(); mem != "" {
			sections = append(sections, "# Project Instructions (CLAUDE.md)\n\n"+mem)
		}
	}

	if a.opts.AutoMemory {
		dir := a.opts.AutoMemoryDir
		if dir == "" {
			dir = AutoMemoryDir(a.WorkDir)
		}
		if auto := LoadAutoMemorySectionAt(dir); auto != "" {
			sections = append(sections, "# Auto memory\n\n"+auto)
		}
	}

	if len(sections) == 0 {
		return ""
	}
	return strings.Join(sections, "\n\n")
}

// loadMemory returns cached CLAUDE.md content, re-reading only when it can have
// changed. The cache is valid for one memoryCacheKey — the work directory and
// the memory options — and revalidated in two parts: re-stat the discovery
// candidates (notices created/deleted memory files) and the files actually
// read on the last load (notices edited roots AND imports).
func (a *Assembler) loadMemory() string {
	a.mu.Lock()
	defer a.mu.Unlock()

	key := memoryCacheKey{workDir: a.WorkDir, memory: a.opts.Memory}
	candidates := MemoryCandidateMtimes(a.WorkDir, a.opts.Memory)
	if a.memLoaded && key == a.memKey &&
		mtimesEqual(candidates, a.memCandidates) && mtimesEqual(currentMtimes(a.memMtimes), a.memMtimes) {
		return a.memCache
	}
	a.memCache, a.memMtimes = LoadMemory(a.WorkDir, a.opts.Memory)
	a.memCandidates, a.memKey, a.memLoaded = candidates, key, true
	return a.memCache
}

// currentMtimes re-stats the paths in prev, returning their current mtimes
// (absent paths are omitted, so a deleted import invalidates the cache).
func currentMtimes(prev map[string]int64) map[string]int64 {
	result := make(map[string]int64, len(prev))
	for p := range prev {
		if info, err := os.Stat(p); err == nil {
			result[p] = info.ModTime().UnixNano()
		}
	}
	return result
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
