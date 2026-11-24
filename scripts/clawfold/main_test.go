package main

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/clawfold"
	"github.com/SocialGouv/iterion/internal/gittest"
)

const consumer = `package main

import "github.com/SocialGouv/claw-code-go/pkg/api"

func main() { println(api.Hello()) }
`

func TestUsageErrorsExitTwo(t *testing.T) {
	t.Parallel()
	// Every planned command names a scratch root: a check that let the
	// invocation through must fail there, never reach the checkout the
	// tests run in.
	scratch := t.TempDir()
	for _, args := range [][]string{
		nil,
		{"fold"},
		{"rewrite"},
		{"apply"},
		{"apply", "-slice", "1", "-switch"},
		{"plan", "extra"},
		{"plan", "-nope"},
		{"plan", "-slice", "1"},
		{"verify", "-switch"},
		{"verify", "-slice", "-1"},
		{"plan", "-plan", "abc"},
		{"verify", "-slice", "0"},
		{"apply", "-switch", "-plan", "abc"},
		{"apply", "-switch", "-cap", "7"},
	} {
		if len(args) > 0 && (args[0] == "plan" || args[0] == "apply" || args[0] == "verify") {
			args = append(args, "-root", scratch)
		}
		var stdout, stderr bytes.Buffer
		if code := run(args, &stdout, &stderr); code != 2 {
			t.Errorf("clawfold %v: exit %d, want 2 (stderr %q)", args, code, stderr.String())
		}
	}
}

func TestRewriteThenUnrewriteGivesTheFileBack(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	file := filepath.Join(dir, "main.go")
	if err := os.WriteFile(file, []byte(consumer), 0o644); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"rewrite", dir}, &stdout, &stderr); code != 0 {
		t.Fatalf("rewrite: exit %d: %s", code, stderr.String())
	}
	if strings.TrimSpace(stdout.String()) != file {
		t.Fatalf("rewrite listed %q, want the one file", stdout.String())
	}
	got, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), `"github.com/SocialGouv/iterion/pkg/claw/pkg/api"`) {
		t.Fatalf("after rewrite:\n%s", got)
	}
	stdout.Reset()
	if code := run([]string{"unrewrite", file}, &stdout, &stderr); code != 0 {
		t.Fatalf("unrewrite: exit %d: %s", code, stderr.String())
	}
	if got, _ := os.ReadFile(file); string(got) != consumer {
		t.Fatalf("after unrewrite:\n%s", got)
	}
}

func TestRewriteReportsAFileItCannotProve(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	bad := "package x\n\nimport \"github.com/SocialGouv/claw-code-go/\\x61pi\"\n"
	if err := os.WriteFile(filepath.Join(dir, "x.go"), []byte(bad), 0o644); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"rewrite", dir}, &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), "escape") {
		t.Fatalf("exit %d, stderr %q: want 1 and the escape refusal", code, stderr.String())
	}
}

// The command end to end on a miniature: the pin read from vendor/modules.txt,
// and `apply -slice` bound to the plan id `plan` printed.
func TestApplyLandsOnlyThePlanItIsGiven(t *testing.T) {
	t.Parallel()
	src := gittest.SourceRepo(t)
	write := func(dir, rel, body string) {
		t.Helper()
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	api := "package api\n\n// Hello is linked.\nfunc Hello() string { return \"hi\" }\n"
	write(src, "go.mod", "module github.com/SocialGouv/claw-code-go\n\ngo 1.26\n")
	write(src, "LICENSE", "MIT\n")
	write(src, "pkg/api/api.go", api)
	gittest.Run(t, src, "add", "-A")
	gittest.Run(t, src, "commit", "-q", "-m", "claw")
	pin := gittest.Run(t, src, "rev-parse", "HEAD")

	root := gittest.SourceRepo(t)
	write(root, "go.mod", "module github.com/SocialGouv/iterion\n\ngo 1.26\n")
	write(root, "vendor/modules.txt", "# github.com/SocialGouv/claw-code-go v0.0.0-20261106142829-"+pin[:12]+"\n## explicit; go 1.26\ngithub.com/SocialGouv/claw-code-go/pkg/api\n")
	write(root, "vendor/github.com/SocialGouv/claw-code-go/LICENSE", "MIT\n")
	write(root, "vendor/github.com/SocialGouv/claw-code-go/pkg/api/api.go", api)
	tree, err := clawfold.ReadTree(src, pin)
	if err != nil {
		t.Fatal(err)
	}
	version := "v0.0.0-20261106142829-" + pin[:12]
	sum, err := clawfold.ModuleHash(tree, clawfold.SourceModule, version)
	if err != nil {
		t.Fatal(err)
	}
	write(root, "go.sum", clawfold.SourceModule+" "+version+" "+sum+"\n")
	gittest.Run(t, root, "add", "-A")
	gittest.Run(t, root, "commit", "-q", "-m", "iterion")

	// A tracked Go file missing on disk (a sparse checkout): the switch
	// cannot be planned, the slices still are.
	write(root, "cmd/x/x.go", "package main\n\nfunc main() {}\n")
	gittest.Run(t, root, "add", "-A")
	gittest.Run(t, root, "commit", "-q", "-m", "a consumer")
	if err := os.Remove(filepath.Join(root, "cmd/x/x.go")); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"plan", "-root", root, "-source", src}, &stdout, &stderr); code != 0 || !strings.Contains(stderr.String(), "the switch is not planned") {
		t.Fatalf("plan over a sparse tree: exit %d, stderr %q: want the plan, the switch named unplanned", code, stderr.String())
	}
	gittest.Run(t, root, "checkout", "--", "cmd/x/x.go")
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"plan", "-root", root, "-source", src}, &stdout, &stderr); code != 0 {
		t.Fatalf("plan: exit %d: %s", code, stderr.String())
	}
	m := regexp.MustCompile(`plan ([0-9a-f]{12}) — slices`).FindStringSubmatch(stdout.String())
	if m == nil {
		t.Fatalf("plan printed no id:\n%s", stdout.String())
	}
	for _, args := range [][]string{
		{"apply", "-root", root, "-source", src, "-slice", "1"},
		{"apply", "-root", root, "-source", src, "-slice", "1", "-plan", "000000000000"},
		{"apply", "-root", root, "-source", src, "-slice", "9", "-plan", m[1]},
	} {
		stderr.Reset()
		if code := run(args, &stdout, &stderr); code != 2 {
			t.Fatalf("clawfold %v: exit %d, want 2 (%s)", args[6:], code, stderr.String())
		}
	}
	if _, err := os.Stat(filepath.Join(root, "pkg/claw/LICENSE")); err == nil {
		t.Fatal("a refused apply wrote the slice")
	}
	stderr.Reset()
	if code := run([]string{"apply", "-root", root, "-source", src, "-slice", "1", "-plan", m[1]}, &stdout, &stderr); code != 0 {
		t.Fatalf("apply with the printed id: exit %d: %s", code, stderr.String())
	}
	if got, err := os.ReadFile(filepath.Join(root, "pkg/claw/LICENSE")); err != nil || string(got) != "MIT\n" {
		t.Fatalf("slice 1 did not land: %q, %v", got, err)
	}
}
