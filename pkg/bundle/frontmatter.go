package bundle

import (
	"os"
	"strings"

	"go.yaml.in/yaml/v2"

	"github.com/SocialGouv/iterion/pkg/dsl/workflowfile"
)

// Frontmatter is the optional `## ---` … `## ---` YAML block at the top of a
// main.bot. It lets a loose .bot file or a bundle carry catalog metadata
// (name / description / triggers / capabilities) inline. For a bundle the
// manifest is authoritative; a non-empty frontmatter value OVERRIDES the
// manifest's triggers/capabilities at discovery time
// (botregistry.parseBundle). bundlelint flags that silent override (C221).
type Frontmatter struct {
	Name         string   `yaml:"name"`
	Description  string   `yaml:"description"`
	Triggers     []string `yaml:"triggers"`
	Capabilities []string `yaml:"capabilities"`
}

// ReadFrontmatter reads the file at path and returns its parsed frontmatter,
// or nil when the file is unreadable or carries no `## ---` block.
func ReadFrontmatter(path string) *Frontmatter {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	return ParseFrontmatter(raw)
}

// ParseFrontmatter pulls a `## ---` … `## ---` block from the top of the file
// and YAML-decodes the inner content. The block is allowed only at the very
// top of the file, optionally after blank lines. Returns nil when the block
// is absent or malformed.
//
// The fence and the inner lines are comment lines of the workflow source, so
// they follow the lexer's definition of a comment (workflowfile.CommentText):
// `# ---` and `## ---` are the same fence, `# name: x` and `## name: x` the
// same line. A reader that only knew `##` would silently drop the whole
// catalogue identity of a file the parser accepts.
func ParseFrontmatter(raw []byte) *Frontmatter {
	lines := strings.Split(string(raw), "\n")
	i := 0
	for i < len(lines) && strings.TrimSpace(lines[i]) == "" {
		i++
	}
	if i >= len(lines) || !isFrontmatterFence(lines[i]) {
		return nil
	}
	start := i + 1
	end := -1
	for j := start; j < len(lines); j++ {
		if isFrontmatterFence(lines[j]) {
			end = j
			break
		}
	}
	if end < 0 {
		return nil
	}
	var yamlLines []string
	for _, ln := range lines[start:end] {
		if text, ok := workflowfile.CommentText(ln); ok {
			yamlLines = append(yamlLines, text)
		} else {
			yamlLines = append(yamlLines, ln)
		}
	}
	var fm Frontmatter
	if err := yaml.Unmarshal([]byte(strings.Join(yamlLines, "\n")), &fm); err != nil {
		return nil
	}
	return &fm
}

// isFrontmatterFence reports whether line is the `---` comment line that
// opens or closes a frontmatter block, whichever hash count it uses.
func isFrontmatterFence(line string) bool {
	text, ok := workflowfile.CommentText(line)
	return ok && strings.TrimSpace(text) == workflowfile.FrontmatterFence
}
