// Package bots exposes the productised bot recipes shipped with iterion
// (the named team — Featurly, Billy, Willy, …) as an embed.FS so the
// binary can run them by basename from any working directory.
//
// Lookup order at launch time (see pkg/server.resolveWorkflowPath):
//  1. resolve the requested path against the server WorkDir;
//  2. on miss, if the path is a bare basename matching an embedded
//     recipe, materialise the embedded bot into the run store
//     and use that path.
//
// An embedded bot is its main.bot and, for a bot in several files, the
// lib/ fragments its main imports: Materialize writes them together, so
// a materialised main compiles to the program the .bot unit is in the
// tree. Nothing else of the bundle travels — not the manifest (the engine
// floor it would declare is met by construction, the binary that carries
// the bot being the one that reads it), not the skills its prompts name,
// not the .md design journals — to keep the binary slim. Bundle directories (`<name>/main.bot`
// + manifest + skills + prompts + attachments) are NOT embedded either —
// they have to be loaded by explicit path (`iterion run bots/<name>/`
// or against the packed `<name>.botz`); embedding them would lose
// the adjacent skills/prompts/attachments resources that make a
// bundle a bundle, plus encoding the whole tree as embedded bytes
// inflates the binary far more than a single .bot does.
package bots

import (
	"bytes"
	"embed"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/SocialGouv/iterion/pkg/dsl/unit"
)

// The three productised bots (feature_dev, whole_improve_loop,
// branch_improve_loop) ship embedded: `<name>/main.bot`, plus `<name>/lib`
// for a bot in several files (feature-dev). Their manifest.yaml +
// README.md are stripped to keep the binary slim. Larger bundles
// (whats-next, docs-refresh, sec-audit-*, secured-renovacy, review-pr)
// carry skills/prompts/attachments alongside main.bot and are
// deliberately NOT embedded; they have to be loaded by explicit path
// (`iterion run bots/<name>/` or against the packed `.botz`).
//
//go:embed feature-dev/main.bot feature-dev/lib whole-improve-loop/main.bot branch-improve-loop/main.bot
var Files embed.FS

// List returns the relative paths (within the embed FS) of all embedded
// workflow recipes, sorted alphabetically. A fragment — a file under a
// bot's lib/ directory — is a piece of a recipe, not one, and is not
// listed.
func List() []string {
	var out []string
	_ = fs.WalkDir(Files, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if d.Name() == unit.FragmentDir && strings.Contains(p, "/") {
				return fs.SkipDir
			}
			return nil
		}
		out = append(out, p)
		return nil
	})
	sort.Strings(out)
	return out
}

// botFiles lists the embedded files of the bot that owns name — every
// file under the bot's directory, in the order they are written — with
// the bot's directory and name cleaned. name must be an embedded FILE: a
// miss, or a directory, is fs.ErrNotExist.
func botFiles(name string) (dir, cleaned string, paths []string, err error) {
	cleaned = path.Clean(filepath.ToSlash(name))
	info, err := fs.Stat(Files, cleaned)
	if err != nil {
		return "", "", nil, err
	}
	if info.IsDir() {
		return "", "", nil, &fs.PathError{Op: "open", Path: cleaned, Err: fs.ErrNotExist}
	}
	dir = botDirOf(cleaned)
	var all []string
	err = fs.WalkDir(Files, dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			all = append(all, p)
		}
		return nil
	})
	if err != nil {
		return "", "", nil, err
	}
	return dir, cleaned, orderForWrite(dir, all), nil
}

// botDirOf is the directory of the bot that owns the embedded file at the
// cleaned slash path: the parent of the nearest lib/ ancestor when the
// file is a fragment, the file's own directory otherwise — the unit
// loader's rule, which reads a fragment at any depth under lib/. A
// top-level directory named lib is a bot of its own.
func botDirOf(cleaned string) string {
	dir := path.Dir(cleaned)
	for d := dir; strings.Contains(d, "/"); d = path.Dir(d) {
		if path.Base(d) == unit.FragmentDir {
			return path.Dir(d)
		}
	}
	return dir
}

// orderForWrite is paths — the files of the bot at dir — in the order they
// are written: the fragments (under dir/lib/, at any depth) first, then
// the mains, each group sorted, so a main on disk never wants for a
// fragment, whatever the names sort like.
func orderForWrite(dir string, paths []string) []string {
	var fragments, mains []string
	prefix := dir + "/" + unit.FragmentDir + "/"
	for _, p := range paths {
		if strings.HasPrefix(p, prefix) {
			fragments = append(fragments, p)
		} else {
			mains = append(mains, p)
		}
	}
	sort.Strings(fragments)
	sort.Strings(mains)
	return append(fragments, mains...)
}

// Sources returns the embedded bot that owns name as a files map keyed by
// slash path relative to the bot's directory ("main.bot", "lib/nodes.bot"),
// with name's own key — the shape unit.LoadMap reads. ok is false when
// name is not an embedded file.
func Sources(name string) (files map[string]string, main string, ok bool) {
	dir, cleaned, paths, err := botFiles(name)
	if err != nil {
		return nil, "", false
	}
	files = make(map[string]string, len(paths))
	for _, p := range paths {
		data, err := Files.ReadFile(p)
		if err != nil {
			return nil, "", false
		}
		files[strings.TrimPrefix(p, dir+"/")] = string(data)
	}
	return files, strings.TrimPrefix(cleaned, dir+"/"), true
}

// Materialize writes the bot that owns name — every embedded file of it,
// so a bot in several files lands with the fragments its main imports —
// under root, and returns the on-disk path of name. A file whose cached
// bytes already match is left alone; the fragments are written before the
// mains, whichever file name is, so a main on disk never wants for one.
// name must be an embedded FILE: a miss, or a directory, is
// fs.ErrNotExist, and nothing is written.
func Materialize(root, name string) (string, error) {
	_, cleaned, paths, err := botFiles(name)
	if err != nil {
		return "", err
	}
	for _, p := range paths {
		data, err := Files.ReadFile(p)
		if err != nil {
			return "", err
		}
		if err := writeIfChanged(filepath.Join(root, filepath.FromSlash(p)), data); err != nil {
			return "", err
		}
	}
	return filepath.Join(root, filepath.FromSlash(cleaned)), nil
}

// writeIfChanged writes data at dst unless the file already holds it, and
// publishes it atomically — a temp file beside dst, renamed into place —
// so a reader opening dst while another materialisation of the same bot
// rewrites it sees the previous bytes or the new ones, never a truncated
// file. Bytes are compared, not lengths: a same-length edit would
// otherwise be served from the stale copy forever.
func writeIfChanged(dst string, data []byte) error {
	if existing, err := os.ReadFile(dst); err == nil && bytes.Equal(existing, data) {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), "."+filepath.Base(dst)+".*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), dst)
}
