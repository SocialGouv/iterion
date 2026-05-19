package runview

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// BundleSkill is one entry in a bundle's skill catalog. Mirrors the
// shape of a parsed SKILL.md frontmatter block plus the relative path
// inside the bundle's skills/ directory. The studio's chatbox skill
// picker consumes this list verbatim.
type BundleSkill struct {
	// Name is the skill identifier — the SKILL.md basename without
	// extension, OR the `name:` frontmatter field when present.
	Name string `json:"name"`
	// Description is the one-line summary parsed from the SKILL.md
	// frontmatter's `description:` field. Empty when the file has no
	// frontmatter (rare) or the field is absent.
	Description string `json:"description,omitempty"`
	// Path is the skill's path relative to the bundle's skills/
	// directory. For a top-level SKILL.md it's just the filename;
	// for skill directories it's the directory name (the SDK reads
	// `<dir>/SKILL.md` inside).
	Path string `json:"path"`
}

// ListRunBundleSkills enumerates the skill catalog of the bundle
// backing the given run. Returns an empty slice (no error) when the
// run has no backing bundle or its bundle has no skills directory.
//
// The bundle for a run is located via the parent run's BundlePath
// (set at launch). Cloud runs that don't carry a local BundlePath
// surface an empty catalog — the studio falls back to a "no skills
// available" badge in that case.
func (s *Service) ListRunBundleSkills(ctx context.Context, runID string) ([]BundleSkill, error) {
	run, err := s.store.LoadRun(ctx, runID)
	if err != nil {
		return nil, fmt.Errorf("load run: %w", err)
	}
	if run.BundlePath == "" {
		return []BundleSkill{}, nil
	}
	// BundlePath can point to either a directory bundle or a .botz
	// archive. Skills live under <BundlePath>/skills for the
	// directory form; for the archive form we'd need to extract first,
	// which is heavier than the catalog endpoint should be. Cap the
	// archive case to "skills unsupported" for now — operators using
	// .botz can still pack a directory-form copy alongside.
	skillsDir := filepath.Join(run.BundlePath, "skills")
	info, err := os.Stat(skillsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return []BundleSkill{}, nil
		}
		return nil, fmt.Errorf("stat skills dir: %w", err)
	}
	if !info.IsDir() {
		return []BundleSkill{}, nil
	}
	entries, err := os.ReadDir(skillsDir)
	if err != nil {
		return nil, fmt.Errorf("read skills dir: %w", err)
	}
	out := make([]BundleSkill, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		if entry.IsDir() {
			// Skill directory: parse <dir>/SKILL.md frontmatter.
			sk, ok := readSkillFile(filepath.Join(skillsDir, name, "SKILL.md"))
			if !ok {
				continue
			}
			if sk.Name == "" {
				sk.Name = name
			}
			sk.Path = name
			out = append(out, sk)
			continue
		}
		// Top-level *.md skill file.
		if !strings.HasSuffix(strings.ToLower(name), ".md") {
			continue
		}
		sk, ok := readSkillFile(filepath.Join(skillsDir, name))
		if !ok {
			continue
		}
		if sk.Name == "" {
			sk.Name = strings.TrimSuffix(name, filepath.Ext(name))
		}
		sk.Path = name
		out = append(out, sk)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// readSkillFile opens a SKILL.md and pulls `name:` + `description:`
// out of its YAML-ish frontmatter block (delimited by `---` lines).
// Returns ok=true even when the frontmatter is missing — the body of
// the skill exists, so the catalog entry is still meaningful. ok=false
// only when the file can't be opened.
func readSkillFile(path string) (BundleSkill, bool) {
	f, err := os.Open(path)
	if err != nil {
		return BundleSkill{}, false
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 1<<20)
	sk := BundleSkill{}
	// Walk to the opening "---", then collect lines until the closing
	// "---". Tolerant of leading whitespace and missing frontmatter
	// (a SKILL.md without a frontmatter block is valid; the catalog
	// row just lacks a description).
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
			sk.Name = val
		case "description":
			sk.Description = val
		}
	}
	return sk, true
}
