package git

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Each spelling of a git spawn the recognizer must see, and the look-alikes it
// must not count, as one package spread over three files. A site is
// identified by the comment closing its line, so the cases read as source.
func TestGitSpawnSitesRecognisesEveryResolvedSpelling(t *testing.T) {
	sources := map[string]string{
		"a.go": `package p

import (
	"context"
	"os/exec"

	"github.com/SocialGouv/iterion/internal/envtrust"
)

type runner struct{ bin string }
type unkeyedRunner struct{ bin string }
type assignedRunner struct{ bin string }
type docker struct{ bin string }
type sib struct{ path string }

const gitName = "git"

func literal() { exec.Command("git", "status") } // LITERAL
func raw()     { exec.Command(` + "`git`" + `, "status") } // RAW

func direct() {
	bin, err := envtrust.LookPath("git")
	_ = err
	exec.Command(bin, "status") // DIRECT
}

func copied() {
	p, _ := exec.LookPath("git")
	q := p
	exec.Command(q, "status") // COPIED
}

func declared() {
	var p, _ = exec.LookPath("git")
	exec.Command(p, "status") // DECLARED
}

func withContext(ctx context.Context, ctxs []context.Context) {
	p, _ := exec.LookPath("git")
	exec.CommandContext(ctx, p, "status")         // CONTEXT
	exec.CommandContext(ctxs[0], "git", "status") // CTX-INDEX
}

func closure() {
	p, _ := exec.LookPath("git")
	f := func() { exec.Command(p, "status") } // CLOSURE
	f()
}

func field() {
	p, _ := exec.LookPath("git")
	r := runner{bin: p}
	exec.Command(r.bin, "status") // FIELD
}

func unkeyed() {
	p, _ := exec.LookPath("git")
	r := unkeyedRunner{p}
	exec.Command(r.bin, "status") // UNKEYED
}

func assigned() {
	p, _ := exec.LookPath("git")
	var r assignedRunner
	r.bin = p
	exec.Command(r.bin, "status") // ASSIGNED-FIELD
}

func gitBinary() (string, error) { return envtrust.LookPath("git") }

func helper() {
	bin, _ := gitBinary()
	exec.Command(bin, "status") // HELPER
}

func gitPath() string {
	p, _ := exec.LookPath("git")
	return p
}

func helperValue() { exec.Command(gitPath(), "status") } // HELPER-VALUE

// early is declared before the function it returns from: only a second
// marking pass sees it.
func early() string { return late() }

func late() string {
	p, _ := exec.LookPath("git")
	return p
}

func chain()    { exec.Command(early(), "status") }   // CHAIN
func constant() { exec.Command(gitName, "status") } // CONST

func newSib() sib {
	p, _ := exec.LookPath("git")
	return sib{path: p}
}

func otherTool() {
	bin, _ := exec.LookPath("go")
	exec.Command(bin, "version") // OTHER-TOOL
}

func mapKey() {
	k := "x"
	p, _ := exec.LookPath("git")
	m := map[string]string{k: p}
	_ = m
	exec.Command(k, "status") // MAP-KEY
}

func sameFieldNameOtherType() {
	d := docker{bin: "/usr/bin/docker"}
	exec.Command(d.bin, "ps") // NAME-COLLISION
}

func prose() {
	// exec.Command("git", "status") is how not to do it.
}

type tool struct{ binary string }

func table() {
	t := tool{binary: "git"}
	exec.Command(t.binary, "--version") // TOOL-TABLE
}

func gitNaked() (p string) {
	p, _ = exec.LookPath("git")
	return
}

func naked() { exec.Command(gitNaked(), "status") } // NAKED-RETURN

func notGit() string {
	f := func() string {
		p, _ := exec.LookPath("git")
		return p
	}
	_ = f
	return "ls"
}

func notEnclosing() { exec.Command(notGit()) } // NOT-ENCLOSING
`,
		// The struct is declared and filled in a.go, spawned here.
		"b.go": `package p

import "os/exec"

func (s sib) run() { exec.Command(s.path, "status") } // SIBLING
`,
		"c.go": `package p

import osexec "os/exec"

func alias() { osexec.Command("git", "status") } // ALIAS

func aliasResolved() {
	p, _ := osexec.LookPath("git")
	osexec.Command(p, "status") // ALIAS-RESOLVED
}
`,
	}
	dir := t.TempDir()
	byPath := map[string][]byte{}
	for name, src := range sources {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
			t.Fatal(err)
		}
		byPath[path] = []byte(src)
	}
	sites, err := gitSpawnSitesInDir(byPath)
	if err != nil {
		t.Fatal(err)
	}
	literals := map[string]bool{"LITERAL": true, "RAW": true, "CTX-INDEX": true, "ALIAS": true}
	got := map[string]bool{}
	for path, list := range sites {
		src := string(byPath[path])
		for _, s := range list {
			line := src[s.start : s.start+strings.Index(src[s.start:], "\n")]
			tag := line[strings.LastIndex(line, "// ")+3:]
			if got[tag] {
				t.Errorf("site %s counted twice", tag)
			}
			got[tag] = true
			if want := !literals[tag]; s.resolved != want {
				t.Errorf("site %s: resolved=%v, want %v", tag, s.resolved, want)
			}
		}
	}
	want := []string{"LITERAL", "RAW", "DIRECT", "COPIED", "DECLARED", "CONTEXT", "CTX-INDEX", "CLOSURE", "FIELD",
		"UNKEYED", "ASSIGNED-FIELD", "HELPER", "HELPER-VALUE", "CHAIN", "CONST", "SIBLING", "ALIAS", "ALIAS-RESOLVED",
		"NAKED-RETURN"}
	for _, tag := range want {
		if !got[tag] {
			t.Errorf("git spawn %s not recognised", tag)
		}
	}
	for _, tag := range []string{"OTHER-TOOL", "MAP-KEY", "NAME-COLLISION", "TOOL-TABLE", "NOT-ENCLOSING"} {
		if got[tag] {
			t.Errorf("%s counted as git: another tool's path, a map key, another type's field of the same name, a table of tools naming git, a closure's return are not git", tag)
		}
	}
	if len(got) != len(want) {
		t.Errorf("found %d sites %v, want %d (the prose mention is not a call)", len(got), got, len(want))
	}

	// A directory that never spells git is not even parsed.
	quiet := map[string][]byte{filepath.Join(dir, "q.go"): []byte("package q\n\nthis does not parse\n")}
	if s, err := gitSpawnSitesInDir(quiet); err != nil || len(s) != 0 {
		t.Errorf("a directory that never spells git was read: sites %v, err %v", s, err)
	}
}

// What the directory scan decides before any type check, and what it reads
// per package, per configuration and production first — each on its own.
func TestGitSpawnSitesInDirScope(t *testing.T) {
	scan := func(t *testing.T, files map[string]string) (map[string][]gitSite, map[string][]byte, error) {
		t.Helper()
		dir := t.TempDir()
		sources := map[string][]byte{}
		for name, src := range files {
			path := filepath.Join(dir, name)
			if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
				t.Fatal(err)
			}
			sources[path] = []byte(src)
		}
		sites, err := gitSpawnSitesInDir(sources)
		return sites, sources, err
	}
	tags := func(sites map[string][]gitSite, sources map[string][]byte) map[string]bool {
		got := map[string]bool{}
		for path, list := range sites {
			src := string(sources[path])
			for _, s := range list {
				line := src[s.start : s.start+strings.Index(src[s.start:], "\n")]
				got[line[strings.LastIndex(line, "// ")+3:]] = true
			}
		}
		return got
	}
	t.Run("a dir that spells git but imports no os/exec is not parsed", func(t *testing.T) {
		sites, _, err := scan(t, map[string]string{"a.go": "package p\n\nconst name = \"git\"\n", "b.go": "not go at all"})
		if err != nil || len(sites) != 0 {
			t.Errorf("sites %v, err %v: the prefilter read a dir that cannot spawn", sites, err)
		}
	})
	t.Run("a raw-string git alone passes the prefilter", func(t *testing.T) {
		sites, sources, err := scan(t, map[string]string{"a.go": "package p\n\nimport \"os/exec\"\n\nfunc f() { exec.Command(`git`, \"status\") } // RAW-ONLY\n"})
		if err != nil || !tags(sites, sources)["RAW-ONLY"] {
			t.Errorf("sites %v, err %v: a raw-string git spawn was not seen", sites, err)
		}
	})
	t.Run("a parse error in a dir that can spawn git is reported", func(t *testing.T) {
		_, _, err := scan(t, map[string]string{"a.go": "package p\n\nimport \"os/exec\"\n\nfunc f() { exec.Command(\"git\") }\n", "b.go": "not go at all"})
		if err == nil {
			t.Error("a file that does not parse was skipped silently")
		}
	})
	t.Run("the external test package is checked apart", func(t *testing.T) {
		sites, sources, err := scan(t, map[string]string{
			"x.go":      "package p\n\nimport \"os/exec\"\n\nvar bin = \"ls\"\n\nfunc f() { exec.Command(bin) } // PROD-BIN\n",
			"x_test.go": "package p_test\n\nimport \"os/exec\"\n\nvar bin, _ = exec.LookPath(\"git\")\n\nfunc g() { exec.Command(bin) } // XTEST-BIN\n",
		})
		got := tags(sites, sources)
		if err != nil || !got["XTEST-BIN"] || got["PROD-BIN"] {
			t.Errorf("sites %v, err %v: want the external test's git spawn and not the production one", got, err)
		}
	})
	t.Run("a test handing git to a production runner leaves the runner alone", func(t *testing.T) {
		sites, sources, err := scan(t, map[string]string{
			"r.go":      "package p\n\nimport \"os/exec\"\n\ntype runner struct{ bin string }\n\nfunc (r runner) run() { exec.Command(r.bin) } // PROD-RUNNER\n",
			"r_test.go": "package p\n\nimport \"os/exec\"\n\nfunc h() { p, _ := exec.LookPath(\"git\"); runner{bin: p}.run(); exec.Command(p) } // TEST-SPAWN\n",
		})
		got := tags(sites, sources)
		if err != nil || got["PROD-RUNNER"] || !got["TEST-SPAWN"] {
			t.Errorf("sites %v, err %v: want the test's own spawn, not the production runner", got, err)
		}
	})
	t.Run("a lookup only another platform builds is still read", func(t *testing.T) {
		sites, sources, err := scan(t, map[string]string{
			"git_other.go":   "//go:build !windows\n\npackage p\n\nvar gitBin = \"x\"\n",
			"git_windows.go": "//go:build windows\n\npackage p\n\nimport \"os/exec\"\n\nvar gitBin, _ = exec.LookPath(\"git\")\n\nfunc run() { exec.Command(gitBin, \"status\") } // WINDOWS-ONLY\n",
		})
		if err != nil || !tags(sites, sources)["WINDOWS-ONLY"] {
			t.Errorf("sites %v, err %v: the windows-only lookup was not read", sites, err)
		}
	})
	// A typed package: its production file spawns a command by value.
	typedProd := "package p\n\nimport \"os/exec\"\n\nfunc tool(name string) { exec.Command(name) }\n"
	t.Run("a file under a package's own build tag is read, literal and value", func(t *testing.T) {
		sites, sources, err := scan(t, map[string]string{
			"p.go": typedProd,
			"p_clilive_test.go": "//go:build clilive\n\npackage p\n\nimport \"os/exec\"\n\nfunc a() { exec.Command(\"git\", \"status\") } // TAGGED-LITERAL\n\n" +
				"func b() { g, _ := exec.LookPath(\"git\"); exec.Command(g, \"status\") } // TAGGED-VALUE\n",
		})
		got := tags(sites, sources)
		if err != nil || !got["TAGGED-LITERAL"] || !got["TAGGED-VALUE"] {
			t.Errorf("sites %v, err %v: a clilive file of a typed package went unread", got, err)
		}
	})
	t.Run("a platform file spawning a value a shared file defines is read", func(t *testing.T) {
		sites, sources, err := scan(t, map[string]string{
			"shared.go":      "package p\n\nimport \"os/exec\"\n\nvar gitBin, _ = exec.LookPath(\"git\")\n",
			"run_windows.go": "package p\n\nimport \"os/exec\"\n\nfunc run() { exec.Command(gitBin, \"status\") } // WINDOWS-SPAWN\n",
		})
		if err != nil || !tags(sites, sources)["WINDOWS-SPAWN"] {
			t.Errorf("sites %v, err %v: a windows file spawning the shared git value went unread", sites, err)
		}
	})
	t.Run("a cgo file is read", func(t *testing.T) {
		sites, sources, err := scan(t, map[string]string{
			"p.go":     typedProd,
			"p_cgo.go": "//go:build cgo\n\npackage p\n\nimport \"os/exec\"\n\nfunc c() { g, _ := exec.LookPath(\"git\"); exec.Command(g, \"status\") } // CGO\n",
		})
		if err != nil || !tags(sites, sources)["CGO"] {
			t.Errorf("sites %v, err %v: a cgo file of a typed package went unread", sites, err)
		}
	})
	t.Run("a cgo-off file is type-checked", func(t *testing.T) {
		sites, sources, err := scan(t, map[string]string{
			"p.go":       typedProd,
			"p_nocgo.go": "//go:build !cgo\n\npackage p\n\nimport \"os/exec\"\n\nfunc n() { g, _ := exec.LookPath(\"git\"); exec.Command(g, \"status\") } // NO-CGO\n",
		})
		if err != nil || !tags(sites, sources)["NO-CGO"] {
			t.Errorf("sites %v, err %v: a !cgo file's value spawn went unread", sites, err)
		}
	})
	t.Run("a package tag on another platform, and a tag negated, are type-checked", func(t *testing.T) {
		sites, sources, err := scan(t, map[string]string{
			"p.go":                 typedProd,
			"p_desktop_windows.go": "//go:build desktop\n\npackage p\n\nimport \"os/exec\"\n\nfunc w() { g, _ := exec.LookPath(\"git\"); exec.Command(g, \"status\") } // DESKTOP-WINDOWS\n",
			"p_release.go":         "//go:build desktop && !dev\n\npackage p\n\nimport \"os/exec\"\n\nfunc r() { g, _ := exec.LookPath(\"git\"); exec.Command(g, \"status\") } // RELEASE\n",
		})
		got := tags(sites, sources)
		if err != nil || !got["DESKTOP-WINDOWS"] || !got["RELEASE"] {
			t.Errorf("sites %v, err %v: a tagged windows file or a `desktop && !dev` file went unread", got, err)
		}
	})
	t.Run("every combination a file's constraint asks for is built", func(t *testing.T) {
		val := "\n\npackage p\n\nimport \"os/exec\"\n\nfunc %s() { g, _ := exec.LookPath(\"git\"); exec.Command(g, \"status\") } // %s\n"
		sites, sources, err := scan(t, map[string]string{
			"p.go":                 typedProd,
			"p_tagcgo.go":          "//go:build desktop && cgo" + fmt.Sprintf(val, "tc", "TAG-CGO"),
			"p_cgo_darwin.go":      "//go:build cgo" + fmt.Sprintf(val, "cd", "CGO-DARWIN"),
			"p_release_windows.go": "//go:build desktop && !dev" + fmt.Sprintf(val, "rw", "RELEASE-WINDOWS"),
			"p_dev.go":             "//go:build dev\n\npackage p\n",
		})
		got := tags(sites, sources)
		for _, tag := range []string{"TAG-CGO", "CGO-DARWIN", "RELEASE-WINDOWS"} {
			if err != nil || !got[tag] {
				t.Errorf("sites %v, err %v: %s went unread", got, err, tag)
			}
		}
	})
	t.Run("a lookup and a spawn first covered by different builds are checked together", func(t *testing.T) {
		for want, files := range map[string]map[string]string{
			"TAG-X-WINDOWS": {
				"bin_windows.go": "package p\n\nimport \"os/exec\"\n\nvar gitBin, _ = exec.LookPath(\"git\")\n",
				"bin_other.go":   "//go:build !windows\n\npackage p\n\nvar gitBin = \"sh\"\n",
				"run_desktop.go": "//go:build desktop\n\npackage p\n\nimport \"os/exec\"\n\nfunc run() { exec.Command(gitBin, \"status\") } // TAG-X-WINDOWS\n",
				"run_other.go":   "//go:build !desktop\n\npackage p\n\nfunc run() {}\n",
			},
			"TAG-X-TAG": {
				"bin_b.go":    "//go:build b\n\npackage p\n\nimport \"os/exec\"\n\nvar gitBin, _ = exec.LookPath(\"git\")\n",
				"bin_notb.go": "//go:build !b\n\npackage p\n\nvar gitBin = \"sh\"\n",
				"run_a.go":    "//go:build a\n\npackage p\n\nimport \"os/exec\"\n\nfunc run() { exec.Command(gitBin, \"status\") } // TAG-X-TAG\n",
				"run_nota.go": "//go:build !a\n\npackage p\n\nfunc run() {}\n",
			},
		} {
			sites, sources, err := scan(t, files)
			if err != nil || !tags(sites, sources)[want] {
				t.Errorf("sites %v, err %v: %s went unread", sites, err, want)
			}
		}
	})
	t.Run("exclusive tags set together do not lend one build's git to the other", func(t *testing.T) {
		sites, sources, err := scan(t, map[string]string{
			"a_git.go": "//go:build b\n\npackage p\n\nimport \"os/exec\"\n\nvar tool, _ = exec.LookPath(\"git\")\n",
			"b_sh.go":  "//go:build a\n\npackage p\n\nvar tool = \"sh\"\n",
			"run.go":   "//go:build a\n\npackage p\n\nimport \"os/exec\"\n\nfunc run() { exec.Command(tool, \"-c\", \"true\") } // SH-ONLY\n",
		})
		if err != nil || tags(sites, sources)["SH-ONLY"] {
			t.Errorf("sites %v, err %v: a spawn of sh was taken for git through a set no build compiles", sites, err)
		}
	})
	t.Run("two imports ending alike do not blind the package", func(t *testing.T) {
		sites, sources, err := scan(t, map[string]string{
			"p.go":      "package p\n\nimport \"os/exec\"\n\nvar gitBin, _ = exec.LookPath(\"git\")\n\nfunc run() { exec.Command(gitBin, \"status\") } // V2-SAFE\n",
			"p_test.go": "package p\n\nimport (\n\t\"math/rand/v2\"\n\t\"github.com/alicebob/miniredis/v2\"\n)\n\nvar _, _ = rand.IntN, miniredis.Run\n",
		})
		if err != nil || !tags(sites, sources)["V2-SAFE"] {
			t.Errorf("sites %v, err %v: an import collision of fake package names was taken for a twin, and the package read without types", sites, err)
		}
	})
	t.Run("exclusive method twins do not lend one build's git to the other", func(t *testing.T) {
		sites, sources, err := scan(t, map[string]string{
			"r.go":     "package p\n\ntype R struct{}\n",
			"a_git.go": "//go:build b\n\npackage p\n\nimport \"os/exec\"\n\nfunc (R) tool() string { p, _ := exec.LookPath(\"git\"); return p }\n",
			"b_sh.go":  "//go:build a\n\npackage p\n\nfunc (R) tool() string { return \"sh\" }\n",
			"run.go":   "//go:build a\n\npackage p\n\nimport \"os/exec\"\n\nfunc run() { exec.Command(R{}.tool(), \"-c\", \"true\") } // METHOD-SH\n",
		})
		if err != nil || tags(sites, sources)["METHOD-SH"] {
			t.Errorf("sites %v, err %v: a method twin made a spawn of sh look like git", sites, err)
		}
	})
	t.Run("a file needing two tags of three gets its own build, without the exclusive third", func(t *testing.T) {
		sites, sources, err := scan(t, map[string]string{
			"p.go":      typedProd,
			"a.go":      "package p\n\ntype A struct{}\n",
			"m_dev.go":  "//go:build desktop && dev\n\npackage p\n\nfunc (A) mode() string { return \"dev\" }\n",
			"m_prod.go": "//go:build desktop && production\n\npackage p\n\nfunc (A) mode() string { return \"prod\" }\n",
			"top.go":    "//go:build desktop && production\n\npackage p\n\nimport \"os/exec\"\n\nfunc top() { g, _ := exec.LookPath(\"git\"); exec.Command(g, \"status\") } // THREE-TAGS\n",
		})
		if err != nil || !tags(sites, sources)["THREE-TAGS"] {
			t.Errorf("sites %v, err %v: a `desktop && production` file was read only beside its exclusive dev twin", sites, err)
		}
	})
	t.Run("a twin read literally stays literal when an import collision follows", func(t *testing.T) {
		sites, sources, err := scan(t, map[string]string{
			"r.go":       "package p\n\ntype R struct{}\n",
			"a_git.go":   "//go:build b\n\npackage p\n\nimport \"os/exec\"\n\nfunc (R) tool() string { p, _ := exec.LookPath(\"git\"); return p }\n",
			"b_sh.go":    "//go:build a\n\npackage p\n\nfunc (R) tool() string { return \"sh\" }\n",
			"run.go":     "//go:build a\n\npackage p\n\nimport \"os/exec\"\n\nfunc run() { exec.Command(R{}.tool(), \"-c\", \"true\") } // TWIN-THEN-IMPORTS\n",
			"z_v2.go":    "package p\n\nimport (\n\t\"math/rand/v2\"\n\t\"github.com/alicebob/miniredis/v2\"\n)\n\nvar _, _ = rand.IntN, miniredis.Run\n",
			"z_sh_go.go": "//go:build b\n\npackage p\n",
		})
		if err != nil || tags(sites, sources)["TWIN-THEN-IMPORTS"] {
			t.Errorf("sites %v, err %v: an import collision after a real twin undid the twin's verdict", sites, err)
		}
	})
	t.Run("an aliased import colliding with another is not a twin", func(t *testing.T) {
		sites, sources, err := scan(t, map[string]string{
			"p.go":      "package p\n\nimport \"os/exec\"\n\nvar gitBin, _ = exec.LookPath(\"git\")\n\nfunc run() { exec.Command(gitBin, \"status\") } // V2-ALIAS\n",
			"p_test.go": "package p\n\nimport (\n\tv2 \"math/rand/v2\"\n\t\"github.com/alicebob/miniredis/v2\"\n)\n\nvar _, _ = v2.IntN, miniredis.Run\n",
		})
		if err != nil || !tags(sites, sources)["V2-ALIAS"] {
			t.Errorf("sites %v, err %v: an aliased import collision blinded the package", sites, err)
		}
	})
	t.Run("method twins sorted the other way still read literally", func(t *testing.T) {
		sites, sources, err := scan(t, map[string]string{
			"r.go":     "package p\n\ntype R struct{}\n",
			"a_sh.go":  "//go:build a\n\npackage p\n\nfunc (R) tool() string { return \"sh\" }\n",
			"b_git.go": "//go:build b\n\npackage p\n\nimport \"os/exec\"\n\nfunc (R) tool() string { p, _ := exec.LookPath(\"git\"); return p }\n",
			"run.go":   "//go:build a\n\npackage p\n\nimport \"os/exec\"\n\nfunc run() { exec.Command(R{}.tool(), \"-c\", \"true\") } // METHOD-SH-REVERSED\n",
		})
		if err != nil || tags(sites, sources)["METHOD-SH-REVERSED"] {
			t.Errorf("sites %v, err %v: a method twin made a spawn of sh look like git", sites, err)
		}
	})
	t.Run("a lookup and a spawn under different tags meet in a build of their own", func(t *testing.T) {
		sites, sources, err := scan(t, map[string]string{
			"p.go":         typedProd,
			"a.go":         "package p\n\ntype A struct{}\n",
			"m_dev.go":     "//go:build dev\n\npackage p\n\nfunc (A) mode() string { return \"dev\" }\n",
			"m_prod.go":    "//go:build production\n\npackage p\n\nfunc (A) mode() string { return \"prod\" }\n",
			"bin_prod.go":  "//go:build production\n\npackage p\n\nimport \"os/exec\"\n\nvar gitBin, _ = exec.LookPath(\"git\")\n",
			"bin_other.go": "//go:build !production\n\npackage p\n\nvar gitBin = \"sh\"\n",
			"run.go":       "//go:build desktop\n\npackage p\n\nimport \"os/exec\"\n\nfunc run() { exec.Command(gitBin, \"status\") } // PAIR-PROD-DESKTOP\n",
		})
		if err != nil || !tags(sites, sources)["PAIR-PROD-DESKTOP"] {
			t.Errorf("sites %v, err %v: a production lookup and a desktop spawn were never checked together", sites, err)
		}
	})
	t.Run("a variable twin keeps its verdict through a later import collision", func(t *testing.T) {
		sites, sources, err := scan(t, map[string]string{
			"a_git.go": "//go:build b\n\npackage p\n\nimport \"os/exec\"\n\nvar tool, _ = exec.LookPath(\"git\")\n",
			"b_sh.go":  "//go:build a\n\npackage p\n\nvar tool = \"sh\"\n",
			"run.go":   "//go:build a\n\npackage p\n\nimport \"os/exec\"\n\nfunc run() { exec.Command(tool, \"-c\", \"true\") } // VAR-TWIN-THEN-IMPORTS\n",
			"z_v2.go":  "package p\n\nimport (\n\t\"math/rand/v2\"\n\t\"github.com/alicebob/miniredis/v2\"\n)\n\nvar _, _ = rand.IntN, miniredis.Run\n",
		})
		if err != nil || tags(sites, sources)["VAR-TWIN-THEN-IMPORTS"] {
			t.Errorf("sites %v, err %v: a later import collision undid a variable twin's verdict", sites, err)
		}
	})
	t.Run("an alias on the colliding import is not a twin either", func(t *testing.T) {
		sites, sources, err := scan(t, map[string]string{
			"p.go":      "package p\n\nimport \"os/exec\"\n\nvar gitBin, _ = exec.LookPath(\"git\")\n\nfunc run() { exec.Command(gitBin, \"status\") } // V2-SECOND-ALIAS\n",
			"p_test.go": "package p\n\nimport (\n\t\"math/rand/v2\"\n\tv2 \"github.com/alicebob/miniredis/v2\"\n)\n\nvar _, _ = rand.IntN, v2.Run\n",
		})
		if err != nil || !tags(sites, sources)["V2-SECOND-ALIAS"] {
			t.Errorf("sites %v, err %v: an aliased colliding import blinded the package", sites, err)
		}
	})
	t.Run("a file naming git and spawning it, held only beside its exclusive twin, gets its own build", func(t *testing.T) {
		sites, sources, err := scan(t, map[string]string{
			"a.go":      "package p\n\ntype A struct{}\n",
			"m_dev.go":  "//go:build desktop && dev\n\npackage p\n\nfunc (A) mode() string { return \"dev\" }\n",
			"m_prod.go": "//go:build desktop && production\n\npackage p\n\nfunc (A) mode() string { return \"prod\" }\n",
			"top.go":    "//go:build desktop && production\n\npackage p\n\nimport \"os/exec\"\n\nfunc top() { g, _ := exec.LookPath(\"git\"); exec.Command(g, \"status\") } // SELF-PAIR\n",
		})
		if err != nil || !tags(sites, sources)["SELF-PAIR"] {
			t.Errorf("SELF-PAIR sites %v, err %v", sites, err)
		}
	})
	t.Run("a single-tag build that twins on linux is no cover: darwin compiles it", func(t *testing.T) {
		sites, sources, err := scan(t, map[string]string{
			"x_linux.go": "package p\n\nvar mode = 1\n",
			"x_a.go":     "//go:build a\n\npackage p\n\nvar mode = 2\n",
			"top.go":     "//go:build a\n\npackage p\n\nimport \"os/exec\"\n\nfunc top() { g, _ := exec.LookPath(\"git\"); exec.Command(g, \"status\"); _ = mode } // PLATFORM-TWIN\n",
		})
		if err != nil || !tags(sites, sources)["PLATFORM-TWIN"] {
			t.Errorf("PLATFORM-TWIN sites %v, err %v", sites, err)
		}
	})
	t.Run("a pair build that twins on linux is skipped for darwin's", func(t *testing.T) {
		sites, sources, err := scan(t, map[string]string{
			"x_linux.go":   "package p\n\nvar mode = 1\n",
			"x_prod.go":    "//go:build production\n\npackage p\n\nvar mode = 2\n",
			"bin_prod.go":  "//go:build production\n\npackage p\n\nimport \"os/exec\"\n\nvar gitBin, _ = exec.LookPath(\"git\")\n",
			"bin_other.go": "//go:build !production\n\npackage p\n\nvar gitBin = \"sh\"\n",
			"run.go":       "//go:build desktop\n\npackage p\n\nimport \"os/exec\"\n\nfunc run() { exec.Command(gitBin, \"status\"); _ = mode } // PAIR-PLATFORM-TWIN\n",
		})
		if err != nil || !tags(sites, sources)["PAIR-PLATFORM-TWIN"] {
			t.Errorf("PAIR-PLATFORM-TWIN sites %v, err %v", sites, err)
		}
	})
	t.Run("pointer-receiver twins are twins", func(t *testing.T) {
		sites, sources, err := scan(t, map[string]string{
			"p.go":      "package p\n\nimport \"os/exec\"\n\nfunc tool(name string) { exec.Command(name) }\n",
			"a.go":      "package p\n\ntype A struct{}\n",
			"m_dev.go":  "//go:build desktop && dev\n\npackage p\n\nfunc (a *A) mode() string { return \"dev\" }\n",
			"m_prod.go": "//go:build desktop && production\n\npackage p\n\nfunc (a *A) mode() string { return \"prod\" }\n",
			"top.go":    "//go:build desktop && production\n\npackage p\n\nimport \"os/exec\"\n\nfunc top() { g, _ := exec.LookPath(\"git\"); exec.Command(g, \"status\") } // POINTER-TWINS\n",
		})
		if err != nil || !tags(sites, sources)["POINTER-TWINS"] {
			t.Errorf("POINTER-TWINS sites %v, err %v", sites, err)
		}
	})
	t.Run("a twin through an alias receiver", func(t *testing.T) {
		sites, sources, err := scan(t, map[string]string{
			"p.go":      "package p\n\nimport \"os/exec\"\n\nfunc tool(name string) { exec.Command(name) }\n",
			"a.go":      "package p\n\ntype A struct{}\n\ntype B = A\n",
			"m_dev.go":  "//go:build desktop && dev\n\npackage p\n\nfunc (A) mode() string { return \"dev\" }\n",
			"m_prod.go": "//go:build desktop && production\n\npackage p\n\nfunc (B) mode() string { return \"prod\" }\n",
			"top.go":    "//go:build desktop && production\n\npackage p\n\nimport \"os/exec\"\n\nfunc top() { g, _ := exec.LookPath(\"git\"); exec.Command(g, \"status\") } // ALIAS-RECEIVER\n",
		})
		if err != nil || !tags(sites, sources)["ALIAS-RECEIVER"] {
			t.Errorf("ALIAS-RECEIVER sites %v, err %v", sites, err)
		}
	})
	t.Run("a twin through a parenthesised receiver", func(t *testing.T) {
		sites, sources, err := scan(t, map[string]string{
			"p.go":      "package p\n\nimport \"os/exec\"\n\nfunc tool(name string) { exec.Command(name) }\n",
			"a.go":      "package p\n\ntype A struct{}\n",
			"m_dev.go":  "//go:build desktop && dev\n\npackage p\n\nfunc (a (A)) mode() string { return \"dev\" }\n",
			"m_prod.go": "//go:build desktop && production\n\npackage p\n\nfunc (a A) mode() string { return \"prod\" }\n",
			"top.go":    "//go:build desktop && production\n\npackage p\n\nimport \"os/exec\"\n\nfunc top() { g, _ := exec.LookPath(\"git\"); exec.Command(g, \"status\") } // PAREN-RECEIVER\n",
		})
		if err != nil || !tags(sites, sources)["PAREN-RECEIVER"] {
			t.Errorf("PAREN-RECEIVER sites %v, err %v", sites, err)
		}
	})
	t.Run("two blank type declarations are no twin", func(t *testing.T) {
		sites, sources, err := scan(t, map[string]string{
			"p.go":      "package p\n\nimport \"os/exec\"\n\nfunc tool(name string) { exec.Command(name) }\n",
			"a.go":      "package p\n\ntype A struct{}\n\ntype _ int\n\ntype _ string\n",
			"m_dev.go":  "//go:build desktop && dev\n\npackage p\n\nfunc (A) mode() string { return \"dev\" }\n",
			"m_prod.go": "//go:build desktop && production\n\npackage p\n\nfunc (A) mode() string { return \"prod\" }\n",
			"top.go":    "//go:build desktop && production\n\npackage p\n\nimport \"os/exec\"\n\nfunc top() { g, _ := exec.LookPath(\"git\"); exec.Command(g, \"status\") } // BLANK-TYPES\n",
		})
		if err != nil || !tags(sites, sources)["BLANK-TYPES"] {
			t.Errorf("BLANK-TYPES sites %v, err %v", sites, err)
		}
	})
	t.Run("a twin through an alias of the pointer receiver", func(t *testing.T) {
		sites, sources, err := scan(t, map[string]string{
			"p.go":      "package p\n\nimport \"os/exec\"\n\nfunc tool(name string) { exec.Command(name) }\n",
			"a.go":      "package p\n\ntype A struct{}\n\ntype P = *A\n",
			"m_dev.go":  "//go:build desktop && dev\n\npackage p\n\nfunc (*A) mode() string { return \"dev\" }\n",
			"m_prod.go": "//go:build desktop && production\n\npackage p\n\nfunc (P) mode() string { return \"prod\" }\n",
			"top.go":    "//go:build desktop && production\n\npackage p\n\nimport \"os/exec\"\n\nfunc top() { g, _ := exec.LookPath(\"git\"); exec.Command(g, \"status\") } // ALIAS-POINTER-RECEIVER\n",
		})
		if err != nil || !tags(sites, sources)["ALIAS-POINTER-RECEIVER"] {
			t.Errorf("ALIAS-POINTER-RECEIVER sites %v, err %v", sites, err)
		}
	})
	t.Run("a method the other tag adds at depth zero does not hide the promoted one", func(t *testing.T) {
		sites, sources, err := scan(t, map[string]string{
			"a.go":      "package p\n\ntype Inner struct{}\n\ntype A struct{ Inner }\n",
			"m_dev.go":  "//go:build desktop && dev\n\npackage p\n\nfunc (Inner) bin() string { return \"git\" }\n",
			"m_prod.go": "//go:build desktop && production\n\npackage p\n\nfunc (A) bin() string { return \"sh\" }\n",
			"top.go":    "//go:build desktop && dev\n\npackage p\n\nimport \"os/exec\"\n\nfunc top() { var a A; exec.Command(a.bin(), \"status\") } // DEPTH-SHADOW\n",
		})
		if err != nil || !tags(sites, sources)["DEPTH-SHADOW"] {
			t.Errorf("DEPTH-SHADOW sites %v, err %v", sites, err)
		}
	})
	t.Run("a selector the mixed set makes ambiguous is read in the real build", func(t *testing.T) {
		sites, sources, err := scan(t, map[string]string{
			"a.go":      "package p\n\ntype D struct{}\n\ntype P struct{}\n\ntype A struct {\n\tD\n\tP\n}\n",
			"m_dev.go":  "//go:build desktop && dev\n\npackage p\n\nfunc (D) bin() string { return \"git\" }\n",
			"m_prod.go": "//go:build desktop && production\n\npackage p\n\nfunc (P) bin() string { return \"sh\" }\n",
			"top.go":    "//go:build desktop && dev\n\npackage p\n\nimport \"os/exec\"\n\nfunc top() { var a A; exec.Command(a.bin(), \"status\") } // AMBIGUOUS\n",
		})
		if err != nil || !tags(sites, sources)["AMBIGUOUS"] {
			t.Errorf("AMBIGUOUS sites %v, err %v", sites, err)
		}
	})
	t.Run("a pair that meets only in the mixed set gets its own build", func(t *testing.T) {
		sites, sources, err := scan(t, map[string]string{
			"a.go":      "package p\n\ntype D struct{}\n\ntype P struct{}\n\ntype A struct {\n\tD\n\tP\n}\n",
			"m_none.go": "//go:build !dev && !production\n\npackage p\n\nfunc (D) bin() string { return \"sh\" }\n",
			"m_dev.go":  "//go:build dev\n\npackage p\n\nfunc (D) bin() string { return \"sh\" }\n",
			"m_prod.go": "//go:build production\n\npackage p\n\nfunc (P) bin() string { return \"git\" }\n",
			"run.go":    "//go:build desktop\n\npackage p\n\nimport \"os/exec\"\n\nfunc run() { var a A; exec.Command(a.bin(), \"status\") } // PAIR-AMBIGUOUS\n",
		})
		if err != nil || !tags(sites, sources)["PAIR-AMBIGUOUS"] {
			t.Errorf("PAIR-AMBIGUOUS sites %v, err %v", sites, err)
		}
	})
	t.Run("a file naming git and spawning it, rebound only in the mixed set, gets its own build", func(t *testing.T) {
		sites, sources, err := scan(t, map[string]string{
			"a.go":      "package p\n\ntype Inner struct{}\n\ntype A struct{ Inner }\n",
			"m_prod.go": "//go:build desktop && production\n\npackage p\n\nfunc (A) bin() string { return \"sh\" }\n",
			"top.go":    "//go:build desktop && dev\n\npackage p\n\nimport \"os/exec\"\n\nfunc (Inner) bin() string { return \"git\" }\n\nfunc top() { var a A; exec.Command(a.bin(), \"status\") } // SELF-SHADOW\n",
		})
		if err != nil || !tags(sites, sources)["SELF-SHADOW"] {
			t.Errorf("SELF-SHADOW sites %v, err %v", sites, err)
		}
	})
	t.Run("a stub for another platform does not hide a lookup a tagged build pairs with its spawn", func(t *testing.T) {
		sites, sources, err := scan(t, map[string]string{
			"a.go": `package p

type Inner struct{}

type T struct{ Inner }
`,
			"fallback.go": `//go:build !production || dev

package p

func (Inner) bin() string { return "sh" }
`,
			"m_windows.go": `package p

func (T) bin() string { return "cmd" }
`,
			"sp.go": `//go:build desktop

package p

import "os/exec"

func run() { var t T; exec.Command(t.bin(), "status") } // TAGGED-LOOKUP-BESIDE-STUB
`,
			"src.go": `//go:build production && !dev

package p

import "os/exec"

func (Inner) bin() string { g, _ := exec.LookPath("git"); return g }
`,
			"x_windows.go": `//go:build desktop && production

package p

import "os/exec"

func x() { exec.Command("explorer").Run() }
`,
		})
		if err != nil || !tags(sites, sources)["TAGGED-LOOKUP-BESIDE-STUB"] {
			t.Errorf("TAGGED-LOOKUP-BESIDE-STUB sites %v, err %v", sites, err)
		}
	})
	t.Run("a stub for another platform does not hide a lookup a platform file pairs with its spawn", func(t *testing.T) {
		sites, sources, err := scan(t, map[string]string{
			"a.go": `package p

type Inner struct{}

type T struct{ Inner }
`,
			"fallback.go": `//go:build !production || dev

package p

func (Inner) bin() string { return "sh" }
`,
			"lookup_windows.go": `//go:build production

package p

import "os/exec"

var winGit, _ = exec.LookPath("git")
`,
			"m_windows.go": `package p

func (T) bin() string { return "cmd" }
`,
			"sp.go": `//go:build desktop

package p

import "os/exec"

func run() { var t T; exec.Command(t.bin(), "status") } // PLATFORM-LOOKUP-BESIDE-STUB
`,
			"src.go": `//go:build production && !dev

package p

import "os/exec"

func (Inner) bin() string { g, _ := exec.LookPath("git"); return g }
`,
		})
		if err != nil || !tags(sites, sources)["PLATFORM-LOOKUP-BESIDE-STUB"] {
			t.Errorf("PLATFORM-LOOKUP-BESIDE-STUB sites %v, err %v", sites, err)
		}
	})
	t.Run("a file whose single-tag build does not compile is read in the build that does", func(t *testing.T) {
		sites, sources, err := scan(t, map[string]string{
			"a.go": `package p

type A struct{}
`,
			"m_dev.go": `//go:build desktop && dev

package p

func (A) path() string { return gitPath }
`,
			"m_prod.go": `//go:build desktop && production

package p

func (A) path() string { return "sh" }
`,
			"top.go": `//go:build desktop

package p

import "os/exec"

var gitPath, _ = exec.LookPath("git")

func run() { var a A; exec.Command(a.path(), "status") } // BROKEN-SINGLE-SELF
`,
		})
		if err != nil || !tags(sites, sources)["BROKEN-SINGLE-SELF"] {
			t.Errorf("BROKEN-SINGLE-SELF sites %v, err %v", sites, err)
		}
	})
	t.Run("a pair whose single-tag build does not compile is read in the build that does", func(t *testing.T) {
		sites, sources, err := scan(t, map[string]string{
			"a.go": `package p

type A struct{}
`,
			"m_dev.go": `//go:build desktop && dev

package p

func (A) path() string { return gitPath }
`,
			"m_prod.go": `//go:build desktop && production

package p

func (A) path() string { return "sh" }
`,
			"sp.go": `//go:build desktop

package p

import "os/exec"

func run() { var a A; exec.Command(a.path(), "status") } // BROKEN-SINGLE-PAIR
`,
			"src.go": `//go:build desktop

package p

import "os/exec"

var gitPath, _ = exec.LookPath("git")
`,
		})
		if err != nil || !tags(sites, sources)["BROKEN-SINGLE-PAIR"] {
			t.Errorf("BROKEN-SINGLE-PAIR sites %v, err %v", sites, err)
		}
	})
	t.Run("a value carried through a file only one platform builds is read in that platform's build", func(t *testing.T) {
		sites, sources, err := scan(t, map[string]string{
			"src.go":         "package p\n\nimport \"os/exec\"\n\nvar gitPath, _ = exec.LookPath(\"git\")\n",
			"bin_windows.go": "package p\n\nvar bin = gitPath\n",
			"bin_other.go":   "//go:build !windows\n\npackage p\n\nimport \"os\"\n\nvar bin = os.Getenv(\"ITERION_GIT\")\n",
			"run_desktop.go": "//go:build desktop\n\npackage p\n\nimport \"os/exec\"\n\nfunc run() error { return exec.Command(bin, \"status\").Run() } // CARRIER\n",
			"run_other.go":   "//go:build !desktop\n\npackage p\n\nfunc run() error { return nil }\n",
		})
		if err != nil || !tags(sites, sources)["CARRIER"] {
			t.Errorf("CARRIER sites %v, err %v", sites, err)
		}
	})
	t.Run("git handed to a wrapper named by a value is a git site", func(t *testing.T) {
		sites, sources, err := scan(t, map[string]string{
			"a.go": "package p\n\nimport \"os/exec\"\n\nfunc gc() {\n\tnice, _ := exec.LookPath(\"nice\")\n\texec.Command(nice, \"git\", \"gc\").Run() // WRAPPER\n}\n",
		})
		if err != nil || !tags(sites, sources)["WRAPPER"] {
			t.Errorf("WRAPPER sites %v, err %v", sites, err)
		}
	})
	t.Run("a literal handed to a wrapper named by a literal is no git site", func(t *testing.T) {
		sites, _, err := scan(t, map[string]string{
			"a.go": "package p\n\nimport \"os/exec\"\n\nfunc echo() { exec.Command(\"echo\", \"git\").Run() }\n",
		})
		if err != nil || len(sites) != 0 {
			t.Errorf("echo git counted: sites %v, err %v", sites, err)
		}
	})
	t.Run("execabs is os/exec", func(t *testing.T) {
		sites, sources, err := scan(t, map[string]string{
			"a.go": "package p\n\nimport exec \"golang.org/x/sys/execabs\"\n\nfunc status() { exec.Command(\"git\", \"status\").Run() } // EXECABS\n",
			"b.go": "package p\n\nimport \"golang.org/x/sys/execabs\"\n\nfunc log() { bin, _ := execabs.LookPath(\"git\"); execabs.Command(bin, \"log\").Run() } // EXECABS-VALUE\n",
		})
		if got := tags(sites, sources); err != nil || !got["EXECABS"] || !got["EXECABS-VALUE"] {
			t.Errorf("execabs sites %v, err %v", sites, err)
		}
	})
	t.Run("a package with more tags than the sweep enumerates fails it by name", func(t *testing.T) {
		files := map[string]string{"run.go": "package p\n\nimport \"os/exec\"\n\nfunc run(bin string) { exec.Command(bin) }\n\nvar _ = \"git\"\n"}
		for i := 0; i <= gitSweepMaxTags; i++ {
			files[fmt.Sprintf("t%d.go", i)] = fmt.Sprintf("//go:build tag%d\n\npackage p\n", i)
		}
		_, _, err := scan(t, files)
		if err == nil || !strings.Contains(err.Error(), "gitSweepMaxTags") {
			t.Errorf("a package of %d tags swept without failing: err %v", gitSweepMaxTags+1, err)
		}
	})
	t.Run("a cgo file is left out of a cgo-off build, so its !cgo twin is read", func(t *testing.T) {
		sites, sources, err := scan(t, map[string]string{
			"a_cgo.go":   "package p\n\nimport \"C\"\n\nvar gitBin = \"sh\"\n",
			"b_nocgo.go": "//go:build !cgo\n\npackage p\n\nimport \"os/exec\"\n\nvar gitBin, _ = exec.LookPath(\"git\")\n",
			"run.go":     "package p\n\nimport \"os/exec\"\n\nfunc run() { exec.Command(gitBin, \"status\") } // C-TWIN\n",
		})
		if err != nil || !tags(sites, sources)["C-TWIN"] {
			t.Errorf("sites %v, err %v: the !cgo lookup was hidden by the cgo file sorted first", sites, err)
		}
	})
	t.Run("a file no configuration builds is read for its literal spawns", func(t *testing.T) {
		sites, sources, err := scan(t, map[string]string{
			"p.go":             typedProd,
			"p_linux_arm64.go": "package p\n\nimport \"os/exec\"\n\nfunc d() { exec.Command(\"git\", \"status\") } // ARM64\n",
		})
		if err != nil || !tags(sites, sources)["ARM64"] {
			t.Errorf("sites %v, err %v: an arm64-only file went unread", sites, err)
		}
	})
	t.Run("a stub for another platform does not hide the real lookup", func(t *testing.T) {
		sites, sources, err := scan(t, map[string]string{
			"git_a_other.go": "//go:build !unix\n\npackage p\n\nvar gitBin = \"x\"\n",
			"git_unix.go":    "//go:build unix\n\npackage p\n\nimport \"os/exec\"\n\nvar gitBin, _ = exec.LookPath(\"git\")\n\nfunc run() { exec.Command(gitBin, \"status\") } // TWIN\n",
		})
		if err != nil || !tags(sites, sources)["TWIN"] {
			t.Errorf("sites %v, err %v: the unix lookup was hidden by the stub sorted first", sites, err)
		}
	})
}

// The constraints are read where go/build reads them — the file header,
// `//go:build` or a legacy `// +build`, indented or not, and nowhere else —
// and every name a file imports os/exec or execabs by is a spawn's package.
func TestGitSpawnSitesReadsHeadersAndEveryExecName(t *testing.T) {
	scan := func(t *testing.T, files map[string]string) map[string]bool {
		t.Helper()
		dir := t.TempDir()
		sources := map[string][]byte{}
		for name, src := range files {
			path := filepath.Join(dir, name)
			if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
				t.Fatal(err)
			}
			sources[path] = []byte(src)
		}
		sites, err := gitSpawnSitesInDir(sources)
		if err != nil {
			t.Fatalf("sweep: %v", err)
		}
		got := map[string]bool{}
		for path, list := range sites {
			src := string(sources[path])
			for _, s := range list {
				line := src[s.start : s.start+strings.Index(src[s.start:], "\n")]
				got[line[strings.LastIndex(line, "// ")+3:]] = true
			}
		}
		return got
	}
	lookup := "package p\n\nimport \"os/exec\"\n\nfunc gitPath() string {\n\tp, _ := exec.LookPath(\"git\")\n\treturn p\n}\n"
	twinDesktop := "//go:build desktop\n\npackage p\n\nimport \"os/exec\"\n\nfunc bin() string {\n\tp, _ := exec.LookPath(\"git\")\n\treturn p\n}\n"
	run := "package p\n\nimport \"os/exec\"\n\nfunc run() { exec.Command(bin(), \"status\").Run() } // %s\n"
	t.Run("a legacy +build file is built under its tag", func(t *testing.T) {
		got := scan(t, map[string]string{
			"tag_desktop.go": "//go:build desktop\n\npackage p\n",
			"lookup.go":      lookup,
			"run_legacy.go":  "// +build desktop\n\npackage p\n\nimport \"os/exec\"\n\nfunc run() { exec.Command(gitPath(), \"status\").Run() } // PLUSBUILD\n",
		})
		if !got["PLUSBUILD"] {
			t.Errorf("PLUSBUILD sites %v", got)
		}
	})
	t.Run("a legacy +build twin is left out of the build its tag excludes it from", func(t *testing.T) {
		got := scan(t, map[string]string{
			"path_desktop.go": twinDesktop,
			"path_other.go":   "// +build !desktop\n\npackage p\n\nfunc bin() string { return \"true\" }\n",
			"run.go":          strings.Replace(run, "%s", "PLUSBUILD-TWIN", 1),
		})
		if !got["PLUSBUILD-TWIN"] {
			t.Errorf("PLUSBUILD-TWIN sites %v", got)
		}
	})
	t.Run("an indented //go:build twin is left out of the build its tag excludes it from", func(t *testing.T) {
		got := scan(t, map[string]string{
			"path_desktop.go": twinDesktop,
			"path_other.go":   "\t//go:build !desktop\n\npackage p\n\nfunc bin() string { return \"true\" }\n",
			"run.go":          strings.Replace(run, "%s", "INDENTED-TWIN", 1),
		})
		if !got["INDENTED-TWIN"] {
			t.Errorf("INDENTED-TWIN sites %v", got)
		}
	})
	t.Run("a twin whose constraint follows a byte order mark is left out of the build its tag excludes it from", func(t *testing.T) {
		got := scan(t, map[string]string{
			"path_desktop.go": twinDesktop,
			"path_other.go":   "\uFEFF//go:build !desktop\n\npackage p\n\nfunc bin() string { return \"true\" }\n",
			"run.go":          strings.Replace(run, "%s", "BOM-TWIN", 1),
		})
		if !got["BOM-TWIN"] {
			t.Errorf("BOM-TWIN sites %v", got)
		}
	})
	t.Run("a twin renamed by a //line directive keeps its own constraint", func(t *testing.T) {
		got := scan(t, map[string]string{
			"path_desktop.go": twinDesktop,
			"path_other.go":   "//go:build !desktop\n\n//line generated.go:1\npackage p\n\nfunc bin() string { return \"true\" }\n",
			"run.go":          strings.Replace(run, "%s", "LINE-TWIN", 1),
		})
		if !got["LINE-TWIN"] {
			t.Errorf("LINE-TWIN sites %v", got)
		}
	})
	t.Run("a //line directive does not move a file's sites to the file it names", func(t *testing.T) {
		dir := t.TempDir()
		files := map[string]string{
			"a.go": "//line b.go:1\npackage p\n\nimport \"os/exec\"\n\nfunc run() { exec.Command(\"git\", \"status\").Run() }\n",
			"b.go": "package p\n\nfunc other() {}\n",
		}
		sources := map[string][]byte{}
		for name, src := range files {
			sources[filepath.Join(dir, name)] = []byte(src)
		}
		sites, err := gitSpawnSitesInDir(sources)
		if err != nil || len(sites[filepath.Join(dir, "a.go")]) != 1 || len(sites[filepath.Join(dir, "b.go")]) != 0 {
			t.Errorf("a.go's spawn filed under %v, err %v", sites, err)
		}
	})
	t.Run("two twins a //line directive gives one name are still two builds", func(t *testing.T) {
		got := scan(t, map[string]string{
			"path_desktop.go": "//go:build desktop\n\n//line path.go:1\npackage p\n\nimport \"os/exec\"\n\nfunc bin() string {\n\tp, _ := exec.LookPath(\"git\")\n\treturn p\n}\n",
			"path_other.go":   "//go:build !desktop\n\n//line path.go:1\npackage p\n\nfunc bin() string { return \"true\" }\n",
			"run.go":          strings.Replace(run, "%s", "LINE-KEY", 1),
		})
		if !got["LINE-KEY"] {
			t.Errorf("LINE-KEY sites %v", got)
		}
	})
	t.Run("a test file a //line directive gives a production name is read with the tests", func(t *testing.T) {
		got := scan(t, map[string]string{
			"helper_test.go": "package p\n\nimport \"os/exec\"\n\nvar gitBin, _ = exec.LookPath(\"git\")\n",
			"x_test.go":      "//line x.go:1\npackage p\n\nimport \"os/exec\"\n\nfunc runIt() { exec.Command(gitBin, \"status\").Run() } // LINE-TEST\n",
			"p.go":           "package p\n",
		})
		if !got["LINE-TEST"] {
			t.Errorf("LINE-TEST sites %v", got)
		}
	})
	t.Run("a //go:build line outside the header is no build tag", func(t *testing.T) {
		fixtures := "package p\n\nvar fixtures = []string{\n"
		for i := 0; i <= gitSweepMaxTags; i++ {
			fixtures += "\t`\n//go:build feature" + strings.Repeat("x", i) + "\n\npackage gen\n`,\n"
		}
		got := scan(t, map[string]string{
			"run.go":      "package p\n\nimport \"os/exec\"\n\nfunc run() {\n\tbin, _ := exec.LookPath(\"git\")\n\texec.Command(bin, \"status\").Run() // FIXTURE-TAGS\n}\n",
			"gen_test.go": fixtures + "}\n",
		})
		if !got["FIXTURE-TAGS"] {
			t.Errorf("FIXTURE-TAGS sites %v", got)
		}
	})
	t.Run("a file importing os/exec and execabs keeps its os/exec spawns", func(t *testing.T) {
		got := scan(t, map[string]string{
			"a.go": "package p\n\nimport (\n\t\"os/exec\"\n\n\t\"golang.org/x/sys/execabs\"\n)\n\nfunc run() {\n\texec.Command(\"git\", \"status\").Run() // BOTH-LITERAL\n\tbin, _ := exec.LookPath(\"git\")\n\texec.Command(bin, \"log\").Run() // BOTH-VALUE\n\texecabs.Command(\"true\").Run()\n}\n",
		})
		if !got["BOTH-LITERAL"] || !got["BOTH-VALUE"] {
			t.Errorf("BOTH sites %v", got)
		}
	})
	t.Run("a file importing os/exec under two names keeps the spawns of both", func(t *testing.T) {
		got := scan(t, map[string]string{
			"a.go": "package p\n\nimport (\n\t\"os/exec\"\n\tosexec \"os/exec\"\n)\n\nfunc run() {\n\texec.Command(\"git\", \"status\").Run() // TWICE-PLAIN\n\tosexec.Command(\"git\", \"log\").Run() // TWICE-NAMED\n}\n",
		})
		if !got["TWICE-PLAIN"] || !got["TWICE-NAMED"] {
			t.Errorf("TWICE sites %v", got)
		}
	})
}

// TestScanGitSitesFailsOnACappedPackage drives the sweep's own entry point:
// a package past gitSweepMaxTags fails the whole sweep, naming its directory,
// rather than being dropped from it.
func TestScanGitSitesFailsOnACappedPackage(t *testing.T) {
	root := t.TempDir()
	write := func(rel, src string) {
		path := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("ok/a.go", "package ok\n\nimport \"os/exec\"\n\nfunc run() { exec.Command(\"git\", \"status\").Run() }\n")
	write("cmd/capped/run.go", "package main\n\nimport \"os/exec\"\n\nfunc run(bin string) { exec.Command(bin) }\n\nvar _ = \"git\"\n")
	for i := 0; i <= gitSweepMaxTags; i++ {
		write(fmt.Sprintf("cmd/capped/t%d.go", i), fmt.Sprintf("//go:build tag%d\n\npackage main\n", i))
	}
	_, err := scanGitSites(root)
	if err == nil {
		t.Fatalf("a package of %d tags was dropped from the sweep without failing it", gitSweepMaxTags+1)
	}
	if !strings.Contains(err.Error(), filepath.Join("cmd", "capped")) {
		t.Errorf("the sweep's error does not name the capped directory: %v", err)
	}
}
