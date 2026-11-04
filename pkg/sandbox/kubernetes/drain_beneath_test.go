//go:build unix

package kubernetes

import (
	"archive/tar"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
// the last races a writer, and the last settles.
func TestTheDrainLandsEachMemberWhereItsNameSpells(t *testing.T) {
	const pre = "the host held me before the drain"
	scratch := []podMember{podFile("./scratch.txt", "only the raced attempt carries me")}
	for _, tc := range []struct {
		name     string
		pre      []hostEntry
		attempts [][]podMember
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
			kubectl, calls := archiveShimSeq(t, archives, len(archives)-1, measuredRaceStderr)
			r := drainRun([]sandbox.ReadBackDir{{ContainerPath: "/iterion/artifact-files", HostDir: hostDst}}, kubectl)

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
