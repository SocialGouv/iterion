// Package skilllib implements iterion's first-class skill library: a
// standalone, operator-curated store of Claude-Code-style SKILL.md skills,
// stored globally (~/.iterion/skills/) with an optional per-project override,
// and referenced from workflows via the DSL `skills:` field. See ADR-059 and
// docs/skills-library.md.
//
// The library is the hand-authored/editable half of the "hybride" model; the
// plugin path (plugin.SynthesizeSkillsManifest) remains the third-party-pack
// import route. Both mirror into <workspace>/.claude/skills/ at run start via
// the shared runtime.reconcileSkillFile collision policy.
package skilllib

import (
	"bufio"
	"io"
	"strings"
)

// ScanFrontmatter reads leading YAML-ish frontmatter (delimited by `---`
// lines) from r and returns the `name:` and `description:` fields. It is
// tolerant of a missing frontmatter block (returns empty strings) and never
// errors on content — only the caller's file open can fail. This is the single
// shared parser used by both the skill library and runview's bundle-skill
// catalog (runview.readSkillFile), so the two never drift.
func ScanFrontmatter(r io.Reader) (name, description string) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), 1<<20)
	// Walk to the opening "---", then collect key:value lines until the
	// closing "---". Tolerant of leading whitespace and a file with no
	// frontmatter block at all (the first content line ends the scan).
	opened := false
	for scanner.Scan() {
		line := strings.TrimRight(scanner.Text(), "\r")
		trimmed := strings.TrimSpace(line)
		if !opened {
			if trimmed == "---" {
				opened = true
				continue
			}
			if trimmed == "" {
				continue
			}
			break // first non-frontmatter content line → no frontmatter
		}
		if trimmed == "---" {
			break
		}
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		key := strings.ToLower(strings.TrimSpace(k))
		val := strings.TrimSpace(v)
		val = strings.TrimPrefix(val, "\"")
		val = strings.TrimSuffix(val, "\"")
		switch key {
		case "name":
			name = val
		case "description":
			description = val
		}
	}
	return name, description
}
