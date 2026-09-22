// Command docslinks checks every relative link and `#anchor` in the
// repository's tracked markdown against the real tree, the way GitHub
// resolves them (see internal/docsguard). It is what `task docs:links` runs.
//
//	docslinks                       # full report over the working tree
//	docslinks -base origin/main     # only the links broken since the merge base
//	docslinks -since <sha>          # only the links broken relative to that tree (CI, shallow clone)
//	docslinks -rev <sha>            # full report over a committed tree
//	docslinks -format github        # one ::error annotation per broken link
//
// Exit status: 0 when nothing is broken (or nothing new, with -base/-since),
// 1 when something is, 2 on a usage or I/O error.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"

	"github.com/SocialGouv/iterion/internal/docsguard"
	gitlib "github.com/SocialGouv/iterion/pkg/git"
)

func main() {
	root := flag.String("root", "", "repository root (default: the git toplevel of the working directory)")
	base := flag.String("base", "", "report only the links broken since the merge base of this revision and HEAD")
	since := flag.String("since", "", "report only the links broken relative to exactly this tree (no merge base: what a shallow CI clone can compare)")
	rev := flag.String("rev", "", "check the committed tree at this revision instead of the working tree")
	format := flag.String("format", "text", "text | github (workflow annotations)")
	exclude := flag.String("exclude", "vendor,third_party,node_modules", "comma-separated directory names that are not this repository's documentation")
	headings := flag.String("headings", "", "debug: print `line<TAB>raw heading<TAB>anchor` for this markdown file and exit")
	flag.Parse()

	if err := run(*root, *base, *since, *rev, *format, splitList(*exclude), *headings); err != nil {
		var exit exitError
		if e, ok := err.(exitError); ok {
			exit = e
		} else {
			fmt.Fprintln(os.Stderr, "docslinks:", err)
			exit = 2
		}
		os.Exit(int(exit))
	}
}

type exitError int

func (e exitError) Error() string { return fmt.Sprintf("exit %d", int(e)) }

func run(root, base, since, rev, format string, excluded []string, headingsOf string) error {
	if base != "" && since != "" {
		return fmt.Errorf("-base and -since are exclusive")
	}
	if headingsOf != "" {
		content, err := os.ReadFile(headingsOf)
		if err != nil {
			return err
		}
		for _, h := range docsguard.Parse(headingsOf, content).Headings {
			fmt.Printf("%d\t%s\t%s\n", h.Line, h.Raw, h.Anchor)
		}
		return nil
	}

	if root == "" {
		out, err := git("", "rev-parse", "--show-toplevel")
		if err != nil {
			return fmt.Errorf("not inside a git repository (pass -root): %w", err)
		}
		root = strings.TrimSpace(out)
	}

	var (
		docs   []string
		broken []docsguard.Broken
		err    error
	)
	if rev != "" {
		treeFS := &docsguard.GitTreeFS{Root: root, Rev: rev}
		if docs, err = treeFS.Files(".md"); err != nil {
			return err
		}
		docs = filterExcluded(docs, excluded)
		broken, err = (&docsguard.Checker{FS: treeFS}).Check(docs)
	} else {
		if docs, err = worktreeDocs(root, excluded); err != nil {
			return err
		}
		broken, err = (&docsguard.Checker{FS: docsguard.OSFS{Root: root}}).Check(docs)
	}
	if err != nil {
		return err
	}

	if base == "" && since == "" {
		report(broken, format)
		fmt.Fprintf(os.Stderr, "docslinks: %d broken link(s) in %d file(s); %d markdown file(s) scanned\n",
			len(broken), files(broken), len(docs))
		if len(broken) > 0 {
			return exitError(1)
		}
		return nil
	}

	against := since
	if base != "" {
		mergeBase, err := git(root, "merge-base", base, "HEAD")
		if err != nil {
			return fmt.Errorf("merge-base of %s and HEAD: %w", base, err)
		}
		against = strings.TrimSpace(mergeBase)
	}
	baseFS := &docsguard.GitTreeFS{Root: root, Rev: against}
	baseDocs, err := baseFS.Files(".md")
	if err != nil {
		return err
	}
	baseDocs = filterExcluded(baseDocs, excluded)
	baseBroken, err := (&docsguard.Checker{FS: baseFS}).Check(baseDocs)
	if err != nil {
		return fmt.Errorf("checking the base tree %s: %w", against, err)
	}
	fresh := docsguard.NewSince(broken, baseBroken)
	report(fresh, format)
	fmt.Fprintf(os.Stderr, "docslinks: %d link(s) broken relative to %s (backlog: %d there, %d now)\n",
		len(fresh), against[:min(12, len(against))], len(baseBroken), len(broken))
	if len(fresh) > 0 {
		return exitError(1)
	}
	return nil
}

func report(broken []docsguard.Broken, format string) {
	for _, b := range broken {
		switch format {
		case "github":
			msg := b.Reason
			if b.Nearest != "" {
				msg += " (nearest: #" + b.Nearest + ")"
			}
			// Annotation text is a single line; commas and newlines are escaped
			// per the workflow-command syntax.
			fmt.Printf("::error file=%s,line=%d,title=broken link (%s)::%s\n",
				b.File, b.Line, escapeAnnotation(b.Target), escapeAnnotation(msg))
		default:
			fmt.Println(b.String())
		}
	}
}

func escapeAnnotation(s string) string {
	r := strings.NewReplacer("%", "%25", "\r", "%0D", "\n", "%0A", ":", "%3A", ",", "%2C")
	return r.Replace(s)
}

// worktreeDocs lists the tracked markdown files plus the untracked ones git
// does not ignore, so a new page is checked before it is added.
func worktreeDocs(root string, excluded []string) ([]string, error) {
	tracked, err := git(root, "ls-files", "-z", "--", "*.md", "**/*.md")
	if err != nil {
		return nil, err
	}
	untracked, err := git(root, "ls-files", "-z", "--others", "--exclude-standard", "--", "*.md", "**/*.md")
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var docs []string
	for _, chunk := range []string{tracked, untracked} {
		for _, name := range strings.Split(strings.TrimSuffix(chunk, "\x00"), "\x00") {
			if name == "" || seen[name] {
				continue
			}
			seen[name] = true
			docs = append(docs, name)
		}
	}
	sort.Strings(docs)
	return filterExcluded(docs, excluded), nil
}

func filterExcluded(docs, excluded []string) []string {
	out := docs[:0:0]
	for _, d := range docs {
		if !docsguard.Excluded(d, excluded) {
			out = append(out, d)
		}
	}
	return out
}

func files(broken []docsguard.Broken) int {
	seen := map[string]bool{}
	for _, b := range broken {
		seen[b.File] = true
	}
	return len(seen)
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func git(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	// An inherited GIT_DIR would make the answer about another repository.
	cmd.Env = gitlib.SanitizeEnv(os.Environ())
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return string(out), nil
}
