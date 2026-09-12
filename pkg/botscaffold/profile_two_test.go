package botscaffold

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
	"github.com/SocialGouv/iterion/pkg/internal/appinfo"
)

// Every workflow a template renders — main.bot and a shape's child .bot —
// is written in syntax profile 2, and the manifest declares the engine
// floor it was given.
func TestEveryTemplateRendersProfileTwoWithAFloor(t *testing.T) {
	specs := map[string]Spec{"": minimalSpec()}
	for _, shape := range Shapes() {
		specs[shape] = templateForShape(t, shape).Spec
	}
	for shape, spec := range specs {
		t.Run("shape="+shape, func(t *testing.T) {
			spec.Slug = "probe"
			spec.EngineFloor = "3.141.0"
			dir := t.TempDir()
			if _, err := Scaffold(dir, spec); err != nil {
				t.Fatalf("Scaffold: %v", err)
			}
			var bots int
			_ = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
				if err != nil || d.IsDir() || !strings.HasSuffix(path, ".bot") {
					return err
				}
				bots++
				src, _ := os.ReadFile(path)
				pr := parser.Parse(path, string(src))
				if pr.File.EffectiveProfile() != 2 {
					t.Errorf("%s reads as profile %d", path, pr.File.EffectiveProfile())
				}
				for _, dg := range pr.Diagnostics {
					if dg.Severity == parser.SeverityError {
						t.Errorf("%s: %v", path, dg)
					}
				}
				return nil
			})
			if bots == 0 {
				t.Fatalf("no .bot rendered")
			}
			m, err := bundle.LoadManifest(filepath.Join(dir, "manifest.yaml"))
			if err != nil || m == nil || m.Requires == nil || m.Requires.Iterion != ">= 3.141.0" {
				t.Fatalf("manifest floor: %v %+v", err, m)
			}
		})
	}
}

// With no floor given, the scaffold writes this build's version when it is
// orderable and nothing on a dev build — which `iterion validate` then asks
// for (C252) rather than a release number guessed here.
func TestScaffoldFloorIsThisBuildsVersion(t *testing.T) {
	prev := appinfo.Version
	t.Cleanup(func() { appinfo.Version = prev })

	appinfo.Version = "v3.150.2+abc123"
	dir := t.TempDir()
	if _, err := Scaffold(dir, minimalSpec()); err != nil {
		t.Fatal(err)
	}
	m, err := bundle.LoadManifest(filepath.Join(dir, "manifest.yaml"))
	if err != nil || m.Requires == nil || m.Requires.Iterion != ">= 3.150.2" {
		t.Fatalf("pinned build: %v %+v", err, m)
	}

	appinfo.Version = "dev"
	dir = t.TempDir()
	if _, err := Scaffold(dir, minimalSpec()); err != nil {
		t.Fatal(err)
	}
	m, err = bundle.LoadManifest(filepath.Join(dir, "manifest.yaml"))
	if err != nil || m.Requires != nil {
		t.Fatalf("dev build: %v %+v", err, m.Requires)
	}
}

// The scaffold quotes its .bot strings with the lexer's own escapes: a
// model name or a var default holding a quote, a tab or a control character
// renders a file the profile-2 lexer reads back to the same value, where
// Go's %q emitted sequences the lexer refuses.
func TestScaffoldQuotesForTheLexer(t *testing.T) {
	odd := "a \"quoted\" value\twith a tab and a \x01 control byte"
	spec := minimalSpec()
	spec.Model = odd
	spec.Vars = []VarSpec{{Name: "note", Type: "string", Default: odd}}
	dir := t.TempDir()
	if _, err := Scaffold(dir, spec); err != nil {
		t.Fatalf("Scaffold: %v", err)
	}
	src, _ := os.ReadFile(filepath.Join(dir, "main.bot"))
	pr := parser.Parse("main.bot", string(src))
	for _, d := range pr.Diagnostics {
		if d.Severity == parser.SeverityError {
			t.Fatalf("rendered file does not parse: %v\n%s", d, src)
		}
	}
	if len(pr.File.Agents) == 0 || pr.File.Agents[0].Model != odd {
		t.Fatalf("model read back as %q", pr.File.Agents[0].Model)
	}
	var note string
	if pr.File.Vars != nil {
		for _, v := range pr.File.Vars.Fields {
			if v.Name == "note" && v.Default != nil {
				note = v.Default.StrVal
			}
		}
	}
	if note != odd {
		t.Fatalf("var default read back as %q\n%s", note, src)
	}
}
