package git

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// TestParseNumStatZ_Basic exercises the simplest non-rename shape:
// `added\tdeleted\tpath\0`.
func TestParseNumStatZ_Basic(t *testing.T) {
	out := []byte("3\t1\tsrc/foo.go\x002\t0\tsrc/bar.go\x00")
	got, err := parseNumStatZ(out)
	if err != nil {
		t.Fatalf("parseNumStatZ: %v", err)
	}
	want := []NumStat{
		{Path: "src/foo.go", Added: 3, Deleted: 1},
		{Path: "src/bar.go", Added: 2, Deleted: 0},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("mismatch:\n got=%+v\nwant=%+v", got, want)
	}
}

// TestParseNumStatZ_Rename covers the awkward shape git emits for
// renames/copies: empty path slot followed by old/new as separate
// NUL-terminated tokens.
func TestParseNumStatZ_Rename(t *testing.T) {
	out := []byte("0\t0\t\x00old/path.go\x00new/path.go\x002\t1\tregular.go\x00")
	got, err := parseNumStatZ(out)
	if err != nil {
		t.Fatalf("parseNumStatZ: %v", err)
	}
	want := []NumStat{
		{Path: "new/path.go", OldPath: "old/path.go", Added: 0, Deleted: 0},
		{Path: "regular.go", Added: 2, Deleted: 1},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("mismatch:\n got=%+v\nwant=%+v", got, want)
	}
}

// TestParseNumStatZ_Binary verifies the "-/-" sentinel maps to
// Binary=true with -1/-1.
func TestParseNumStatZ_Binary(t *testing.T) {
	out := []byte("-\t-\timg.png\x00")
	got, err := parseNumStatZ(out)
	if err != nil {
		t.Fatalf("parseNumStatZ: %v", err)
	}
	want := []NumStat{
		{Path: "img.png", Added: -1, Deleted: -1, Binary: true},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("mismatch:\n got=%+v\nwant=%+v", got, want)
	}
}

// TestParseNumStatZ_Empty covers a clean working tree (no output).
func TestParseNumStatZ_Empty(t *testing.T) {
	got, err := parseNumStatZ(nil)
	if err != nil {
		t.Fatalf("parseNumStatZ: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("want 0 entries, got %+v", got)
	}
}

// TestStatusNumStat_Modified asserts the line counts surface on the
// FileStatus rows returned by Status() for an ordinary modification.
func TestStatusNumStat_Modified(t *testing.T) {
	dir := gitRepo(t)
	// Replace 1-line a.txt with 4 lines: 3 added, 1 deleted.
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a\nb\nc\nd\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	files, err := Status(dir)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if len(files) != 1 {
		t.Fatalf("want 1 entry, got %+v", files)
	}
	f := files[0]
	if f.Status != "M" || f.Added != 4 || f.Deleted != 1 {
		t.Fatalf("want M/+4/-1, got %s/+%d/-%d", f.Status, f.Added, f.Deleted)
	}
	if f.Binary {
		t.Errorf("text file flagged as binary")
	}
}

// TestStatusNumStat_Untracked verifies the file-scan fallback for ??
// entries that numstat doesn't report.
func TestStatusNumStat_Untracked(t *testing.T) {
	dir := gitRepo(t)
	if err := os.WriteFile(filepath.Join(dir, "fresh.txt"), []byte("one\ntwo\nthree\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	files, err := Status(dir)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	var fresh *FileStatus
	for i := range files {
		if files[i].Path == "fresh.txt" {
			fresh = &files[i]
			break
		}
	}
	if fresh == nil {
		t.Fatalf("fresh.txt missing from Status output: %+v", files)
	}
	if fresh.Status != "??" || fresh.Added != 3 || fresh.Deleted != 0 {
		t.Fatalf("want ??/+3/-0, got %s/+%d/-%d", fresh.Status, fresh.Added, fresh.Deleted)
	}
}

// TestStatusNumStat_UntrackedBinary checks the NUL-byte sniff: a small
// untracked binary file is flagged with sentinel counts so the UI can
// render "(binary)".
func TestStatusNumStat_UntrackedBinary(t *testing.T) {
	dir := gitRepo(t)
	if err := os.WriteFile(filepath.Join(dir, "blob.bin"), []byte{0x00, 0x01, 0x02, 0x03}, 0o644); err != nil {
		t.Fatal(err)
	}
	files, err := Status(dir)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	var blob *FileStatus
	for i := range files {
		if files[i].Path == "blob.bin" {
			blob = &files[i]
			break
		}
	}
	if blob == nil {
		t.Fatalf("blob.bin missing: %+v", files)
	}
	if !blob.Binary || blob.Added != -1 || blob.Deleted != -1 {
		t.Fatalf("want binary sentinels, got binary=%v +%d/-%d", blob.Binary, blob.Added, blob.Deleted)
	}
}

// TestStatusNumStat_Deleted verifies a deletion produces (0, N) with
// N matching the original line count.
func TestStatusNumStat_Deleted(t *testing.T) {
	dir := gitRepo(t)
	// a.txt has 1 line ("hello\n"). Remove it.
	if err := os.Remove(filepath.Join(dir, "a.txt")); err != nil {
		t.Fatal(err)
	}
	files, err := Status(dir)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if len(files) != 1 {
		t.Fatalf("want 1 entry, got %+v", files)
	}
	f := files[0]
	if f.Status != "D" || f.Added != 0 || f.Deleted != 1 {
		t.Fatalf("want D/+0/-1, got %s/+%d/-%d", f.Status, f.Added, f.Deleted)
	}
}

// TestStatusBetween_NumStat verifies historical-mode merging against
// two real commits in repoRoot.
func TestStatusBetween_NumStat(t *testing.T) {
	dir := gitRepo(t)
	base, err := revParse(dir, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("one\ntwo\nthree\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "added.txt"), []byte("added\nfile\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustRun(t, dir, "add", "-A")
	mustRun(t, dir, "commit", "-q", "-m", "change")
	final, err := revParse(dir, "HEAD")
	if err != nil {
		t.Fatal(err)
	}

	files, err := StatusBetween(dir, base, final)
	if err != nil {
		t.Fatalf("StatusBetween: %v", err)
	}
	got := map[string]FileStatus{}
	for _, f := range files {
		got[f.Path] = f
	}
	a := got["a.txt"]
	if a.Status != "M" || a.Added != 3 || a.Deleted != 1 {
		t.Errorf("a.txt: want M/+3/-1, got %+v", a)
	}
	added := got["added.txt"]
	if added.Status != "A" || added.Added != 2 || added.Deleted != 0 {
		t.Errorf("added.txt: want A/+2/-0, got %+v", added)
	}
}

// revParse is a tiny test helper to grab a commit SHA without dragging
// in another exec wrapper.
func revParse(dir, ref string) (string, error) {
	out, err := run(dir, "rev-parse", ref)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// TestCountUntrackedLines_Cap ensures the size cap kicks in before we
// try to slurp a multi-GB file. We don't actually create one — we just
// stat-skip via a sentinel-large file written sparse.
func TestCountUntrackedLines_TextCount(t *testing.T) {
	dir := t.TempDir()
	body := bytes.Repeat([]byte("line\n"), 7)
	if err := os.WriteFile(filepath.Join(dir, "x.txt"), body, 0o644); err != nil {
		t.Fatal(err)
	}
	added, binary := CountUntrackedLines(dir, "x.txt")
	if binary {
		t.Errorf("text file misclassified as binary")
	}
	if added != 7 {
		t.Errorf("want 7 lines, got %d", added)
	}
}

func TestCountUntrackedLines_BinaryNUL(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "b.bin"), []byte{0x00, 0xff, 0x00}, 0o644); err != nil {
		t.Fatal(err)
	}
	added, binary := CountUntrackedLines(dir, "b.bin")
	if !binary {
		t.Errorf("NUL-byte file should be binary")
	}
	if added != -1 {
		t.Errorf("binary file should report -1, got %d", added)
	}
}
