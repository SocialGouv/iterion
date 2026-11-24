package runtime

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/claw/internal/config"
	clawctx "github.com/SocialGouv/iterion/pkg/claw/internal/context"
)

// scopedPromptConfig has every memory scope field set to a value no other
// field shares, so a field wired to the wrong option is seen.
func scopedPromptConfig() PromptConfig {
	return PromptConfig{
		MemorySkipUser:         true,
		MemoryRoot:             "/srv/repo",
		MemorySkipWorkspace:    true,
		MemorySkipOuter:        true,
		MemoryClaudeCodeLayout: true,
	}
}

// Each scope field reaches exactly its own assembler option, and nothing else
// moves.
func TestAssembleOptionsMapsEveryMemoryScopeField(t *testing.T) {
	cases := []struct {
		name string
		set  func(*PromptConfig)
		want clawctx.MemoryOptions
	}{
		{"MemorySkipUser", func(p *PromptConfig) { p.MemorySkipUser = true }, clawctx.MemoryOptions{SkipUser: true}},
		{"MemoryRoot", func(p *PromptConfig) { p.MemoryRoot = "/srv/repo" }, clawctx.MemoryOptions{Root: "/srv/repo"}},
		{"MemorySkipWorkspace", func(p *PromptConfig) { p.MemorySkipWorkspace = true }, clawctx.MemoryOptions{SkipWorkspace: true}},
		{"MemorySkipOuter", func(p *PromptConfig) { p.MemorySkipOuter = true }, clawctx.MemoryOptions{SkipOuter: true}},
		{"MemoryClaudeCodeLayout", func(p *PromptConfig) { p.MemoryClaudeCodeLayout = true }, clawctx.MemoryOptions{ClaudeCodeLayout: true}},
		{"all five", func(p *PromptConfig) { *p = scopedPromptConfig() }, clawctx.MemoryOptions{
			SkipUser: true, Root: "/srv/repo", SkipWorkspace: true, SkipOuter: true, ClaudeCodeLayout: true,
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var p PromptConfig
			tc.set(&p)
			if got := p.AssembleOptions().Memory; got != tc.want {
				t.Errorf("Memory = %+v, want %+v", got, tc.want)
			}
		})
	}

	// The scope rides along with the existing memory options, which still map.
	p := scopedPromptConfig()
	p.MemoryWalkUp, p.MemoryImports, p.MemoryMaxBytes = true, true, 4096
	want := clawctx.MemoryOptions{
		WalkUp: true, Imports: true, MaxBytes: 4096,
		SkipUser: true, Root: "/srv/repo", SkipWorkspace: true, SkipOuter: true, ClaudeCodeLayout: true,
	}
	if got := p.AssembleOptions().Memory; got != want {
		t.Errorf("Memory = %+v, want %+v", got, want)
	}
}

// A host that never sets the scope gets the unscoped options, whichever preset
// it starts from.
func TestPromptConfigPresetsAreUnscoped(t *testing.T) {
	unscoped := func(m clawctx.MemoryOptions) bool {
		return !m.SkipUser && m.Root == "" && !m.SkipWorkspace && !m.SkipOuter && !m.ClaudeCodeLayout
	}
	for name, p := range map[string]PromptConfig{
		"zero":                     {},
		"DefaultPromptConfig":      DefaultPromptConfig(),
		"MinimalPromptConfig":      MinimalPromptConfig(),
		"ResolvePromptConfig(nil)": ResolvePromptConfig(nil),
		"ResolvePromptConfig(all explicit)": ResolvePromptConfig(&config.RuntimePromptConfig{
			ProjectInstructions: boolPtr(true), MemoryWalkUp: boolPtr(true), MemoryImports: boolPtr(true),
		}),
	} {
		if m := p.AssembleOptions().Memory; !unscoped(m) {
			t.Errorf("%s: scope set by default: %+v", name, m)
		}
	}
	if got := DefaultPromptConfig().AssembleOptions().Memory; got != clawctx.DefaultMemoryOptions() {
		t.Errorf("DefaultPromptConfig memory options = %+v, want %+v", got, clawctx.DefaultMemoryOptions())
	}
}

// The scope is what the host decided about the files the sections read; the
// section overrides turn sections on and off and leave it alone — a reset
// that quietly widened a restriction would put back what the host kept out.
func TestApplyPromptSectionOverridesKeepsTheMemoryScope(t *testing.T) {
	cases := []struct {
		name     string
		minimal  bool
		only     []string
		disable  []string
		wantSect func(PromptConfig) bool
	}{
		{"minimal", true, nil, nil, func(p PromptConfig) bool { return !p.ProjectInstructions && !p.Environment }},
		{"only list", false, []string{"project-instructions"}, nil, func(p PromptConfig) bool { return p.ProjectInstructions && !p.Environment }},
		{"disable list", false, nil, []string{"environment"}, func(p PromptConfig) bool { return p.ProjectInstructions && !p.Environment }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			start := DefaultPromptConfig()
			scope := scopedPromptConfig()
			start.MemorySkipUser, start.MemoryRoot, start.MemorySkipWorkspace = scope.MemorySkipUser, scope.MemoryRoot, scope.MemorySkipWorkspace
			start.MemorySkipOuter, start.MemoryClaudeCodeLayout = scope.MemorySkipOuter, scope.MemoryClaudeCodeLayout
			cfg := &Config{Prompt: &start}

			if err := ApplyPromptSectionOverrides(cfg, tc.minimal, tc.only, tc.disable); err != nil {
				t.Fatal(err)
			}
			got := *cfg.Prompt
			if !tc.wantSect(got) {
				t.Errorf("the section overrides did not apply: %+v", got)
			}
			if got.MemorySkipUser != scope.MemorySkipUser || got.MemoryRoot != scope.MemoryRoot ||
				got.MemorySkipWorkspace != scope.MemorySkipWorkspace || got.MemorySkipOuter != scope.MemorySkipOuter ||
				got.MemoryClaudeCodeLayout != scope.MemoryClaudeCodeLayout {
				t.Errorf("memory scope changed by the overrides: %+v", got)
			}
		})
	}
}

// The scope fields are values, not sections: a restriction's zero value is
// "everything", so naming one in an exclusive-enable list would turn the
// restriction ON while switching every real section off. They are not in the
// registry, and naming one is the same explicit error as any unknown section.
func TestMemoryScopeIsNotASection(t *testing.T) {
	for _, name := range []string{
		"memory-skip-user", "memorySkipUser", "memory_root", "memory-root", "memory-skip-workspace",
		"memory-skip-outer", "memory-claude-code-layout", "memoryClaudeCodeLayout",
	} {
		cfg := &Config{}
		err := ApplyPromptSectionOverrides(cfg, false, []string{name}, nil)
		if err == nil || !strings.Contains(err.Error(), "unknown prompt section") {
			t.Errorf("%q: want an unknown-section error, got %v", name, err)
		}
	}
	for _, name := range PromptSectionNames() {
		if strings.Contains(name, "skip") || strings.Contains(name, "root") || strings.Contains(name, "layout") {
			t.Errorf("registry lists %q, a scope value rather than a section", name)
		}
	}
}
