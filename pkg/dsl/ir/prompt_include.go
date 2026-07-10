package ir

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// maxPromptIncludeBytes caps the size of a single file inlined by an
// {{include "..."}} marker. Rules files are markdown, a few hundred
// lines at most; the cap keeps a stray large/binary file from bloating
// the compiled prompt body (and, transitively, the run payload).
const maxPromptIncludeBytes = 256 * 1024 // 256 KiB

// promptIncludeRe matches an include marker inside a prompt body:
//
//	{{include "relative/path.md"}}
//
// Surrounding whitespace inside the braces is tolerated. The captured
// group is the relative path. The pattern is deliberately strict (a
// double-quoted path only) so it never misfires on ordinary template
// references such as {{vars.include}} or {{outputs.include.x}}.
var promptIncludeRe = regexp.MustCompile(`\{\{\s*include\s+"([^"]*)"\s*\}\}`)

// expandPromptIncludes replaces every {{include "..."}} marker in a
// prompt body with the verbatim contents of the referenced file,
// resolved relative to baseDir (the directory of the .bot source).
//
// Expansion happens at compile time, once, before ParseRefs runs — so
// the injected text becomes part of the resolved prompt body in the IR
// (auditable, statically validated, no runtime file reads). A marker
// whose file cannot be resolved is replaced with the empty string and
// the error is collected so the caller can raise a diagnostic without
// leaving a broken marker for ParseRefs to trip over.
//
// Paths are constrained to baseDir's subtree: absolute paths and any
// path escaping the base (via ..) are rejected, and files larger than
// maxPromptIncludeBytes are refused.
func expandPromptIncludes(body, baseDir string) (string, []error) {
	if !strings.Contains(body, "{{") {
		return body, nil
	}
	var errs []error
	out := promptIncludeRe.ReplaceAllStringFunc(body, func(match string) string {
		rel := promptIncludeRe.FindStringSubmatch(match)[1]
		content, err := readPromptInclude(baseDir, rel)
		if err != nil {
			errs = append(errs, err)
			return ""
		}
		return content
	})
	return out, errs
}

// readPromptInclude validates a relative include path against baseDir
// and returns the file contents. It refuses absolute paths, paths that
// escape baseDir, missing files, and files over the size cap.
func readPromptInclude(baseDir, rel string) (string, error) {
	if rel == "" {
		return "", fmt.Errorf("include: empty path")
	}
	if filepath.IsAbs(rel) {
		return "", fmt.Errorf("include %q: absolute paths are not allowed (use a path relative to the .bot file)", rel)
	}
	if baseDir == "" {
		baseDir = "."
	}
	full := filepath.Join(baseDir, filepath.Clean(rel))
	// Confine the resolved path to baseDir's subtree (lexical guard).
	if err := confineToBase(baseDir, full); err != nil {
		return "", fmt.Errorf("include %q: %w", rel, err)
	}
	info, err := os.Stat(full)
	if err != nil {
		return "", fmt.Errorf("include %q: %w", rel, err)
	}
	if info.IsDir() {
		return "", fmt.Errorf("include %q: is a directory, not a file", rel)
	}
	// Re-check containment AFTER symlink resolution: a symlink INSIDE
	// baseDir pointing outside it passes the lexical guard above but would
	// still leak arbitrary host files (secrets, /etc/passwd) into the
	// prompt. Resolve both operands so the comparison is apples-to-apples
	// — matches the symlink-aware containment used by pkg/server and
	// pkg/bundle.
	realBase, err := filepath.EvalSymlinks(baseDir)
	if err != nil {
		return "", fmt.Errorf("include %q: resolve base dir: %w", rel, err)
	}
	realFull, err := filepath.EvalSymlinks(full)
	if err != nil {
		return "", fmt.Errorf("include %q: %w", rel, err)
	}
	if err := confineToBase(realBase, realFull); err != nil {
		return "", fmt.Errorf("include %q: %w", rel, err)
	}
	if info.Size() > maxPromptIncludeBytes {
		return "", fmt.Errorf("include %q: file is %d bytes, over the %d byte limit", rel, info.Size(), maxPromptIncludeBytes)
	}
	data, err := os.ReadFile(full)
	if err != nil {
		return "", fmt.Errorf("include %q: %w", rel, err)
	}
	return string(data), nil
}

// confineToBase reports an error if full is not baseDir itself or a path
// underneath it. Both are compared lexically via filepath.Rel; callers
// that need symlink safety resolve their operands first.
func confineToBase(baseDir, full string) error {
	within, err := filepath.Rel(baseDir, full)
	if err != nil || within == ".." || strings.HasPrefix(within, ".."+string(filepath.Separator)) {
		return fmt.Errorf("path escapes the .bot directory")
	}
	return nil
}
