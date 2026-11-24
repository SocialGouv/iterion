package model

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The derivation's own witness: every read form it must see, and every
// spelling that hides a read, each answered by name.
func TestEnvReadsSeesEveryReadForm(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	write := func(name, src string) string {
		t.Helper()
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	seen := write("seen.go", `package p

import (
	"os/exec"
	o "os"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/unix"

	"github.com/SocialGouv/iterion/internal/envtrust"
	"github.com/SocialGouv/iterion/internal/isolationenv"
)

func direct() string    { return o.Getenv("ALIASED") }
func lookup() bool      { _, ok := o.LookupEnv("LOOKED_UP"); return ok }
func trusted() string   { return envtrust.Inherited("ENVTRUST") }
func planted() string   { return envtrust.Planted("PLANTED") }
func isolated() string  { return isolationenv.Value("ISOLATION") }
func sys() string       { v, _ := syscall.Getenv("SYSCALL"); return v }
func unixRead() string  { v, _ := unix.Getenv("UNIX"); return v }
func expanded() string  { return o.ExpandEnv("$EXPAND_A/${EXPAND_B} cost $5 pid $$") }
func home() string      { h, _ := o.UserHomeDir(); return h }
func config() string    { c, _ := o.UserConfigDir(); return c }
func tmp() string       { return o.TempDir() }
func paren() string     { return (o.Getenv)("PAREN_FUN") + o.Getenv(("PAREN_ARG")) }
func spawn()            { _ = exec.Command("ls") }
func wd() string        { w, _ := o.Getwd(); a, _ := filepath.Abs("."); return w + a }

func local() string {
	get := o.Getenv
	again := get
	var copied = get
	return get("LOCAL") + again("COPIED") + copied("VAR_COPY")
}

var pkgReader = o.Getenv

// first is declared before the reader it copies: only a second marking
// pass sees it.
var first = second
var second = o.Getenv

func chained() string { return first("CHAINED") }

func defaulted(env func(string) string) string {
	if nil == env {
		env = o.Getenv
	}
	return env("DEFAULTED")
}

func overridden(in func(string) string) string {
	get := o.Getenv
	if in != nil {
		get = in
	}
	return get("OVERRIDE")
}
`)
	// A package variable read from another file of the package.
	seen2 := write("seen2.go", `package p

func cross() string { return pkgReader("CROSS_FILE") }
`)
	names, problems, err := envReads([]string{seen, seen2})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range problems {
		t.Errorf("a decidable read was refused: %s", p)
	}
	for _, want := range []string{"ALIASED", "LOOKED_UP", "ENVTRUST", "PLANTED", "ISOLATION", "SYSCALL", "UNIX",
		"EXPAND_A", "EXPAND_B", "HOME", "XDG_CONFIG_HOME", "TMPDIR", "PAREN_FUN", "PAREN_ARG", "PATH", "PWD",
		"LOCAL", "COPIED", "VAR_COPY", "CHAINED", "DEFAULTED", "OVERRIDE", "CROSS_FILE"} {
		if !slices.Contains(names, want) {
			t.Errorf("the read of %s was not seen (derived %v)", want, names)
		}
	}
	for _, bogus := range []string{"5", "$"} {
		if slices.Contains(names, bogus) {
			t.Errorf("os.ExpandEnv's %q is not a variable, but was derived (%v)", bogus, names)
		}
	}

	// Each spelling below hides what it reads; each must be reported at its
	// own line, and none may contribute a name.
	hidden := write("hidden.go", `package q

import "os"

type holder struct{ get func(string) string }

func use(func(string) string) {}

func nonLiteral(name string) string { return os.Getenv(name) } // NON-LITERAL
func passed()                       { use(os.Getenv) }           // PASSED
func field() holder                 { return holder{get: os.Getenv} } // FIELD
func returned() func(string) string { return os.Getenv }        // RETURNED
func onward() {
	get := os.Getenv
	use(get) // ONWARD
}
func expandValue(s string) string { return os.ExpandEnv(s) } // EXPAND-VALUE
func viaReader(n string) string { g := os.Getenv; return g(n) } // VIA-READER-NON-LITERAL
func reader() (get func(string) string) { get = os.Getenv; return } // NAMED-RESULT

var Getter = os.Getenv // EXPORTED
`)
	dot := write("dot.go", `package r

import . "os" // DOT

func dotted() string { return Getenv("DOTTED") }
`)
	src := map[string][]string{}
	for _, path := range []string{hidden, dot} {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		src[path] = strings.Split(string(raw), "\n")
	}
	names, problems, err = envReads([]string{hidden, dot})
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 0 {
		t.Errorf("hidden reads contributed names %v: a name the derivation cannot decide must not read as decided", names)
	}
	reported := map[string]int{}
	for _, p := range problems {
		// "<file>:<line>:<col>: message"
		parts := strings.SplitN(p, ":", 3)
		if len(parts) < 3 {
			t.Fatalf("problem without a position: %s", p)
		}
		line := 0
		for _, c := range parts[1] {
			line = line*10 + int(c-'0')
		}
		text := src[parts[0]][line-1]
		tag := text[strings.LastIndex(text, "// ")+3:]
		reported[tag]++
	}
	for _, tag := range []string{"NON-LITERAL", "PASSED", "FIELD", "RETURNED", "ONWARD", "EXPAND-VALUE",
		"VIA-READER-NON-LITERAL", "NAMED-RESULT", "EXPORTED", "DOT"} {
		if reported[tag] == 0 {
			t.Errorf("the hidden read %s was not reported (reports: %v)", tag, problems)
		}
	}
}

// Each implicit read on its own: two forms that derive the same name would
// otherwise vouch for each other.
func TestEnvReadsSeesEachImplicitReadAlone(t *testing.T) {
	t.Parallel()
	cases := []struct {
		imp, call string
		want      []string
	}{
		{`"os"`, `_, _ = os.UserHomeDir()`, []string{"HOME"}},
		{`"os"`, `_, _ = os.UserCacheDir()`, []string{"HOME", "XDG_CACHE_HOME"}},
		{`"os"`, `_, _ = os.UserConfigDir()`, []string{"HOME", "XDG_CONFIG_HOME"}},
		{`"os"`, `_ = os.TempDir()`, []string{"TMPDIR"}},
		{`"os"`, `_, _ = os.MkdirTemp("", "x")`, []string{"TMPDIR"}},
		{`"os"`, `_, _ = os.CreateTemp("", "x")`, []string{"TMPDIR"}},
		{`"os"`, `_, _ = os.Getwd()`, []string{"PWD"}},
		{`"path/filepath"`, `_, _ = filepath.Abs(".")`, []string{"PWD"}},
		{`"os/exec"`, `_ = exec.Command("ls")`, []string{"PATH"}},
		{`"os/exec"`, `_ = exec.CommandContext(nil, "ls")`, []string{"PATH"}},
		{`"os/exec"`, `_, _ = exec.LookPath("ls")`, []string{"PATH"}},
		{`"github.com/SocialGouv/iterion/internal/envtrust"`, `_, _ = envtrust.LookPath("ls")`, []string{"PATH"}},
	}
	for i, c := range cases {
		path := filepath.Join(t.TempDir(), "implicit.go")
		src := "package p\n\nimport " + c.imp + "\n\nfunc f() { " + c.call + " }\n"
		if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
			t.Fatal(err)
		}
		names, problems, err := envReads([]string{path})
		if err != nil || len(problems) != 0 {
			t.Fatalf("case %d (%s): err %v, problems %v", i, c.call, err, problems)
		}
		if !slices.Equal(names, c.want) {
			t.Errorf("%s derives %v, want %v", c.call, names, c.want)
		}
	}
}

// Two spellings that read nothing the derivation must refuse: a build-tag
// twin redeclaring a package reader, and a parameter named like the import.
func TestEnvReadsAcceptsTwinsAndShadows(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	files := map[string]string{
		"a_unix.go":    "//go:build unix\n\npackage p\n\nimport \"os\"\n\nvar getenv = os.Getenv\n\nfunc a() string { return getenv(\"TWIN_UNIX\") }\n",
		"a_windows.go": "//go:build windows\n\npackage p\n\nimport \"os\"\n\nvar getenv = os.Getenv\n\nfunc b() string { return getenv(\"TWIN_WINDOWS\") }\n",
		"b_js.go":      "//go:build js\n\npackage p\n\nvar stubFirst = func(string) string { return \"\" }\n",
		"c_linux.go":   "//go:build linux\n\npackage p\n\nimport \"os\"\n\nvar stubFirst = os.Getenv\n\nfunc c() string { return stubFirst(\"TWIN_STUB_FIRST\") }\n",
		"shadow.go":    "package p\n\nimport \"os\"\n\ntype fake struct{}\n\nfunc (fake) Getenv(string) string { return \"\" }\n\nvar _ = os.Getpid\n\nfunc r(os fake) string { return os.Getenv(\"SHADOWED\") }\n",
	}
	var paths []string
	for name, src := range files {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, path)
	}
	names, problems, err := envReads(paths)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range problems {
		t.Errorf("refused: %s", p)
	}
	for _, want := range []string{"TWIN_UNIX", "TWIN_WINDOWS", "TWIN_STUB_FIRST"} {
		if !slices.Contains(names, want) {
			t.Errorf("the twin's read of %s was not seen (derived %v)", want, names)
		}
	}
	if slices.Contains(names, "SHADOWED") {
		t.Errorf("a parameter named like the os import was taken for the package: SHADOWED derived (%v)", names)
	}
}
