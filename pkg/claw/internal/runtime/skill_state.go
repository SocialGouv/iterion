package runtime

import (
	"fmt"
	"strings"
	"sync"
)

// ActiveSkill captures the metadata of the skill currently scoping the
// conversation. When AllowedTools is non-empty, tool dispatches are filtered
// against this list (with a small set of always-allowed tools).
type ActiveSkill struct {
	Name         string
	AllowedTools []string // user-visible (used in error messages)
}

// alwaysAllowedTools are dispatched regardless of the active skill's
// allowed-tools list. They cover meta-operations the model needs to recover
// or change context.
var alwaysAllowedTools = map[string]struct{}{
	"skill":       {},
	"tool_search": {},
	"todo_write":  {},
}

// SkillStateLock guards the active skill on the loop. The normalized slice
// is computed once on Set so CheckAllowed is allocation-free.
type SkillStateLock struct {
	mu              sync.Mutex
	s               *ActiveSkill
	normalizedTools []string
}

// Set replaces the current active skill.
func (l *SkillStateLock) Set(s *ActiveSkill) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.s = s
	if s == nil || len(s.AllowedTools) == 0 {
		l.normalizedTools = nil
		return
	}
	l.normalizedTools = make([]string, len(s.AllowedTools))
	for i, t := range s.AllowedTools {
		l.normalizedTools[i] = normalizeToolName(t)
	}
}

// Clear drops the current active skill.
func (l *SkillStateLock) Clear() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.s = nil
	l.normalizedTools = nil
}

// Get returns a copy of the active skill (or nil). Used by tests and
// diagnostics — CheckAllowed avoids this on the hot path.
func (l *SkillStateLock) Get() *ActiveSkill {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.s == nil {
		return nil
	}
	cp := *l.s
	if l.s.AllowedTools != nil {
		cp.AllowedTools = append([]string(nil), l.s.AllowedTools...)
	}
	return &cp
}

// CheckAllowed returns nil if toolName may run under the current active
// skill restrictions, or an error otherwise. With no active skill (or no
// allowed-tools), every tool is permitted. The hot path (no active skill)
// is allocation-free.
func (l *SkillStateLock) CheckAllowed(toolName string) error {
	l.mu.Lock()
	if l.s == nil || len(l.normalizedTools) == 0 {
		l.mu.Unlock()
		return nil
	}
	if _, ok := alwaysAllowedTools[toolName]; ok {
		l.mu.Unlock()
		return nil
	}
	target := normalizeToolName(toolName)
	for _, n := range l.normalizedTools {
		if n == target {
			l.mu.Unlock()
			return nil
		}
	}
	// Hold the originals for the error message before unlocking.
	skillName := l.s.Name
	allowed := strings.Join(l.s.AllowedTools, ", ")
	l.mu.Unlock()
	return fmt.Errorf("tool %q is not in allowed-tools for active skill %q; allowed: [%s]",
		toolName, skillName, allowed)
}

// normalizeToolName collapses CamelCase / snake_case variants so that
// "BashTool" / "Bash" / "bash" match the dispatched "bash" identifier.
func normalizeToolName(name string) string {
	s := strings.TrimSuffix(name, "Tool")
	s = strings.TrimSuffix(s, "_tool")
	return strings.ToLower(strings.ReplaceAll(s, "_", ""))
}
