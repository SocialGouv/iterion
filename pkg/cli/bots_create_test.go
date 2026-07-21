package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/botscaffold"
	"github.com/SocialGouv/iterion/pkg/bundle"
)

// inTempWorkspace chdirs into a fresh temp dir for the duration of the
// test — BotsCreate resolves Dest relative to the working directory.
func inTempWorkspace(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	prev, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("Chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(prev) })
	return dir
}

// jsonPrinter complements schedule_test.go's testPrinter (human format)
// for the machine-output assertions.
func jsonPrinter() (*Printer, *bytes.Buffer) {
	buf := &bytes.Buffer{}
	return &Printer{W: buf, Format: OutputJSON}, buf
}

func TestBotsCreate_ProducesDiscoverableBundle(t *testing.T) {
	inTempWorkspace(t)
	p, out := testPrinter()

	if err := BotsCreate(BotsCreateOptions{Slug: "my-bot"}, p); err != nil {
		t.Fatalf("BotsCreate: %v", err)
	}

	dir := filepath.Join("bots", "my-bot")
	// The manifest is what makes this a first-class bot rather than a
	// loose .bot file — discoverable by `bots list`, editable in the
	// studio, dispatchable and schedulable.
	m, err := bundle.LoadManifest(filepath.Join(dir, "manifest.yaml"))
	if err != nil || m == nil {
		t.Fatalf("LoadManifest: %v (m=%v)", err, m)
	}
	if m.Name != "my-bot" {
		t.Errorf("manifest name = %q, want my-bot", m.Name)
	}

	// The workflow's parse+compile guarantee is Scaffold's, covered in
	// pkg/botscaffold — not re-asserted here.
	if !strings.Contains(out.String(), "Bot created") {
		t.Errorf("human output missing header:\n%s", out.String())
	}
}

func TestBotsCreate_TemplateAppliesAndOverridesWin(t *testing.T) {
	inTempWorkspace(t)
	p, _ := testPrinter()

	display := "My Reviewer"
	opts := BotsCreateOptions{
		Slug:        "rev-bot",
		Template:    "code-reviewer",
		DisplayName: display,
	}
	if err := BotsCreate(opts, p); err != nil {
		t.Fatalf("BotsCreate: %v", err)
	}

	m, err := bundle.LoadManifest(filepath.Join("bots", "rev-bot", "manifest.yaml"))
	if err != nil || m == nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	if m.DisplayName != display {
		t.Errorf("display_name = %q, want %q (the override must win)", m.DisplayName, display)
	}
	// The template's own fields survive where no override was given.
	if m.Description == "" {
		t.Error("description empty: the code-reviewer template's description was dropped")
	}

	// The template's var (base) must reach the workflow.
	src, err := os.ReadFile(filepath.Join("bots", "rev-bot", "main.bot"))
	if err != nil {
		t.Fatalf("read main.bot: %v", err)
	}
	if !strings.Contains(string(src), "base") {
		t.Errorf("template var 'base' missing from generated main.bot:\n%s", src)
	}
}

func TestBotsCreate_Rejects(t *testing.T) {
	tests := []struct {
		name string
		opts BotsCreateOptions
		want string
	}{
		{"invalid slug", BotsCreateOptions{Slug: "Not A Slug"}, "slug"},
		{"empty slug", BotsCreateOptions{Slug: ""}, "slug"},
		{"unknown template", BotsCreateOptions{Slug: "ok-bot", Template: "nope"}, "unknown template"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			inTempWorkspace(t)
			p, _ := testPrinter()
			err := BotsCreate(tt.opts, p)
			if err == nil {
				t.Fatalf("BotsCreate(%+v) = nil, want error", tt.opts)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %q, want it to mention %q", err, tt.want)
			}
		})
	}
}

func TestBotsCreate_RefusesExistingDirectory(t *testing.T) {
	inTempWorkspace(t)
	p, _ := testPrinter()

	if err := BotsCreate(BotsCreateOptions{Slug: "dup-bot"}, p); err != nil {
		t.Fatalf("first BotsCreate: %v", err)
	}
	err := BotsCreate(BotsCreateOptions{Slug: "dup-bot"}, p)
	if err == nil {
		t.Fatal("second BotsCreate = nil, want a collision error (must never clobber an authored bot)")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Errorf("error = %q, want it to mention the collision", err)
	}
}

func TestBotsCreate_JSONOutput(t *testing.T) {
	dir := inTempWorkspace(t)
	p, out := jsonPrinter()

	if err := BotsCreate(BotsCreateOptions{Slug: "json-bot"}, p); err != nil {
		t.Fatalf("BotsCreate: %v", err)
	}
	var res botscaffold.Result
	if err := json.Unmarshal(out.Bytes(), &res); err != nil {
		t.Fatalf("unmarshal %q: %v", out.String(), err)
	}
	// Dest is anchored to the workdir, so machine consumers get an
	// unambiguous path rather than one relative to the caller's cwd.
	if want := filepath.Join(dir, "bots", "json-bot"); res.Dir != want {
		t.Errorf("Dir = %q, want %q", res.Dir, want)
	}
	if len(res.Files) == 0 {
		t.Error("Files is empty")
	}
}

// TestBotsCreate_WorkdirAnchorsDestAndCatalog guards the convention
// alignment with `bots install`: --dest is resolved against --workdir,
// not the process cwd.
func TestBotsCreate_WorkdirAnchorsDest(t *testing.T) {
	inTempWorkspace(t)
	elsewhere := t.TempDir()
	p, _ := testPrinter()

	opts := BotsCreateOptions{Slug: "anchored", Workdir: elsewhere}
	if err := BotsCreate(opts, p); err != nil {
		t.Fatalf("BotsCreate: %v", err)
	}
	if _, err := os.Stat(filepath.Join(elsewhere, "bots", "anchored", "main.bot")); err != nil {
		t.Errorf("bundle not written under --workdir: %v", err)
	}
}

func TestBotsCreate_HonoursDest(t *testing.T) {
	inTempWorkspace(t)
	p, _ := testPrinter()

	if err := BotsCreate(BotsCreateOptions{Slug: "elsewhere", Dest: "custom"}, p); err != nil {
		t.Fatalf("BotsCreate: %v", err)
	}
	if _, err := os.Stat(filepath.Join("custom", "elsewhere", "main.bot")); err != nil {
		t.Errorf("bundle not written under --dest: %v", err)
	}
}

// TestBotsTemplates_MatchesStudioGallery guards the parity that motivated
// folding creation into one engine: the CLI must offer exactly the
// templates the studio builder shows, never a drifted subset.
func TestBotsTemplates_MatchesStudioGallery(t *testing.T) {
	p, out := jsonPrinter()
	if err := BotsTemplates(p); err != nil {
		t.Fatalf("BotsTemplates: %v", err)
	}
	var got struct {
		Templates []botscaffold.Template `json:"templates"`
	}
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	want := botscaffold.Templates()
	if len(got.Templates) != len(want) {
		t.Fatalf("got %d templates, want %d", len(got.Templates), len(want))
	}
	for i, tpl := range want {
		if got.Templates[i].ID != tpl.ID {
			t.Errorf("template[%d].ID = %q, want %q", i, got.Templates[i].ID, tpl.ID)
		}
	}
}
