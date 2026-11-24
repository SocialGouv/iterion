package tools

import (
	"embed"
	"io/fs"
	"path"
	"sort"
	"strings"
	"sync"
)

//go:embed all:bundled_skills
var bundledSkillsFS embed.FS

// bundledSkillsRoot is the prefix used inside bundledSkillsFS.
const bundledSkillsRoot = "bundled_skills"

var (
	bundledIndexOnce sync.Once
	// bundledIndex maps "<plugin>:<skill>" → relative path inside bundledSkillsFS.
	bundledIndex map[string]string
)

func buildBundledIndex() {
	bundledIndex = make(map[string]string)
	plugins, err := fs.ReadDir(bundledSkillsFS, bundledSkillsRoot)
	if err != nil {
		return
	}
	for _, plug := range plugins {
		if !plug.IsDir() {
			continue
		}
		pluginName := plug.Name()
		skillsDir := path.Join(bundledSkillsRoot, pluginName, "skills")
		skills, err := fs.ReadDir(bundledSkillsFS, skillsDir)
		if err != nil {
			continue
		}
		for _, s := range skills {
			if !s.IsDir() {
				continue
			}
			candidate := path.Join(skillsDir, s.Name(), "SKILL.md")
			if _, err := fs.Stat(bundledSkillsFS, candidate); err == nil {
				bundledIndex[pluginName+":"+s.Name()] = candidate
			}
		}
	}
}

func getBundledIndex() map[string]string {
	bundledIndexOnce.Do(buildBundledIndex)
	return bundledIndex
}

// lookupBundledNamespaced finds the embed:// URI for a "<plugin>:<skill>" pair.
func lookupBundledNamespaced(plugin, skill string) (string, bool) {
	idx := getBundledIndex()
	rel, ok := idx[plugin+":"+skill]
	if !ok {
		return "", false
	}
	return embedScheme + rel, true
}

// lookupBundledBare resolves a bare skill name (no plugin prefix) across the
// embedded bundle. Returns the embed:// URI and canonical name on a unique
// match, or ambiguousCandidates (length >= 2) when several plugins ship the
// same skill name.
func lookupBundledBare(name string) (uri string, canonical string, ambiguousCandidates []string, err error) {
	idx := getBundledIndex()
	var matches []string
	var paths []string
	for canon, rel := range idx {
		colon := strings.IndexByte(canon, ':')
		if colon < 0 {
			continue
		}
		if canon[colon+1:] == name {
			matches = append(matches, canon)
			paths = append(paths, rel)
		}
	}
	switch len(matches) {
	case 0:
		return "", "", nil, nil
	case 1:
		return embedScheme + paths[0], matches[0], nil, nil
	default:
		return "", "", matches, nil
	}
}

// ListBundledSkills returns canonical "<plugin>:<skill>" names of every
// embedded skill, sorted for stable output.
func ListBundledSkills() []string {
	idx := getBundledIndex()
	out := make([]string, 0, len(idx))
	for k := range idx {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
