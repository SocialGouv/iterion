package docsguard

import (
	"bytes"
	"errors"
	"fmt"
	"html"
	"io/fs"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"

	gitlib "github.com/SocialGouv/iterion/pkg/git"
)

// A markdown link is checked the way GitHub resolves it: a relative path is
// taken from the linking file's directory, a `#fragment` against the anchors
// GitHub generates for the target page — one per heading through the slug
// rule below, plus every explicit `id="…"` / `name="…"` the page carries in
// raw HTML. A `/`-prefixed target is reported (GitHub serves it against the
// site origin, not this repository). Links inside fenced code, inline code,
// HTML comments and YAML front matter are not links and are never reported;
// a fragment on a non-markdown file (`file.go#L12`) is accepted as is, since
// GitHub serves those anchors from the file view.
//
// Known blind spots, each with zero live instances in this repository today
// (a finding is only as good as its class): link TEXT wrapped across lines
// (`[text` on one line, `](target)` on the next) is not seen — the scan is
// line-local; an HTML `href=` whose quoting is not exactly `href="…"` is
// not seen; the site-side slug check decodes only the five entities its
// heading-text reducer spells out; a page larger than what GitHub's
// renderer serves (huge pages are truncated) is held to anchors the
// rendered page may not carry.

// Link is one link occurrence in a markdown file.
type Link struct {
	// File is the linking document, slash-separated and relative to the root.
	File string
	Line int
	// Target is the destination exactly as written in the source.
	Target string
}

// Broken is a link whose target does not resolve.
type Broken struct {
	Link
	// Reason says what did not resolve; it is written for a human.
	Reason string
	// Nearest is the closest existing anchor when the fragment is what is
	// missing, so the fix is one edit away from the report. Empty otherwise.
	Nearest string
}

// Key identifies a broken link independently of its line number, so the same
// defect keeps its identity when prose moves around it. It is what the delta
// against a base revision compares.
func (b Broken) Key() string { return b.File + "\t" + b.Target }

func (b Broken) String() string {
	s := fmt.Sprintf("%s:%d: (%s) %s", b.File, b.Line, b.Target, b.Reason)
	if b.Nearest != "" {
		s += fmt.Sprintf(" — nearest: #%s", b.Nearest)
	}
	return s
}

// FS is what the checker needs from a tree: the bytes of a file and whether a
// path exists. Two implementations ship — the working tree, and a committed
// revision read through git — so a report can be diffed against the merge
// base without checking anything out.
type FS interface {
	ReadFile(name string) ([]byte, error)
	// Stat reports whether name exists and whether it is a directory; a
	// missing path returns an error that satisfies errors.Is(err, fs.ErrNotExist).
	Stat(name string) (isDir bool, err error)
}

// OSFS reads a tree from the filesystem, rooted at Root.
type OSFS struct{ Root string }

func (o OSFS) ReadFile(name string) ([]byte, error) {
	return os.ReadFile(filepath.Join(o.Root, filepath.FromSlash(name)))
}

func (o OSFS) Stat(name string) (bool, error) {
	info, err := os.Stat(filepath.Join(o.Root, filepath.FromSlash(name)))
	if err != nil {
		return false, err
	}
	return info.IsDir(), nil
}

// GitTreeFS reads a committed tree: `git show <rev>:<path>` for contents, and
// the recursive listing of the tree (files and directories) for existence.
type GitTreeFS struct {
	Root string
	Rev  string

	entries map[string]bool // path → isDir; nil until listed
}

func (g *GitTreeFS) ReadFile(name string) ([]byte, error) {
	out, err := g.git("show", g.Rev+":"+name)
	if err != nil {
		return nil, fmt.Errorf("git show %s:%s: %w", g.Rev, name, err)
	}
	return out, nil
}

func (g *GitTreeFS) Stat(name string) (bool, error) {
	if err := g.list(); err != nil {
		return false, err
	}
	name = strings.TrimSuffix(name, "/")
	if name == "" || name == "." {
		return true, nil
	}
	isDir, ok := g.entries[name]
	if !ok {
		return false, fs.ErrNotExist
	}
	return isDir, nil
}

// Files returns every path in the tree with one of the given suffixes.
func (g *GitTreeFS) Files(suffix string) ([]string, error) {
	if err := g.list(); err != nil {
		return nil, err
	}
	var out []string
	for name, isDir := range g.entries {
		if !isDir && strings.HasSuffix(name, suffix) {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out, nil
}

func (g *GitTreeFS) list() error {
	if g.entries != nil {
		return nil
	}
	// -t lists the tree entries (directories) as well as the blobs.
	out, err := g.git("ls-tree", "-r", "-t", "-z", "--name-only", g.Rev)
	if err != nil {
		return fmt.Errorf("git ls-tree %s: %w", g.Rev, err)
	}
	dirOut, err := g.git("ls-tree", "-r", "-t", "-d", "-z", "--name-only", g.Rev)
	if err != nil {
		return fmt.Errorf("git ls-tree -d %s: %w", g.Rev, err)
	}
	dirs := map[string]bool{}
	for _, name := range strings.Split(strings.TrimSuffix(string(dirOut), "\x00"), "\x00") {
		if name != "" {
			dirs[name] = true
		}
	}
	g.entries = map[string]bool{}
	for _, name := range strings.Split(strings.TrimSuffix(string(out), "\x00"), "\x00") {
		if name != "" {
			g.entries[name] = dirs[name]
		}
	}
	return nil
}

func (g *GitTreeFS) git(args ...string) ([]byte, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = g.Root
	// An inherited GIT_DIR would make the answer about another repository.
	cmd.Env = gitlib.SanitizeEnv(os.Environ())
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

// Slug turns a heading's text into the anchor GitHub generates for it
// (html-pipeline's TableOfContentsFilter): lowercase, drop every character
// that is not a letter, a mark, a decimal digit, connector punctuation, a
// space or a hyphen, then one hyphen per space. Runs of spaces are NOT
// collapsed — `Backend parity — claw ↔ claude_code` becomes
// `backend-parity--claw--claude_code`, a double hyphen where each stripped
// symbol left its two surrounding spaces. A leading emoji leaves a leading
// hyphen for the same reason.
//
// Duplicate slugs in one page are the Slugger's job; this function is pure.
func Slug(text string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(text) {
		switch {
		case r == ' ':
			b.WriteByte('-')
		case r == '-', r == '_':
			b.WriteRune(r)
		case unicode.IsLetter(r), unicode.IsMark(r), unicode.Is(unicode.Nd, r), unicode.Is(unicode.Pc, r):
			b.WriteRune(r)
		}
	}
	return b.String()
}

// Slugger numbers repeated slugs the way GitHub does: the second `Bug Fixes`
// on a page is `bug-fixes-1`, the third `bug-fixes-2`, counting per base slug.
type Slugger struct{ seen map[string]int }

func (s *Slugger) Slug(text string) string {
	if s.seen == nil {
		s.seen = map[string]int{}
	}
	base := Slug(text)
	n := s.seen[base]
	s.seen[base]++
	if n == 0 {
		return base
	}
	return base + "-" + strconv.Itoa(n)
}

// HeadingText reduces a heading's markdown to the text GitHub slugs: the
// content of code spans and links, no image, no HTML tag, entities decoded,
// emphasis markers dropped, emoji shortcodes removed like the emoji glyphs
// they render to. Only the markdown edges are trimmed: an image or a
// shortcode at the start leaves its following space behind, and GitHub
// keeps it (`# :tada: Release` and `# 🎉 Release` both anchor at
// `#-release`).
func HeadingText(md string) string {
	s := strings.TrimSpace(md)
	s = imageRe.ReplaceAllString(s, "")
	s = linkRe.ReplaceAllString(s, "$1")
	s = refLinkRe.ReplaceAllString(s, "$1")
	// A code span's text is literal: `fail <name>` renders `<name>` as text,
	// not as a tag, so it is escaped here and restored by the entity decoding
	// below, after the tag stripping has run.
	s = codeSpanRe.ReplaceAllStringFunc(s, func(span string) string {
		return html.EscapeString(strings.Trim(span, "`"))
	})
	s = htmlTagRe.ReplaceAllString(s, "")
	s = shortcodeRe.ReplaceAllString(s, "")
	s = html.UnescapeString(s)
	s = underscoreEmphasisRe.ReplaceAllString(s, "$1$2$3")
	return s
}

var (
	imageRe    = regexp.MustCompile(`!\[[^\]]*\]\([^)]*\)`)
	linkRe     = regexp.MustCompile(`\[([^\]]*)\]\([^)]*\)`)
	refLinkRe  = regexp.MustCompile(`\[([^\]]*)\]\[[^\]]*\]`)
	codeSpanRe = regexp.MustCompile("`+[^`]*`+")
	htmlTagRe  = regexp.MustCompile(`<[^>]+>`)
	// A shortcode renders to an emoji glyph, which the slug rule drops.
	shortcodeRe = regexp.MustCompile(`:[a-z0-9_+-]+:`)
	// `_word_` and `__word__` are emphasis when the underscores flank the
	// text; an underscore inside a word (claude_code) is a character.
	underscoreEmphasisRe = regexp.MustCompile(`(^|[\s(])_{1,2}([^_\s][^_]*?[^_\s]|[^_\s])_{1,2}($|[\s).,;:!?])`)

	// Indentation is not bounded: GitHub renders a `##` continued inside a
	// list item (four spaces in) as a heading, and a phantom anchor from an
	// indented code block costs less than a phantom broken link.
	atxHeadingRe   = regexp.MustCompile(`^\s*(?:(?:>|[-*+]|\d+[.)])\s+)*(#{1,6})(?:[ \t]+(.*?))?(?:[ \t]+#+)?[ \t]*$`)
	setextLineRe   = regexp.MustCompile(`^\s{0,3}(=+|-{3,})\s*$`)
	htmlHeadingRe  = regexp.MustCompile(`(?i)<h[1-6][^>]*>(.*?)</h[1-6]>`)
	htmlAnchorRe   = regexp.MustCompile(`(?i)<[a-z][a-z0-9]*\b[^>]*\s(?:id|name)="([^"]+)"`)
	fenceRe        = regexp.MustCompile("^\\s*(?:>\\s?)*(`{3,}|~{3,})")
	refDefRe       = regexp.MustCompile(`^\s{0,3}\[([^\]^][^\]]*)\]:\s*(<[^>]*>|\S+)`)
	htmlHrefRe     = regexp.MustCompile(`(?i)<(?:a|img)\b[^>]*\s(?:href|src)="([^"]+)"`)
	schemeRe       = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9+.-]*:`)
	blockquoteRe   = regexp.MustCompile(`^\s{0,3}(?:>\s?)+`)
	listOrHeadRe   = regexp.MustCompile(`^\s{0,3}(?:[-*+]\s|\d+[.)]\s|#|\||>)`)
	frontMatterEnd = regexp.MustCompile(`^(---|\.\.\.)\s*$`)
)

// Document is one parsed markdown file: the anchors it defines and the links
// it carries.
type Document struct {
	Path    string
	Anchors map[string]bool
	Links   []Link
	// Headings lists the headings in document order with the anchor each
	// received — the sequence the duplicate numbering depends on.
	Headings []Heading
}

// Heading is one heading of a document as written, and its anchor.
type Heading struct {
	Line   int
	Raw    string
	Anchor string
}

func (d *Document) addHeading(line int, raw, text string, slugger *Slugger) {
	anchor := slugger.Slug(HeadingText(text))
	d.Anchors[anchor] = true
	d.Headings = append(d.Headings, Heading{Line: line, Raw: raw, Anchor: anchor})
}

// Parse extracts the anchors and links of one markdown file. Fenced code,
// inline code, HTML comments and a leading YAML front matter are skipped.
func Parse(name string, content []byte) *Document {
	doc := &Document{Path: name, Anchors: map[string]bool{}}
	var slugger Slugger
	lines := strings.Split(strings.ReplaceAll(string(content), "\r\n", "\n"), "\n")

	var (
		fenceChar  byte
		fenceLen   int
		inFence    bool
		inComment  bool
		inFront    bool
		prevText   string // previous non-skipped line, for setext headings
		prevIsText bool
	)
	refDefs := map[string]Link{}

	for i, raw := range lines {
		lineNo := i + 1
		line := raw

		if i == 0 && frontMatterEnd.MatchString(line) && strings.HasPrefix(line, "---") {
			inFront = true
			continue
		}
		if inFront {
			if frontMatterEnd.MatchString(line) {
				inFront = false
			}
			continue
		}

		if !inFence {
			line, inComment = stripComments(line, inComment)
			if inComment && strings.TrimSpace(line) == "" {
				prevIsText = false
				continue
			}
		}

		if m := fenceRe.FindStringSubmatch(line); m != nil {
			marker := m[1]
			if !inFence {
				inFence, fenceChar, fenceLen = true, marker[0], len(marker)
				prevIsText = false
				continue
			}
			rest := strings.TrimSpace(line[strings.Index(line, marker)+len(marker):])
			if marker[0] == fenceChar && len(marker) >= fenceLen && rest == "" {
				inFence = false
				continue
			}
		}
		if inFence {
			continue
		}

		// An explicit anchor is an anchor wherever it sits, including inside
		// a heading line (`## <a name="tiers"></a>Review tiers`).
		for _, m := range htmlAnchorRe.FindAllStringSubmatch(line, -1) {
			doc.Anchors[m[1]] = true
		}
		if m := atxHeadingRe.FindStringSubmatch(line); m != nil {
			doc.addHeading(lineNo, raw, m[2], &slugger)
			prevIsText = false
			continue
		}
		if setextLineRe.MatchString(line) && prevIsText {
			doc.addHeading(lineNo-1, prevText, prevText, &slugger)
			prevIsText = false
			continue
		}
		for _, m := range htmlHeadingRe.FindAllStringSubmatch(line, -1) {
			doc.addHeading(lineNo, raw, m[1], &slugger)
		}

		if m := refDefRe.FindStringSubmatch(line); m != nil {
			refDefs[strings.ToLower(m[1])] = Link{File: name, Line: lineNo, Target: strings.Trim(m[2], "<>")}
			prevIsText = false
			continue
		}

		scan := codeSpanRe.ReplaceAllString(line, "")
		for _, target := range inlineLinkTargets(scan) {
			doc.Links = append(doc.Links, Link{File: name, Line: lineNo, Target: target})
		}
		for _, m := range htmlHrefRe.FindAllStringSubmatch(scan, -1) {
			doc.Links = append(doc.Links, Link{File: name, Line: lineNo, Target: m[1]})
		}

		trimmed := strings.TrimSpace(blockquoteRe.ReplaceAllString(line, ""))
		prevIsText = trimmed != "" && !listOrHeadRe.MatchString(blockquoteRe.ReplaceAllString(line, ""))
		prevText = trimmed
	}

	// A reference definition is checked once, where it is written; a usage
	// of an undefined reference renders as plain text and is no link at all.
	defs := make([]Link, 0, len(refDefs))
	for _, l := range refDefs {
		defs = append(defs, l)
	}
	sort.Slice(defs, func(i, j int) bool { return defs[i].Line < defs[j].Line })
	doc.Links = append(doc.Links, defs...)
	return doc
}

// stripComments removes `<!-- … -->` spans from a line, carrying an unclosed
// comment across lines: the returned flag says whether the line ended inside
// a comment.
func stripComments(line string, inComment bool) (string, bool) {
	var out strings.Builder
	for {
		if inComment {
			end := strings.Index(line, "-->")
			if end < 0 {
				return out.String(), true
			}
			line = line[end+3:]
		}
		start := strings.Index(line, "<!--")
		if start < 0 {
			out.WriteString(line)
			return out.String(), false
		}
		out.WriteString(line[:start])
		line = line[start+4:]
		inComment = true
	}
}

// inlineLinkTargets returns the destinations of every `[text](dest)` and
// `![alt](dest)` on a line. A destination may be wrapped in `<…>`, may carry
// a `"title"`, and may contain balanced parentheses.
func inlineLinkTargets(line string) []string {
	var targets []string
	for i := 0; i < len(line); i++ {
		if line[i] != ']' || i+1 >= len(line) || line[i+1] != '(' {
			continue
		}
		// Walk back to the matching '[' so `[a [b] c](d)` is one link.
		depth := 0
		open := -1
		for j := i; j >= 0; j-- {
			switch line[j] {
			case ']':
				depth++
			case '[':
				depth--
				if depth == 0 {
					open = j
				}
			}
			if open >= 0 {
				break
			}
		}
		if open < 0 {
			continue
		}
		// Walk forward to the matching ')'.
		dest, end := destination(line[i+2:])
		if end < 0 {
			continue
		}
		if dest != "" {
			targets = append(targets, dest)
		}
		i = i + 2 + end
	}
	return targets
}

// destination parses a link destination starting right after `](` and
// returns it with the index of the closing parenthesis (relative to s), or -1.
func destination(s string) (string, int) {
	if strings.HasPrefix(s, "<") {
		close := strings.Index(s, ">")
		if close < 0 {
			return "", -1
		}
		end := strings.Index(s[close:], ")")
		if end < 0 {
			return "", -1
		}
		return s[1:close], close + end
	}
	depth := 0
	for k := 0; k < len(s); k++ {
		switch s[k] {
		case '(':
			depth++
		case ')':
			if depth == 0 {
				dest := s[:k]
				// Drop an optional title: `dest "title"` or `dest 'title'`.
				if sp := strings.IndexAny(dest, " \t"); sp >= 0 {
					dest = dest[:sp]
				}
				return dest, k
			}
			depth--
		}
	}
	return "", -1
}

// Checker resolves links against a tree and remembers the pages it parsed.
type Checker struct {
	FS   FS
	docs map[string]*Document
}

// IsExternal reports whether a target leaves the repository: a URL with a
// scheme, or a protocol-relative one.
func IsExternal(target string) bool {
	return schemeRe.MatchString(target) || strings.HasPrefix(target, "//")
}

// Check parses every listed markdown file and resolves each of its links.
// The result is sorted by file, then line.
func (c *Checker) Check(docs []string) ([]Broken, error) {
	c.docs = map[string]*Document{}
	var broken []Broken
	for _, name := range docs {
		doc, err := c.document(name)
		if err != nil {
			return nil, err
		}
		for _, l := range doc.Links {
			b, err := c.resolve(l)
			if err != nil {
				return nil, err
			}
			if b != nil {
				broken = append(broken, *b)
			}
		}
	}
	sort.Slice(broken, func(i, j int) bool {
		if broken[i].File != broken[j].File {
			return broken[i].File < broken[j].File
		}
		return broken[i].Line < broken[j].Line
	})
	return broken, nil
}

func (c *Checker) document(name string) (*Document, error) {
	if doc, ok := c.docs[name]; ok {
		return doc, nil
	}
	content, err := c.FS.ReadFile(name)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", name, err)
	}
	doc := Parse(name, content)
	c.docs[name] = doc
	return doc, nil
}

func (c *Checker) resolve(l Link) (*Broken, error) {
	target := strings.TrimSpace(l.Target)
	if target == "" || IsExternal(target) {
		return nil, nil
	}
	pathPart, fragment, _ := strings.Cut(target, "#")
	if q := strings.Index(pathPart, "?"); q >= 0 {
		pathPart = pathPart[:q]
	}
	if decoded, err := url.PathUnescape(pathPart); err == nil {
		pathPart = decoded
	}
	if decoded, err := url.PathUnescape(fragment); err == nil {
		fragment = decoded
	}

	var resolved string
	switch {
	case pathPart == "":
		resolved = l.File
	case strings.HasPrefix(pathPart, "/"):
		// GitHub serves a /-prefixed target against the site origin
		// (https://github.com/…), not against the repository: an in-repo
		// page is not reachable that way, so the link is reported however
		// well the path would fit the tree.
		return &Broken{Link: l, Reason: fmt.Sprintf("%s starts with / — GitHub serves it against the site origin, not this repository; write the path relative to this file", pathPart)}, nil
	default:
		resolved = path.Join(path.Dir(l.File), pathPart)
	}
	if resolved == ".." || strings.HasPrefix(resolved, "../") {
		return &Broken{Link: l, Reason: "target is outside the repository"}, nil
	}
	if resolved == "." {
		resolved = ""
	}

	isDir, err := c.FS.Stat(resolved)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return &Broken{Link: l, Reason: fmt.Sprintf("file not found: %s", resolved)}, nil
		}
		return nil, fmt.Errorf("stat %s: %w", resolved, err)
	}
	if fragment == "" {
		return nil, nil
	}
	if isDir {
		// GitHub renders a directory's README on its tree page, so a
		// fragment on a directory link points into that README.
		readme := path.Join(resolved, "README.md")
		if _, err := c.FS.Stat(readme); err != nil {
			return &Broken{Link: l, Reason: fmt.Sprintf("fragment on a directory without README.md: %s", resolved)}, nil
		}
		resolved = readme
	}
	if !strings.EqualFold(path.Ext(resolved), ".md") {
		return nil, nil // `file.go#L12` and friends: served by the file view.
	}
	doc, err := c.document(resolved)
	if err != nil {
		return nil, err
	}
	if doc.Anchors[fragment] {
		return nil, nil
	}
	return &Broken{
		Link:    l,
		Reason:  fmt.Sprintf("anchor #%s not found in %s", fragment, resolved),
		Nearest: nearest(fragment, doc.Anchors),
	}, nil
}

// nearest returns the anchor closest to want by edit distance, preferring a
// shared prefix on ties, or "" when the page has no anchor at all.
func nearest(want string, anchors map[string]bool) string {
	best, bestDist := "", -1
	for a := range anchors {
		d := levenshtein(want, a)
		if bestDist < 0 || d < bestDist || (d == bestDist && a < best) {
			best, bestDist = a, d
		}
	}
	return best
}

func levenshtein(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	prev := make([]int, len(rb)+1)
	cur := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		cur[0] = i
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(rb)]
}

// Excluded reports whether a path sits under one of the excluded prefixes
// (directories that are not this repository's documentation: vendored
// modules, third-party trees, installed packages).
func Excluded(name string, prefixes []string) bool {
	for _, p := range prefixes {
		p = strings.TrimSuffix(p, "/")
		if name == p || strings.HasPrefix(name, p+"/") || strings.Contains(name, "/"+p+"/") {
			return true
		}
	}
	return false
}

// NewSince returns the broken links of head whose Key is absent from base:
// what a change introduced, independent of the backlog it inherited.
func NewSince(head, base []Broken) []Broken {
	known := make(map[string]bool, len(base))
	for _, b := range base {
		known[b.Key()] = true
	}
	var out []Broken
	for _, b := range head {
		if !known[b.Key()] {
			out = append(out, b)
		}
	}
	return out
}
