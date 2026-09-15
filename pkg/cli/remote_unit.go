package cli

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
	"github.com/SocialGouv/iterion/pkg/dsl/unit"
	"github.com/SocialGouv/iterion/pkg/dsl/unparse"
)

// prepareUnit reads the bot at filePath as its unit — the file and the
// fragments its imports reach, beside it — resolves every {{include}}
// beside the file that carries it, and writes the program out flat, as one
// file: what an upload carries, since a server pod has none of the files
// beside the source. The one shape every remote surface sends (launch,
// resume, cost preview), so a run's inline identity is the flattened text:
// a resume re-flattens the same unit into the same text, an edited fragment
// or include into another, which the server's gate then refuses without
// --force. A bot in one file with no include uploads byte-identical to what
// it always did, so the identity of its runs does not move.
func prepareUnit(filePath string) (string, error) {
	src, err := os.ReadFile(filePath)
	if err != nil {
		return "", err
	}
	abs := filePath
	if a, err := filepath.Abs(filePath); err == nil {
		abs = a
	}
	u := unit.LoadDirWithMain(abs, abs, src)
	for _, d := range u.Diagnostics {
		if d.Severity == parser.SeverityError {
			return "", fmt.Errorf("%s: %s", filePath, d.Error())
		}
	}
	if u.Merged == nil {
		return "", fmt.Errorf("%s: no workflow found", filePath)
	}
	if len(u.Files) == 1 && !carriesInclude(u) {
		return string(src), nil
	}
	if err := ir.InlinePromptIncludes(u.Merged); err != nil {
		return "", fmt.Errorf("%s: %w", filePath, err)
	}
	flat := unparse.Unparse(u.Merged)
	// The text that travels must be the program that was read: a writer
	// that lost a construct would launch something else, silently.
	if err := unparse.Verify(u.Merged, flat); err != nil {
		return "", fmt.Errorf("%s: cannot be written out as one file: %w", filePath, err)
	}
	return flat, nil
}

// carriesInclude reports whether any prompt of the unit reads a file.
func carriesInclude(u *unit.Unit) bool {
	for _, p := range u.Merged.Prompts {
		if ir.HasPromptInclude(p.Body) {
			return true
		}
	}
	return false
}
