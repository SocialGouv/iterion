// Package unit loads a bot's compilation unit: its main file and the
// fragments the file imports, transitively, merged into one ast.File
// (ADR-098 §3). A `.bot` that carries `import "lib/x.bot"` lines is not a
// program until its fragments are merged in — the compiler refuses one
// alone (C030) — and this package is the one place that merges them, for
// every surface that turns a path or a set of files into a workflow.
package unit

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"sort"
	"strings"

	"github.com/SocialGouv/iterion/pkg/dsl/ast"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

// Reader reads one file of the unit by its slash path relative to the
// unit's root ("main.bot", "lib/a.bot"). It reports a wrapped
// os.ErrNotExist for a file that is not there, and ErrOutside for a path
// the unit may not read: one that resolves beyond the root through a
// symlink, or names a file the reader does not hold.
type Reader func(rel string) ([]byte, error)

// ErrOutside is what a Reader returns for a path beyond the unit's root.
var ErrOutside = errors.New("outside the unit")

// FragmentDir is the directory, under the unit's root, that holds every
// fragment: an import must resolve into it, and nothing under it is ever
// discovered as a workflow of its own.
const FragmentDir = "lib"

// File is one file of the unit, as read and as parsed.
type File struct {
	// Rel is the slash path from the unit's root: "main.bot", "lib/a.bot".
	Rel string
	// Name is what the parser was given as the file's name — the absolute
	// path on disk, or the key of a files map — and what every Span of
	// AST carries; an include resolves against it.
	Name    string
	Source  []byte
	AST     *ast.File
	Profile int
}

// Unit is the loaded and merged compilation unit.
type Unit struct {
	// Root is the directory of the main file on disk, "" for a files map.
	Root string
	// Main is the main file's Rel.
	Main string
	// Files holds the main first, then the fragments in the order the
	// imports reach them (depth first).
	Files []File
	// Merged is the unit's program: the main's AST with every fragment's
	// declarations appended, its keyed blocks merged, its imports cleared
	// and its subbot sources made relative to the root. It is what the
	// compiler gets and what an inline launch writes out flat —
	// unparse.Unparse of it carries no import line, since it has none.
	Merged *ast.File
	// Digest identifies the unit's source: the files' relative paths and
	// contents, in a fixed order. It changes when any file changes.
	Digest string
	// Diagnostics holds every file's parse diagnostics plus the loader's
	// own (an unreadable fragment, a cycle, a path outside lib/, a
	// declaration made twice), each positioned in its file.
	Diagnostics []parser.Diagnostic
}

// HasErrors reports whether any diagnostic is an error.
func (u *Unit) HasErrors() bool {
	for _, d := range u.Diagnostics {
		if d.Severity == parser.SeverityError {
			return true
		}
	}
	return false
}

// LoadDir loads the unit whose main file is at mainPath on disk. The
// unit's root is the main's directory; a fragment is read only when it
// resolves under that root through no symlink (the cloud snapshot never
// follows one either, so a unit valid here launches from a snapshot).
// Files are parsed under their absolute path, so an include inside a
// fragment resolves beside the fragment.
func LoadDir(mainPath string) *Unit {
	abs, err := filepath.Abs(mainPath)
	if err != nil {
		abs = mainPath
	}
	root := filepath.Dir(abs)
	realRoot := root
	if r, err := filepath.EvalSymlinks(root); err == nil {
		realRoot = r
	}
	read := func(rel string) ([]byte, error) {
		full := filepath.Join(root, filepath.FromSlash(rel))
		info, err := os.Lstat(full)
		if err != nil {
			return nil, err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("%w: %s is a symlink", ErrOutside, rel)
		}
		real, err := filepath.EvalSymlinks(full)
		if err != nil {
			return nil, err
		}
		if !within(real, realRoot) {
			return nil, fmt.Errorf("%w: %s resolves to %s", ErrOutside, rel, real)
		}
		return os.ReadFile(real)
	}
	u := Load(read, filepath.Base(abs), func(rel string) string {
		return filepath.Join(root, filepath.FromSlash(rel))
	})
	u.Root = root
	return u
}

// LoadMap loads the unit from a files map keyed by slash path relative to
// the root — a bot source, a snapshot — with main as the main's key.
// Files are parsed under their key.
func LoadMap(files map[string]string, main string) *Unit {
	read := func(rel string) ([]byte, error) {
		src, ok := files[rel]
		if !ok {
			return nil, fmt.Errorf("%w: %s", os.ErrNotExist, rel)
		}
		return []byte(src), nil
	}
	return Load(read, main, func(rel string) string { return rel })
}

// Load loads the unit through read, starting at main, naming each file for
// the parser with name.
func Load(read Reader, main string, name func(rel string) string) *Unit {
	l := &loader{read: read, name: name, u: &Unit{Main: path.Clean(main)}, state: map[string]int{}}
	l.visit(l.u.Main, nil, "")
	l.u.Files = l.files
	l.merge()
	l.u.Digest = Digest(l.u.Files)
	return l.u
}

// Digest is the unit's source identity: sha256 over the files' relative
// paths and their contents' digests, sorted by path.
func Digest(files []File) string {
	sorted := make([]File, len(files))
	copy(sorted, files)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Rel < sorted[j].Rel })
	h := sha256.New()
	for _, f := range sorted {
		sum := sha256.Sum256(f.Source)
		h.Write([]byte(f.Rel))
		h.Write([]byte{0})
		h.Write(sum[:])
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

const (
	stateLoading = -1
)

type loader struct {
	read  Reader
	name  func(rel string) string
	u     *Unit
	files []File
	// state maps a rel to its index in files once loaded, stateLoading
	// while its imports are being followed (a cycle meets it there), and
	// is absent for a file never reached.
	state map[string]int
	stack []string
}

func (l *loader) visit(rel string, from *ast.ImportDecl, fromName string) {
	if st, seen := l.state[rel]; seen {
		if st == stateLoading {
			l.diag(parser.DiagImportCycle, fromName, from, fmt.Sprintf("import cycle: %s → %s", strings.Join(l.stack, " → "), rel))
		}
		return
	}
	isMain := from == nil
	if !isMain && !strings.HasPrefix(rel, FragmentDir+"/") {
		l.diag(parser.DiagBadImportPath, fromName, from, fmt.Sprintf("import %q resolves to %s, outside the bot's `%s/` directory where every fragment lives", from.Path, rel, FragmentDir))
		return
	}
	src, err := l.read(rel)
	if err != nil {
		switch {
		case isMain:
			l.u.Diagnostics = append(l.u.Diagnostics, parser.Diagnostic{Code: parser.DiagImportUnreadable, Severity: parser.SeverityError, File: l.name(rel), Line: 1, Column: 1, Message: fmt.Sprintf("cannot read %s: %v", rel, err), Hint: parser.HintFor(parser.DiagImportUnreadable)})
		case errors.Is(err, ErrOutside):
			l.diag(parser.DiagImportUnreadable, fromName, from, fmt.Sprintf("import %q is not read: %v", from.Path, err))
		case errors.Is(err, os.ErrNotExist):
			l.diag(parser.DiagImportUnreadable, fromName, from, fmt.Sprintf("import %q: no such fragment (%s)", from.Path, rel))
		default:
			l.diag(parser.DiagImportUnreadable, fromName, from, fmt.Sprintf("import %q: cannot read %s: %v", from.Path, rel, err))
		}
		return
	}
	name := l.name(rel)
	l.state[rel] = stateLoading
	l.stack = append(l.stack, rel)
	pr := parser.Parse(name, string(src))
	l.u.Diagnostics = append(l.u.Diagnostics, pr.Diagnostics...)
	f := File{Rel: rel, Name: name, Source: src, AST: pr.File}
	if pr.File != nil {
		f.Profile = pr.File.EffectiveProfile()
	}
	l.files = append(l.files, f)
	index := len(l.files) - 1
	if pr.File != nil {
		base := path.Dir(rel)
		for _, im := range pr.File.Imports {
			child := path.Clean(path.Join(base, im.Path))
			l.visit(child, im, name)
		}
	}
	l.stack = l.stack[:len(l.stack)-1]
	l.state[rel] = index
}

// diag records a loader diagnostic at the import that led to it.
func (l *loader) diag(code parser.DiagCode, file string, at *ast.ImportDecl, msg string) {
	d := parser.Diagnostic{Code: code, Severity: parser.SeverityError, File: file, Message: msg, Hint: parser.HintFor(code)}
	if at != nil {
		d.Line, d.Column = at.Span.Start.Line, at.Span.Start.Column
	}
	l.u.Diagnostics = append(l.u.Diagnostics, d)
}

// ---- merging ----

// merge builds Merged: the main's AST, then every fragment's declarations
// appended in the order the files were reached. Every slice field of
// ast.File is concatenated by reflection — a kind added to the AST later
// is merged without anyone remembering to list it — and the four keyed
// blocks (vars, presets, attachments, secrets) merge by key. A name
// declared in two files, or a key declared twice anywhere, is E010 naming
// both places; the compiler's own uniqueness checks then see no cross-file
// duplicate and report the rest as ever.
func (l *loader) merge() {
	if len(l.files) == 0 || l.files[0].AST == nil {
		l.u.Merged = &ast.File{}
		return
	}
	main := l.files[0]
	merged := *main.AST
	merged.Imports = nil
	mv := reflect.ValueOf(&merged).Elem()
	// Fresh slices, so appending never writes into the main's own arrays.
	for i := 0; i < mv.NumField(); i++ {
		if fv := mv.Field(i); fv.Kind() == reflect.Slice && fv.Len() > 0 {
			fresh := reflect.MakeSlice(fv.Type(), 0, fv.Len())
			fv.Set(reflect.AppendSlice(fresh, fv))
		}
	}
	for _, f := range l.files[1:] {
		if f.AST == nil {
			continue
		}
		fv := reflect.ValueOf(f.AST).Elem()
		for i := 0; i < mv.NumField(); i++ {
			field := mv.Type().Field(i)
			if field.Name == "Imports" || mv.Field(i).Kind() != reflect.Slice {
				continue
			}
			mv.Field(i).Set(reflect.AppendSlice(mv.Field(i), fv.Field(i)))
		}
	}
	l.mergeKeyedBlocks(&merged)
	l.canonicaliseSubbots(&merged)
	l.checkNamedDuplicates(&merged)
	l.u.Merged = &merged
}

// mergeKeyedBlocks merges the at-most-one blocks by key: a fragment's block
// joins the main's (or becomes it); a key declared twice, in one block or
// across files, is E010 naming both.
func (l *loader) mergeKeyedBlocks(merged *ast.File) {
	// vars
	{
		seen := map[string]ast.Pos{}
		var fields []*ast.VarField
		var block *ast.VarsBlock
		for _, f := range l.files {
			if f.AST == nil || f.AST.Vars == nil {
				continue
			}
			if block == nil {
				b := *f.AST.Vars
				block = &b
			}
			for _, v := range f.AST.Vars.Fields {
				if l.dup(seen, "var", v.Name, v.Span.Start) {
					continue
				}
				fields = append(fields, v)
			}
		}
		if block != nil {
			block.Fields = fields
			merged.Vars = block
		}
	}
	// presets
	{
		seen := map[string]ast.Pos{}
		var entries []*ast.Preset
		var block *ast.PresetsBlock
		for _, f := range l.files {
			if f.AST == nil || f.AST.Presets == nil {
				continue
			}
			if block == nil {
				b := *f.AST.Presets
				block = &b
			}
			for _, p := range f.AST.Presets.Entries {
				if l.dup(seen, "preset", p.Name, p.Span.Start) {
					continue
				}
				entries = append(entries, p)
			}
		}
		if block != nil {
			block.Entries = entries
			merged.Presets = block
		}
	}
	// attachments
	{
		seen := map[string]ast.Pos{}
		var fields []*ast.AttachmentField
		var block *ast.AttachmentsBlock
		for _, f := range l.files {
			if f.AST == nil || f.AST.Attachments == nil {
				continue
			}
			if block == nil {
				b := *f.AST.Attachments
				block = &b
			}
			for _, a := range f.AST.Attachments.Fields {
				if l.dup(seen, "attachment", a.Name, a.Span.Start) {
					continue
				}
				fields = append(fields, a)
			}
		}
		if block != nil {
			block.Fields = fields
			merged.Attachments = block
		}
	}
	// secrets
	{
		seen := map[string]ast.Pos{}
		var fields []*ast.SecretField
		var block *ast.SecretsBlock
		for _, f := range l.files {
			if f.AST == nil || f.AST.Secrets == nil {
				continue
			}
			if block == nil {
				b := *f.AST.Secrets
				block = &b
			}
			for _, s := range f.AST.Secrets.Fields {
				if l.dup(seen, "secret", s.Name, s.Span.Start) {
					continue
				}
				fields = append(fields, s)
			}
		}
		if block != nil {
			block.Fields = fields
			merged.Secrets = block
		}
	}
}

// dup records name's first position and reports (E010) a second one, in
// the same file or another. It returns true for the duplicate.
func (l *loader) dup(seen map[string]ast.Pos, kind, name string, at ast.Pos) bool {
	first, ok := seen[name]
	if !ok {
		seen[name] = at
		return false
	}
	l.u.Diagnostics = append(l.u.Diagnostics, parser.Diagnostic{
		Code: parser.DiagDuplicateDecl, Severity: parser.SeverityError,
		File: at.File, Line: at.Line, Column: at.Column,
		Message: fmt.Sprintf("%s %q is declared twice: here and at %s:%d", kind, name, l.relOf(first.File), first.Line),
		Hint:    parser.HintFor(parser.DiagDuplicateDecl),
	})
	return true
}

// relOf is the unit-relative path of a parse name, for messages.
func (l *loader) relOf(name string) string {
	for _, f := range l.files {
		if f.Name == name {
			return f.Rel
		}
	}
	return name
}

// nodeKinds are the declaration kinds that share the node namespace
// (C041: a node ID is unique across all kinds); every other named kind has
// a namespace of its own. Names are the ast.File field names.
var nodeKinds = map[string]bool{
	"Agents": true, "Judges": true, "Routers": true, "Humans": true, "Tools": true, "Computes": true,
	"Emits": true, "Waits": true, "AwaitAnswers": true, "Fails": true, "Subbots": true, "Groups": true, "Uses": true,
}

// checkNamedDuplicates reports (E010) a name declared in two DIFFERENT
// files of the unit — walking every slice field of ast.File whose elements
// carry a Name (or, for a `use`, a Prefix) — and a second workflow. A
// duplicate inside one file is left to the compiler, which reports it
// today; two files, it could not have named.
func (l *loader) checkNamedDuplicates(merged *ast.File) {
	type first struct {
		file string
		line int
	}
	seen := map[string]first{} // namespace + "\x00" + name
	if len(merged.Workflows) > 1 {
		w0, w1 := merged.Workflows[0], merged.Workflows[1]
		l.u.Diagnostics = append(l.u.Diagnostics, parser.Diagnostic{
			Code: parser.DiagDuplicateDecl, Severity: parser.SeverityError,
			File: w1.Span.Start.File, Line: w1.Span.Start.Line, Column: w1.Span.Start.Column,
			Message: fmt.Sprintf("a unit has one workflow: %q here and %q at %s:%d", w1.Name, w0.Name, l.relOf(w0.Span.Start.File), w0.Span.Start.Line),
			Hint:    parser.HintFor(parser.DiagDuplicateDecl),
		})
	}
	mv := reflect.ValueOf(merged).Elem()
	for i := 0; i < mv.NumField(); i++ {
		field := mv.Type().Field(i)
		fv := mv.Field(i)
		if fv.Kind() != reflect.Slice || field.Name == "Workflows" || field.Name == "Comments" {
			continue
		}
		namespace := field.Name
		if nodeKinds[field.Name] {
			namespace = "node"
		}
		for j := 0; j < fv.Len(); j++ {
			el := fv.Index(j)
			if el.Kind() == reflect.Pointer {
				el = el.Elem()
			}
			if el.Kind() != reflect.Struct {
				continue
			}
			name, pos, ok := namedAt(el)
			if !ok || name == "" {
				continue
			}
			key := namespace + "\x00" + name
			if prev, dup := seen[key]; dup {
				if prev.file != pos.File {
					l.u.Diagnostics = append(l.u.Diagnostics, parser.Diagnostic{
						Code: parser.DiagDuplicateDecl, Severity: parser.SeverityError,
						File: pos.File, Line: pos.Line, Column: pos.Column,
						Message: fmt.Sprintf("%s %q is declared in two files: here and at %s:%d", kindWord(field.Name), name, l.relOf(prev.file), prev.line),
						Hint:    parser.HintFor(parser.DiagDuplicateDecl),
					})
				}
				continue
			}
			seen[key] = first{pos.File, pos.Line}
		}
	}
}

// namedAt reads a declaration's name (its Name field, or the Prefix of a
// `use`) and its start position.
func namedAt(el reflect.Value) (string, ast.Pos, bool) {
	nameField := el.FieldByName("Name")
	if !nameField.IsValid() || nameField.Kind() != reflect.String {
		nameField = el.FieldByName("Prefix")
	}
	if !nameField.IsValid() || nameField.Kind() != reflect.String {
		return "", ast.Pos{}, false
	}
	span := el.FieldByName("Span")
	if !span.IsValid() {
		return nameField.String(), ast.Pos{}, true
	}
	s, ok := span.Interface().(ast.Span)
	if !ok {
		return nameField.String(), ast.Pos{}, true
	}
	return nameField.String(), s.Start, true
}

// kindWord turns an ast.File field name into the word a message uses:
// "Agents" → "agent", "AwaitAnswers" → "await_answers", "MCPServers" →
// "mcp_server".
func kindWord(field string) string {
	switch field {
	case "MCPServers":
		return "mcp_server"
	case "AwaitAnswers":
		return "await_answers"
	}
	var b strings.Builder
	for i, r := range field {
		if i > 0 && r >= 'A' && r <= 'Z' {
			b.WriteByte('_')
		}
		b.WriteRune(r)
	}
	return strings.TrimSuffix(strings.ToLower(b.String()), "s")
}

// canonicaliseSubbots rewrites, on the merged copy only, the `source:` of
// every subbot declared in a fragment so it is relative to the unit's root
// — the base every host resolves a child against — instead of the
// fragment's own directory, which is what the author wrote it against.
// The fragment's AST keeps the text as written, for the save by provenance.
func (l *loader) canonicaliseSubbots(merged *ast.File) {
	relByName := map[string]string{}
	for _, f := range l.files {
		relByName[f.Name] = f.Rel
	}
	for i, sb := range merged.Subbots {
		rel, ok := relByName[sb.Span.Start.File]
		if !ok || rel == l.u.Main || sb.Source == "" || filepath.IsAbs(sb.Source) || strings.HasPrefix(sb.Source, "/") {
			continue
		}
		cp := *sb
		cp.Source = path.Clean(path.Join(path.Dir(rel), filepath.ToSlash(sb.Source)))
		merged.Subbots[i] = &cp
	}
}

// within reports whether p lies under root (both resolved).
func within(p, root string) bool {
	rel, err := filepath.Rel(root, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}
