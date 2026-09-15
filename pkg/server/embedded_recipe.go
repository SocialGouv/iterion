package server

import (
	"fmt"

	"github.com/SocialGouv/iterion/bots"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
	"github.com/SocialGouv/iterion/pkg/dsl/unit"
	"github.com/SocialGouv/iterion/pkg/dsl/unparse"
)

// embeddedRecipe is the embedded bot that owns name, as ONE program: the
// bytes of a bot in one file, or, for a bot in several files, the merged
// unit written out flat. The studio keeps the text it is handed as its
// inline launch source and may save it as a new file, so the text has to
// be a program on its own — the main alone would leave its imports
// unresolved wherever it lands. ok is false when name is not an embedded
// file; err reports a unit that does not load, a defect of the embed
// rather than a miss.
func embeddedRecipe(name string) (source string, ok bool, err error) {
	files, main, ok := bots.Sources(name)
	if !ok {
		return "", false, nil
	}
	u := unit.LoadMap(files, main)
	if len(u.Files) == 1 {
		return files[main], true, nil
	}
	for _, d := range u.Diagnostics {
		if d.Severity == parser.SeverityError {
			return "", true, fmt.Errorf("embedded bot %s does not load as a unit: %s", name, d.Error())
		}
	}
	return unparse.Unparse(u.Merged), true, nil
}
