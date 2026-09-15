package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"

	"github.com/SocialGouv/iterion/pkg/dsl/ast"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
	"github.com/SocialGouv/iterion/pkg/dsl/unit"
	"github.com/SocialGouv/iterion/pkg/dsl/unparse"
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
}

func unitInfoOf(u *unit.Unit, reqPath string) *unitInfo {
	info := &unitInfo{Root: filepath.ToSlash(filepath.Dir(reqPath)), Main: u.Main, Revision: u.Digest}
	for _, f := range u.Files {
		fi := unitFileInfo{Rel: f.Rel}
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

// hasProvenance reports whether any declaration, block or comment of the
// document names a file: what MarshalFileWithProvenance writes and the
// transport never does.
func hasProvenance(f *ast.File) bool {
	found := false
	walkCarriers(f, func(span ast.Span) { found = found || span.Start.File != "" })
	return found
}

// walkCarriers visits the span of every top-level declaration, comment,
// keyed block and block entry of a document — every carrier of provenance.
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

// entriesOf is the one slice field of a keyed block (Fields, Entries).
func entriesOf(block reflect.Value) reflect.Value {
	for i := 0; i < block.NumField(); i++ {
		if block.Field(i).Kind() == reflect.Slice {
			return block.Field(i)
		}
	}
	return reflect.Value{}
}

// splitByProvenance takes a document of a merged unit apart, file by file:
// each declaration, comment and block entry goes to the file its
// provenance names — to the main when it names none, which is where the
// editor puts a new declaration — on a skeleton that keeps each file's own
// header (its profile and its import lines, which the editor does not
// edit). A provenance naming a file the unit does not have is refused.
func splitByProvenance(doc *ast.File, u *unit.Unit) (map[string]*ast.File, error) {
	parts := make(map[string]*ast.File, len(u.Files))
	for _, f := range u.Files {
		skel := &ast.File{}
		if f.AST != nil {
			skel.Profile = f.AST.Profile
			skel.Imports = f.AST.Imports
		}
		parts[f.Rel] = skel
	}
	owner := func(span ast.Span) (*ast.File, error) {
		if span.Start.File == "" {
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
				p, err := owner(span)
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
			// Each entry goes where it was, and a block exists in a file
			// because an entry of it does. A block emptied of every entry
			// keeps its header where the header was written — what the
			// writer puts on a single file too — never a bare header in
			// a file whose entries all live elsewhere.
			entries := entriesOf(fv.Elem())
			if !entries.IsValid() || entries.Len() == 0 {
				blockSpan, _ := spanOf(fv)
				p, err := owner(blockSpan)
				if err != nil {
					return nil, err
				}
				ensureBlock(reflect.ValueOf(p).Elem().Field(i), fv.Type())
				continue
			}
			for j := 0; j < entries.Len(); j++ {
				el := entries.Index(j)
				span, _ := spanOf(el)
				q, err := owner(span)
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
// each declaration to the file its provenance names, only the files whose
// program changed rewritten — byte for byte untouched otherwise — every
// main of the directory that imports a rewritten fragment checked to still
// compile, and the writes published as one journaled transaction under
// the files' locks, after the revision the document was opened at is
// found unchanged on disk.
func (s *Server) saveUnit(w http.ResponseWriter, r *http.Request, req saveFileRequest, absPath string, doc *ast.File, current []byte) {
	if req.CreateOnly {
		httpError(w, http.StatusUnprocessableEntity, "a bot in several files cannot be saved as a new file: save it in place, or write the flattened program by hand")
		return
	}
	u := unit.LoadDirWithMain(absPath, absPath, current)
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
	parts, err := splitByProvenance(doc, u)
	if err != nil {
		httpError(w, http.StatusUnprocessableEntity, "%v", err)
		return
	}
	var staged []stagedUnitFile
	stagedText := map[string][]byte{}
	for _, f := range u.Files {
		part := parts[f.Rel]
		if f.AST != nil && sameProgram(part, f.AST) {
			continue
		}
		text := unparse.Unparse(part)
		if err := unparse.Verify(part, text); err != nil {
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
	if err := siblingImportersStillCompile(u, stagedText); err != nil {
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
	Path              string          `json:"path"`
	ConfirmedDiskPath string          `json:"confirmed_disk_path,omitempty"`
	Unit              *unitInfo       `json:"unit"`
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
	writeJSON(w, parseResponse{Document: json.RawMessage(docJSON), Diagnostics: diags, Unit: info})
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
	parts, err := splitByProvenance(doc, u)
	if err != nil {
		httpError(w, http.StatusUnprocessableEntity, "%v", err)
		return
	}
	out := map[string]string{}
	for _, f := range u.Files {
		part := parts[f.Rel]
		if f.AST != nil && sameProgram(part, f.AST) {
			continue
		}
		text := unparse.Unparse(part)
		if err := unparse.Verify(part, text); err != nil {
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
