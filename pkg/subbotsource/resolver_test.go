package subbotsource

import (
	"context"
	"path/filepath"
	"testing"
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
