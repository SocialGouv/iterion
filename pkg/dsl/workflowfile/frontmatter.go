package workflowfile

import (
	"sort"

	"go.yaml.in/yaml/v2"
)

// Frontmatter is the catalog identity a `.bot` carries in the `## ---` …
// `## ---` comment block at its head: the four keys the catalogue reads.
// It is decoded by DecodeFrontmatter alone, whichever reader holds the
// block — the bundle loader off a file's first lines, the author writer off
// a parsed file's head comments, `fmt --to yaml` for its notices — so every
// reader gives the same block the same identity, and a block one of them
// cannot read is a block none of them reads.
type Frontmatter struct {
	Name         string   `yaml:"name"`
	Description  string   `yaml:"description"`
	Triggers     []string `yaml:"triggers"`
	Capabilities []string `yaml:"capabilities"`
}

// Empty reports whether f carries none of the four keys' values — no block,
// or one that reads and says nothing the catalogue uses.
func (f *Frontmatter) Empty() bool {
	return f == nil || f.Name == "" && f.Description == "" && len(f.Triggers) == 0 && len(f.Capabilities) == 0
}

// FrontmatterKeys are the four keys of a catalog identity, in the order the
// author writer puts them under `catalog:`.
var FrontmatterKeys = []string{"name", "description", "triggers", "capabilities"}

// DecodeFrontmatter reads the YAML text between the fences of a frontmatter
// block — its lines with the hashes off (CommentText), joined by newlines —
// as the ONE reading of a catalog identity: yaml.v2, into the four keys. A
// key named twice keeps its last value, as the catalogue has always read
// it; a scalar where a list is expected is a block the reading does not
// read; a key beyond the four is not an error — it is returned in extra,
// sorted, each once, for a reader that says what the identity leaves
// behind. An error is a block the reading does not read, and the caller
// says what that means for it: no identity for the catalogue, no `catalog:`
// in the author document, the reason of `fmt --to yaml`'s note.
func DecodeFrontmatter(text string) (fm *Frontmatter, extra []string, err error) {
	var read struct {
		Frontmatter `yaml:",inline"`
		Rest        map[string]any `yaml:",inline"`
	}
	if err := yaml.Unmarshal([]byte(text), &read); err != nil {
		return nil, nil, err
	}
	for k := range read.Rest {
		extra = append(extra, k)
	}
	sort.Strings(extra)
	fm = &Frontmatter{}
	*fm = read.Frontmatter
	return fm, extra, nil
}
