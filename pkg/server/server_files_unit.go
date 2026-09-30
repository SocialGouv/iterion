package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"

	"github.com/SocialGouv/iterion/pkg/dsl/ast"
	"github.com/SocialGouv/iterion/pkg/dsl/canon"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
	"github.com/SocialGouv/iterion/pkg/dsl/unit"
	"github.com/SocialGouv/iterion/pkg/dsl/workflowfile"
)

// unitInfo describes the unit a document was opened from: a bot in several
// files (`import "lib/x.bot"`), merged into one document whose every
// declaration names its file. The revision is the unit's digest — every
// file's path and content — and a save presents it back, so a file edited
// on disk in between is a conflict, never a silent overwrite.
type unitInfo struct {
	// Root is the unit's directory, as the request named its main.
	Root string `json:"root"`
	// Main is the main file's path from Root.
	Main     string         `json:"main"`
	Revision string         `json:"revision"`
	Files    []unitFileInfo `json:"files"`
}

type unitFileInfo struct {
	Rel     string   `json:"rel"`
	Profile int      `json:"profile,omitempty"`
	Imports []string `json:"imports,omitempty"`
	// Digest is sha256 of the file's content as the answer read it. The
	// unit's Revision covers the files the unit HOLDS; a file a per-file
	// edit newly imports is none of them, and the save rewrites it from a
	// document built on that read — so the claim carries it back and the
	// save compares, or a colleague's edit since the apply is overwritten
	// in silence.
	Digest string `json:"digest,omitempty"`
}

// canonReason is a canon refusal without its sentinel prefix, for a message
// that already says which file it is about and what was being attempted.
func canonReason(err error) string {
	return strings.TrimPrefix(err.Error(), canon.ErrRefused.Error()+": ")
}

func unitInfoOf(u *unit.Unit, reqPath string) *unitInfo {
	// Files is never nil: the picker of the Source view iterates it, and a
	// JSON `null` would reach a TS `UnitFileInfo[]` that declares itself
	// non-nullable.
	info := &unitInfo{Root: filepath.ToSlash(filepath.Dir(reqPath)), Main: u.Main, Revision: u.Digest, Files: []unitFileInfo{}}
	for _, f := range u.Files {
		fi := unitFileInfo{Rel: f.Rel, Digest: fileDigest(f.Source)}
		if f.AST != nil {
			fi.Profile = f.AST.Profile
			for _, im := range f.AST.Imports {
				fi.Imports = append(fi.Imports, im.Path)
			}
		}
		info.Files = append(info.Files, fi)
	}
	return info
}

// fileDigest is the content identity one file's claim carries back to the
// save: sha256 of its bytes, the same hash the unit's Digest composes.
func fileDigest(source []byte) string {
	sum := sha256.Sum256(source)
	return hex.EncodeToString(sum[:])
}

// hasProvenance reports whether any declaration, block or comment of the
// document names a file: what MarshalFileWithProvenance writes and the
// transport never does.
func hasProvenance(f *ast.File) bool {
	found := false
	walkCarriers(f, func(span ast.Span) { found = found || span.Start.File != "" })
	return found
}

// walkCarriers visits the span of every top-level declaration, comment,
// keyed block, block entry and block comment of a document — every carrier
// of provenance.
func walkCarriers(f *ast.File, visit func(ast.Span)) {
	dv := reflect.ValueOf(f).Elem()
	for i := 0; i < dv.NumField(); i++ {
		fv := dv.Field(i)
		switch fv.Kind() {
		case reflect.Slice:
			for j := 0; j < fv.Len(); j++ {
				if span, ok := spanOf(fv.Index(j)); ok {
					visit(span)
				}
			}
		case reflect.Pointer:
			if fv.IsNil() {
				continue
			}
			if span, ok := spanOf(fv); ok {
				visit(span)
			}
			if entries := entriesOf(fv.Elem()); entries.IsValid() {
				for j := 0; j < entries.Len(); j++ {
					if span, ok := spanOf(entries.Index(j)); ok {
						visit(span)
					}
				}
			}
			if cs := commentsOf(fv.Elem()); cs.IsValid() {
				for j := 0; j < cs.Len(); j++ {
					if span, ok := spanOf(cs.Index(j)); ok {
						visit(span)
					}
				}
			}
		}
	}
}

var spanType = reflect.TypeOf(ast.Span{})

func spanOf(v reflect.Value) (ast.Span, bool) {
	for v.IsValid() && v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return ast.Span{}, false
		}
		v = v.Elem()
	}
	if !v.IsValid() || v.Kind() != reflect.Struct {
		return ast.Span{}, false
	}
	sf := v.FieldByName("Span")
	if !sf.IsValid() || sf.Type() != spanType {
		return ast.Span{}, false
	}
	return sf.Interface().(ast.Span), true
}

// entriesOf is the entry list of a keyed block — `Fields` or `Entries`.
// NAMED, not guessed: a block has more than one slice field (it carries
// the comments written around it too), and "the first slice field" would
// return whichever the struct happens to declare first — a field reorder
// would then route the comment list as the entries.
func entriesOf(block reflect.Value) reflect.Value {
	for _, name := range []string{"Fields", "Entries"} {
		if f := block.FieldByName(name); f.IsValid() && f.Kind() == reflect.Slice {
			return f
		}
	}
	return reflect.Value{}
}

// commentsOf is a carrier's comment list, invalid when it has none.
func commentsOf(v reflect.Value) reflect.Value {
	f := v.FieldByName("Comments")
	if !f.IsValid() || f.Kind() != reflect.Slice {
		return reflect.Value{}
	}
	return f
}

// splitByProvenance takes a document of a merged unit apart, file by file:
// each declaration, comment and block entry goes to the file its
// provenance names — to the main when it names none, which is where the
// editor puts a new declaration — on a skeleton that keeps each file's own
// header. The header is the stored file's (its profile and its import
// lines) unless headers names the file, which is how a save carries a
// header the per-file editor changed: the document the client holds names
// them (unit_files), and the skeleton takes the CLAIMED profile and import
// lines, so the change is written instead of dropped. A provenance naming
// a file the unit does not have is refused.
func splitByProvenance(doc *ast.File, u *unit.Unit, headers map[string]unitFileInfo) (map[string]*ast.File, error) {
	parts := make(map[string]*ast.File, len(u.Files))
	for _, f := range u.Files {
		skel := &ast.File{}
		if h, ok := headers[f.Rel]; ok {
			skel.Profile = h.Profile
			for _, p := range h.Imports {
				skel.Imports = append(skel.Imports, &ast.ImportDecl{Path: p})
			}
		} else if f.AST != nil {
			skel.Profile = f.AST.Profile
			skel.Imports = f.AST.Imports
		}
		parts[f.Rel] = skel
	}
	// A declaration the unit already holds in a FRAGMENT that comes back
	// with no provenance did not appear in the editor: the client dropped
	// the file it came from, and routing it to the main would move it out
	// of its fragment in silence. A name no fragment holds is new, and
	// goes to the main.
	held := fragmentOwners(u)
	owner := func(span ast.Span, key string) (*ast.File, error) {
		if span.Start.File == "" {
			if rel, ok := held[key]; ok {
				return nil, fmt.Errorf("the document lost the provenance of %s, which lives in %s: reopen the file (the editor dropped the file it came from)", describeKey(key), rel)
			}
			return parts[u.Main], nil
		}
		p, ok := parts[span.Start.File]
		if !ok {
			return nil, fmt.Errorf("the document places a declaration in %q, which is not a file of this bot (%s)", span.Start.File, strings.Join(unitRels(u), ", "))
		}
		return p, nil
	}
	dv := reflect.ValueOf(doc).Elem()
	dt := dv.Type()
	for i := 0; i < dv.NumField(); i++ {
		field := dt.Field(i)
		fv := dv.Field(i)
		if field.Name == "Imports" || field.Name == "Profile" {
			continue
		}
		switch fv.Kind() {
		case reflect.Slice:
			for j := 0; j < fv.Len(); j++ {
				el := fv.Index(j)
				span, _ := spanOf(el)
				p, err := owner(span, declKey(field.Name, el))
				if err != nil {
					return nil, err
				}
				pf := reflect.ValueOf(p).Elem().Field(i)
				pf.Set(reflect.Append(pf, el))
			}
		case reflect.Pointer:
			if fv.IsNil() {
				continue
			}
			// The comments written around the block go where they were
			// written, like its entries: a block is a carrier, and
			// without this a save of a bot in several files dropped
			// every comment of its `vars:`, `presets:`, `secrets:` and
			// `attachments:` — silently, and with the save reporting
			// success, which is the whole of #1282 on this path.
			if cs := commentsOf(fv.Elem()); cs.IsValid() {
				for j := 0; j < cs.Len(); j++ {
					el := cs.Index(j)
					span, _ := spanOf(el)
					q, err := owner(span, "")
					if err != nil {
						return nil, err
					}
					target := reflect.ValueOf(q).Elem().Field(i)
					ensureBlock(target, fv.Type())
					tc := commentsOf(target.Elem())
					tc.Set(reflect.Append(tc, el))
				}
			}
			// Each entry goes where it was, and a block exists in a file
			// because an entry of it does. A block emptied of every entry
			// keeps its header where the header was written — what the
			// writer puts on a single file too — never a bare header in
			// a file whose entries all live elsewhere.
			entries := entriesOf(fv.Elem())
			if !entries.IsValid() || entries.Len() == 0 {
				blockSpan, _ := spanOf(fv)
				p, err := owner(blockSpan, "")
				if err != nil {
					return nil, err
				}
				ensureBlock(reflect.ValueOf(p).Elem().Field(i), fv.Type())
				continue
			}
			for j := 0; j < entries.Len(); j++ {
				el := entries.Index(j)
				span, _ := spanOf(el)
				q, err := owner(span, declKey(field.Name, el))
				if err != nil {
					return nil, err
				}
				target := reflect.ValueOf(q).Elem().Field(i)
				ensureBlock(target, fv.Type())
				te := entriesOf(target.Elem())
				te.Set(reflect.Append(te, el))
			}
		}
	}
	ensureInlinePrompts(parts, doc)
	restoreSubbotSources(parts, u)
	return parts, nil
}

// ensureInlinePrompts gives each file the inline prompts its nodes refer
// to. The merged document holds ONE declaration for a text several files
// wrote inline (unit.Load keeps the first); the writer puts an inline
// prompt back on the property that refers to it, so every file whose
// nodes refer to one needs its declaration in its own part — with this
// file's provenance.
func ensureInlinePrompts(parts map[string]*ast.File, doc *ast.File) {
	inline := map[string]*ast.PromptDecl{}
	for _, p := range doc.Prompts {
		if p.Inline {
			inline[p.Name] = p
		}
	}
	if len(inline) == 0 {
		return
	}
	for rel, part := range parts {
		declared := map[string]bool{}
		for _, p := range part.Prompts {
			declared[p.Name] = true
		}
		for _, name := range promptRefsOf(part) {
			p, ok := inline[name]
			if !ok || declared[name] {
				continue
			}
			cp := *p
			cp.Span = ast.Span{Start: ast.Pos{File: rel}, End: ast.Pos{File: rel}}
			part.Prompts = append(part.Prompts, &cp)
			declared[name] = true
		}
	}
}

// promptRefsOf lists the prompt names the nodes of f refer to — the
// System, User, Instructions and InteractionPrompt properties of every
// kind that carries one, group members included — by reflection over the
// fields' names, so a kind added later is walked without being listed.
func promptRefsOf(f *ast.File) []string {
	var out []string
	var walk func(v reflect.Value)
	walk = func(v reflect.Value) {
		for v.IsValid() && v.Kind() == reflect.Pointer {
			if v.IsNil() {
				return
			}
			v = v.Elem()
		}
		if !v.IsValid() {
			return
		}
		switch v.Kind() {
		case reflect.Struct:
			for i := 0; i < v.NumField(); i++ {
				field := v.Type().Field(i)
				if !field.IsExported() {
					continue
				}
				fv := v.Field(i)
				switch field.Name {
				case "System", "User", "Instructions", "InteractionPrompt":
					if fv.Kind() == reflect.String && fv.String() != "" {
						out = append(out, fv.String())
					}
					continue
				}
				walk(fv)
			}
		case reflect.Slice:
			for i := 0; i < v.Len(); i++ {
				walk(v.Index(i))
			}
		}
	}
	walk(reflect.ValueOf(f))
	return out
}

// restoreSubbotSources puts a subbot's `source:` back as its file wrote
// it. The merged document carries every fragment's subbot with a
// ROOT-relative source (unit.CanonicalSubbotSource — what every host
// resolves), while the fragment writes it relative to its own directory:
// the canonical form written back verbatim would move the child one lib/
// deeper at every save. A source the fragment already wrote is kept byte
// for byte; one the editor changed is written in the fragment's own
// terms. The main's subbots travel as written and stay so.
func restoreSubbotSources(parts map[string]*ast.File, u *unit.Unit) {
	for _, f := range u.Files {
		part := parts[f.Rel]
		if part == nil || f.AST == nil || f.Rel == u.Main {
			continue
		}
		written := make(map[string]string, len(f.AST.Subbots))
		for _, sb := range f.AST.Subbots {
			written[sb.Name] = sb.Source
		}
		for i, sb := range part.Subbots {
			source := unit.AuthoredSubbotSource(f.Rel, sb.Source)
			if w, ok := written[sb.Name]; ok && unit.CanonicalSubbotSource(f.Rel, w) == sb.Source {
				source = w
			}
			if source == sb.Source {
				continue
			}
			cp := *sb
			cp.Source = source
			part.Subbots[i] = &cp
		}
	}
}

// declKey names a declaration or a block entry by its kind (the ast.File
// field) and its name — its Name field, or the Prefix of a `use` — "" for
// an element with neither (a comment).
func declKey(field string, el reflect.Value) string {
	for el.IsValid() && el.Kind() == reflect.Pointer {
		if el.IsNil() {
			return ""
		}
		el = el.Elem()
	}
	if !el.IsValid() || el.Kind() != reflect.Struct {
		return ""
	}
	name := el.FieldByName("Name")
	if !name.IsValid() || name.Kind() != reflect.String {
		name = el.FieldByName("Prefix")
	}
	if !name.IsValid() || name.Kind() != reflect.String || name.String() == "" {
		return ""
	}
	return field + "\x00" + name.String()
}

// describeKey renders a declKey for a message: `agent "worker"`.
func describeKey(key string) string {
	field, name, _ := strings.Cut(key, "\x00")
	return strings.ToLower(strings.TrimSuffix(field, "s")) + " " + strconv.Quote(name)
}

// fragmentOwners maps every named declaration and block entry a FRAGMENT
// of the unit holds, by declKey, to that fragment.
func fragmentOwners(u *unit.Unit) map[string]string {
	out := map[string]string{}
	for _, f := range u.Files {
		if f.Rel == u.Main || f.AST == nil {
			continue
		}
		fv := reflect.ValueOf(f.AST).Elem()
		for i := 0; i < fv.NumField(); i++ {
			field := fv.Type().Field(i)
			v := fv.Field(i)
			switch v.Kind() {
			case reflect.Slice:
				for j := 0; j < v.Len(); j++ {
					if key := declKey(field.Name, v.Index(j)); key != "" {
						out[key] = f.Rel
					}
				}
			case reflect.Pointer:
				if v.IsNil() {
					continue
				}
				entries := entriesOf(v.Elem())
				if !entries.IsValid() {
					continue
				}
				for j := 0; j < entries.Len(); j++ {
					if key := declKey(field.Name, entries.Index(j)); key != "" {
						out[key] = f.Rel
					}
				}
			}
		}
	}
	return out
}

func ensureBlock(field reflect.Value, t reflect.Type) {
	if field.IsNil() {
		field.Set(reflect.New(t.Elem()))
	}
}

func unitRels(u *unit.Unit) []string {
	out := make([]string, 0, len(u.Files))
	for _, f := range u.Files {
		out = append(out, f.Rel)
	}
	return out
}

// sameProgram reports whether two files are the same program: the same
// declarations, headers and imports, positions aside.
func sameProgram(a, b *ast.File) bool {
	ja, errA := ast.MarshalFile(a)
	jb, errB := ast.MarshalFile(b)
	return errA == nil && errB == nil && bytes.Equal(ja, jb)
}

// stagedUnitFile is one file of a unit the save rewrites.
type stagedUnitFile struct {
	rel, abs string
	before   []byte
	after    string
}

// saveUnit saves a document of a bot in several files back into them:
// each declaration to the file its provenance names, each file's header
// (its `dsl:` profile and its import lines) from the document's claim when
// the request carries one (unit_files) — the per-file editor may change
// both, and the stored files' headers would drop the change — only the
// files whose program changed rewritten — byte for byte untouched
// otherwise — every main of the directory that imports a rewritten
// fragment checked to still compile, and the writes published as one
// journaled transaction under the files' locks, after the revision the
// document was opened at is found unchanged on disk.
func (s *Server) saveUnit(w http.ResponseWriter, r *http.Request, req saveFileRequest, absPath string, doc *ast.File, current []byte) {
	if req.CreateOnly {
		httpError(w, http.StatusUnprocessableEntity, "a bot in several files cannot be saved as a new file: save it in place, or write the flattened program by hand")
		return
	}
	u := unit.LoadDirWithMain(absPath, absPath, current)
	// The unit was read from disk, and the refusal below answers the
	// diagnostic to the client: it names the fragment at fault by its
	// unit-relative path (#1934).
	relUnitDiagnostics(u)
	if d := firstErrorDiagnostic(u.Diagnostics); d != "" {
		httpError(w, http.StatusUnprocessableEntity, "the bot's files do not load as one unit (%s): fix them on disk before saving from the studio", d)
		return
	}
	if req.Revision == "" {
		httpError(w, http.StatusUnprocessableEntity, "%s is a bot in several files and the document names no revision: reopen the file in the studio (an older client saves a single file)", req.Path)
		return
	}
	if req.Revision != u.Digest {
		httpError(w, http.StatusConflict, "the files of %s changed on disk since the document was opened: reopen it and redo the edit", req.Path)
		return
	}
	if !hasProvenance(doc) {
		httpError(w, http.StatusUnprocessableEntity, "the document carries no provenance for a bot in several files: reopen %s in the studio (an older client would fold every file into the main)", req.Path)
		return
	}
	target, headers, added, err := s.claimedTarget(u, unitRequest{main: u.Main, abs: absPath, root: u.Root}, req.UnitFiles, true)
	if err != nil {
		var conflict claimConflict
		if errors.As(err, &conflict) {
			httpError(w, http.StatusConflict, "%v", err)
			return
		}
		httpError(w, http.StatusUnprocessableEntity, "%v", err)
		return
	}
	parts, err := splitByProvenance(doc, target, headers)
	if err != nil {
		httpError(w, http.StatusUnprocessableEntity, "%v", err)
		return
	}
	var staged []stagedUnitFile
	stagedText := map[string][]byte{}
	for _, f := range target.Files {
		part := parts[f.Rel]
		if f.AST != nil && sameProgram(part, f.AST) {
			continue
		}
		text, err := canon.Text(f.Rel, part, f.Source)
		if err != nil {
			if errors.Is(err, canon.ErrRefused) {
				httpError(w, http.StatusUnprocessableEntity, "%s cannot be saved from the studio: %s. Leave the file as it is, or edit it directly", f.Rel, canonReason(err))
				return
			}
			httpError(w, http.StatusUnprocessableEntity, "%s cannot be saved as .bot source without changing it: %v", f.Rel, err)
			return
		}
		staged = append(staged, stagedUnitFile{rel: f.Rel, abs: f.Name, before: f.Source, after: text})
		stagedText[f.Rel] = []byte(text)
	}
	mainText := string(current)
	if t, ok := stagedText[u.Main]; ok {
		mainText = string(t)
	}
	if len(staged) == 0 {
		writeJSON(w, saveFileResponse{Path: req.Path, Source: mainText, ConfirmedDiskPath: absPath, Revision: u.Digest})
		return
	}
	if err := siblingImportersStillCompile(target, stagedText); err != nil {
		httpError(w, http.StatusUnprocessableEntity, "%v", err)
		return
	}
	paths := make([]string, 0, len(staged))
	for _, f := range staged {
		paths = append(paths, f.abs)
	}
	locks, err := acquireAuthoringLocalLocks(r.Context(), paths)
	if err != nil {
		s.authoringError(w, r, err)
		return
	}
	defer closeAuthoringLocalLocks(locks)
	// Under the locks: the unit is still the one the document was opened at.
	if again := unit.LoadDir(absPath); again.Digest != u.Digest {
		httpError(w, http.StatusConflict, "the files of %s changed on disk while the save waited for their locks: reopen it and redo the edit", req.Path)
		return
	}
	// The unit's digest covers the files it HOLDS; a file the claim newly
	// imports is none of them, and it is rewritten from the document built
	// on the apply's read. Re-read it here, or a colleague's edit made
	// while the save waited is overwritten in silence.
	for _, f := range added {
		b, readErr := os.ReadFile(f.Name) // #nosec G304 -- a fragment of the unit, confined by the loader that read it
		if readErr != nil || !bytes.Equal(b, f.Source) {
			httpError(w, http.StatusConflict, "%s changed on disk while the save waited for the locks: reopen the bot and redo the edit", f.Rel)
			return
		}
	}
	previews := make([]authoringPreviewFile, 0, len(staged))
	for _, f := range staged {
		previews = append(previews, authoringPreviewFile{Scope: "unit", Path: f.rel, Operation: "update", Before: string(f.before), After: f.after})
	}
	if _, err := s.publishAuthoringLocal(r.Context(), locks, previews, false); err != nil {
		var conflict authoringConflictError
		if errors.As(err, &conflict) {
			httpError(w, http.StatusConflict, "%v", err)
			return
		}
		httpError(w, http.StatusInternalServerError, "write error: %v", err)
		return
	}
	written := make([]string, 0, len(staged))
	for _, f := range staged {
		written = append(written, f.rel)
	}
	writeJSON(w, saveFileResponse{Path: req.Path, Source: mainText, ConfirmedDiskPath: absPath, Revision: unit.LoadDir(absPath).Digest, Files: written})
}

// siblingImportersStillCompile reloads every other workflow file of the
// unit's directory that imports a fragment about to be rewritten, against
// the staged contents, and refuses the save when one of them no longer
// loads or compiles: a fragment shared by two mains must not break the
// other in silence.
func siblingImportersStillCompile(u *unit.Unit, staged map[string][]byte) error {
	entries, err := os.ReadDir(u.Root)
	if err != nil {
		return err
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() || e.Name() == u.Main || !workflowfile.IsWorkflowFile(e.Name()) {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)
	for _, name := range names {
		sibling := filepath.Join(u.Root, name)
		before := unit.LoadDir(sibling)
		imports := false
		for _, f := range before.Files {
			if _, ok := staged[f.Rel]; ok && f.Rel != before.Main {
				imports = true
			}
		}
		if !imports {
			continue
		}
		after := unit.LoadDirStaged(sibling, staged)
		// The sibling was read from disk, and the refusal answers its
		// diagnostic to the client — relative names (#1934).
		relUnitDiagnostics(after)
		if d := firstErrorDiagnostic(after.Diagnostics); d != "" {
			return fmt.Errorf("saving would break %s, which imports a rewritten fragment: %s", name, d)
		}
		if after.Merged == nil {
			return fmt.Errorf("saving would break %s, which imports a rewritten fragment: no workflow found", name)
		}
		if cr := ir.Compile(after.Merged); cr.HasErrors() {
			for _, d := range cr.Diagnostics {
				if d.Severity == ir.SeverityError {
					return fmt.Errorf("saving would break %s, which imports a rewritten fragment: %s", name, d.Error())
				}
			}
		}
	}
	return nil
}

func firstErrorDiagnostic(diags []parser.Diagnostic) string {
	for _, d := range diags {
		if d.Severity == parser.SeverityError {
			return d.Error()
		}
	}
	return ""
}

// authoringStepHook is a test seam: called before each transition of every
// authoring transaction with the transition's name, a fault it returns
// interrupts the write there. Nil outside tests.
var authoringStepHook func(string) error

// publishAuthoringLocal writes a set of files as one journaled transaction:
// every candidate is prepared before any byte is exposed, then each is
// published in turn, and a failure rolls the published ones back. The
// recovery records name what was retained; nothing is replayed by itself.
// retain keeps each transaction's journal and displaced original once it
// published — what the assistant's commit returns to the dock as the
// recovery of the version it replaced. A save from the editor has no
// reader for them: it drops them, or every save would leave a copy of
// every version ever saved in the bot's own directory.
func (s *Server) publishAuthoringLocal(ctx context.Context, locks []*authoringLocalLock, previews []authoringPreviewFile, retain bool) ([]authoringRecovery, error) {
	var ignore func(string)
	s.stateMu.RLock()
	if s.watcher != nil {
		ignore = s.watcher.IgnorePath
	}
	s.stateMu.RUnlock()
	transactions := make([]*authoringLocalTransaction, 0, len(previews))
	defer func() {
		for _, tx := range transactions {
			tx.close()
		}
	}()
	recovery := func() []authoringRecovery {
		out := make([]authoringRecovery, 0, len(transactions))
		for _, tx := range transactions {
			out = append(out, tx.recovery())
		}
		return out
	}
	failure := func(err error) ([]authoringRecovery, error) {
		locations := make([]string, 0, len(transactions))
		for _, tx := range transactions {
			locations = append(locations, tx.recovery().Record)
		}
		return recovery(), fmt.Errorf("%w; recovery records (retained without automatic replay): %s", err, strings.Join(locations, "; "))
	}
	for i, preview := range previews {
		tx, err := prepareAuthoringLocal(locks[i], preview, ignore, authoringStepHook)
		if tx != nil {
			transactions = append(transactions, tx)
		}
		if err != nil {
			return failure(err)
		}
	}
	for _, tx := range transactions {
		err := ctx.Err()
		if err == nil {
			err = tx.publish()
		}
		if err != nil {
			rollbackErrs := s.rollbackAuthoring(transactions)
			if len(rollbackErrs) > 0 {
				return failure(fmt.Errorf("write %s:%s failed: %w; rollback incomplete: %s", tx.preview.Scope, tx.preview.Path, err, strings.Join(rollbackErrs, "; ")))
			}
			return failure(fmt.Errorf("write %s:%s failed: %w; earlier files were rolled back", tx.preview.Scope, tx.preview.Path, err))
		}
	}
	// Every file is published: the journals and the displaced originals
	// are for the recovery of a write that did not finish, and one that
	// finished leaves nothing to recover — not a copy of every version
	// ever saved, in the bot's own directory. A record that will not go
	// is left where it is and named; the files are written either way.
	if retain {
		return recovery(), nil
	}
	for _, tx := range transactions {
		if err := tx.complete(); err != nil {
			s.logger.Warn("authoring: a finished write of %s keeps its record: %v", tx.preview.Path, err)
		}
	}
	return recovery(), nil
}

// unitOpenResponse is the open response of a bot in several files.
type unitOpenResponse struct {
	Source            string          `json:"source"`
	Document          json.RawMessage `json:"document"`
	Diagnostics       []string        `json:"diagnostics,omitempty"`
	Path              string          `json:"path,omitempty"`
	ConfirmedDiskPath string          `json:"confirmed_disk_path,omitempty"`
	Unit              *unitInfo       `json:"unit"`
	// Bindable is false when the file does not parse: the document is then
	// what the parser SALVAGED, and binding it would make the next save
	// write that back over what the author wrote.
	Bindable bool `json:"bindable"`
}

// parseUnitFiles parses a bot in several files, given as a files map, as its
// unit: one document with each declaration's file on it, and the unit's
// revision — what the cloud editor opens a bundle's main with.
func (s *Server) parseUnitFiles(w http.ResponseWriter, req parseRequest) {
	main := req.Main
	if main == "" {
		main = "main.bot"
	}
	if !workflowfile.IsWorkflowFile(main) {
		httpError(w, http.StatusBadRequest, "main %q is not a workflow file: a unit is read from a .bot", main)
		return
	}
	u := unit.LoadMap(req.Files, main)
	var diags []string
	for _, d := range u.Diagnostics {
		diags = append(diags, d.Error())
	}
	if u.Merged == nil {
		writeJSON(w, parseResponse{Diagnostics: diags})
		return
	}
	docJSON, err := ast.MarshalFileWithProvenance(u.Merged, "")
	if err != nil {
		httpError(w, http.StatusInternalServerError, "marshal error: %v", err)
		return
	}
	info := unitInfoOf(u, main)
	info.Root = ""
	// A unit stays writable even when it does not LOAD — the posture
	// /api/files/open holds for a unit on disk. The write back goes through
	// unparseUnitFiles, which refuses one that does not load and names the
	// fragment at fault, so nothing lands on the author's files; refusing
	// here would leave a buffer no save could place.
	//
	// A main that did not PARSE is another matter: the merged document is
	// then the main's salvage plus the fragments, so it is a salvage, and
	// the verdict says so — the same rule as /api/files/open and
	// /api/examples for a unit on disk.
	mainParse := parser.Parse(main, req.Files[main])
	writeJSON(w, parseResponse{Document: json.RawMessage(docJSON), Diagnostics: diags, Unit: info, Bindable: !parseHasErrors(mainParse.Diagnostics)})
}

// unparseUnitFiles writes a document of a bot in several files back into
// them, by provenance, and returns the files whose program changed — and
// only those, so the caller patches them into the bundle it holds.
func (s *Server) unparseUnitFiles(w http.ResponseWriter, req unparseRequest, doc *ast.File) {
	main := req.Main
	if main == "" {
		main = "main.bot"
	}
	if !workflowfile.IsWorkflowFile(main) {
		httpError(w, http.StatusBadRequest, "main %q is not a workflow file: a unit is written back from a .bot", main)
		return
	}
	u := unit.LoadMap(req.Files, main)
	if d := firstErrorDiagnostic(u.Diagnostics); d != "" {
		httpError(w, http.StatusUnprocessableEntity, "the bundle's files do not load as one unit (%s): fix them before saving", d)
		return
	}
	// The revision the document was opened at, against the files as they
	// are now: a fragment a colleague changed since would otherwise come
	// back rewritten with this document's stale text, and the bundle's
	// version CAS covers only the fetch-to-write window.
	if req.Revision == "" {
		httpError(w, http.StatusUnprocessableEntity, "the document names no revision for a bot in several files: reopen the bot (an older client saves a single file)")
		return
	}
	if req.Revision != u.Digest {
		httpError(w, http.StatusConflict, "the files of the bot changed since the document was opened: reopen it and redo the edit")
		return
	}
	if u.Merged == nil {
		httpError(w, http.StatusUnprocessableEntity, "the bundle's files hold no program")
		return
	}
	if len(u.Files) > 1 && !hasProvenance(doc) {
		httpError(w, http.StatusUnprocessableEntity, "the document carries no provenance for a bot in several files: reopen the bot (an older client would fold every file into the main)")
		return
	}
	target, headers, _, err := s.claimedTarget(u, unitRequest{files: req.Files, main: main}, req.UnitFiles, true)
	if err != nil {
		var conflict claimConflict
		if errors.As(err, &conflict) {
			httpError(w, http.StatusConflict, "%v", err)
			return
		}
		httpError(w, http.StatusUnprocessableEntity, "%v", err)
		return
	}
	parts, err := splitByProvenance(doc, target, headers)
	if err != nil {
		httpError(w, http.StatusUnprocessableEntity, "%v", err)
		return
	}
	out := map[string]string{}
	for _, f := range target.Files {
		part := parts[f.Rel]
		if f.AST != nil && sameProgram(part, f.AST) {
			continue
		}
		text, err := canon.Text(f.Rel, part, f.Source)
		if err != nil {
			if errors.Is(err, canon.ErrRefused) {
				httpError(w, http.StatusUnprocessableEntity, "%s cannot be saved from the studio: %s. Leave the file as it is, or edit it directly", f.Rel, canonReason(err))
				return
			}
			httpError(w, http.StatusUnprocessableEntity, "%s cannot be rendered as .bot source without changing it: %v", f.Rel, err)
			return
		}
		out[f.Rel] = text
	}
	source := req.Files[main]
	if t, ok := out[main]; ok {
		source = t
	}
	// The revision the bundle has once the rewritten files are patched in:
	// what the next save presents.
	patched := make(map[string]string, len(req.Files))
	for rel, text := range req.Files {
		patched[rel] = text
	}
	for rel, text := range out {
		patched[rel] = text
	}
	writeJSON(w, unparseResponse{Source: source, Files: out, Revision: unit.LoadMap(patched, main).Digest})
}

// unitRequest is where a per-file request of the Source view reads its bot
// from: a cloud bundle's files map, or a main in the workspace. It keeps
// what staging one file needs, so the picker's render and its apply cannot
// disagree on which unit they mean — and so a disk unit is re-staged
// through LoadDirStaged, which keeps every file's on-disk name (what an
// `include` and a `subbot` resolve against) instead of a map key.
type unitRequest struct {
	unit  *unit.Unit
	files map[string]string // set for a cloud bundle
	main  string
	abs   string // set for a unit on disk
	root  string // the unit's directory on disk, "" for a files map
}

// stage reloads the unit with rel's text replaced by source.
func (ur unitRequest) stage(rel, source string) *unit.Unit {
	if ur.files != nil {
		patched := make(map[string]string, len(ur.files))
		for k, v := range ur.files {
			patched[k] = v
		}
		patched[rel] = source
		return unit.LoadMap(patched, ur.main)
	}
	// Keyed the way loadDir keys its staged map: from the directory of the
	// MAIN on disk, not by the unit's rel. For a main that is itself a
	// `lib/` fragment the two differ, and a mismatched key is read as "no
	// staged text" — the loader falls back to disk and the route answers
	// 200 with the author's edit silently gone.
	key := rel
	if ur.root != "" {
		if k, err := filepath.Rel(filepath.Dir(ur.abs), filepath.Join(ur.root, filepath.FromSlash(rel))); err == nil {
			key = filepath.ToSlash(k)
		}
	}
	return unit.LoadDirStaged(ur.abs, map[string][]byte{key: []byte(source)})
}

// claimConflict is a claimed-unit validation failure that is a STALENESS
// conflict (409), not a refusal (422): a file the claim adds changed since
// the apply read it into the document.
type claimConflict struct{ msg string }

func (e claimConflict) Error() string { return e.msg }

// claimedTarget resolves the unit a request's claimed file list
// (unit_files) makes of the stored one: each file's profile and import
// lines AS THE CLIENT KNOWS them, which a per-file edit may have changed —
// the two header fields the merged document does not carry. The answer is
// the stored unit itself, with nil headers, when the claim adds nothing to
// it — the exact path a client without the claim takes — and otherwise a
// unit of the CLAIMED membership, whose kept files keep their stored AST
// (what "did this file's program change" is judged against) and whose
// skeleton headers come from the claim, so the change is written instead
// of dropped. added holds the files the claim joins to the unit, with
// their content as read NOW; a disk save re-reads them under the locks.
// forWrite says the answer backs a WRITE: a file the claim adds is then
// checked against the digest the claim carries (the unit's revision never
// covered it). A render passes false — showing the document's truth about
// its files is not a write, and refusing it would strand an unsaved header
// edit behind a staleness verdict only a save needs.
func (s *Server) claimedTarget(u *unit.Unit, ur unitRequest, claimed []unitFileInfo, forWrite bool) (target *unit.Unit, headers map[string]unitFileInfo, added []unit.File, err error) {
	if len(claimed) == 0 || claimedMatchesStored(u, claimed) {
		return u, nil, nil, nil
	}
	probe := ur.probeUnit(u, claimed)
	return finishClaimed(u, probe, claimed, forWrite)
}

// claimedMatchesStored reports whether the claimed file list is the stored
// unit's, file for file and header for header.
func claimedMatchesStored(u *unit.Unit, claimed []unitFileInfo) bool {
	if len(claimed) != len(u.Files) {
		return false
	}
	stored := make(map[string]unit.File, len(u.Files))
	for _, f := range u.Files {
		stored[f.Rel] = f
	}
	for _, cf := range claimed {
		f, ok := stored[cf.Rel]
		if !ok {
			return false
		}
		profile, imports := storedHeader(f)
		if profile != cf.Profile || !importPathsEqual(imports, cf.Imports) {
			return false
		}
	}
	return true
}

// storedHeader is a stored file's header as a claim carries it: the raw
// `dsl:` profile (0 when the file declares none) and the import paths as
// written.
func storedHeader(f unit.File) (profile int, imports []string) {
	if f.AST == nil {
		return 0, nil
	}
	for _, im := range f.AST.Imports {
		imports = append(imports, im.Path)
	}
	return f.AST.Profile, imports
}

// importPathsEqual reports whether two import lists name the same paths in
// the same order.
func importPathsEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// probeUnit loads the unit a claimed file list describes, so its
// membership comes from the CLAIMED import lines rather than the stored
// ones: every file the stored unit holds is staged as its claimed header
// alone — the `import` lines, no declaration — and the loader walks those;
// a file the claim ADDS is not staged, so the loader reads its real
// content, from the bundle's files map or from disk with its confinement,
// and walks its own imports from there. The probe's entries for the kept
// files are the synthetic headers', not the files': what the declarations
// are is the stored unit's to say.
func (ur unitRequest) probeUnit(stored *unit.Unit, claimed []unitFileInfo) *unit.Unit {
	held := make(map[string]bool, len(stored.Files))
	for _, f := range stored.Files {
		held[f.Rel] = true
	}
	// The synthetic main keeps a workflow when the real one declares one —
	// named after it, so an error that mentions it (a fragment that declares
	// a workflow of its own meets the unit's one-workflow rule, E010) reads
	// as the REAL main's workflow, not as a "probe" the author never wrote.
	// A main living below a lib/ directory whose staged text has no
	// workflow is read as a FRAGMENT of the bot above (unit.fragmentAlone
	// judges the staged text), and the probe's whole membership is rebased
	// under names nothing claims.
	stub := ""
	for _, f := range stored.Files {
		if f.Rel == stored.Main && f.AST != nil && len(f.AST.Workflows) > 0 {
			stub = f.AST.Workflows[0].Name
		}
	}
	if ur.files != nil {
		m := make(map[string]string, len(claimed))
		for _, cf := range claimed {
			if held[cf.Rel] {
				m[cf.Rel] = claimedHeaderText(cf, mainStub(cf, stored, stub))
			} else if src, ok := ur.files[cf.Rel]; ok {
				m[cf.Rel] = src
			}
			// An added file the bundle does not hold stays out of the map:
			// the loader reports it unreadable at the import that names it.
		}
		return unit.LoadMap(m, ur.main)
	}
	staged := make(map[string][]byte, len(claimed))
	for _, cf := range claimed {
		if !held[cf.Rel] {
			continue
		}
		// Keyed the way stage keys its overlay: from the directory of the
		// main on disk, which differs from the unit's rels when the main is
		// itself a `lib/` fragment.
		key := cf.Rel
		if ur.root != "" {
			if k, err := filepath.Rel(filepath.Dir(ur.abs), filepath.Join(ur.root, filepath.FromSlash(cf.Rel))); err == nil {
				key = filepath.ToSlash(k)
			}
		}
		staged[key] = []byte(claimedHeaderText(cf, mainStub(cf, stored, stub)))
	}
	return unit.LoadDirStaged(ur.abs, staged)
}

// mainStub is the stub workflow's name for the claimed main, "" for every
// other file (never a fragment: the unit's one-workflow rule would fire
// inside the probe).
func mainStub(cf unitFileInfo, stored *unit.Unit, stub string) string {
	if cf.Rel == stored.Main {
		return stub
	}
	return ""
}

// claimedHeaderText renders a claimed file header — its `import` lines —
// as a whole file's text: what probeUnit stages for a file the stored unit
// holds, so the loader walks the CLAIMED imports and the membership is the
// claim's, while no declaration of the stored file leaks into the probe.
// stubWorkflow, when set, adds a stub workflow of that name — the real
// main's, so the staged main keeps its main-ness below a lib/ directory
// and any error that names the workflow names the author's own.
func claimedHeaderText(cf unitFileInfo, stubWorkflow string) string {
	var b strings.Builder
	for _, p := range cf.Imports {
		fmt.Fprintf(&b, "import %s\n", strconv.Quote(p))
	}
	if stubWorkflow != "" {
		fmt.Fprintf(&b, "\nworkflow %s:\n  entry: done\n", stubWorkflow)
	}
	return b.String()
}

// citeStoredMainWorkflow rewrites the probe's duplicate-workflow
// diagnostic (E010) so the main-side citation is the STORED main's real
// workflow position. The probe's main is a synthetic header: its stub
// workflow sits wherever the claim's import lines leave it — a line that
// shifts with the claim's import count and points at a place the author's
// file does not have. Everything else the probe cites is real (fragments
// are read from their real content); when the real main declares no
// workflow there is no stub, and nothing to rewrite.
func citeStoredMainWorkflow(stored, probe *unit.Unit) {
	if probe.Merged == nil || len(probe.Merged.Workflows) < 2 {
		return
	}
	var mainWf *ast.WorkflowDecl
	for _, f := range stored.Files {
		if f.Rel == stored.Main && f.AST != nil && len(f.AST.Workflows) > 0 {
			mainWf = f.AST.Workflows[0]
		}
	}
	if mainWf == nil {
		return
	}
	other := probe.Merged.Workflows[1]
	for i, d := range probe.Diagnostics {
		if d.Code == parser.DiagDuplicateDecl && strings.HasPrefix(d.Message, "a unit has one workflow:") {
			probe.Diagnostics[i].Message = fmt.Sprintf("a unit has one workflow: %q here and %q at %s:%d", other.Name, mainWf.Name, stored.Main, mainWf.Span.Start.Line)
		}
	}
}

// relUnitDiagnostics rewrites a disk-loaded unit's diagnostics to the
// unit's relative names, the way the loader's own messages already cite a
// file (relOf) — for every unit whose diagnostics cross to a client: a
// refusal's 422 body IS the diagnostic, and an open, an apply or an
// example's answer carries them in a 200, so either forwarded as is
// discloses the server's directory layout (#1918, #1934). On disk the
// loader parses every file under its ABSOLUTE path — an include resolves
// beside it — and every name a diagnostic of such a unit can carry is
// Join(Root, Rel): LoadDir, LoadDirWithMain and LoadDirStaged name no file
// otherwise, and a files map names by Rel already (Root "") — so cutting
// the root answers the position field and every message that cites a name
// verbatim, and no Name→Rel table maps a string the cut does not. The one
// diagnostic text that ever carried an absolute name from OUTSIDE the root
// — the confinement refusal's resolved path — names the import as written
// since #1918, loader-side. Only the per-request load handed to a client
// is rewritten; a unit the server keeps — what an operator's logs hold of
// it — keeps its absolute names.
func relUnitDiagnostics(u *unit.Unit) {
	if u.Root == "" {
		return // a files map names every file by its rel already
	}
	rootPrefix := u.Root + string(os.PathSeparator)
	for i, d := range u.Diagnostics {
		d.File = filepath.ToSlash(strings.TrimPrefix(d.File, rootPrefix))
		d.Message = strings.ReplaceAll(d.Message, rootPrefix, "")
		u.Diagnostics[i] = d
	}
}

// finishClaimed validates the probe of a claimed file list against the
// claim and assembles the unit the save or the render works from: the
// stored unit's own entries for the files it holds (their declarations are
// the comparison a rewrite is judged against), the probe's for the ones
// the claim adds (their content as read now — and for a WRITE,
// digest-checked against the read the claim carries, since the unit's
// revision never covered them). headers maps every claimed file to the
// header the split's skeleton must take: the claim's, not the stored
// file's.
func finishClaimed(stored, probe *unit.Unit, claimed []unitFileInfo, forWrite bool) (*unit.Unit, map[string]unitFileInfo, []unit.File, error) {
	citeStoredMainWorkflow(stored, probe)
	relUnitDiagnostics(probe)
	if d := firstErrorDiagnostic(probe.Diagnostics); d != "" {
		return nil, nil, nil, fmt.Errorf("the files the document claims do not load as one unit: %s", d)
	}
	byRel := make(map[string]unitFileInfo, len(claimed))
	mainSeen := false
	for _, cf := range claimed {
		if _, dup := byRel[cf.Rel]; dup {
			return nil, nil, nil, fmt.Errorf("the document's file list names %s twice", cf.Rel)
		}
		byRel[cf.Rel] = cf
		mainSeen = mainSeen || cf.Rel == stored.Main
	}
	if !mainSeen {
		return nil, nil, nil, fmt.Errorf("the document's file list does not hold the bot's main (%s)", stored.Main)
	}
	held := make(map[string]unit.File, len(stored.Files))
	for _, f := range stored.Files {
		held[f.Rel] = f
	}
	files := make([]unit.File, 0, len(probe.Files))
	var added []unit.File
	for _, pf := range probe.Files {
		cf, ok := byRel[pf.Rel]
		if !ok {
			return nil, nil, nil, fmt.Errorf("the document's `import` lines reach %s, which its file list does not claim", pf.Rel)
		}
		delete(byRel, pf.Rel)
		if f, ok := held[pf.Rel]; ok {
			files = append(files, f)
			continue
		}
		// A file the claim ADDS was never covered by the unit's revision, so
		// the digest of the read the document was built from is the only
		// thing that stands between a colleague's edit and an overwrite. A
		// claim without it cannot be told fresh from stale: refused, not
		// trusted.
		if cf.Digest == "" {
			return nil, nil, nil, fmt.Errorf("the document's file list claims %s, a file this bot did not hold, without the digest of the read it was seen in: reopen the bot and redo the edit", pf.Rel)
		}
		if forWrite && cf.Digest != fileDigest(pf.Source) {
			return nil, nil, nil, claimConflict{fmt.Sprintf("%s changed since it was read into this bot: reopen it and redo the edit", pf.Rel)}
		}
		added = append(added, pf)
		files = append(files, pf)
	}
	if len(byRel) > 0 {
		rels := make([]string, 0, len(byRel))
		for rel := range byRel {
			rels = append(rels, rel)
		}
		sort.Strings(rels)
		return nil, nil, nil, fmt.Errorf("the document's file list claims %s, which no `import` line reaches", strings.Join(rels, ", "))
	}
	headers := make(map[string]unitFileInfo, len(claimed))
	for _, cf := range claimed {
		headers[cf.Rel] = cf
	}
	return &unit.Unit{Root: stored.Root, Main: stored.Main, Files: files}, headers, added, nil
}

// resolveUnitRequest is the ONE place the two modes are told apart. A path
// goes through s.safePath — the audited workspace boundary — and through
// workflowfile.IsWorkflowFile, like every other route that reads a bot.
func (s *Server) resolveUnitRequest(files map[string]string, main, path string) (unitRequest, error) {
	if len(files) > 0 {
		if main == "" {
			main = "main.bot"
		}
		if !workflowfile.IsWorkflowFile(main) {
			return unitRequest{}, fmt.Errorf("main %q is not a workflow file: a unit is read from a .bot", main)
		}
		return unitRequest{unit: unit.LoadMap(files, main), files: files, main: main}, nil
	}
	if path == "" {
		return unitRequest{}, errors.New("a per-file request names no bot: give the bundle's files and its main, or the workspace path of the main")
	}
	if !workflowfile.IsWorkflowFile(path) {
		return unitRequest{}, fmt.Errorf("%s is not a workflow file: a unit is read from a .bot", path)
	}
	abs, err := s.safePath(path)
	if err != nil {
		return unitRequest{}, err
	}
	u := unit.LoadDir(abs)
	return unitRequest{unit: u, main: u.Main, abs: abs, root: u.Root}, nil
}

// unitFile is the unit's file named rel, and whether it holds one. A rel
// the unit does not hold is an error wherever it appears: answering the
// whole unit, or answering unchanged, would let a stale or mistyped name
// read as success and send the author's typed text to the bin — including
// the name of a fragment deleted under the open picker, which the staged
// text would otherwise resurrect.
func unitFile(u *unit.Unit, rel string) (unit.File, bool) {
	for _, f := range u.Files {
		if f.Rel == rel {
			return f, true
		}
	}
	return unit.File{}, false
}

// unparseUnitPart renders ONE file of a unit: what the Source view's picker
// shows for the file it is on. Read-only — no revision is presented and
// nothing is written — so a file of a unit that no longer loads, or one the
// writer cannot reproduce, is still readable. The render follows the
// document's claimed file list (unit_files) when the request carries one:
// a header a per-file edit changed and has not saved yet is rendered as
// claimed, or the view would show the stored header over a document that
// no longer has it — and a re-apply of that text would silently revert the
// edit. Staleness of a file the claim newly imports is NOT judged here:
// the render writes nothing, and refusing it would strand the unsaved
// header edit behind a verdict only a save needs — the save makes it, as
// a conflict, when it runs.
func (s *Server) unparseUnitPart(w http.ResponseWriter, req unparseRequest, doc *ast.File) {
	ur, err := s.resolveUnitRequest(req.Files, req.Main, req.Path)
	if err != nil {
		httpError(w, http.StatusBadRequest, "%v", err)
		return
	}
	if ur.unit.Merged == nil {
		httpError(w, http.StatusUnprocessableEntity, "the bot's files hold no program")
		return
	}
	target, headers, _, err := s.claimedTarget(ur.unit, ur, req.UnitFiles, false)
	if err != nil {
		httpError(w, http.StatusUnprocessableEntity, "%v", err)
		return
	}
	f, ok := unitFile(target, req.File)
	if !ok {
		httpError(w, http.StatusUnprocessableEntity, "%q is not a file of this bot (%s)", req.File, strings.Join(unitRels(target), ", "))
		return
	}
	parts, err := splitByProvenance(doc, target, headers)
	if err != nil {
		httpError(w, http.StatusUnprocessableEntity, "%v", err)
		return
	}
	// A unit's `bindable` is the MAIN's parse alone, so a FRAGMENT the
	// parser could only salvage leaves the buffer unsalvaged and this view
	// on — and the loader keeps that file's partial AST, so the part here
	// is missing everything after the error. Rendering it back would show
	// the author a text their file does not contain and hide the very lines
	// they have to fix, which is what this route exists not to do.
	if spr := parser.Parse(f.Rel, string(f.Source)); parseHasErrors(spr.Diagnostics) {
		// The diagnostic itself, not a guess about it: several error-severity
		// diagnostics leave a COMPLETE ast (an unknown property, a bad `dsl:`
		// value), so "does not parse" would be false of them — while the
		// render is still not the file, which is what this refuses.
		writeJSON(w, unparseResponse{
			Source:  string(f.Source),
			Refused: fmt.Sprintf("iterion could not read all of %s (%s), so the document holds only what could be read of it — repair it where this bot's files live", f.Rel, firstParseError(spr.Diagnostics)),
			Stored:  true,
		})
		return
	}
	text, err := canon.Text(f.Rel, parts[f.Rel], f.Source)
	if err != nil {
		if errors.Is(err, canon.ErrRefused) {
			// The writer cannot reproduce this file. Rendering it anyway
			// would show the author a text their file does not contain —
			// the lie the salvage path already refuses to tell — so the
			// file's own text comes back, with the reason it is the only
			// thing that can.
			writeJSON(w, unparseResponse{Source: string(f.Source), Refused: canonReason(err), Stored: true})
			return
		}
		httpError(w, http.StatusUnprocessableEntity, "%s cannot be rendered as .bot source without changing it: %v", f.Rel, err)
		return
	}
	writeJSON(w, unparseResponse{Source: text})
}

// parseUnitWithFile re-parses a unit with ONE file replaced by the text the
// Source view's picker holds: the author edits a real file, and the merged
// document the canvas and the save both work from is rebuilt from it. The
// staged text's `import` lines decide the staged unit's membership and its
// `dsl:` line its file's profile — the two header fields the merged
// document does not carry — and the answer's file list names them, so the
// save (saveUnit / unparseUnitFiles) is handed the claim back as
// unit_files and writes the header from it.
//
// It answers no revision. A revision is a claim about the files at REST —
// the token saveUnit and unparseUnitFiles compare against disk, or against
// the bundle as stored — and an overlay changed neither. Answering the
// staged unit's digest would make every later save a false conflict;
// answering the current one would adopt a colleague's edit made meanwhile
// and destroy the conflict detection outright. The client keeps the
// revision it opened at.
func (s *Server) parseUnitWithFile(w http.ResponseWriter, req parseRequest) {
	ur, err := s.resolveUnitRequest(req.Files, req.Main, req.Path)
	if err != nil {
		httpError(w, http.StatusBadRequest, "%v", err)
		return
	}
	if _, ok := unitFile(ur.unit, req.File); !ok {
		httpError(w, http.StatusUnprocessableEntity, "%q is not a file of this bot (%s)", req.File, strings.Join(unitRels(ur.unit), ", "))
		return
	}
	// The staged text must PARSE on its own. A file the parser can only
	// salvage merges as the declarations it COULD read, and a document
	// missing them writes that file back short — 200, no diagnostic, the
	// author's declarations gone. The refusal is what the Source view shows
	// instead, which is where the author fixes it.
	//
	// It is a parse check and nothing more: text that parses but declares
	// less than it did is the author's edit, and /api/parse answers parse
	// diagnostics alone on every one of its three shapes. What the program
	// compiles to is /api/validate's question.
	pr := parser.Parse(req.File, req.Source)
	if parseHasErrors(pr.Diagnostics) {
		var errs []string
		for _, d := range pr.Diagnostics {
			if d.Severity == parser.SeverityError {
				errs = append(errs, d.Error())
			}
		}
		httpError(w, http.StatusUnprocessableEntity, "%s does not parse, so it cannot be applied — the rest of the bot would be saved without what it declares: %s", req.File, strings.Join(errs, "; "))
		return
	}
	staged := ur.stage(req.File, req.Source)
	// Both units were read from disk, and both the refusal below and the
	// 200's diagnostics cross to the client — relative names (#1934). The
	// stored unit's are cut too so the refusal's already-known comparison
	// (stagedLoadFailure) matches rel against rel.
	relUnitDiagnostics(ur.unit)
	relUnitDiagnostics(staged)
	// What an apply may not carry, refused by name: a staged `import` the
	// loader cannot follow — a fragment that is not there, a path outside
	// the bot's lib/ directory, a cycle — or one that pulls in a fragment
	// the parser could only salvage. Both leave the merged document short
	// of declarations a save would then drop. Errors the stored unit
	// already has are the author's existing business, and ones an edit
	// introduces in a file the bot already holds (a duplicate declaration,
	// say) are surfaced as diagnostics and saved as ever: the save can
	// carry both.
	if msg := stagedLoadFailure(ur.unit, staged, req.File); msg != "" {
		httpError(w, http.StatusUnprocessableEntity, "%s", msg)
		return
	}
	var diags []string
	for _, d := range staged.Diagnostics {
		diags = append(diags, d.Error())
	}
	if staged.Merged == nil {
		writeJSON(w, parseResponse{Diagnostics: diags})
		return
	}
	// staged.Root is "" for a bundle's files map and the directory for a
	// unit on disk: provenance names each file the way the document the
	// client already holds does, so a save can still tell the files apart.
	docJSON, err := ast.MarshalFileWithProvenance(staged.Merged, staged.Root)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "marshal error: %v", err)
		return
	}
	info := unitInfoOf(staged, staged.Main)
	info.Root = ""
	info.Revision = ""
	mainSource := ""
	if mf, ok := unitFile(staged, staged.Main); ok {
		mainSource = string(mf.Source)
	}
	writeJSON(w, parseResponse{
		Document:    json.RawMessage(docJSON),
		Diagnostics: diags,
		Unit:        info,
		Bindable:    !parseHasErrors(parser.Parse(staged.Main, mainSource).Diagnostics),
	})
}

// openedText is the current text of the workspace file a document was
// opened from — the BEFORE a fold is judged against (#1612). Empty, with
// no error, when the document has no workspace file to be about: an
// unbound buffer, or a path with a scheme (a cloud `botsource://` bot,
// whose writes are judged by the bot-source routes on the two texts they
// hold). A path that IS named and cannot be resolved is an error, never a
// silent "nothing to compare": that is how a guard stops firing quietly.
func (s *Server) openedText(w http.ResponseWriter, path string) ([]byte, bool) {
	if path == "" || strings.Contains(path, "://") {
		return nil, true
	}
	if !workflowfile.IsWorkflowFile(path) {
		// The same bound every other route that reads a bot applies. Without
		// it this answers the whole content of any workspace file, since a
		// refusal echoes the text it read.
		httpError(w, http.StatusBadRequest, "%s is not a workflow file: a document is rendered against a .bot", path)
		return nil, false
	}
	abs, err := s.safePath(path)
	if err != nil {
		httpError(w, http.StatusBadRequest, "%v", err)
		return nil, false
	}
	b, err := os.ReadFile(abs)
	if errors.Is(err, os.ErrNotExist) {
		// A path the buffer is headed for but that is not written yet.
		return nil, true
	}
	if err != nil {
		httpError(w, http.StatusInternalServerError, "read error: %v", err)
		return nil, false
	}
	return b, true
}

// stagedLoadFailure names what a per-file apply may not carry: a staged
// header the loader cannot follow — an `import` naming no file of the bot,
// a path outside its lib/ directory, a cycle — or one that pulls in a
// fragment the parser could only salvage. Both leave the merged document
// short of declarations a save would then drop, so the apply is refused
// instead, and the refusal names the file and the reason. Errors the
// stored unit already has pass through as diagnostics, and so do the ones
// an edit introduces in a file the bot already holds (a duplicate
// declaration, say): the save can carry both, and the diagnostics say what
// broke.
func stagedLoadFailure(stored, staged *unit.Unit, file string) string {
	prev := make(map[string]bool, len(stored.Diagnostics))
	for _, d := range stored.Diagnostics {
		if d.Severity == parser.SeverityError {
			prev[string(d.Code)+"\x00"+d.File+"\x00"+d.Message] = true
		}
	}
	held := make(map[string]bool, 2*len(stored.Files))
	for _, f := range stored.Files {
		held[f.Name] = true
		held[f.Rel] = true
	}
	var errs []string
	for _, d := range staged.Diagnostics {
		if d.Severity != parser.SeverityError {
			continue
		}
		if prev[string(d.Code)+"\x00"+d.File+"\x00"+d.Message] {
			continue
		}
		switch d.Code {
		case parser.DiagImportUnreadable, parser.DiagBadImportPath, parser.DiagImportCycle:
			errs = append(errs, d.Error())
		default:
			// A parse error in a file the apply's imports NEWLY brought in:
			// its declarations merge as what the parser could read of them.
			if !held[d.File] {
				errs = append(errs, d.Error())
			}
		}
	}
	if len(errs) == 0 {
		return ""
	}
	return fmt.Sprintf("%s changes this bot's `import` lines, and with them the unit does not load: %s. A fragment an import names must exist under the bot's lib/ directory and parse whole — fix the import, or change it where this bot's files live and reopen the bot", file, strings.Join(errs, "; "))
}

// unitFoldReason names a file of the unit whose multi-line form the
// FLATTENED text does not carry. It is a question about what is handed
// over, not about each file on its own: the merged program is rendered at
// the merged profile, so a unit can hold a file `iterion fmt` refuses and
// still flatten faithfully, and a unit of files it accepts can flatten
// folded. Asking per file answered both the wrong way round — it blocked a
// download that was byte-faithful and stayed silent on one that was not.
//
// Empty when nothing is lost, and empty too when the unit cannot be
// resolved, which is the one case the merged view had nothing to be about.
// It ANNOTATES a display; the writes are refused on their own, each
// against the file it is about.
func (s *Server) unitFoldReason(source string, files map[string]string, main, path string) string {
	ur, err := s.resolveUnitRequest(files, main, path)
	if err != nil || ur.unit.Merged == nil {
		return ""
	}
	for _, f := range ur.unit.Files {
		if line, size, folds := canon.Folds(f.Rel, string(f.Source), source); folds {
			return fmt.Sprintf("%s: the value at line %d is written over several lines and the merged program has no form for one — its %d characters come back as a single line (#1612)", f.Rel, line, size)
		}
	}
	return ""
}

// firstParseError is the first error-severity diagnostic of a parse, for a
// message that quotes what happened rather than characterising it.
func firstParseError(diags []parser.Diagnostic) string {
	for _, d := range diags {
		if d.Severity == parser.SeverityError {
			return d.Error()
		}
	}
	return "unreadable"
}
