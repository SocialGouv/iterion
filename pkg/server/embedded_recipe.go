package server

import (
	"fmt"

	"github.com/SocialGouv/iterion/bots"
	"github.com/SocialGouv/iterion/pkg/dsl/ast"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
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
// file; err reports a bot that does not load as a unit, a defect of the
// embed rather than a miss.
func embeddedRecipe(name string) (source string, ok bool, err error) {
	files, main, ok := bots.Sources(name)
	if !ok {
		return "", false, nil
	}
	source, err = recipeFromSources(name, files, main)
	return source, true, err
}

// recipeFromSources is the bot a files map holds, as one program: the
// main's own bytes when it imports nothing (what the parser has to say
// about them is the editor's to show), the merged unit written out flat
// when it does. A main whose imports do not resolve is an error, never the
// main alone — that text would carry imports nothing can resolve wherever
// it lands.
func recipeFromSources(name string, files map[string]string, main string) (string, error) {
	u := unit.LoadMap(files, main)
	if len(u.Files) == 0 || u.Files[0].AST == nil || len(u.Files[0].AST.Imports) == 0 {
		return files[main], nil
	}
	for _, d := range u.Diagnostics {
		if d.Severity == parser.SeverityError {
			return "", fmt.Errorf("bot %s does not load as a unit: %s", name, d.Error())
		}
	}
	return flatProgram(name, u.Merged)
}

// flatProgram writes a merged unit out as one program — its {{include}}
// markers inlined, the text verified to read back as the unit it came
// from: what launches inline or is saved as a new file must be the program
// the unit is, not a writer's approximation of it.
func flatProgram(name string, merged *ast.File) (string, error) {
	if err := ir.InlinePromptIncludes(merged); err != nil {
		return "", fmt.Errorf("%s: %w", name, err)
	}
	flat := unparse.Unparse(merged)
	if err := unparse.Verify(merged, flat); err != nil {
		return "", fmt.Errorf("%s: the flat program does not read back as the unit: %w", name, err)
	}
	return flat, nil
}
