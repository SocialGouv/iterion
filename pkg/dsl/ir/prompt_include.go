package ir

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/SocialGouv/iterion/pkg/dsl/ast"
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
// resolved relative to baseDir — the directory of the file that declares
// the prompt: the .bot source, or a bundle's prompts/ for a prompts/*.md
// (never the process working directory, which on a server is nobody's).
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
func expandPromptIncludes(body, baseDir string, budget *includeBudget) (string, []error) {
	return expandPromptIncludesNested(body, baseDir, nil, budget)
}

// maxPromptIncludeDepth bounds include nesting: an included file may include
// another (relative to ITS OWN directory), to this depth; a cycle is refused
// by the path stack before the depth is reached.
const maxPromptIncludeDepth = 8

// maxPromptIncludeTotalBytes bounds what ONE FILE's includes expand to, all
// prompts and all levels together, and maxPromptIncludeExpansions how many
// files are read for it. Depth and the cycle check bound neither: a file
// may be included again beside itself (a shared preamble), so a tree grows
// as fan-out^depth — nine files of 659 bytes reached 1.3 MB in 5.7 s — and
// the expansion runs in the server at cloud publish, over files a tenant
// wrote. The budget is per FILE, not per prompt: a budget per prompt is
// multiplied by the prompt count, and 3 000 prompts of four includes each
// inside their own budget made 5 GB of heap out of 310 KB of source.
const (
	maxPromptIncludeTotalBytes = 1 << 20
	maxPromptIncludeExpansions = 256
)

// includeBudget is what one file's expansion has consumed so far. Once
// blown, every further marker is stripped without a second refusal: the
// first one names the budget, thousands more would name nothing new.
type includeBudget struct {
	bytes      int64
	expansions int
	blown      bool
}

// expandPromptIncludesNested expands to a fixed point, so no marker
// survives the expansion: a marker inside an included file used to be left
// in the body, where the next compile — on a runner, with no source file
// — resolved it against ITS working directory.
func expandPromptIncludesNested(body, baseDir string, stack []string, budget *includeBudget) (string, []error) {
	if !strings.Contains(body, "{{") {
		return body, nil
	}
	var errs []error
	out := promptIncludeRe.ReplaceAllStringFunc(body, func(match string) string {
		rel := promptIncludeRe.FindStringSubmatch(match)[1]
		if budget.blown {
			return ""
		}
		budget.expansions++
		if budget.expansions > maxPromptIncludeExpansions {
			budget.blown = true
			errs = append(errs, fmt.Errorf("include %q: more than %d files expanded for one .bot (a nested include tree grows as fan-out^depth)", rel, maxPromptIncludeExpansions))
			return ""
		}
		content, full, err := readPromptIncludeAt(baseDir, rel)
		if err != nil {
			errs = append(errs, err)
			return ""
		}
		budget.bytes += int64(len(content))
		if budget.bytes > maxPromptIncludeTotalBytes {
			budget.blown = true
			errs = append(errs, fmt.Errorf("include %q: the file's includes expand past %d bytes", rel, maxPromptIncludeTotalBytes))
			return ""
		}
		if !HasPromptInclude(content) {
			return content
		}
		if len(stack) >= maxPromptIncludeDepth {
			errs = append(errs, fmt.Errorf("include %q: nested deeper than %d levels", rel, maxPromptIncludeDepth))
			return ""
		}
		for _, seen := range stack {
			if seen == full {
				errs = append(errs, fmt.Errorf("include %q: includes itself (cycle through %s)", rel, strings.Join(stack, " > ")))
				return ""
			}
		}
		nested, nestedErrs := expandPromptIncludesNested(content, filepath.Dir(full), append(stack, full), budget)
		errs = append(errs, nestedErrs...)
		return nested
	})
	return out, errs
}

// HasPromptInclude reports whether a prompt body carries an include marker.
func HasPromptInclude(body string) bool {
	return promptIncludeRe.MatchString(body)
}

// promptSourceDir is the directory an include in a prompt resolves against:
// the directory of the file the prompt was read from, when that names an
// existing regular file on this host. A synthetic name — "" from the JSON
// transport, "<inline>" from an inline launch, "studio.bot" from the
// studio's parse endpoint — is not a file, and filepath.Dir of it is ".":
// the process working directory, which on a server is nobody's and on a
// runner is the pod's own. The compiler refuses those; the export
// (InlinePromptIncludes) is stricter still and wants an absolute path.
func promptSourceDir(file string) (string, error) {
	if file == "" {
		return "", errors.New("the prompt has no source file (an inline or transported prompt must carry its includes resolved)")
	}
	info, err := os.Stat(file)
	if err != nil || info.IsDir() {
		return "", fmt.Errorf("the prompt's source %q is not a file on this host (an inline source cannot carry an include — run the file itself)", file)
	}
	return filepath.Dir(file), nil
}

// InlinePromptIncludes resolves every {{include "..."}} marker in the
// file's prompts INTO the prompt bodies, each relative to its prompt's own
// source file, so the AST is self-contained: what travels — a queue message,
// an inline upload — then compiles anywhere, without the files that sat
// beside the source. A prompt whose recorded source file is not a file on
// this host (an inline upload parsed as "<inline>", a client's path) has
// nothing to resolve against: its markers are an error, never a lookup in
// the process working directory, which on a server is nobody's.
func InlinePromptIncludes(f *ast.File) error {
	var errs []string
	budget := &includeBudget{} // one per file, shared by every prompt
	for _, p := range f.Prompts {
		if !HasPromptInclude(p.Body) {
			continue
		}
		// An absolute path that exists here: a relative one would be looked
		// up in the process working directory, which is the lookup this
		// function exists to refuse.
		if info, err := os.Stat(p.Span.Start.File); !filepath.IsAbs(p.Span.Start.File) || err != nil || info.IsDir() {
			errs = append(errs, fmt.Sprintf("prompt %q: an {{include}} cannot be resolved — its source file %q is not an absolute path to a file on this host", p.Name, p.Span.Start.File))
			continue
		}
		body, incErrs := expandPromptIncludes(p.Body, filepath.Dir(p.Span.Start.File), budget)
		if len(incErrs) > 0 {
			for _, e := range incErrs {
				errs = append(errs, fmt.Sprintf("prompt %q: %v", p.Name, e))
			}
			continue
		}
		p.Body = body
	}
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}

// readPromptInclude validates a relative include path against baseDir
// and returns the file contents. It refuses absolute paths, paths that
// escape baseDir, missing files, and files over the size cap.
func readPromptInclude(baseDir, rel string) (string, error) {
	content, _, err := readPromptIncludeAt(baseDir, rel)
	return content, err
}

// readPromptIncludeAt is readPromptInclude returning the resolved path as
// well, the base a nested include resolves from.
func readPromptIncludeAt(baseDir, rel string) (string, string, error) {
	if rel == "" {
		return "", "", fmt.Errorf("include: empty path")
	}
	if filepath.IsAbs(rel) {
		return "", "", fmt.Errorf("include %q: absolute paths are not allowed (use a path relative to the file that contains the include)", rel)
	}
	if baseDir == "" {
		baseDir = "."
	}
	full := filepath.Join(baseDir, filepath.Clean(rel))
	// Confine the resolved path to baseDir's subtree (lexical guard).
	if err := confineToBase(baseDir, full); err != nil {
		return "", "", fmt.Errorf("include %q: %w", rel, err)
	}
	info, err := os.Stat(full)
	if err != nil {
		return "", "", fmt.Errorf("include %q: %w", rel, err)
	}
	if info.IsDir() {
		return "", "", fmt.Errorf("include %q: is a directory, not a file", rel)
	}
	// Re-check containment AFTER symlink resolution: a symlink INSIDE
	// baseDir pointing outside it passes the lexical guard above but would
	// still leak arbitrary host files (secrets, /etc/passwd) into the
	// prompt. Resolve both operands so the comparison is apples-to-apples
	// — matches the symlink-aware containment used by pkg/server and
	// pkg/bundle.
	realBase, err := filepath.EvalSymlinks(baseDir)
	if err != nil {
		return "", "", fmt.Errorf("include %q: resolve base dir: %w", rel, err)
	}
	realFull, err := filepath.EvalSymlinks(full)
	if err != nil {
		return "", "", fmt.Errorf("include %q: %w", rel, err)
	}
	if err := confineToBase(realBase, realFull); err != nil {
		return "", "", fmt.Errorf("include %q: %w", rel, err)
	}
	if info.Size() > maxPromptIncludeBytes {
		return "", "", fmt.Errorf("include %q: file is %d bytes, over the %d byte limit", rel, info.Size(), maxPromptIncludeBytes)
	}
	data, err := os.ReadFile(full)
	if err != nil {
		return "", "", fmt.Errorf("include %q: %w", rel, err)
	}
	return string(data), full, nil
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
