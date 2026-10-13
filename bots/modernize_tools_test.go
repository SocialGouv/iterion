package bots

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// requireModernizeTools skips a modernize bot-level test when a tool it
// needs is missing on a developer's host — and FAILS instead under CI, where
// a skipped guard test is a green no-op: the exact class these tests exist
// to refuse (a verdict that passed because a check never ran).
func requireModernizeTools(t *testing.T) {
	t.Helper()
	for _, tool := range []string{"python3", "git", "yq"} {
		if _, err := exec.LookPath(tool); err != nil {
			if os.Getenv("CI") != "" {
				t.Fatalf("%s not on PATH under CI — the modernize guard tests would skip into a green no-op; install it in the workflow", tool)
			}
			t.Skipf("%s not on PATH", tool)
		}
	}
}

// restrictedPATH builds a bin dir holding symlinks to the named tools only,
// so a script run with PATH=<dir> sees exactly those — the way to prove what
// a node does when `yq` is NOT on PATH without uninstalling anything.
//
// The link must reach something the restricted dir can RUN: on a dev host a
// LookPath hit can be a shim script (pyenv's python3 is `#!/usr/bin/env
// bash`), and the kernel then finds no bash in the restricted dir — the node
// dies on an empty report that reads as a bug in what is under test (#2232).
// An env-shebang script is the host leaking into the guard: skip it locally
// with the reason named, refuse it under CI like a missing tool
// (requireModernizeTools's doctrine — a skipped guard test is a green no-op
// there). A script with an absolute interpreter (`#!/bin/sh`) starts fine
// anywhere and is linked like a binary.
func restrictedPATH(t *testing.T, tools ...string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, tool := range tools {
		p, err := exec.LookPath(tool)
		if err != nil {
			t.Skipf("%s not on PATH", tool)
		}
		if real, err := filepath.EvalSymlinks(p); err == nil {
			p = real
		}
		if shebang := shebangLine(p); strings.HasPrefix(shebang, "#!") {
			if interp := shebangInterpreter(shebang); interp == "" {
				msg := fmt.Sprintf("%s resolves to the script %s (%s) — a host shim the restricted PATH cannot run; install a real %s", tool, p, shebang, tool)
				if os.Getenv("CI") != "" {
					t.Fatalf("%s — the modernize guard tests would skip into a green no-op", msg)
				}
				t.Skip(msg)
			}
		}
		if err := os.Symlink(p, filepath.Join(dir, tool)); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// shebangLine reads the file's first line when it starts with #! — two bytes
// of probing plus one line, never a whole binary.
func shebangLine(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	head := make([]byte, 2)
	if _, err := f.Read(head); err != nil || string(head) != "#!" {
		return ""
	}
	line, _ := bufio.NewReader(f).ReadString('\n')
	return "#!" + strings.TrimSpace(line)
}

// shebangInterpreter names the interpreter a shebang line invokes when the
// restricted dir can start it — an absolute, existing path (`#!/bin/sh`) —
// and "" when it cannot: an env-mediated one (`#!/usr/bin/env bash`, whose
// lookup the restricted PATH is built to control), a relative one, or a
// missing one.
func shebangInterpreter(shebang string) string {
	fields := strings.Fields(strings.TrimPrefix(shebang, "#!"))
	if len(fields) == 0 || !filepath.IsAbs(fields[0]) {
		return ""
	}
	if base := filepath.Base(fields[0]); base == "env" {
		return ""
	}
	if _, err := os.Stat(fields[0]); err != nil {
		return ""
	}
	return fields[0]
}
