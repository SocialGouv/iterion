// Command probecheck is the harness of the F20 authoring probe for the
// YAML twin (docs/references/dsl-authoring-probe.md): it reads author
// documents the way `iterion validate` will once the surface ships (lot 5,
// PR C) — the converter, then the compiler on the unit — and prints every
// diagnostic in the validate's own line form, so a probe run on the
// prototype is measured with the same classes as a run on a `.bot`. Not a
// product command: `go run ./pkg/dsl/author/internal/probecheck <file.bot.yaml>…`.
//
// With --write, it prints the document a `.bot` writes as (author.Write):
// how the gallery's shapes are handed to the probe's agent.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/SocialGouv/iterion/pkg/dsl/author"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
	"github.com/SocialGouv/iterion/pkg/dsl/unit"
)

func main() {
	args := os.Args[1:]
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: probecheck <file.bot.yaml>… | probecheck --write <file.bot>…")
		os.Exit(2)
	}
	if args[0] == "--write" {
		os.Exit(write(args[1:]))
	}
	failed := false
	for _, path := range args {
		if check(path) {
			failed = true
		}
	}
	if failed {
		os.Exit(1)
	}
}

// check reads one document and prints its diagnostics; it reports whether
// any is an error.
func check(path string) bool {
	src, err := os.ReadFile(path) // #nosec G304 -- the path the operator named
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", path, err)
		return true
	}
	res := author.Parse(path, src)
	errors, warnings := 0, 0
	for _, d := range res.Diagnostics {
		fmt.Println(d.Error())
		if d.Severity == parser.SeverityError {
			errors++
		} else {
			warnings++
		}
	}
	botPath := strings.TrimSuffix(path, ".yaml")
	if botPath == path {
		botPath = path + ".bot"
	}
	u := unit.LoadDirWithMainAST(botPath, path, res.File, src)
	for _, d := range u.Diagnostics {
		fmt.Println(d.Error())
		if d.Severity == parser.SeverityError {
			errors++
		} else {
			warnings++
		}
	}
	// Compiled whatever the parse said, as `iterion validate` compiles a
	// .bot the parser recovered on: the first draft's compile diagnostics
	// count on both surfaces alike.
	if u.Merged != nil && len(u.Merged.Workflows) > 0 {
		cr := ir.Compile(u.Merged)
		for _, d := range cr.Diagnostics {
			fmt.Println(d.Error())
			if d.Severity == ir.SeverityError {
				errors++
			} else {
				warnings++
			}
		}
	} else if errors == 0 {
		fmt.Printf("%s: no workflow found\n", path)
		errors++
	}
	verdict := "OK"
	if errors > 0 {
		verdict = "INVALID"
	}
	fmt.Printf("%s: %s (%d error(s), %d warning(s))\n", filepath.Base(path), verdict, errors, warnings)
	return errors > 0
}

// write prints the author document of each .bot named.
func write(paths []string) int {
	code := 0
	for _, path := range paths {
		src, err := os.ReadFile(path) // #nosec G304 -- the path the operator named
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", path, err)
			code = 1
			continue
		}
		pr := parser.Parse(path, string(src))
		hasErrors := false
		for _, d := range pr.Diagnostics {
			if d.Severity == parser.SeverityError {
				fmt.Fprintln(os.Stderr, d.Error())
				hasErrors = true
			}
		}
		// A refused source — not UTF-8 (E006), or anything the parser cannot
		// shape — comes back with no program at all (File nil): there is
		// nothing to write, the errors above are the whole answer.
		if hasErrors || pr.File == nil {
			code = 1
			continue
		}
		out, err := author.Write(pr.File)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", path, err)
			code = 1
			continue
		}
		if len(paths) > 1 {
			fmt.Printf("# ---- %s\n", path)
		}
		os.Stdout.Write(out)
	}
	return code
}
