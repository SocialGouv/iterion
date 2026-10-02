package kubernetes

import (
	"archive/tar"
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/sandbox"
)

// hostileArchive builds a tar stream member by member, so a test can send the
// names a cooperative archiver would never produce. The archiver runs in the
// sandbox, on an image the workflow chooses: these are the names the host has
// to answer for.
func hostileArchive(t *testing.T, members []tar.Header, bodies map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, hdr := range members {
		h := hdr
		if h.Typeflag == tar.TypeReg {
			h.Size = int64(len(bodies[h.Name]))
		}
		if h.Mode == 0 {
			h.Mode = 0o644
		}
		h.ModTime = time.Unix(1700000000, 0)
		if err := tw.WriteHeader(&h); err != nil {
			t.Fatal(err)
		}
		if h.Typeflag == tar.TypeReg {
			if _, err := tw.Write([]byte(bodies[h.Name])); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// hostileHost lays out a host clone with the three files the export must
// never overwrite, and returns it with a Run wired to extract into it.
func hostileHost(t *testing.T) (string, *Run) {
	t.Helper()
	host := t.TempDir()
	if err := os.MkdirAll(filepath.Join(host, ".git", "hooks"), 0o755); err != nil {
		t.Fatal(err)
	}
	for rel, body := range map[string]string{
		".git/config":              "HOST-CONFIG",
		".git/iterion-credentials": "HOST-CREDENTIAL",
	} {
		if err := os.WriteFile(filepath.Join(host, filepath.FromSlash(rel)), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return host, &Run{driver: &Driver{logger: iterlog.Nop()}, info: sandbox.RunInfo{WorkspacePath: host}}
}

// assertHostUntouched fails when the export reached one of the three paths
// the host keeps for itself.
func assertHostUntouched(t *testing.T, host, what string) {
	t.Helper()
	for rel, want := range map[string]string{
		".git/config":              "HOST-CONFIG",
		".git/iterion-credentials": "HOST-CREDENTIAL",
	} {
		got, err := os.ReadFile(filepath.Join(host, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatalf("%s: %s is gone: %v", what, rel, err)
		}
		if string(got) != want {
			t.Errorf("%s: the archive overwrote the host's %s: %q", what, rel, got)
		}
	}
	entries, err := os.ReadDir(filepath.Join(host, ".git", "hooks"))
	if err != nil {
		t.Fatalf("%s: .git/hooks is gone: %v", what, err)
	}
	if len(entries) != 0 {
		t.Errorf("%s: the archive planted %d file(s) in the host's .git/hooks", what, len(entries))
	}
}

// The exclusions answer for the FILE a member reaches, not for the text of
// the name it carries. The same file has many names — a tar --exclude pattern
// matches one of them — and a link member reaches a file whose name appears
// nowhere in the archive.
func TestTheExportRefusesEveryNameThatReachesAnExcludedFile(t *testing.T) {
	for _, tc := range []struct {
		name    string
		members []tar.Header
		bodies  map[string]string
	}{
		{
			name:    "the canonical name a cooperative archiver writes",
			members: []tar.Header{{Name: "./.git/config", Typeflag: tar.TypeReg}},
			bodies:  map[string]string{"./.git/config": "POD-CONFIG"},
		},
		{
			name:    "the same file without the leading ./",
			members: []tar.Header{{Name: ".git/config", Typeflag: tar.TypeReg}},
			bodies:  map[string]string{".git/config": "POD-CONFIG"},
		},
		{
			name:    "a . component in the middle",
			members: []tar.Header{{Name: "./.git/./config", Typeflag: tar.TypeReg}},
			bodies:  map[string]string{"./.git/./config": "POD-CONFIG"},
		},
		{
			name:    "a .. that comes back",
			members: []tar.Header{{Name: "./.git/hooks/../config", Typeflag: tar.TypeReg}},
			bodies:  map[string]string{"./.git/hooks/../config": "POD-CONFIG"},
		},
		{
			name:    "a doubled separator",
			members: []tar.Header{{Name: ".//.git//iterion-credentials", Typeflag: tar.TypeReg}},
			bodies:  map[string]string{".//.git//iterion-credentials": "POD-CREDENTIAL"},
		},
		{
			name: "a link member aiming a later write into .git/hooks",
			members: []tar.Header{
				{Name: "./.git/x", Typeflag: tar.TypeSymlink, Linkname: "hooks"},
				{Name: "./.git/x/pre-push", Typeflag: tar.TypeReg, Mode: 0o755},
			},
			bodies: map[string]string{"./.git/x/pre-push": "#!/bin/sh\n"},
		},
		{
			name: "a link member aiming a later write at the config",
			members: []tar.Header{
				{Name: "./.git/y", Typeflag: tar.TypeSymlink, Linkname: "config"},
				{Name: "./.git/y", Typeflag: tar.TypeReg},
			},
			bodies: map[string]string{"./.git/y": "POD-CONFIG"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			host, run := hostileHost(t)
			if _, err := run.extractExport(bytes.NewReader(hostileArchive(t, tc.members, tc.bodies)), host); err != nil {
				t.Fatalf("extract: %v", err)
			}
			assertHostUntouched(t, host, tc.name)
		})
	}
}

// A member whose name leaves the workspace is refused, and the export
// continues: the run's own work still lands.
func TestTheExportWritesNothingOutsideTheWorkspace(t *testing.T) {
	host, run := hostileHost(t)
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	members := []tar.Header{
		{Name: "./../escaped.txt", Typeflag: tar.TypeReg},
		{Name: "/etc/absolute.txt", Typeflag: tar.TypeReg},
		{Name: "./out", Typeflag: tar.TypeSymlink, Linkname: outside},
		{Name: "./out/through-a-link.txt", Typeflag: tar.TypeReg},
		{Name: "./work.txt", Typeflag: tar.TypeReg},
	}
	bodies := map[string]string{
		"./../escaped.txt":         "X",
		"/etc/absolute.txt":        "X",
		"./out/through-a-link.txt": "X",
		"./work.txt":               "the run's own work",
	}
	extracted, err := run.extractExport(bytes.NewReader(hostileArchive(t, members, bodies)), host)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(host), "escaped.txt")); err == nil {
		t.Error("a ../ member wrote beside the workspace")
	}
	if _, err := os.Stat(filepath.Join(outside, "through-a-link.txt")); err == nil {
		t.Error("a member wrote outside the workspace through a link the archive left first")
	}
	got, err := os.ReadFile(filepath.Join(host, "work.txt"))
	if err != nil || string(got) != "the run's own work" {
		t.Fatalf("the run's own file did not land: %q %v — a refusal must not cost the export", got, err)
	}
	if !extracted["work.txt"] {
		t.Errorf("extracted = %v, want the file that landed", extracted)
	}
	for rel := range extracted {
		// The refused link became no link, so the member that named it is an
		// ordinary file INSIDE the workspace — which is the point: a refusal
		// moves the write back inside, it does not follow it out.
		if strings.Contains(rel, "escaped") || strings.Contains(rel, "absolute") || rel == "out" {
			t.Errorf("extracted names a member the host refused: %q", rel)
		}
	}
}

// What the run DID write comes back whole: contents, mode and modification
// time, which is what git reads to decide a file is unchanged.
func TestTheExportRestoresWhatTheRunWrote(t *testing.T) {
	host, run := hostileHost(t)
	members := []tar.Header{
		{Name: "./src", Typeflag: tar.TypeDir, Mode: 0o755},
		{Name: "./src/main.go", Typeflag: tar.TypeReg, Mode: 0o644},
		{Name: "./run.sh", Typeflag: tar.TypeReg, Mode: 0o755},
		{Name: "./link.go", Typeflag: tar.TypeSymlink, Linkname: "src/main.go"},
		{Name: "./.git/packed-refs", Typeflag: tar.TypeReg, Mode: 0o644},
	}
	bodies := map[string]string{
		"./src/main.go":      "package main\n",
		"./run.sh":           "#!/bin/sh\n",
		"./.git/packed-refs": "ref\n",
	}
	extracted, err := run.extractExport(bytes.NewReader(hostileArchive(t, members, bodies)), host)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	for _, rel := range []string{"src/main.go", "run.sh", ".git/packed-refs"} {
		if !extracted[rel] {
			t.Errorf("extracted = %v, want %s", extracted, rel)
		}
	}
	if got, err := os.ReadFile(filepath.Join(host, "src", "main.go")); err != nil || string(got) != "package main\n" {
		t.Errorf("src/main.go = %q (%v)", got, err)
	}
	info, err := os.Stat(filepath.Join(host, "run.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Errorf("run.sh mode = %v, want 0755: an executable the run produced comes back executable", info.Mode().Perm())
	}
	if !info.ModTime().Equal(time.Unix(1700000000, 0)) {
		t.Errorf("run.sh mtime = %v, want the archive's: git reads mtime to decide what changed", info.ModTime())
	}
	target, err := os.Readlink(filepath.Join(host, "link.go"))
	if err != nil || target != "src/main.go" {
		t.Errorf("link.go -> %q (%v), want the link the run left inside the workspace", target, err)
	}
}

// A member carrying setuid is written without it: nothing a workspace needs
// back, and a way to keep privilege outside the sandbox.
func TestTheExportDropsSetuidFromWhatItWrites(t *testing.T) {
	host, run := hostileHost(t)
	members := []tar.Header{{Name: "./tool", Typeflag: tar.TypeReg, Mode: 0o4755}}
	if _, err := run.extractExport(bytes.NewReader(hostileArchive(t, members, map[string]string{"./tool": "x"})), host); err != nil {
		t.Fatalf("extract: %v", err)
	}
	info, err := os.Stat(filepath.Join(host, "tool"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSetuid != 0 {
		t.Errorf("tool mode = %v, want the setuid bit dropped", info.Mode())
	}
}

// A symlink the HOST clone already holds — a repository may legitimately
// contain one — is not a door for the archive: a member sent through it is
// refused when it would land outside the workspace.
func TestTheExportRefusesAMemberSentThroughAHostSymlink(t *testing.T) {
	host, run := hostileHost(t)
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(host, "escape")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("src", filepath.Join(host, "inside")); err != nil {
		t.Fatal(err)
	}
	members := []tar.Header{
		{Name: "./escape/landed.txt", Typeflag: tar.TypeReg},
		{Name: "./src", Typeflag: tar.TypeDir, Mode: 0o755},
		{Name: "./inside/kept.txt", Typeflag: tar.TypeReg},
	}
	bodies := map[string]string{"./escape/landed.txt": "X", "./inside/kept.txt": "the run's own work"}
	if _, err := run.extractExport(bytes.NewReader(hostileArchive(t, members, bodies)), host); err != nil {
		t.Fatalf("extract: %v", err)
	}
	if _, err := os.Stat(filepath.Join(outside, "landed.txt")); err == nil {
		t.Error("a member wrote outside the workspace through a symlink the host clone already held")
	}
	// A link that stays inside is not in the way: the member lands.
	if got, err := os.ReadFile(filepath.Join(host, "src", "kept.txt")); err != nil || string(got) != "the run's own work" {
		t.Errorf("a member through an in-workspace link did not land: %q %v", got, err)
	}
}

// The decision is about the file a write REACHES, and a directory that does
// not exist yet is created through whatever link sits above it. A host clone
// without .git/hooks — git invoked with an empty template dir leaves none —
// is the case where the link is the only thing standing there.
func TestTheExportRefusesALandingThroughAnAncestorThatDoesNotExistYet(t *testing.T) {
	host, run := hostileHost(t)
	if err := os.RemoveAll(filepath.Join(host, ".git", "hooks")); err != nil {
		t.Fatal(err)
	}
	members := []tar.Header{
		{Name: "./a", Typeflag: tar.TypeDir, Mode: 0o755},
		{Name: "./a/x", Typeflag: tar.TypeSymlink, Linkname: "../.git"},
		{Name: "./a/x/hooks/pre-commit", Typeflag: tar.TypeReg, Mode: 0o755},
		{Name: "./a/x/config", Typeflag: tar.TypeReg},
	}
	bodies := map[string]string{
		"./a/x/hooks/pre-commit": "#!/bin/sh\n",
		"./a/x/config":           "POD-CONFIG",
	}
	if _, err := run.extractExport(bytes.NewReader(hostileArchive(t, members, bodies)), host); err != nil {
		t.Fatalf("extract: %v", err)
	}
	if _, err := os.Stat(filepath.Join(host, ".git", "hooks", "pre-commit")); err == nil {
		t.Error("a member landed in .git/hooks through a link whose directory did not exist yet")
	}
	got, err := os.ReadFile(filepath.Join(host, ".git", "config"))
	if err != nil || string(got) != "HOST-CONFIG" {
		t.Errorf("the host's .git/config was overwritten through the link: %q (%v)", got, err)
	}
}
