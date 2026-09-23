package cli

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/dsl/author"
	"github.com/SocialGouv/iterion/pkg/dsl/unparse"
)

// authorDocument is an author document opened for validation: the program it
// describes, every position of it on the document, and the .bot it stands
// for — beside it, written or not.
type authorDocument struct {
	parsePath string // the document's absolute path: the name every position carries
	botPath   string // the .bot the document stands for
	src       []byte
	res       *author.Result
	text      string // the .bot text the program writes as, where its frontmatter is read
	verifyErr error  // E054: the program has no written .bot form
}

// openAuthorDocument reads an author document and opens the bundle of the
// .bot it stands for, when there is one. The path is absolutised first: an
// include in a prompt resolves beside the document, and the compiler refuses
// a relative source name rather than read beside the process's directory.
func openAuthorDocument(path string) (*authorDocument, *bundle.Bundle, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, nil, fmt.Errorf("cannot resolve %s: %w", path, err)
	}
	src, err := os.ReadFile(abs)
	if err != nil {
		return nil, nil, fmt.Errorf("cannot read file: %w", err)
	}
	doc := &authorDocument{
		parsePath: abs,
		// `x.bot.yaml` stands for `x.bot`: the `.yaml` off, whatever its case.
		botPath: abs[:len(abs)-len(".yaml")],
		src:     src,
		res:     author.Parse(abs, src),
	}
	// The .bot text the program writes as — where its frontmatter and its
	// syntax are read, errors or not — and, when the document reads without
	// an error, the text that has to read back as the same program: when it
	// does not, the program has no written form (E054).
	doc.text = unparse.Unparse(doc.res.File)
	if !doc.res.HasErrors() {
		doc.verifyErr = unparse.Verify(doc.res.File, doc.text)
	}
	var b *bundle.Bundle
	if dir := bundle.DirForMainBot(doc.botPath); dir != "" {
		// The bundle of the .bot the document stands for, that .bot written
		// or not: its prompts/, skills/, presets/ and manifest are in scope,
		// as they are for `validate bots/x/main.bot`.
		if b, err = bundle.OpenDirWithMain(dir, doc.botPath); err != nil {
			return nil, nil, fmt.Errorf("%s stands for the entrypoint of bundle %s, which does not open: %w", path, dir, err)
		}
	}
	return doc, b, nil
}

// frontmatterText is the text a bundle's frontmatter is read from: a .bot's
// own bytes, or the .bot text an author document writes as.
func frontmatterText(doc *authorDocument, src []byte) []byte {
	if doc != nil {
		return []byte(doc.text)
	}
	return src
}

// mainPathOf is the .bot a validation is about: the file itself, or the .bot
// an author document stands for.
func mainPathOf(doc *authorDocument, path string) string {
	if doc != nil {
		return doc.botPath
	}
	return path
}

// documentNameOf is the name of the document's own file in its unit — the
// one file the .bot edits `fix` plans never apply to — or "" for a .bot.
func documentNameOf(doc *authorDocument) string {
	if doc != nil {
		return doc.parsePath
	}
	return ""
}
