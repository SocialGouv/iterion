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
// writer cannot reproduce, is still readable.
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
	f, ok := unitFile(ur.unit, req.File)
	if !ok {
		httpError(w, http.StatusUnprocessableEntity, "%q is not a file of this bot (%s)", req.File, strings.Join(unitRels(ur.unit), ", "))
		return
	}
	parts, err := splitByProvenance(doc, ur.unit)
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
			writeJSON(w, unparseResponse{Source: string(f.Source), Refused: canonReason(err)})
			return
		}
		httpError(w, http.StatusUnprocessableEntity, "%s cannot be rendered as .bot source without changing it: %v", f.Rel, err)
		return
	}
	writeJSON(w, unparseResponse{Source: text})
}

// parseUnitWithFile re-parses a unit with ONE file replaced by the text the
// Source view's picker holds: the author edits a real file, and the merged
// document the canvas and the save both work from is rebuilt from it.
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
	cur, _ := unitFile(ur.unit, req.File) // present: the guard above returned otherwise
	if !sameImports(cur.AST, pr.File) {
		httpError(w, http.StatusUnprocessableEntity, "%s changes this bot's `import` lines, which the per-file editor cannot apply: a save writes each declaration back to the file it came from and reads the imports from the files themselves. Removing one here would leave the import in place and empty the fragment; adding one would make every later save refuse. Edit the import on disk (or in the bundle's files) and reopen the bot.", req.File)
		return
	}
	// Same reason as the imports: splitByProvenance rebuilds every part with
	// the STORED file's profile, so a `dsl:` line changed here is never
	// written — while the declarations WOULD have been read under the new
	// one, which is how a quoted value changes meaning between the two.
	if cur.AST != nil && pr.File != nil && cur.AST.EffectiveProfile() != pr.File.EffectiveProfile() {
		httpError(w, http.StatusUnprocessableEntity, "%s changes this bot's `dsl:` profile, which the per-file editor cannot apply: a save writes each declaration back under the profile the file already has, so the change would be dropped and the values read under the other profile. Change it where this bot's files live and reopen the bot.", req.File)
		return
	}
	staged := ur.stage(req.File, req.Source)
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

// sameImports reports whether two readings of one file carry the same
// import lines, in the same order. The per-file editor may not change
// them: splitByProvenance rebuilds every file's Imports — and the unit's
// membership — from the files on disk, so a changed import is not written
// but silently dropped, taking the fragment it names with it.
func sameImports(a, b *ast.File) bool {
	var left, right []string
	if a != nil {
		for _, im := range a.Imports {
			left = append(left, im.Path)
		}
	}
	if b != nil {
		for _, im := range b.Imports {
			right = append(right, im.Path)
		}
	}
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
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
