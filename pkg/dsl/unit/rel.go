package unit

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
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

// relPrefix is the root as a name prefix, "" for a files map.
func (u *Unit) relPrefix() string {
	if u.Root == "" {
		return ""
	}
	return u.Root + string(os.PathSeparator)
}

// RelDiagnostics rewrites a disk-loaded unit's diagnostics to the unit's
// relative names, the way the loader's own messages already cite a file
// (relOf). A no-op for a files map, whose files are named by rel already.
func (u *Unit) RelDiagnostics() {
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
// errors).
func RelTextRoot(root, s string) string {
	if root == "" {
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
