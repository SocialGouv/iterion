package bundle

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// tarEntry is one file or directory to include in a fixture archive.
// Type defaults to a regular file when Body is non-nil.
type tarEntry struct {
	Name     string
	Mode     int64
	Body     []byte
	Typeflag byte // 0 = auto (Reg/Dir based on Name suffix)
	Linkname string
}

// buildBotz writes a `.botz` tar.gz archive to dest with the given entries.
// Used to construct test fixtures; not part of the production bundle API.
func buildBotz(t *testing.T, dest string, entries []tarEntry) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	f, err := os.Create(dest)
	if err != nil {
		t.Fatalf("create %s: %v", dest, err)
	}
	defer f.Close()
	gz := gzip.NewWriter(f)
	defer gz.Close()
	tw := tar.NewWriter(gz)
	defer tw.Close()

	for _, e := range entries {
		typ := e.Typeflag
		mode := e.Mode
		if typ == 0 {
			if len(e.Body) == 0 && (len(e.Name) == 0 || e.Name[len(e.Name)-1] == '/') {
				typ = tar.TypeDir
				if mode == 0 {
					mode = 0o755
				}
			} else {
				typ = tar.TypeReg
				if mode == 0 {
					mode = 0o644
				}
			}
		}
		hdr := &tar.Header{
			Name:     e.Name,
			Mode:     mode,
			Size:     int64(len(e.Body)),
			Typeflag: typ,
			Linkname: e.Linkname,
		}
		if typ != tar.TypeReg && typ != tar.TypeRegA {
			hdr.Size = 0
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("write header %s: %v", e.Name, err)
		}
		if hdr.Size > 0 {
			if _, err := tw.Write(e.Body); err != nil {
				t.Fatalf("write body %s: %v", e.Name, err)
			}
		}
	}
}

const minimalBotIter = `
schema empty:
  ok: bool

prompt sys:
  System.

prompt usr:
  User.

agent runner:
  model: "test-model"
  input: empty
  output: empty
  system: sys
  user: usr

workflow demo:
  entry: runner
  runner -> done
`

// fixtureMinimalBundle builds the smallest valid `.botz` (just `bot.iter`).
func fixtureMinimalBundle(t *testing.T) string {
	t.Helper()
	dest := filepath.Join(t.TempDir(), "minimal.botz")
	buildBotz(t, dest, []tarEntry{
		{Name: "bot.iter", Body: []byte(minimalBotIter)},
	})
	return dest
}

// fixtureBundleWithSkillsPrompts builds a bundle with skills/, prompts/,
// and a manifest.yaml.
func fixtureBundleWithSkillsPrompts(t *testing.T) string {
	t.Helper()
	dest := filepath.Join(t.TempDir(), "with-skills.botz")
	buildBotz(t, dest, []tarEntry{
		{Name: "bot.iter", Body: []byte(minimalBotIter)},
		{Name: "manifest.yaml", Body: []byte("name: test-bundle\nversion: 0.1.0\nschema_version: 1\n")},
		{Name: "skills/", Typeflag: tar.TypeDir},
		{Name: "skills/probe.md", Body: []byte("# Probe skill\n\nReturn the string 'ok'.\n")},
		{Name: "prompts/", Typeflag: tar.TypeDir},
		{Name: "prompts/helper.md", Body: []byte("Helper prompt body.\n")},
	})
	return dest
}

func fixturePathTraversal(t *testing.T) string {
	t.Helper()
	dest := filepath.Join(t.TempDir(), "traversal.botz")
	buildBotz(t, dest, []tarEntry{
		{Name: "bot.iter", Body: []byte(minimalBotIter)},
		{Name: "../evil.txt", Body: []byte("pwned")},
	})
	return dest
}

func fixtureAbsolutePath(t *testing.T) string {
	t.Helper()
	dest := filepath.Join(t.TempDir(), "abs.botz")
	buildBotz(t, dest, []tarEntry{
		{Name: "bot.iter", Body: []byte(minimalBotIter)},
		{Name: "/etc/abs.txt", Body: []byte("nope")},
	})
	return dest
}

func fixtureSymlinkEscape(t *testing.T) string {
	t.Helper()
	dest := filepath.Join(t.TempDir(), "symlink.botz")
	buildBotz(t, dest, []tarEntry{
		{Name: "bot.iter", Body: []byte(minimalBotIter)},
		{Name: "link", Typeflag: tar.TypeSymlink, Linkname: "../../escape"},
	})
	return dest
}

func fixtureOversize(t *testing.T) string {
	t.Helper()
	dest := filepath.Join(t.TempDir(), "oversize.botz")
	// We rely on ITERION_BUNDLE_MAX_BYTES being lowered for the test.
	big := bytes.Repeat([]byte{'a'}, 1024)
	buildBotz(t, dest, []tarEntry{
		{Name: "bot.iter", Body: []byte(minimalBotIter)},
		{Name: "big.bin", Body: big},
	})
	return dest
}

// helper: assert a substring is present in an error.
func errContains(t *testing.T, err error, substr string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected error containing %q, got nil", substr)
	}
	if want, got := substr, err.Error(); !contains(got, want) {
		t.Fatalf("error %q does not contain %q", got, want)
	}
}

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && stringIndex(s, sub) >= 0)
}

// stringIndex avoids importing strings just for this one helper.
func stringIndex(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// _ = used to keep fmt import alive in build configurations where one of
// the helpers above gets dead-code-eliminated.
var _ = fmt.Sprintf
