package subbotsource

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/bundle"
)

func TestResolverResolveFilesystemSource(t *testing.T) {
	t.Parallel()

	absChild := filepath.Join(string(filepath.Separator), "shared", "child.bot")
	tests := []struct {
		name      string
		resolver  *Resolver
		parent    string
		requested string
		want      string
	}{
		{
			name:      "relative to parent workflow",
			resolver:  NewResolver(ResolverOptions{}),
			parent:    filepath.Join("project", "bots", "parent.bot"),
			requested: filepath.Join("subbots", "child.bot"),
			want:      filepath.Join("project", "bots", "subbots", "child.bot"),
		},
		{
			name:      "relative parent traversal is cleaned by join",
			resolver:  NewResolver(ResolverOptions{}),
			parent:    filepath.Join("project", "bots", "parent.bot"),
			requested: filepath.Join("..", "shared", "child.bot"),
			want:      filepath.Join("project", "shared", "child.bot"),
		},
		{
			name:      "absolute source remains unchanged",
			resolver:  NewResolver(ResolverOptions{ParentlessBaseDir: filepath.Join("ignored", "workspace")}),
			parent:    filepath.Join("project", "bots", "parent.bot"),
			requested: absChild,
			want:      absChild,
		},
		{
			name:      "inline parent uses launch surface fallback",
			resolver:  NewResolver(ResolverOptions{ParentlessBaseDir: filepath.Join("studio", "workspace")}),
			requested: filepath.Join("bots", "child.bot"),
			want:      filepath.Join("studio", "workspace", "bots", "child.bot"),
		},
		{
			name:      "file parent outranks launch surface fallback",
			resolver:  NewResolver(ResolverOptions{ParentlessBaseDir: filepath.Join("studio", "workspace")}),
			parent:    filepath.Join("external", "bundle", "parent.bot"),
			requested: "child.bot",
			want:      filepath.Join("external", "bundle", "child.bot"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := tt.resolver.Resolve(context.Background(), tt.parent, tt.requested)
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			if got.Kind != KindFile {
				t.Errorf("Kind = %q, want %q", got.Kind, KindFile)
			}
			if got.Path != tt.want {
				t.Errorf("Path = %q, want %q", got.Path, tt.want)
			}
		})
	}
}

func TestResolverResolveLockedBotExport(t *testing.T) {
	t.Parallel()
	root, consumerPath, installed := botResolutionFixture(t)
	hash, err := bundle.ContentHashDir(installed)
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(root, "bots.lock"), fmt.Sprintf(`version: 1
dependencies:
  shared-planner:
    source: https://example.test/shared-planner.git
    ref: v1.0.0
    bundle_sha256: %s
`, hash))

	got, err := NewResolver(ResolverOptions{WorkDir: root}).Resolve(
		context.Background(), consumerPath, "bot://shared-planner/hierarchy-feature-author",
	)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Kind != KindBot || got.Bundle == nil || got.Dependency == nil {
		t.Fatalf("resolution = %+v", got)
	}
	if got.Path != filepath.Join(installed, "main.bot") {
		t.Fatalf("path = %q", got.Path)
	}
	if got.Dependency.BundleSHA256 != hash || got.Dependency.BundleVersion != "1.0.0" {
		t.Fatalf("dependency = %+v", got.Dependency)
	}
}

func TestResolverBotExportFailures(t *testing.T) {
	t.Parallel()
	root, consumerPath, installed := botResolutionFixture(t)
	hash, err := bundle.ContentHashDir(installed)
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(root, "bots.lock"), fmt.Sprintf(`version: 1
dependencies:
  shared-planner:
    source: https://example.test/shared-planner.git
    ref: v1.0.0
    bundle_sha256: %s
`, hash))

	tests := []struct {
		name   string
		parent string
		source string
		want   string
	}{
		{name: "undeclared exact bundle", parent: consumerPath, source: "bot://shared_planner/hierarchy-feature-author", want: "not declared"},
		{name: "missing export", parent: consumerPath, source: "bot://shared-planner/unknown", want: "does not export"},
		{name: "malformed uri", parent: consumerPath, source: "bot://shared-planner/a/b", want: "expected one workflow id"},
		{name: "bare consumer", parent: filepath.Join(root, "loose.bot"), source: "bot://shared-planner/hierarchy-feature-author", want: "requires the parent workflow"},
	}
	writeTestFile(t, filepath.Join(root, "loose.bot"), "workflow loose {}\n")
	resolver := NewResolver(ResolverOptions{WorkDir: root})
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := resolver.Resolve(context.Background(), tt.parent, tt.source)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want substring %q", err, tt.want)
			}
		})
	}
}

func TestResolverRejectsDivergentInstalledBundle(t *testing.T) {
	t.Parallel()
	root, consumerPath, _ := botResolutionFixture(t)
	writeTestFile(t, filepath.Join(root, "bots.lock"), `version: 1
dependencies:
  shared-planner:
    source: https://example.test/shared-planner.git
    ref: v1.0.0
    bundle_sha256: aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
`)
	_, err := NewResolver(ResolverOptions{WorkDir: root}).Resolve(
		context.Background(), consumerPath, "bot://shared-planner/hierarchy-feature-author",
	)
	if err == nil || !strings.Contains(err.Error(), "lock requires") {
		t.Fatalf("error = %v", err)
	}
}

func botResolutionFixture(t *testing.T) (root, consumerPath, installed string) {
	t.Helper()
	root = t.TempDir()
	consumer := filepath.Join(root, "bots", "consumer")
	installed = filepath.Join(root, ".botz", "shared-planner")
	if err := os.MkdirAll(consumer, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(installed, 0o755); err != nil {
		t.Fatal(err)
	}
	consumerPath = filepath.Join(consumer, "main.bot")
	writeTestFile(t, consumerPath, "workflow consumer {}\n")
	writeTestFile(t, filepath.Join(consumer, "manifest.yaml"), `name: consumer
schema_version: 1
dependencies:
  workflows:
    - name: shared-planner
`)
	writeTestFile(t, filepath.Join(installed, "main.bot"), "workflow shared {}\n")
	writeTestFile(t, filepath.Join(installed, "manifest.yaml"), `name: shared-planner
version: 1.0.0
schema_version: 1
enabled: false
exports:
  workflows:
    - id: hierarchy-feature-author
      path: main.bot
`)
	return root, consumerPath, installed
}

func writeTestFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}
