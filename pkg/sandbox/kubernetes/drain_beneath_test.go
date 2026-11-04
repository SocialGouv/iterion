//go:build unix

package kubernetes

import (
	"archive/tar"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/SocialGouv/iterion/pkg/sandbox"
)

// hostEntry is one entry of a host dir, as a scenario lays it before the
// drain or expects it after: a file holding body, a link to target, a
// directory, or — gone — nothing at all.
type hostEntry struct {
	path, body, target string
	dir, gone          bool
}

func hostFile(path, body string) hostEntry   { return hostEntry{path: path, body: body} }
func hostLink(path, target string) hostEntry { return hostEntry{path: path, target: target} }
func hostDir(path string) hostEntry          { return hostEntry{path: path, dir: true} }
func hostGone(path string) hostEntry         { return hostEntry{path: path, gone: true} }

// lay writes e under root, its parents made.
func (e hostEntry) lay(t *testing.T, root string) {
	t.Helper()
	at := filepath.Join(root, filepath.FromSlash(e.path))
	if e.dir {
		if err := os.MkdirAll(at, 0o755); err != nil {
			t.Fatal(err)
		}
		return
	}
	if err := os.MkdirAll(filepath.Dir(at), 0o755); err != nil {
		t.Fatal(err)
	}
	if e.target != "" {
		if err := os.Symlink(e.target, at); err != nil {
			t.Fatal(err)
		}
		return
	}
	if err := os.WriteFile(at, []byte(e.body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// layScript is the shell script that lays entries under root — a writer
// beside the drain (archiveShimSeqLaying): files and directories only.
func layScript(t *testing.T, root string, entries []hostEntry) string {
	t.Helper()
	var b strings.Builder
	for _, e := range entries {
		at := filepath.Join(root, filepath.FromSlash(e.path))
		if strings.Contains(at+e.body, "'") || e.target != "" || e.gone {
			t.Fatalf("layScript lays files and dirs named and filled without a quote, not %+v", e)
		}
		if e.dir {
			b.WriteString("mkdir -p '" + at + "' || exit 1\n")
			continue
		}
		b.WriteString("mkdir -p '" + filepath.Dir(at) + "' && printf '%s' '" + e.body + "' > '" + at + "' || exit 1\n")
	}
	return b.String()
}

// foldDrainNames makes every name the drain hands the kernel resolve, for
// the test, as on a filesystem that folds case and preserves it — APFS's
// default, a casefold directory: to the entry its directory holds under the
// same name in any case, the exact spelling first, or as spelled when none
// stands there. No filesystem this suite runs on folds names.
func foldDrainNames(t *testing.T) {
	t.Helper()
	drainFold = func(d *drainDir, name string) string {
		fd, err := unix.Openat(d.fd, ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
		if err != nil {
			t.Errorf("fold %s: list its directory: %v", name, err)
			return name
		}
		f := os.NewFile(uintptr(fd), d.path)
		names, err := f.Readdirnames(-1)
		_ = f.Close()
		if err != nil {
			t.Errorf("fold %s: list its directory: %v", name, err)
			return name
		}
		if slices.Contains(names, name) {
			return name
		}
		sort.Strings(names)
		for _, n := range names {
			if strings.EqualFold(n, name) {
				return n
			}
		}
		return name
	}
	t.Cleanup(func() { drainFold = nil })
}

// check reports how what stands at e's path under root differs from e,
// read by its path as the host's own readers read it — "" when it does not.
func (e hostEntry) check(root string) string {
	at := filepath.Join(root, filepath.FromSlash(e.path))
	info, err := os.Lstat(at)
	switch {
	case e.gone:
		if !os.IsNotExist(err) {
			return "stands, want nothing there"
		}
		return ""
	case err != nil:
		return err.Error()
	case e.dir:
		if !info.IsDir() {
			return "is a " + info.Mode().Type().String() + ", want a directory"
		}
	case e.target != "":
		target, err := os.Readlink(at)
		if err != nil || target != e.target {
			return "links to " + target + ", want " + e.target
		}
	default:
		got, err := os.ReadFile(at)
		if err != nil || !info.Mode().IsRegular() || string(got) != e.body {
			return "holds " + string(got) + ", want the file " + e.body
		}
	}
	return ""
}

// podMember is one member of a crafted archive: a regular file holding
// body unless hdr says another kind.
type podMember struct {
	hdr  *tar.Header
	body string
}

func podFile(name, body string) podMember {
	return podMember{&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o644}, body}
}
func podDir(name string) podMember {
	return podMember{hdr: &tar.Header{Name: name, Typeflag: tar.TypeDir, Mode: 0o755}}
}
func podLink(name, target string) podMember {
	return podMember{hdr: &tar.Header{Name: name, Typeflag: tar.TypeSymlink, Linkname: target}}
}
func podHardlink(name, target string) podMember {
	return podMember{hdr: &tar.Header{Name: name, Typeflag: tar.TypeLink, Linkname: target}}
}

// deepName stands, in a scenario, for a member name whose path on the
// host is exactly the longest one the host's readers can name
// (drainPathMax, its NUL counted): long enough that a staged sibling
// named by its path would not be. deeperName is one byte longer, and
// deepTop the first directory both names cross.
const (
	deepName   = "DEEP"
	deeperName = "DEEPER"
	deepTop    = "DEEPTOP"
)

// deepMember is the name deepName stands for under root: directories of
// ordinary names down to a one-byte leaf, x.
func deepMember(t *testing.T, root string) string {
	t.Helper()
	length := drainPathMax - 1 - len(root) - 1
	// n directories of at most 200 bytes each, and the n-1 separators
	// between them, fill the length the leaf leaves.
	dirs := length - len("/x")
	n := (dirs + 1 + 200) / 201
	parts := make([]string, n)
	for i := range parts {
		size := (dirs - (n - 1)) / n
		if i < (dirs-(n-1))%n {
			size++
		}
		parts[i] = strings.Repeat("d", size)
	}
	name := strings.Join(parts, "/") + "/x"
	if len(name) != length || strings.Contains(name, "//") {
		t.Fatalf("the deep name is %d bytes, want %d — the scenario proves nothing at another length", len(name), length)
	}
	return name
}

// TestTheDrainLandsEachMemberWhereItsNameSpells: the drain writes every
// member beneath its host dir one component at a time and through no link
// (drainTree), so a member lands where its name spells or not at all, and
// an entry the drain did not make — a link above all — can decline a member
// (counted, named on the loss the run's event carries), never redirect it.
// One scenario per way a host entry or an earlier member once sent a write
// elsewhere, or judged it by a name it did not land at: each attempt but
// the last races a writer, and the last settles. A fold scenario runs on a
// filesystem that folds case (foldDrainNames); between lays host entries
// before the last attempt, a writer beside the drain.
func TestTheDrainLandsEachMemberWhereItsNameSpells(t *testing.T) {
	const pre = "the host held me before the drain"
	scratch := []podMember{podFile("./scratch.txt", "only the raced attempt carries me")}
	for _, tc := range []struct {
		name     string
		fold     bool
		pre      []hostEntry
		attempts [][]podMember
		between  []hostEntry
		declined []sandbox.ReadBackDeclinedMember
		want     []hostEntry
	}{
		// A member whose path crosses a link the archive laid lands nothing
		// behind it, whatever its kind — and the sweep of the settled
		// attempt's record keeps the archive's own d/.
		{
			name: "a dir through the archive's own link",
			attempts: [][]podMember{scratch,
				{podDir("./d/"), podLink("./l", "d"), podDir("./l/sub/")}},
			declined: []sandbox.ReadBackDeclinedMember{{Name: "l/sub", Why: sandbox.DeclinedUnsafeName}},
			want:     []hostEntry{hostDir("d"), hostLink("l", "d"), hostGone("d/sub"), hostGone("scratch.txt")},
		},
		{
			name: "a hardlink through the archive's own link",
			attempts: [][]podMember{scratch,
				{podDir("./d/"), podFile("./a.txt", "a"), podLink("./l", "d"), podHardlink("./l/h", "./a.txt")}},
			declined: []sandbox.ReadBackDeclinedMember{{Name: "l/h", Why: sandbox.DeclinedUnsafeName}},
			want:     []hostEntry{hostFile("a.txt", "a"), hostGone("d/h"), hostGone("scratch.txt")},
		},
		{
			name: "a symlink through the archive's own link",
			attempts: [][]podMember{scratch,
				{podDir("./d/"), podLink("./l", "d"), podLink("./l/s", "../d")}},
			declined: []sandbox.ReadBackDeclinedMember{{Name: "l/s", Why: sandbox.DeclinedUnsafeName}},
			want:     []hostEntry{hostGone("d/s"), hostGone("scratch.txt")},
		},
		// The archive's own link to a host tree: what stands behind it is
		// never reached, so never removed for a member that carries a file
		// or a link there.
		{
			name:     "a file through the archive's own link onto a host tree",
			pre:      []hostEntry{hostFile("d/sub/keep.md", pre)},
			attempts: [][]podMember{{podFile("./a.txt", "a"), podLink("./l", "d"), podFile("./l/sub", "the pod's"), podFile("./after.md", "# complete")}},
			declined: []sandbox.ReadBackDeclinedMember{{Name: "l/sub", Why: sandbox.DeclinedUnsafeName}},
			want:     []hostEntry{hostFile("d/sub/keep.md", pre), hostFile("after.md", "# complete")},
		},
		{
			name:     "a hardlink through the archive's own link onto a host tree",
			pre:      []hostEntry{hostFile("d/sub/keep.md", pre)},
			attempts: [][]podMember{{podFile("./a.txt", "a"), podLink("./l", "d"), podHardlink("./l/sub", "./a.txt"), podFile("./after.md", "# complete")}},
			declined: []sandbox.ReadBackDeclinedMember{{Name: "l/sub", Why: sandbox.DeclinedUnsafeName}},
			want:     []hostEntry{hostFile("d/sub/keep.md", pre), hostFile("after.md", "# complete")},
		},
		{
			name:     "a symlink through the archive's own link onto a host tree",
			pre:      []hostEntry{hostFile("d/sub/keep.md", pre)},
			attempts: [][]podMember{{podFile("./a.txt", "a"), podLink("./l", "d"), podLink("./l/sub", "../a.txt"), podFile("./after.md", "# complete")}},
			declined: []sandbox.ReadBackDeclinedMember{{Name: "l/sub", Why: sandbox.DeclinedUnsafeName}},
			want:     []hostEntry{hostFile("d/sub/keep.md", pre), hostFile("after.md", "# complete")},
		},
		// The reason a member through the archive's own link is declined is
		// that link, whatever stands behind it — a host file included.
		{
			name:     "a file through the archive's own link to the root, onto a host file",
			pre:      []hostEntry{hostFile("out", pre)},
			attempts: [][]podMember{{podLink("./l", "."), podFile("./l/out/x", "x"), podFile("./after.md", "# complete")}},
			declined: []sandbox.ReadBackDeclinedMember{{Name: "l/out/x", Why: sandbox.DeclinedUnsafeName}},
			want:     []hostEntry{hostFile("out", pre), hostFile("after.md", "# complete")},
		},
		{
			name:     "a link through the archive's own link onto a host file",
			pre:      []hostEntry{hostFile("d/f", pre)},
			attempts: [][]podMember{{podLink("./l", "d"), podLink("./l/f", "../after.md"), podFile("./after.md", "# complete")}},
			declined: []sandbox.ReadBackDeclinedMember{{Name: "l/f", Why: sandbox.DeclinedUnsafeName}},
			want:     []hostEntry{hostFile("d/f", pre)},
		},
		{
			name:     "a dir through the archive's own link onto a host file",
			pre:      []hostEntry{hostFile("d/f", pre)},
			attempts: [][]podMember{{podLink("./l", "d"), podDir("./l/f/"), podFile("./after.md", "# complete")}},
			declined: []sandbox.ReadBackDeclinedMember{{Name: "l/f", Why: sandbox.DeclinedUnsafeName}},
			want:     []hostEntry{hostFile("d/f", pre)},
		},
		// A hardlink's target is judged at its own name: one only a link
		// could reach is no file this archive landed.
		{
			name:     "a hardlink to a name only the archive's own link reached",
			attempts: [][]podMember{{podFile("./a.txt", "abc"), podDir("./d/"), podLink("./l", "d"), podFile("./l/x", "abc"), podHardlink("./h", "./d/x")}},
			declined: []sandbox.ReadBackDeclinedMember{
				{Name: "h", Why: sandbox.DeclinedHardlinkTarget},
				{Name: "l/x", Why: sandbox.DeclinedUnsafeName},
			},
			want: []hostEntry{hostGone("d/x"), hostGone("h")},
		},
		// A hardlink's copy is read from the entry standing at its target's
		// name, judged where it stands: a file this archive landed there
		// that a later member re-pointed or took is no file it landed.
		{
			name:     "a hardlink to a landed name a later link re-pointed at a host file",
			pre:      []hostEntry{hostFile("store/own.txt", pre)},
			attempts: [][]podMember{{podFile("./own.txt", "the pod's"), podLink("./own.txt", "store/own.txt"), podHardlink("./borrowed.txt", "./own.txt"), podFile("./after.md", "# complete")}},
			declined: []sandbox.ReadBackDeclinedMember{{Name: "borrowed.txt", Why: sandbox.DeclinedHardlinkTarget}},
			want:     []hostEntry{hostLink("own.txt", "store/own.txt"), hostFile("store/own.txt", pre), hostGone("borrowed.txt"), hostFile("after.md", "# complete")},
		},
		{
			name:     "a hardlink to a landed name a later dir took",
			attempts: [][]podMember{{podFile("./a.txt", "a"), podDir("./a.txt/"), podHardlink("./h", "./a.txt"), podFile("./after.md", "# complete")}},
			declined: []sandbox.ReadBackDeclinedMember{{Name: "h", Why: sandbox.DeclinedHardlinkTarget}},
			want:     []hostEntry{hostDir("a.txt"), hostGone("h"), hostFile("after.md", "# complete")},
		},
		{
			name:     "a hardlink to a landed name whose parent a later member took",
			attempts: [][]podMember{{podFile("./d/sub/a.txt", "a"), podFile("./d", "now a file"), podDir("./d/"), podHardlink("./h", "./d/sub/a.txt"), podFile("./after.md", "# complete")}},
			declined: []sandbox.ReadBackDeclinedMember{{Name: "h", Why: sandbox.DeclinedHardlinkTarget}},
			want:     []hostEntry{hostDir("d"), hostGone("d/sub"), hostGone("h"), hostFile("after.md", "# complete")},
		},
		// A tree the drain did not make — the host's from before the drain,
		// or written beside the drain since — is never removed for a member
		// that carries a file or a link there, however empty.
		{
			name:     "a file over an empty host dir",
			pre:      []hostEntry{hostDir("out")},
			attempts: [][]podMember{{podFile("./out", "the pod's"), podFile("./after.md", "# complete")}},
			declined: []sandbox.ReadBackDeclinedMember{{Name: "out", Why: sandbox.DeclinedPreDrainTree}},
			want:     []hostEntry{hostDir("out"), hostFile("after.md", "# complete")},
		},
		{
			name:     "a link over an empty host dir",
			pre:      []hostEntry{hostDir("out")},
			attempts: [][]podMember{{podLink("./out", "after.md"), podFile("./after.md", "# complete")}},
			declined: []sandbox.ReadBackDeclinedMember{{Name: "out", Why: sandbox.DeclinedPreDrainTree}},
			want:     []hostEntry{hostDir("out"), hostFile("after.md", "# complete")},
		},
		{
			name:     "a file over a raced dir a host writer filled between attempts",
			attempts: [][]podMember{{podFile("./d/sub/x", "raced")}, {podFile("./d", "the pod settled on a file"), podFile("./after.md", "# complete")}},
			between:  []hostEntry{hostFile("d/sub/host.txt", pre)},
			declined: []sandbox.ReadBackDeclinedMember{{Name: "d", Why: sandbox.DeclinedPreDrainTree}},
			want:     []hostEntry{hostFile("d/sub/host.txt", pre), hostGone("d/sub/x"), hostFile("after.md", "# complete")},
		},
		// The sweep removes a dir only a raced attempt made once it is
		// empty: one the settled attempt filled again without naming it
		// stands with what it holds.
		{
			name:     "a raced dir the settled archive fills without naming it",
			attempts: [][]podMember{{podFile("./d/x", "raced")}, {podFile("./d/y", "settled")}},
			want:     []hostEntry{hostFile("d/y", "settled"), hostGone("d/x")},
		},
		// On a filesystem that folds names, a name reaches an entry only
		// where its directory lists it under exactly that spelling: one
		// entry never answers to two keys, so the sweep, the landed record
		// and the declines all name the entry the walk meets.
		{
			name: "a folding filesystem lands every name as the archive spells it",
			fold: true,
			pre:  []hostEntry{hostFile("Report.md", pre), hostDir("Sub")},
			attempts: [][]podMember{scratch,
				{podFile("./Report.md", "# settled"), podDir("./Sub/"), podFile("./Sub/x", "x"), podHardlink("./Sub/h", "./Sub/x"), podLink("./latest", "Report.md")}},
			want: []hostEntry{hostFile("Report.md", "# settled"), hostFile("Sub/x", "x"), hostFile("Sub/h", "x"), hostLink("latest", "Report.md"), hostGone("scratch.txt")},
		},
		{
			name: "a dir the host's filesystem folds onto a raced attempt's spelling",
			fold: true,
			attempts: [][]podMember{{podDir("./Out/"), podFile("./Out/a.txt", "raced")},
				{podDir("./out/"), podFile("./out/a.txt", "settled")}},
			declined: []sandbox.ReadBackDeclinedMember{
				{Name: "out", Why: sandbox.DeclinedUnsafeName},
				{Name: "out/a.txt", Why: sandbox.DeclinedUnsafeName},
			},
			want: []hostEntry{hostGone("Out"), hostGone("out")},
		},
		{
			name:     "two names the host's filesystem folds onto one entry",
			fold:     true,
			attempts: [][]podMember{{podFile("./README.md", "# upper"), podFile("./readme.md", "# lower"), podFile("./after.md", "# complete")}},
			declined: []sandbox.ReadBackDeclinedMember{{Name: "readme.md", Why: sandbox.DeclinedUnsafeName}},
			want:     []hostEntry{hostFile("README.md", "# upper"), hostGone("readme.md"), hostFile("after.md", "# complete")},
		},
		{
			name:     "a path through a dir the host's filesystem folds",
			fold:     true,
			attempts: [][]podMember{{podDir("./Out/"), podFile("./out/a.txt", "# lower"), podFile("./after.md", "# complete")}},
			declined: []sandbox.ReadBackDeclinedMember{{Name: "out/a.txt", Why: sandbox.DeclinedUnsafeName}},
			want:     []hostEntry{hostDir("Out"), hostGone("Out/a.txt"), hostFile("after.md", "# complete")},
		},
		{
			name:     "a path through a file the host's filesystem folds",
			fold:     true,
			attempts: [][]podMember{{podFile("./Out", "a file"), podFile("./out/a.txt", "# lower"), podFile("./after.md", "# complete")}},
			declined: []sandbox.ReadBackDeclinedMember{{Name: "out/a.txt", Why: sandbox.DeclinedUnsafeName}},
			want:     []hostEntry{hostFile("Out", "a file"), hostFile("after.md", "# complete")},
		},
		{
			name: "a hardlink to a name the host's filesystem now folds onto another",
			fold: true,
			attempts: [][]podMember{{podFile("./d/a.txt", "a"), podFile("./d", "now a file"), podDir("./d/"),
				podFile("./d/A.TXT", "the other spelling"), podHardlink("./h", "./d/a.txt"), podFile("./after.md", "# complete")}},
			declined: []sandbox.ReadBackDeclinedMember{{Name: "h", Why: sandbox.DeclinedHardlinkTarget}},
			want:     []hostEntry{hostFile("d/A.TXT", "the other spelling"), hostGone("h"), hostFile("after.md", "# complete")},
		},
		// A member that carries none of the set's bytes never takes what
		// the host held.
		{
			name:     "a link over a host file",
			pre:      []hostEntry{hostFile("report.md", pre)},
			attempts: [][]podMember{{podFile("./target.md", "# the link's target"), podLink("./report.md", "target.md")}},
			declined: []sandbox.ReadBackDeclinedMember{{Name: "report.md", Why: sandbox.DeclinedPreDrainFile}},
			want:     []hostEntry{hostFile("report.md", pre), hostFile("target.md", "# the link's target")},
		},
		{
			name:     "a dir over a host dangling link",
			pre:      []hostEntry{hostLink("l", "missing")},
			attempts: [][]podMember{{podDir("./l/"), podFile("./l/f", "f"), podFile("./after.md", "# complete")}},
			declined: []sandbox.ReadBackDeclinedMember{
				{Name: "l", Why: sandbox.DeclinedPreDrainFile},
				{Name: "l/f", Why: sandbox.DeclinedPreDrainFile},
			},
			want: []hostEntry{hostLink("l", "missing"), hostFile("after.md", "# complete")},
		},
		{
			name:     "a link the kernel refuses, over a host file",
			pre:      []hostEntry{hostFile("empty", pre)},
			attempts: [][]podMember{{podLink("./empty", ""), podFile("./after.md", "# complete")}},
			declined: []sandbox.ReadBackDeclinedMember{{Name: "empty", Why: sandbox.DeclinedUnwritableLink}},
			want:     []hostEntry{hostFile("empty", pre), hostFile("after.md", "# complete")},
		},
		// A host link to a host dir, where the archive carries a dir: the dir
		// member is declined, and so is every member under it — none lands
		// through the link onto what the host or the pod holds behind it.
		{
			name: "a dir over a host link to a dir the archive also carries",
			pre:  []hostEntry{hostDir("d"), hostLink("l", "d")},
			attempts: [][]podMember{{
				podDir("./d/"), podFile("./d/report.md", "# the pod's d/report.md"),
				podDir("./l/"), podFile("./l/report.md", "# the pod's l/report.md"),
			}},
			declined: []sandbox.ReadBackDeclinedMember{
				{Name: "l", Why: sandbox.DeclinedPreDrainFile},
				{Name: "l/report.md", Why: sandbox.DeclinedPreDrainFile},
			},
			want: []hostEntry{hostFile("d/report.md", "# the pod's d/report.md"), hostLink("l", "d")},
		},
		{
			name:     "a dir over a host link to a host tree",
			pre:      []hostEntry{hostFile("d/f", pre), hostLink("l", "d")},
			attempts: [][]podMember{{podDir("./l/"), podFile("./l/f", "# the pod's l/f")}},
			declined: []sandbox.ReadBackDeclinedMember{
				{Name: "l", Why: sandbox.DeclinedPreDrainFile},
				{Name: "l/f", Why: sandbox.DeclinedPreDrainFile},
			},
			want: []hostEntry{hostFile("d/f", pre), hostLink("l", "d")},
		},
		// What the drain overwrote is the drain's from then on: a host file
		// a raced attempt replaced is taken by a settled link or dir, and
		// swept when the settled archive no longer carries it.
		{
			name:     "a settled link over a host file a raced attempt overwrote",
			pre:      []hostEntry{hostFile("report.md", pre)},
			attempts: [][]podMember{{podFile("./report.md", "raced bytes")}, {podFile("./target.md", "settled target"), podLink("./report.md", "target.md")}},
			want:     []hostEntry{hostLink("report.md", "target.md"), hostFile("target.md", "settled target")},
		},
		{
			name:     "a settled dir over a host file a raced attempt overwrote",
			pre:      []hostEntry{hostFile("out", pre)},
			attempts: [][]podMember{{podFile("./out", "raced bytes")}, {podDir("./out/"), podFile("./out/x", "x")}},
			want:     []hostEntry{hostDir("out"), hostFile("out/x", "x")},
		},
		{
			name:     "a host file a raced attempt overwrote, the settled archive no longer carries",
			pre:      []hostEntry{hostFile("old.md", pre)},
			attempts: [][]podMember{{podFile("./old.md", "raced bytes")}, {podFile("./report.md", "# complete")}},
			want:     []hostEntry{hostGone("old.md"), hostFile("report.md", "# complete")},
		},
		// The link already standing there, target for target, is the member
		// landed; a different one is the host's.
		{
			name:     "a link over the identical host link",
			pre:      []hostEntry{hostFile("v1.md", "v1"), hostLink("latest", "v1.md")},
			attempts: [][]podMember{{podFile("./v1.md", "v1"), podFile("./v2.md", "v2"), podLink("./latest", "v1.md")}},
			want:     []hostEntry{hostLink("latest", "v1.md")},
		},
		{
			name:     "a link over a different host link",
			pre:      []hostEntry{hostFile("v1.md", "v1"), hostLink("latest", "v1.md")},
			attempts: [][]podMember{{podFile("./v1.md", "v1"), podFile("./v2.md", "v2"), podLink("./latest", "v2.md")}},
			declined: []sandbox.ReadBackDeclinedMember{{Name: "latest", Why: sandbox.DeclinedPreDrainFile}},
			want:     []hostEntry{hostLink("latest", "v1.md"), hostFile("v2.md", "v2")},
		},
		// A member the host can name lands, however close to the limits: its
		// staged sibling is named from its directory, never by a path or a
		// name built on the member's.
		{
			name:     "a file at the longest path the host names",
			attempts: [][]podMember{{podFile("./"+deepName, "deep"), podFile("./after.md", "# complete")}},
			want:     []hostEntry{hostFile(deepName, "deep"), hostFile("after.md", "# complete")},
		},
		{
			name:     "a link at the longest path the host names",
			attempts: [][]podMember{{podLink("./"+deepName, "y"), podFile("./after.md", "# complete")}},
			want:     []hostEntry{hostLink(deepName, "y"), hostFile("after.md", "# complete")},
		},
		{
			name:     "a file one byte past the longest path the host names",
			attempts: [][]podMember{{podFile("./"+deeperName, "deeper"), podFile("./after.md", "# complete")}},
			declined: []sandbox.ReadBackDeclinedMember{{Name: deeperName, Why: sandbox.DeclinedUnsafeName}},
			want:     []hostEntry{hostGone(deepTop), hostFile("after.md", "# complete")},
		},
		{
			name:     "a file at the longest name the host holds",
			attempts: [][]podMember{{podFile("./"+strings.Repeat("n", 255), "long"), podFile("./after.md", "# complete")}},
			want:     []hostEntry{hostFile(strings.Repeat("n", 255), "long"), hostFile("after.md", "# complete")},
		},
		{
			name:     "a link at the longest name the host holds",
			attempts: [][]podMember{{podLink("./"+strings.Repeat("n", 255), "y"), podFile("./after.md", "# complete")}},
			want:     []hostEntry{hostLink(strings.Repeat("n", 255), "y"), hostFile("after.md", "# complete")},
		},
		{
			name:     "a name longer than the host holds",
			attempts: [][]podMember{{podFile("./d/"+strings.Repeat("n", 256), "long"), podFile("./after.md", "# complete")}},
			declined: []sandbox.ReadBackDeclinedMember{{Name: "d/" + strings.Repeat("n", 256), Why: sandbox.DeclinedUnsafeName}},
			want:     []hostEntry{hostFile("after.md", "# complete")},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fastExportRetries(t)
			hostDst := drainRootDir(t)
			deep := deepMember(t, hostDst)
			spell := strings.NewReplacer(deeperName, deep+"x", deepTop, deep[:strings.Index(deep, "/")], deepName, deep).Replace
			for _, e := range tc.pre {
				e.lay(t, hostDst)
			}
			archives := make([][]byte, len(tc.attempts))
			for i, attempt := range tc.attempts {
				hdrs := make([]*tar.Header, len(attempt))
				bodies := map[string]string{}
				for j, m := range attempt {
					hdr := *m.hdr
					hdr.Name = spell(hdr.Name)
					hdrs[j] = &hdr
					bodies[hdr.Name] = m.body
				}
				archives[i] = tarArchive(t, hdrs, bodies).Bytes()
			}
			kubectl, calls := archiveShimSeqLaying(t, archives, len(archives)-1, measuredRaceStderr, layScript(t, hostDst, tc.between))
			r := drainRun([]sandbox.ReadBackDir{{ContainerPath: "/iterion/artifact-files", HostDir: hostDst}}, kubectl)
			if tc.fold {
				foldDrainNames(t)
			}

			err := r.ExportRunFiles(context.Background())
			if n := calls(); n != len(archives) {
				t.Fatalf("the drain archived the pod %d times, want %d — the scenario never reached its settled attempt", n, len(archives))
			}
			if tc.declined == nil {
				if err != nil {
					t.Errorf("ExportRunFiles = %v, want the settled archive landed whole", err)
				}
			} else {
				want := make([]sandbox.ReadBackDeclinedMember, len(tc.declined))
				for i, m := range tc.declined {
					want[i] = sandbox.ReadBackDeclinedMember{Dir: "/iterion/artifact-files", Name: spell(m.Name), Why: m.Why}
				}
				wantDeclinedLoss(t, err, want...)
			}
			for _, e := range tc.want {
				e.path = spell(e.path)
				if diff := e.check(hostDst); diff != "" {
					t.Errorf("%s %s", strings.ReplaceAll(e.path, deep, "<deep>"), diff)
				}
			}
		})
	}
}

// TestTheDrainListsNoDirectoryItBuiltItself: an entry the drain created
// stands under the name it handed the kernel, where nothing stood under any
// spelling, so the drain knows that spelling without reading its directory
// back (drainDir.spells): a drain into an empty host dir lists none, its
// parents made, its files landed and read again for a hardlink's copy, its
// links renamed into place.
func TestTheDrainListsNoDirectoryItBuiltItself(t *testing.T) {
	hostDst := drainRootDir(t)
	tree := openTestTree(t, hostDst)
	x, err := drainRun(nil, "").extractDrain(tarArchive(t, []*tar.Header{
		{Name: "./a/", Typeflag: tar.TypeDir, Mode: 0o755},
		{Name: "./a/b/c/x", Typeflag: tar.TypeReg, Mode: 0o644},
		{Name: "./a/b/y", Typeflag: tar.TypeReg, Mode: 0o644},
		{Name: "./h", Typeflag: tar.TypeLink, Linkname: "./a/b/c/x"},
		{Name: "./l", Typeflag: tar.TypeSymlink, Linkname: "a/b/y"},
	}, map[string]string{"./a/b/c/x": "x", "./a/b/y": "y"}), tree, newDrainState(sandbox.ReadBackCapBytes))
	if err != nil || x.declined.Count() != 0 {
		t.Fatalf("extractDrain = %v, declined %+v, want every member landed", err, x.declined.List())
	}
	if got, readErr := os.ReadFile(filepath.Join(hostDst, "h")); readErr != nil || string(got) != "x" {
		t.Fatalf("h = %q (%v), want the hardlink's copy of a/b/c/x", got, readErr)
	}
	if n := tree.root.spelling.listings; n != 0 {
		t.Errorf("the drain listed %d directories, want none — every entry it reached is one it created", n)
	}
}

// TestTheDrainDirHoldsItsOwnPromises: the directory calls every judgment
// rests on, each asked directly of the entry the archive cannot lay there
// — a root replaced by a link since it was resolved, a fifo where a file is
// read, a directory that keeps its entries in no name order.
func TestTheDrainDirHoldsItsOwnPromises(t *testing.T) {
	t.Run("the root is opened where it was resolved, never through a link standing there since", func(t *testing.T) {
		dir := drainRootDir(t)
		if err := os.Mkdir(filepath.Join(dir, "real"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("real", filepath.Join(dir, "link")); err != nil {
			t.Fatal(err)
		}
		if d, err := openDrainRoot(filepath.Join(dir, "link")); err == nil {
			d.close()
			t.Fatalf("openDrainRoot opened a link, want it refused — the writes would land where the link points now")
		}
		d, err := openDrainRoot(filepath.Join(dir, "real"))
		if err != nil {
			t.Fatalf("openDrainRoot(real) = %v, want the directory opened", err)
		}
		d.close()
	})
	t.Run("a fifo where a file is read is no file, and the read never waits for a writer", func(t *testing.T) {
		dir := drainRootDir(t)
		if err := syscall.Mkfifo(filepath.Join(dir, "fifo"), 0o644); err != nil {
			t.Skipf("no fifo on this filesystem: %v", err)
		}
		d, err := openDrainRoot(dir)
		if err != nil {
			t.Fatal(err)
		}
		defer d.close()
		if f, err := d.openFile("fifo"); !errors.Is(err, errNotDrainFile) {
			if f != nil {
				_ = f.Close()
			}
			t.Errorf("openFile(fifo) = %v, want errNotDrainFile", err)
		}
	})
	t.Run("a listing is in name order whatever order the directory keeps", func(t *testing.T) {
		dir := drainRootDir(t)
		// Created in no order a directory could keep as name order —
		// neither its own nor the reverse.
		created := []string{"m", "c", "x", "a", "q", "f", "z", "b", "k", "t", "e", "w", "h", "p", "d"}
		for _, name := range created {
			if err := os.WriteFile(filepath.Join(dir, name), nil, 0o644); err != nil {
				t.Fatal(err)
			}
		}
		d, err := openDrainRoot(dir)
		if err != nil {
			t.Fatal(err)
		}
		defer d.close()
		got, err := d.names()
		if err != nil {
			t.Fatal(err)
		}
		want := slices.Sorted(slices.Values(created))
		if !slices.Equal(got, want) {
			t.Errorf("names() = %v, want %v — a walk meets entries in one order, whatever the directory keeps", got, want)
		}
	})
}
