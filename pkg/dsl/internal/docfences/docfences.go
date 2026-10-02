// Package docfences reads the fenced code blocks of the repository's
// documentation — the programs and documents a reader, human or agent,
// copies — for the tests that hold them to the language: the ```iter fences
// to the parser and the compiler (pkg/dsl/ir), the ```yaml author fences to
// the author document's converter, and the ```iter fences to the author
// round trip (pkg/dsl/author). It depends on the standard library alone, so
// any DSL package's tests may read the same fences the same way.
package docfences

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Fence is one fenced block of a markdown file.
type Fence struct {
	File string
	// Line is the line of the opening fence, 1-based.
	Line int
	// Info is the info string after the language tag, trimmed: the policy
	// words a test reads (`fragment`, `invalid:C019`, `author`). Markdown
	// renderers read only the first word, so they are invisible to readers.
	Info string
	// Body is the block's text, the fence's own indentation stripped from
	// every line, ending with a newline.
	Body string
	// Malformed is non-empty when the fence's own shape is wrong: a body
	// line indented less than the fence.
	Malformed string
}

// Files lists the markdown a reader may take DSL from, under the repository
// root: the docs tree, the root skills and README, and every bot's and
// example's own markdown (its README, a static catalogue) and skills —
// sorted.
func Files(root string) ([]string, error) {
	var files []string
	for _, p := range []string{"README.md", "SKILL.md", "SKILL-run-and-refine.md"} {
		if _, err := os.Stat(filepath.Join(root, p)); err == nil {
			files = append(files, filepath.Join(root, p))
		}
	}
	err := filepath.WalkDir(filepath.Join(root, "docs"), func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.HasSuffix(path, ".md") {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("docfences: walk docs: %w", err)
	}
	for _, g := range []string{"bots/*/skills/*.md", "bots/*/*.md", "examples/*/*.md", "examples/*/skills/*.md"} {
		m, err := filepath.Glob(filepath.Join(root, g))
		if err != nil {
			return nil, fmt.Errorf("docfences: %w", err)
		}
		files = append(files, m...)
	}
	sort.Strings(files)
	if len(files) == 0 {
		return nil, fmt.Errorf("docfences: no documentation file under %s", root)
	}
	return files, nil
}

// Extract returns the fences of path opened with ```<lang>. A fence may be
// indented (inside a list item): the same indentation is stripped from its
// body, its closing fence must sit at exactly that indent — a deeper ``` is
// body text, a prompt teaching an agent to emit a code fence — and a body
// line indented less than the fence is reported (Malformed) rather than
// silently kept. The info string is captured whole, so a policy with a
// space in it is seen by the caller instead of silently dropped. A fence
// never closed is returned as an error beside the fences that were.
// HasFence reports whether raw carries a fence opened for lang — any
// spelling CommonMark reads: three or more backticks or tildes, after any
// run of blockquote prefixes, the language after the marker with or
// without a space. The list guard and Extract read through it, so a form
// a renderer reads is a form this package reads too (#1814).
func HasFence(raw []byte, lang string) bool {
	re := openFenceRe(lang)
	for _, line := range strings.Split(string(raw), "\n") {
		if re.MatchString(line) {
			return true
		}
	}
	return false
}

// openFenceRe is the opener of one lang's fences: indentation, any run of
// `> ` blockquote prefixes, a fence of three or more backticks or tildes,
// optional spaces, then the language at a word boundary. The captures are
// the whitespace prefix, the fence marker, and the info string.
func openFenceRe(lang string) *regexp.Regexp {
	return regexp.MustCompile("^(\\s*)((?:> ?){0,6})(`{3,}|~{3,})[ \\t]*" + regexp.QuoteMeta(lang) + "(?:\\b|[ \\t])(.*)$")
}

func Extract(path, lang string) ([]Fence, error) {
	open := openFenceRe(lang)
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("docfences: %w", err)
	}
	defer f.Close()
	var out []Fence
	var cur *Fence
	var body []string
	indent := ""
	var marker string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), 4*1024*1024)
	lineNo := 0
	closesFence := func(line string) bool {
		t := strings.TrimRight(line, " \t")
		rest, ok := strings.CutPrefix(t, indent)
		if !ok {
			// A closing fence may sit at up to three spaces of its own
			// indentation below the opener's (CommonMark) — but only where
			// the opener's prefix is plain indentation: inside a
			// blockquote, the prefix IS the fence's place.
			lead := t[:len(t)-len(strings.TrimLeft(t, " \t"))]
			if len(lead) > 3 || strings.Contains(indent, ">") {
				return false
			}
			rest = strings.TrimLeft(t, " \t")
		}
		if len(rest) < len(marker) || rest[0] != marker[0] {
			return false
		}
		return strings.Trim(rest, string(marker[0])) == ""
	}
	for sc.Scan() {
		lineNo++
		line := sc.Text()
		if cur == nil {
			if m := open.FindStringSubmatch(line); m != nil {
				indent = m[1] + m[2]
				marker = m[3]
				cur = &Fence{File: path, Line: lineNo, Info: strings.TrimSpace(m[4])}
				body = body[:0]
			}
			continue
		}
		if closesFence(line) {
			cur.Body = strings.Join(body, "\n") + "\n"
			out = append(out, *cur)
			cur = nil
			continue
		}
		if strings.TrimSpace(line) != "" && !strings.HasPrefix(line, indent) && cur.Malformed == "" {
			cur.Malformed = fmt.Sprintf("line %d is indented less than its fence", lineNo)
		}
		body = append(body, strings.TrimPrefix(line, indent))
	}
	if err := sc.Err(); err != nil {
		return out, fmt.Errorf("docfences: read %s: %w", path, err)
	}
	if cur != nil {
		return out, fmt.Errorf("%s:%d: %s fence is never closed", path, cur.Line, marker+lang)
	}
	return out, nil
}
