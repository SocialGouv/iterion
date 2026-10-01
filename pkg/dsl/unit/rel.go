package unit

import (
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

// The root cut (#1934): every name a disk-loaded unit's diagnostics and
// errors can carry is Join(Root, Rel) — LoadDir, LoadDirWithMain and
// LoadDirStaged name no file otherwise, and a files map names by Rel
// already (Root "") — so cutting the root answers the position field and
// every message that cites a name verbatim, and no Name→Rel table maps a
// string the cut does not. The one diagnostic text that ever carried an
// absolute name from OUTSIDE the root — the confinement refusal's resolved
// path — names the import as written since #1918, loader-side.
//
// The cut is for the unit a CLIENT reads: a refusal's 4xx body IS the
// diagnostic, an open, an apply, a launch refusal or an example's answer
// carries them to the caller, and either forwarded as is discloses the
// server's directory layout (#1918, #1934, #1970). Only the per-request
// load handed to a client is rewritten; a unit the server keeps — what an
// operator's logs hold of it — keeps its absolute names.
//
// The filesystem-root refusal (#2047) is loud at WARNING severity — never an
// error, so HasErrors stays honest for a unit that loads and runs — and
// where it reaches follows from that: a boundary that serves the unit's
// diagnostics list (the studio's open and apply, a validate) carries E048
// with it; a refusal body built from the first ERROR diagnostic (a save
// that loads the pathological unit) computes the warning and drops it,
// accepted — that body still names the unit's files, and an error severity
// would refuse a unit the engine can run; the launch path serves an error
// TEXT, never the diagnostics list, and names the refusal from
// RootCutRefused instead (relLaunchError).

// relPrefix is the root as a name prefix, "" for a files map — and "" for a
// root that IS the filesystem root: Join("/", rel) is "/rel", so the prefix
// the cut would build is "//", a name nothing carries, and the cut would do
// nothing in silence exactly when the whole filesystem is under the root
// (#2047). Every unit method no-ops on it, and RelDiagnostics names it.
func (u *Unit) relPrefix() string {
	if u.Root == "" || rootIsFilesystemRoot(u.Root) {
		return ""
	}
	return u.Root + string(os.PathSeparator)
}

// rootIsFilesystemRoot reports whether root is the filesystem's own root —
// the pathological unit a LoadDir of a main at "/x.bot" reaches. Absolute
// first: filepath.Dir is also its own fixpoint for "." (a relative path,
// no root at all) and "" — neither is a filesystem root.
func rootIsFilesystemRoot(root string) bool {
	return root != "" && filepath.IsAbs(root) && filepath.Dir(root) == root
}

// RootCutRefused reports whether the unit's root is the filesystem root and
// every Rel* cut therefore no-ops (#2047). A boundary that serves an error
// TEXT rather than the unit's diagnostics — the launch path's
// relLaunchError, whose 400 body is `launch: %v` — cannot ride the E048
// warning RelDiagnostics appends, and names the refusal from this instead.
func (u *Unit) RootCutRefused() bool {
	return rootIsFilesystemRoot(u.Root)
}

// RelDiagnostics rewrites a disk-loaded unit's diagnostics to the unit's
// relative names, the way the loader's own messages already cite a file
// (relOf). A no-op for a files map, whose files are named by rel already.
// A unit rooted at the filesystem root gets NO cut — the prefix it would
// build matches no name — and the refusal is said, once, in a warning: the
// client keeps absolute names and is told why, rather than trusting a cut
// that did nothing (#2047).
func (u *Unit) RelDiagnostics() {
	if rootIsFilesystemRoot(u.Root) {
		for _, d := range u.Diagnostics {
			if d.Code == parser.DiagFilesystemRoot {
				return
			}
		}
		u.Diagnostics = append(u.Diagnostics, parser.Diagnostic{
			Code:     parser.DiagFilesystemRoot,
			Severity: parser.SeverityWarning,
			Message:  "the unit's root is the filesystem root: absolute file names are NOT rewritten for a client — the root cut has no name it can cut",
			File:     u.Main,
			Line:     1,
			Column:   1,
			Hint:     parser.HintFor(parser.DiagFilesystemRoot),
		})
		return
	}
	prefix := u.relPrefix()
	if prefix == "" {
		return
	}
	for i, d := range u.Diagnostics {
		d.File = filepath.ToSlash(strings.TrimPrefix(d.File, prefix))
		d.Message = strings.ReplaceAll(d.Message, prefix, "")
		u.Diagnostics[i] = d
	}
}

// RelText cuts the unit's root from a message — the cut RelDiagnostics
// applies to a diagnostic's Message, for the texts that ride no
// diagnostic: an include's stat error names the resolved path, absolute
// for a disk-loaded unit.
func (u *Unit) RelText(s string) string {
	return RelTextRoot(u.Root, s)
}

// RelTextRoot is the cut itself, for a boundary that holds the root
// rather than the unit (a bundle's directory, for its prompts' read
// errors). A root that is the filesystem root is refused by contract —
// root+separator is "//", a prefix no Join(root, rel) name carries, so
// cutting it would do nothing in silence (#2047): the text comes back
// unchanged, and a unit's RelDiagnostics is what names the case.
func RelTextRoot(root, s string) string {
	if root == "" || rootIsFilesystemRoot(root) {
		return s
	}
	return strings.ReplaceAll(s, root+string(os.PathSeparator), "")
}

// RelName is a file name as a client-boundary message cites it: the
// unit-relative name for a file under the root, the name unchanged for
// anything else (a synthetic name, a name outside the root).
func (u *Unit) RelName(name string) string {
	prefix := u.relPrefix()
	if prefix == "" {
		return name
	}
	return filepath.ToSlash(strings.TrimPrefix(name, prefix))
}

// RelError is err with the unit's root cut from its text — for the errors
// a launch, a publish or a render RETURNS rather than carries in
// u.Diagnostics. The original error, chain intact, comes back when the
// cut changes nothing (a files map, an error that names no file under the
// root); only a text the cut rewrites is re-wrapped.
func (u *Unit) RelError(err error) error {
	if err == nil {
		return nil
	}
	if cut := u.RelText(err.Error()); cut != err.Error() {
		return errors.New(cut)
	}
	return err
}
