//go:build windows

package store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExtendedLengthPath(t *testing.T) {
	deep := strings.Repeat(`d\`, 130) + "leaf"
	long := `C:\` + deep
	for _, tc := range []struct{ in, want string }{
		{`C:\short\dir`, `C:\short\dir`},
		{long, `\\?\` + long},
		{`C:/` + strings.ReplaceAll(deep, `\`, "/"), `\\?\` + long},
		{`\\server\share\` + deep, `\\?\UNC\server\share\` + deep},
		// Already extended or a device path: os passes these through, so must we.
		{`\\?\` + long, `\\?\` + long},
		{`//?/` + long, `//?/` + long},
		{`\??\` + long, `\??\` + long},
		{`\\.\` + long, `\\.\` + long},
		{`//./` + long, `//./` + long},
	} {
		if got := extendedLengthPath(tc.in); got != tc.want {
			t.Errorf("extendedLengthPath(%.40q…) = %.60q…, want %.60q…", tc.in, got, tc.want)
		}
	}
}

// A short relative name is long once resolved against a deep working
// directory: os measures the absolute path, and so must fsyncDir.
func TestExtendedLengthPath_MeasuresARelativePathFromTheWorkingDirectory(t *testing.T) {
	cwd := t.TempDir()
	for len(cwd) < 240 {
		cwd = filepath.Join(cwd, strings.Repeat("c", 40))
	}
	if err := os.MkdirAll(cwd, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Chdir(cwd)
	got := extendedLengthPath("rel")
	want := `\\?\` + filepath.Join(cwd, "rel")
	if got != want {
		t.Fatalf("extendedLengthPath(%q) under a %d-char cwd = %.60q…, want %.60q…", "rel", len(cwd), got, want)
	}
}

// A directory deeper than MAX_PATH syncs like any other: the os package
// writes into it through \\?\ paths, so fsyncDir must reach it too. (On a
// host with long paths enabled this passes either way; the table above is
// what pins the prefix.)
func TestSyncDir_SyncsADirectoryDeeperThanMaxPath(t *testing.T) {
	dir := t.TempDir()
	for len(dir) < 300 {
		dir = filepath.Join(dir, strings.Repeat("d", 40))
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := WriteFileAtomic(filepath.Join(dir, "run.json"), []byte("{}"), 0o600); err != nil {
		t.Fatalf("WriteFileAtomic under a %d-char directory = %v, want nil", len(dir), err)
	}
}
