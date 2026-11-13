package runtime

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/SocialGouv/iterion/pkg/claw/internal/tools"
)

// applySkillSideEffects updates SkillState from an already-parsed skill
// invocation. When the skill has an allowed-tools list, subsequent tool
// dispatches are restricted until the next user turn. A nil invocation is
// a no-op.
func (loop *ConversationLoop) applySkillSideEffects(inv *tools.SkillInvocation) {
	if inv == nil {
		return
	}
	if len(inv.Frontmatter.AllowedTools) == 0 {
		// No restriction: clear any previous active skill so stale state
		// doesn't carry over across distinct skill invocations.
		loop.SkillState.Clear()
		return
	}
	loop.SkillState.Set(&ActiveSkill{
		Name:         inv.Resolution.CanonicalName,
		AllowedTools: append([]string(nil), inv.Frontmatter.AllowedTools...),
	})
}

// SkillList satisfies the slash-command `/skills list` interface.
// Returns canonical "<plugin>:<skill>" names, deduplicated across bundled
// and filesystem sources. Bundled sources are tagged "(bundled)"; filesystem
// sources are tagged with their origin path.
func (loop *ConversationLoop) SkillList() ([]string, error) {
	seen := map[string]string{}
	add := func(canonical, tag string) {
		if _, ok := seen[canonical]; !ok {
			seen[canonical] = tag
		}
	}

	for _, root := range tools.SkillLookupRoots(loop.workspaceRoot()) {
		entries, err := os.ReadDir(root)
		if err != nil {
			continue
		}
		for _, e := range entries {
			full := filepath.Join(root, e.Name())
			if !e.IsDir() {
				if strings.HasSuffix(e.Name(), ".md") {
					add(strings.TrimSuffix(e.Name(), ".md"), "local:"+root)
				}
				continue
			}
			skillsDir := filepath.Join(full, "skills")
			subs, err := os.ReadDir(skillsDir)
			if err != nil {
				if info, err := os.Stat(filepath.Join(full, "SKILL.md")); err == nil && !info.IsDir() {
					add(e.Name(), "local:"+root)
				}
				continue
			}
			for _, s := range subs {
				if !s.IsDir() {
					continue
				}
				if info, err := os.Stat(filepath.Join(skillsDir, s.Name(), "SKILL.md")); err == nil && !info.IsDir() {
					add(e.Name()+":"+s.Name(), "local:"+root)
				}
			}
		}
	}

	for _, name := range tools.ListBundledSkills() {
		add(name, "bundled")
	}

	out := make([]string, 0, len(seen))
	for name, tag := range seen {
		out = append(out, fmt.Sprintf("%s [%s]", name, tag))
	}
	sort.Strings(out)
	return out, nil
}

// SkillInvoke runs a skill on behalf of the user (e.g. /skills invoke).
// Bypasses disable-model-invocation and sets the active skill if the
// frontmatter has allowed-tools.
func (loop *ConversationLoop) SkillInvoke(name, args string) (string, error) {
	input := map[string]any{"skill": name, "args": args}
	out, inv, err := tools.ExecuteSkillEx(input, loop.workspaceRoot(), true)
	if err != nil {
		return "", err
	}
	loop.applySkillSideEffects(inv)
	return out, nil
}

// SkillInstall fetches the named skill from the official Anthropic
// marketplace and writes it to ~/.claude/skills/.
func (loop *ConversationLoop) SkillInstall(name string) error {
	dest, err := tools.InstallSkillFromMarketplace(name, tools.InstallOptions{})
	if err != nil {
		return err
	}
	fmt.Printf("Installed %s → %s\n", name, dest)
	return nil
}
