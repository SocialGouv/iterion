package cli_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestNoIterExtensionAnywhere is the zero-trace guard for the deprecated
// `.iter` workflow extension. Iterion runs workflows from `.bot` files
// (`.botz` for bundles) and rejects every other extension at the CLI,
// server, dispatcher, and studio boundaries — the single source of truth
// is pkg/dsl/workflowfile. The historical `.iter` extension was purged
// from the entire tree (sources, fixtures, docs, configs); this test
// fails CI if it ever creeps back in.
//
// It greps the git-tracked tree (so generated/ignored runtime state under
// `.iterion/` and `.claude/` is out of scope) for the literal `.iter`
// extension token: `\.iter` followed by a non-word, non-dot boundary. That
// matches `foo.iter`, `*.iter`, "the .iter file", but NOT:
//   - `.iterion` (the run-store directory / product name) — `iter` is
//     followed by `i`, a word char,
//   - words like "iteration"/"iterate" — same reason,
//   - the bare ```` ```iter ```` markdown code-fence language tag — no
//     leading dot.
//
// vendor/ is excluded: third-party code (otel `oi.iter`, sqlite
// `ctx.iter`, the tiktoken `.iter` BPE token) legitimately contains the
// substring and is not ours to rewrite.
//
// The generated changelog is excluded for the same reason: it quotes the
// subjects of past commits verbatim — including the one that removed `.iter`
// support — so purging the token there would falsify the record rather than
// fix anything. Those files describe history, never a live reference.
func TestNoIterExtensionAnywhere(t *testing.T) {
	root := repoRootForDocsTest(t)

	cmd := exec.Command("git", "grep", "-nIE", `\.iter([^a-zA-Z0-9._]|$)`,
		"--", ".", ":(exclude)vendor/",
		":(exclude)CHANGELOG.md", ":(exclude)docs/changelog/")
	cmd.Dir = root
	out, err := cmd.Output()
	// git grep exits 1 with no output when there are no matches — the
	// success case. A real match exits 0 with the offending lines.
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok && ee.ExitCode() == 1 && len(out) == 0 {
			return // clean: no `.iter` extension token anywhere
		}
		t.Fatalf("git grep failed: %v\n%s", err, out)
	}

	var violations []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		// `e.iter` is a struct-field access in pkg/store/turn_store_test.go,
		// not the extension — the ERE can't exclude it without dropping
		// legitimate "e.iter file" prose. Skip the exact field-access form.
		if strings.Contains(line, "e.iter,") {
			continue
		}
		// This guard file documents the token in its own comments.
		if strings.Contains(line, "cli_docs_examples_test.go") {
			continue
		}
		violations = append(violations, line)
	}

	if len(violations) > 0 {
		t.Fatalf("the deprecated `.iter` extension reappeared — workflows are "+
			"`.bot` (`.botz` for bundles); purge these references:\n  %s",
			strings.Join(violations, "\n  "))
	}
}

// repoRootForDocsTest walks up from the test's working directory (the package
// dir) until it finds the go.mod, returning the repo root.
func repoRootForDocsTest(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("repo root (go.mod) not found from %s", dir)
		}
		dir = parent
	}
}
