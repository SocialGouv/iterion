package tools

import (
	"encoding/json"
	"fmt"
	"github.com/SocialGouv/claw-code-go/internal/api"
	"os"
	"path/filepath"
	"strings"
)

func SkillTool() api.Tool {
	return api.Tool{
		Name:        "skill",
		Description: "Execute a skill by name. Skills are specialized prompt files that provide domain knowledge.",
		InputSchema: api.InputSchema{
			Type: "object",
			Properties: map[string]api.Property{
				"skill": {Type: "string", Description: "The skill name to execute."},
				"args":  {Type: "string", Description: "Optional arguments for the skill."},
			},
			Required: []string{"skill"},
		},
	}
}

func ExecuteSkill(input map[string]any, workDir string) (string, error) {
	skillName, ok := input["skill"].(string)
	if !ok || skillName == "" {
		return "", fmt.Errorf("skill: 'skill' name is required")
	}
	args, _ := input["args"].(string)

	path, err := resolveSkillPath(skillName, workDir)
	if err != nil {
		return "", err
	}

	content, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("skill: cannot read skill file %s: %v", path, err)
	}

	// Parse description from first line
	text := string(content)
	description := skillName
	if lines := strings.SplitN(text, "\n", 2); len(lines) > 0 {
		firstLine := strings.TrimSpace(lines[0])
		firstLine = strings.TrimPrefix(firstLine, "#")
		firstLine = strings.TrimSpace(firstLine)
		if firstLine != "" {
			description = firstLine
		}
	}

	result := map[string]any{
		"skill":       skillName,
		"path":        path,
		"args":        args,
		"description": description,
		"prompt":      text,
	}
	out, _ := json.MarshalIndent(result, "", "  ")
	return string(out), nil
}

// resolveSkillPath searches for a skill file in standard locations.
func resolveSkillPath(name, workDir string) (string, error) {
	// Normalize: strip .md extension if provided, we'll try with and without
	baseName := strings.TrimSuffix(name, ".md")

	roots := skillLookupRoots(workDir)

	for _, root := range roots {
		// Try exact name
		candidates := []string{
			filepath.Join(root, baseName+".md"),
			filepath.Join(root, baseName),
			filepath.Join(root, name),
		}
		for _, path := range candidates {
			if info, err := os.Stat(path); err == nil && !info.IsDir() {
				return path, nil
			}
		}
	}

	return "", fmt.Errorf("skill: skill %q not found in any search path", name)
}

// skillLookupRoots returns directories to search for skills, in priority order.
func skillLookupRoots(workDir string) []string {
	var roots []string

	// Project-local skill directories (walk up from workDir)
	dir := workDir
	for {
		roots = append(roots,
			filepath.Join(dir, ".skills"),
			filepath.Join(dir, ".claude", "skills"),
			filepath.Join(dir, ".agents", "skills"),
			filepath.Join(dir, ".claw", "skills"),
		)
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}

	// Config-based paths
	if configHome := os.Getenv("CLAW_CONFIG_HOME"); configHome != "" {
		roots = append(roots,
			filepath.Join(configHome, "skills"),
			filepath.Join(configHome, "commands"),
		)
	}
	if codexHome := os.Getenv("CODEX_HOME"); codexHome != "" {
		roots = append(roots,
			filepath.Join(codexHome, "skills"),
			filepath.Join(codexHome, "commands"),
		)
	}

	// Home directory paths
	if home, err := os.UserHomeDir(); err == nil {
		roots = append(roots,
			filepath.Join(home, ".claude", "skills"),
			filepath.Join(home, ".claude", "skills", "omc-learned"),
			filepath.Join(home, ".claw", "skills"),
		)
		if claudeConfigDir := os.Getenv("CLAUDE_CONFIG_DIR"); claudeConfigDir != "" {
			roots = append(roots,
				filepath.Join(claudeConfigDir, "skills"),
				filepath.Join(claudeConfigDir, "commands"),
			)
		}
	}

	return roots
}
