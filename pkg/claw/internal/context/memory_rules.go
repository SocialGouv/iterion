package context

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// ruleFile is one markdown file of a .claude/rules directory.
type ruleFile struct {
	// path is the file as reached from the rules directory; symlinks are not
	// resolved, so it is the name the file is known by.
	path string
	// rel is the slash-separated path relative to the rules directory.
	rel string
}

// discoverRules lists the *.md files under rulesDir, recursively, sorted by
// relative path. A missing or unreadable directory yields nothing, like every
// other unreadable memory file.
//
// Symlinks are followed (Claude Code documents them as the way to share rules
// between projects), a directory is entered at most once so a symlink cycle
// ends instead of looping, and only regular files are listed: reading a FIFO
// or a device named x.md would block or return garbage.
//
// confine is the directory a symlink may not lead out of; empty leaves them
// free. A workspace's rules directory belongs to a repository, and a
// repository does not get to point the walk at the rest of the machine — a
// link to / would otherwise have every build of the prompt walk the whole
// disk. Claude Code applies the same default to a project's rules (measured
// on 2.1.282). The operator's own directories are not confined. The rules
// directory itself, and the .claude above it, count as symlinks too.
//
// The order is an explicit sort, not the order the directory walk happens to
// produce: the walk visits "a/" before "a.md" while the relative paths sort
// "a.md" before "a/b.md", and the injected prompt must not depend on which of
// the two a platform's directory listing favours.
func discoverRules(rulesDir, confine string) []ruleFile {
	var limit string
	if confine != "" {
		real, err := filepath.EvalSymlinks(confine)
		if err != nil || !resolvesWithin(rulesDir, real) {
			return nil
		}
		limit = real
	}

	var rules []ruleFile
	entered := make(map[string]bool)

	var walk func(dir, rel string)
	walk = func(dir, rel string) {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		real, err := filepath.EvalSymlinks(dir)
		if err != nil || entered[real] {
			return
		}
		entered[real] = true

		for _, entry := range entries {
			name := entry.Name()
			path := filepath.Join(dir, name)
			if limit != "" && entry.Type()&fs.ModeSymlink != 0 && !resolvesWithin(path, limit) {
				continue
			}
			// Stat, not the entry's own type: a symlink reports what it
			// points to, and a dangling one is skipped.
			info, err := os.Stat(path)
			if err != nil {
				continue
			}
			relPath := name
			if rel != "" {
				relPath = rel + "/" + name
			}
			switch {
			case info.IsDir():
				walk(path, relPath)
			case info.Mode().IsRegular() && strings.HasSuffix(name, ".md"):
				rules = append(rules, ruleFile{path: path, rel: relPath})
			}
		}
	}
	walk(rulesDir, "")

	sort.Slice(rules, func(i, j int) bool { return rules[i].rel < rules[j].rel })
	return rules
}

// within reports whether real, an already-resolved path, is dir or lies
// below it. dir must already be resolved too; an empty dir admits nothing.
func within(real, dir string) bool {
	if dir == "" {
		return false
	}
	rel, err := filepath.Rel(dir, real)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// resolvesWithin reports whether path, with its symlinks resolved, is dir or
// lies below it. dir must already be resolved; an empty dir admits nothing.
func resolvesWithin(path, dir string) bool {
	if dir == "" {
		return false
	}
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return false
	}
	return within(real, dir)
}

// parseRule returns a rule file's body — frontmatter removed — and whether the
// rule is conditional.
//
// Claude Code loads a conditional rule lazily, when the agent touches a file
// matching its `paths` globs. A static system prompt has no such moment, so
// the caller drops conditional rules instead of applying them everywhere.
//
// Rules are read here rather than through config.ParseFrontmatter, which is
// the CLAUDE.md settings reader: LF only, a closing match that is not tied to
// the start of a line, and no notion of `paths`.
func parseRule(data []byte) (body string, conditional bool) {
	text := strings.TrimPrefix(string(data), "\ufeff")
	front, rest, ok := splitFrontmatter(text)
	if !ok {
		return text, false
	}
	return rest, scopesToPaths(front)
}

// splitFrontmatter separates a leading YAML frontmatter block from the text
// that follows it. The block opens on a first line that is exactly "---" and
// closes on the next line that is exactly "---" (CRLF and trailing blanks
// tolerated). A block that never closes is not frontmatter.
func splitFrontmatter(text string) (front, body string, ok bool) {
	first, _, found := strings.Cut(text, "\n")
	if !found || strings.TrimRight(first, " \t\r") != "---" {
		return "", "", false
	}
	start := len(first) + 1
	for pos := start; pos <= len(text); {
		end := strings.IndexByte(text[pos:], '\n')
		next := len(text)
		line := text[pos:]
		if end >= 0 {
			line = text[pos : pos+end]
			next = pos + end + 1
		}
		if strings.TrimRight(line, " \t\r") == "---" {
			return text[start:pos], text[next:], true
		}
		if end < 0 {
			break
		}
		pos = next
	}
	return "", "", false
}

// pathsKeyLine matches a top-level `paths` key. Only column 0 counts, so a
// "paths:" inside a multi-line value or a nested mapping never does. The
// optional capture group is the value written on the same line.
var pathsKeyLine = regexp.MustCompile(`^(?:paths|"paths"|'paths')[ \t]*:(?:[ \t]+(.*))?$`)

// scopesToPaths reports whether a rule's frontmatter restricts it to file
// patterns. Claude Code's reader (measured on 2.1.282) treats a rule as
// conditional when its `paths` value, split on commas, holds a pattern that is
// neither empty nor `**` once a trailing `/**` is dropped; an absent, empty or
// catch-all `paths` leaves the rule applying everywhere, and so does this.
func scopesToPaths(front string) bool {
	lines := strings.Split(front, "\n")
	for i, raw := range lines {
		m := pathsKeyLine.FindStringSubmatch(strings.TrimRight(raw, "\r"))
		if m == nil {
			continue
		}
		var items []string // YAML scalars, quotes still on
		if inline := strings.TrimSpace(trimYAMLComment(m[1])); inline == "" {
			items = blockListItems(lines[i+1:])
		} else if list, ok := strings.CutPrefix(inline, "["); ok {
			items = strings.Split(strings.TrimSuffix(strings.TrimSpace(list), "]"), ",")
		} else {
			items = []string{inline}
		}
		for _, item := range items {
			// A scalar can itself hold several comma-separated patterns.
			for _, p := range strings.Split(unquote(strings.TrimSpace(item)), ",") {
				p = strings.TrimSuffix(strings.TrimSpace(p), "/**")
				if p != "" && p != "**" {
					return true
				}
			}
		}
		return false
	}
	return false
}

// blockListItems reads the "- item" lines that follow a bare `paths:` key,
// stopping at the first line that is neither blank, a comment nor an item.
func blockListItems(lines []string) []string {
	var items []string
	for _, raw := range lines {
		line := strings.TrimSpace(trimYAMLComment(strings.TrimRight(raw, "\r")))
		if line == "" {
			continue
		}
		item, ok := strings.CutPrefix(line, "-")
		if !ok || (item != "" && item[0] != ' ' && item[0] != '\t') {
			break
		}
		items = append(items, strings.TrimSpace(item))
	}
	return items
}

// trimYAMLComment drops a trailing "# comment": a '#' that starts the value or
// follows whitespace.
func trimYAMLComment(s string) string {
	for i := 0; i < len(s); i++ {
		if s[i] == '#' && (i == 0 || s[i-1] == ' ' || s[i-1] == '\t') {
			return s[:i]
		}
	}
	return s
}

// unquote strips one pair of matching surrounding quotes.
func unquote(s string) string {
	if len(s) >= 2 && (s[0] == '"' || s[0] == '\'') && s[len(s)-1] == s[0] {
		return s[1 : len(s)-1]
	}
	return s
}
