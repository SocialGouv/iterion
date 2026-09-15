package parser

import (
	"strings"

	"github.com/SocialGouv/iterion/pkg/dsl/ast"
)

// parseImportDecl reads `import "lib/x.bot"` — one fragment of the file's
// compilation unit (ADR-098 §3). It sits at the head of the file, after the
// `dsl:` header and the leading comments, before any declaration (E044
// otherwise). The path is a quoted string held to the rules a path can be
// held to without reading the disk (ImportPathError, E045); where it lands
// — under the bot's `lib/`, inside the bot — is the unit loader's to check,
// since a fragment may import its sibling by a bare name. The same path
// imported twice is one import.
func (p *parser) parseImportDecl(f *ast.File, declared bool) {
	t := p.next() // import
	v := p.peek()
	if v.Type != TokenString {
		p.addError(DiagBadImportPath, v, "import takes a quoted path (`import \"lib/x.bot\"`), got "+v.Type.String())
		p.skipToNewline()
		return
	}
	p.next()
	if rest := p.peek(); !lineEnds(rest) && rest.Type != TokenEOF {
		p.addError(DiagBadImportPath, rest, "import takes one path, alone on its line, got '"+rest.Value+"' after it")
		p.skipToNewline()
		return
	}
	if declared {
		p.addError(DiagMisplacedImport, t, "import must sit at the head of the file, before its first declaration")
		return
	}
	if why := ImportPathError(v.Value); why != "" {
		p.addError(DiagBadImportPath, v, "import \""+v.Value+"\": "+why)
		return
	}
	for _, im := range f.Imports {
		if im.Path == v.Value {
			return
		}
	}
	f.Imports = append(f.Imports, &ast.ImportDecl{
		Path: v.Value,
		Span: ast.Span{Start: p.pos(t), End: p.pos(v)},
	})
}

// ImportPathError is why an import path, as written, cannot be one — "" when
// it may. The rules are the portable ones a path is held to before any disk
// is read: relative (no leading `/`, no `//`, no drive letter), no `..`
// segment, `/` as the separator, no NUL, a `.bot` file. The unit loader
// adds what only the disk can say: that the resolved path stays under the
// bot's `lib/`.
func ImportPathError(path string) string {
	switch {
	case path == "":
		return "the path is empty"
	case strings.ContainsRune(path, 0):
		return "the path holds a NUL byte"
	case strings.Contains(path, "\\"):
		return "use `/` as the separator"
	case strings.HasPrefix(path, "/"):
		return "the path is absolute; an import is relative to the file that imports it"
	case len(path) >= 2 && path[1] == ':' && isASCIILetter(path[0]):
		return "the path names a drive; an import is relative to the file that imports it"
	case !strings.HasSuffix(path, ".bot"):
		return "a fragment is a `.bot` file"
	}
	for _, seg := range strings.Split(path, "/") {
		if seg == ".." {
			return "`..` leaves the file's directory; a fragment lives under the bot's `lib/`"
		}
		if seg == "" {
			return "the path holds an empty segment (`//`)"
		}
	}
	return ""
}

func isASCIILetter(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}
