package commands

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"
)

// LoadDirCommands walks from startDir up to the filesystem root and registers
// a slash command for every Markdown file under a `.claude/commands/`
// directory — the project-commands convention Claude Code uses (discovered
// there via --setting-sources project). Each `<name>.md` becomes `/<name>`;
// the file body is printed when the command is invoked (the same minimal
// "documentation that doubles as a command" contract as CLAUDE.md commands).
//
// Directories closer to startDir win on conflict (leaf → root walk; first
// definition for a name is kept), and a command already registered by another
// source (builtins, CLAUDE.md) is not overridden.
func LoadDirCommands(r *Registry, startDir string) error {
	if r == nil {
		return fmt.Errorf("commands: nil registry")
	}
	dirs, err := findAncestorCommandDirs(startDir)
	if err != nil {
		return err
	}
	for _, dir := range dirs {
		entries, rerr := os.ReadDir(dir)
		if rerr != nil {
			continue
		}
		// Deterministic order so a directory with several commands registers
		// reproducibly.
		sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
		for _, e := range entries {
			if e.IsDir() || !strings.EqualFold(filepath.Ext(e.Name()), ".md") {
				continue
			}
			name := strings.ToLower(strings.TrimSuffix(e.Name(), filepath.Ext(e.Name())))
			if name == "" {
				continue
			}
			if _, exists := r.Lookup(name); exists {
				continue
			}
			data, derr := os.ReadFile(filepath.Join(dir, e.Name()))
			if derr != nil {
				continue
			}
			body, desc := stripFrontmatter(string(data))
			source := filepath.Join(dir, e.Name())
			captured := strings.TrimSpace(body)
			cmdName := name
			r.Register(Command{
				Name:            "/" + name,
				Description:     desc,
				Category:        CategoryPlugin,
				ResumeSupported: true,
				Handler: func(args string, _ interface{}) error {
					fmt.Printf("[/%s — from %s]\n", cmdName, source)
					if captured != "" {
						fmt.Println(captured)
					}
					if args != "" {
						fmt.Printf("(args: %s)\n", args)
					}
					return nil
				},
			})
		}
	}
	return nil
}

// findAncestorCommandDirs returns existing `.claude/commands` directories from
// startDir up to the filesystem root, startDir's first (leaf wins).
func findAncestorCommandDirs(startDir string) ([]string, error) {
	abs, err := filepath.Abs(startDir)
	if err != nil {
		return nil, err
	}
	var dirs []string
	for {
		cand := filepath.Join(abs, ".claude", "commands")
		if info, statErr := os.Stat(cand); statErr == nil && info.IsDir() {
			dirs = append(dirs, cand)
		}
		parent := filepath.Dir(abs)
		if parent == abs {
			break
		}
		abs = parent
	}
	return dirs, nil
}

// stripFrontmatter splits an optional leading YAML frontmatter block
// (--- … ---) from the body and returns (body, description). The description
// is the frontmatter `description:` when present, else the first non-empty
// body line, capped for help display.
//
// The opening `---` must be closed by a line that is exactly `---`, and the
// block between must carry at least one `key:` line. Without both checks a
// Markdown horizontal rule reads as frontmatter and the text under it is
// silently dropped — harmless when the body is help text, not harmless when
// it is a prompt sent to a model.
func stripFrontmatter(content string) (body, description string) {
	content = strings.ReplaceAll(content, "\r\n", "\n")
	body = content
	if fm, rest, ok := splitFrontmatter(content); ok {
		body = rest
		for _, line := range strings.Split(fm, "\n") {
			if v, ok := strings.CutPrefix(strings.TrimSpace(line), "description:"); ok {
				description = strings.Trim(strings.TrimSpace(v), `"'`)
				break
			}
		}
	}
	if description == "" {
		for _, line := range strings.Split(body, "\n") {
			if s := strings.TrimSpace(line); s != "" {
				description = s
				break
			}
		}
	}
	return body, capRunes(description, 117)
}

// splitFrontmatter returns the frontmatter block and the body that follows
// it, and whether content opened with a real frontmatter block.
func splitFrontmatter(content string) (fm, rest string, ok bool) {
	after, found := strings.CutPrefix(content, "---\n")
	if !found {
		return "", content, false
	}
	for offset := 0; ; {
		idx := strings.Index(after[offset:], "\n---")
		if idx < 0 {
			return "", content, false
		}
		at := offset + idx
		tail := after[at+len("\n---"):]
		// The closing delimiter owns its whole line: what follows it is
		// whitespace then a newline or the end of the file, never "-" (a
		// `----` rule). Trailing spaces after `---` are common and must not
		// leave a whole YAML block sitting in the body.
		if rest, blank := restOfLineIsBlank(tail); blank {
			tail = rest
			block := after[:at]
			if !hasYAMLKey(block) {
				return "", content, false
			}
			return block, strings.TrimPrefix(tail, "\n"), true
		}
		offset = at + 1
	}
}

// restOfLineIsBlank reports whether the rest of s's first line is blank,
// returning what follows that line. It is true at a newline (rest starts
// after it) and at end of input.
func restOfLineIsBlank(s string) (rest string, blank bool) {
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case ' ', '\t', '\r', '\f', '\v':
			continue
		case '\n':
			return s[i:], true
		default:
			return s, false
		}
	}
	return "", true
}

// hasYAMLKey reports whether block carries at least one `key:` line, which
// is what separates a frontmatter block from a horizontal rule.
func hasYAMLKey(block string) bool {
	for _, line := range strings.Split(block, "\n") {
		line = strings.TrimSpace(line)
		i := strings.IndexByte(line, ':')
		if i <= 0 {
			continue
		}
		key := line[:i]
		if key == strings.TrimSpace(key) && !strings.ContainsAny(key, " \t") {
			return true
		}
	}
	return false
}

// capRunes truncates s to at most n runes, on a rune boundary, appending an
// ellipsis. Slicing by byte splits a multi-byte rune and yields a string
// that is not valid UTF-8 — visible on a public API field.
func capRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n+3 {
		return s
	}
	count := 0
	for i := range s {
		if count == n {
			return s[:i] + "..."
		}
		count++
	}
	return s
}
