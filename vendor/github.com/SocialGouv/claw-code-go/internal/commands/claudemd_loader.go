package commands

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"

	clawctx "github.com/SocialGouv/claw-code-go/internal/context"
)

// LoadClaudeMdCommands walks from startDir up to the filesystem root,
// parses every CLAUDE.md it finds for slash-command definitions, and
// registers them on r.
//
// A slash-command block is an H2 header whose first non-whitespace
// character is "/". The body up to the next H2 (or EOF) becomes the
// command's effect: it is printed to stdout when the command is
// invoked. This is a deliberately minimal contract — CLAUDE.md
// commands are documentation that doubles as a quick reference, not
// arbitrary scripts.
//
// Files closer to startDir win on conflict: walk order is leaf → root
// and the first definition for a given name is kept.
func LoadClaudeMdCommands(r *Registry, startDir string) error {
	if r == nil {
		return fmt.Errorf("claudemd: nil registry")
	}
	files, err := findAncestorClaudeMd(startDir)
	if err != nil {
		return err
	}
	seen := make(map[string]bool)
	for _, path := range files {
		f, err := os.Open(path)
		if err != nil {
			continue
		}
		commands, parseErr := parseClaudeMdCommands(f, path)
		f.Close()
		if parseErr != nil {
			continue
		}
		for _, c := range commands {
			if seen[c.Name] {
				continue
			}
			seen[c.Name] = true
			c.ResumeSupported = true
			r.Register(c)
		}
	}
	return nil
}

// findAncestorClaudeMd returns CLAUDE.md paths from startDir up to /.
// startDir comes first; files higher up are appended in order so the
// caller can let leaves win on conflicts. Delegates to the canonical
// walk-up in internal/context (shared with memory loading).
func findAncestorClaudeMd(startDir string) ([]string, error) {
	return clawctx.AncestorClaudeMdPaths(startDir)
}

// parseClaudeMdCommands scans markdown for H2 headers starting with /.
// Each header opens a command; subsequent text is captured as the body
// until the next H2 (regardless of whether that H2 is a command).
func parseClaudeMdCommands(r io.Reader, source string) ([]Command, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)

	var (
		commands  []Command
		current   *Command
		bodyLines []string
	)

	flush := func() {
		if current == nil {
			return
		}
		// Skip headers with no name after the slash ("## /") — there
		// is nothing for the user to invoke and registering an empty
		// name shadows the registry's lookup table in surprising ways.
		if current.Name == "" {
			current = nil
			bodyLines = nil
			return
		}
		body := strings.TrimSpace(strings.Join(bodyLines, "\n"))
		current.Description = describeFromBody(body)
		body = strings.TrimSpace(body)
		captured := body
		cmdName := current.Name
		current.Handler = func(args string, _ interface{}) error {
			fmt.Printf("[%s — from CLAUDE.md %s]\n", cmdName, source)
			if captured != "" {
				fmt.Println(captured)
			}
			if args != "" {
				fmt.Printf("(args: %s)\n", args)
			}
			return nil
		}
		commands = append(commands, *current)
	}

	for scanner.Scan() {
		line := scanner.Text()
		trimmed := strings.TrimRight(line, " \t")
		if isCommandHeader(trimmed) {
			flush()
			current = &Command{
				Name:     extractCommandName(trimmed),
				Category: CategoryUncategorized,
			}
			bodyLines = nil
			continue
		}
		if isAnyH2(trimmed) {
			// Reached the next H2 that isn't a /command — close the
			// current block and stop accumulating until another /header.
			flush()
			current = nil
			bodyLines = nil
			continue
		}
		if current != nil {
			bodyLines = append(bodyLines, line)
		}
	}
	flush()
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return commands, nil
}

// isCommandHeader reports whether line is an H2 whose name starts with /.
func isCommandHeader(line string) bool {
	if !strings.HasPrefix(line, "## ") {
		return false
	}
	rest := strings.TrimSpace(strings.TrimPrefix(line, "##"))
	return strings.HasPrefix(rest, "/")
}

func isAnyH2(line string) bool {
	return strings.HasPrefix(line, "## ")
}

// extractCommandName pulls "name" out of "## /name [args...]" lines.
// Trailing argument hints in the header are dropped.
func extractCommandName(line string) string {
	rest := strings.TrimSpace(strings.TrimPrefix(line, "##"))
	rest = strings.TrimPrefix(rest, "/")
	for i, r := range rest {
		if r == ' ' || r == '\t' {
			return strings.ToLower(rest[:i])
		}
	}
	return strings.ToLower(rest)
}

// describeFromBody picks a one-line description for the help index.
// We use the first non-empty body line, capped at 80 chars.
func describeFromBody(body string) string {
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if len(line) > 80 {
			return line[:80] + "…"
		}
		return line
	}
	return "Slash command from CLAUDE.md"
}
