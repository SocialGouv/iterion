package botscaffold

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

func minimalSpec() Spec {
	return Spec{
		Slug:         "my-bot",
		Instructions: "Do the thing.",
	}
}

func TestScaffold_Minimal(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "my-bot")
	res, err := Scaffold(dir, minimalSpec())
	if err != nil {
		t.Fatalf("Scaffold: %v", err)
	}
	if len(res.Files) != 3 {
		t.Fatalf("Files = %v, want main.bot + manifest.yaml + README.md", res.Files)
	}
	if _, err := os.Stat(filepath.Join(dir, "skills")); err != nil {
		t.Errorf("skills/ dir missing: %v", err)
	}
	m, err := bundle.LoadManifest(filepath.Join(dir, "manifest.yaml"))
	if err != nil || m == nil {
		t.Fatalf("LoadManifest: %v (m=%v)", err, m)
	}
	if m.Name != "my-bot" || !m.IsEnabled() {
		t.Errorf("manifest name=%q enabled=%v, want my-bot enabled", m.Name, m.IsEnabled())
	}
}

// TestScaffold_SpecMatrix renders each spec variant and re-validates the
// generated main.bot through the runtime's own parse+compile pipeline.
func TestScaffold_SpecMatrix(t *testing.T) {
	full := Spec{
		Slug:        "full-bot",
		DisplayName: "Full Bot",
		Icon:        "🦉",
		Description: "Everything on.",
		WhenToUse:   "Use when testing the scaffolder.",
		Instructions: "Multi-line mission.\n\nWith a blank line and {{vars.topic}} reference.",
		Model:        "anthropic/claude-opus-4-8",
		Backend:      "claude_code",
		Skills:       []string{"house-style"},
		Capabilities: []string{"board.read"},
		Vars: []VarSpec{
			{Name: "topic", Type: "string", Default: "rate limiting", Description: "What to write about."},
			{Name: "count", Type: "int"},
			{Name: "deep", Type: "bool", Default: "true"},
			{Name: "ratio", Type: "float", Default: "0.5"},
		},
		Worktree:     true,
		Sandbox:      true,
		Permission:   "ask",
		MaxCostUSD:   12.5,
		MaxDuration:  "2h",
		ScheduleCron: "0 7 * * 1-5",
	}
	cases := map[string]Spec{
		"minimal": minimalSpec(),
		"full":    full,
	}
	for _, tpl := range Templates() {
		spec := tpl.Spec
		spec.Slug = "tpl-" + tpl.ID
		cases["template-"+tpl.ID] = spec
	}

	for name, spec := range cases {
		t.Run(name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), spec.Slug)
			if _, err := Scaffold(dir, spec); err != nil {
				t.Fatalf("Scaffold: %v", err)
			}
			src, err := os.ReadFile(filepath.Join(dir, "main.bot"))
			if err != nil {
				t.Fatal(err)
			}
			pr := parser.Parse("main.bot", string(src))
			for _, d := range pr.Diagnostics {
				if d.Severity == parser.SeverityError {
					t.Errorf("parse: %s", d.Error())
				}
			}
			cr := ir.Compile(pr.File)
			for _, d := range cr.Diagnostics {
				if d.Severity == ir.SeverityError {
					t.Errorf("compile: %s\n---\n%s", d.Error(), src)
				}
			}
			if cr.Workflow == nil || cr.Workflow.Name != spec.WorkflowName() {
				t.Fatalf("workflow = %v, want name %q", cr.Workflow, spec.WorkflowName())
			}
		})
	}
}

func TestScaffold_ScheduleInvocationInManifest(t *testing.T) {
	spec := minimalSpec()
	spec.ScheduleCron = "0 7 * * 1-5"
	dir := filepath.Join(t.TempDir(), spec.Slug)
	if _, err := Scaffold(dir, spec); err != nil {
		t.Fatalf("Scaffold: %v", err)
	}
	m, err := bundle.LoadManifest(filepath.Join(dir, "manifest.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Invocations) != 1 || m.Invocations[0].Kind != bundle.InvocationKindSchedule ||
		m.Invocations[0].Schedule == nil || m.Invocations[0].Schedule.SuggestedCron != spec.ScheduleCron {
		t.Errorf("invocations = %+v, want one schedule with cron %q", m.Invocations, spec.ScheduleCron)
	}
}

func TestScaffold_RejectsInvalidSpecs(t *testing.T) {
	cases := map[string]func(*Spec){
		"bad slug":       func(s *Spec) { s.Slug = "My Bot" },
		"empty mission":  func(s *Spec) { s.Instructions = "  " },
		"bad permission": func(s *Spec) { s.Permission = "yolo" },
		"bad var name":   func(s *Spec) { s.Vars = []VarSpec{{Name: "1x", Type: "string"}} },
		"bad var type":   func(s *Spec) { s.Vars = []VarSpec{{Name: "x", Type: "list"}} },
		"bad int":        func(s *Spec) { s.Vars = []VarSpec{{Name: "x", Type: "int", Default: "abc"}} },
		"dup var":        func(s *Spec) { s.Vars = []VarSpec{{Name: "x", Type: "string"}, {Name: "x", Type: "int"}} },
		"bad cron":       func(s *Spec) { s.ScheduleCron = "not a cron" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			spec := minimalSpec()
			mutate(&spec)
			dir := filepath.Join(t.TempDir(), "x")
			if _, err := Scaffold(dir, spec); err == nil {
				t.Fatal("expected an error, got nil")
			}
			if _, statErr := os.Stat(filepath.Join(dir, "main.bot")); statErr == nil {
				t.Fatal("invalid spec still wrote main.bot")
			}
		})
	}
}

func TestScaffold_RefusesExistingBundle(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "my-bot")
	if _, err := Scaffold(dir, minimalSpec()); err != nil {
		t.Fatal(err)
	}
	_, err := Scaffold(dir, minimalSpec())
	if err == nil || !strings.Contains(err.Error(), "already contains") {
		t.Fatalf("expected already-contains error, got %v", err)
	}
}
