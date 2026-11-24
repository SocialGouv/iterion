package docker

import (
	"context"
	"errors"
	"github.com/SocialGouv/iterion/pkg/sandbox"
	"strings"
	"testing"
)

type privateIdentity struct {
	path   string
	called bool
}

func (g *privateIdentity) HostPath() string { return g.path }
func (g *privateIdentity) ValidateHostPath(ctx context.Context) error {
	g.called = true
	return errors.New("private-original-replaced")
}
func TestRunFilesGrantDockerCreateBorder(t *testing.T) {
	p := t.TempDir()
	g := &privateIdentity{path: p}
	spec := sandbox.Spec{Mode: sandbox.ModeInline, Image: "iterion-toctou-witness:never-pulled", WorkspaceFolder: LegacyDefaultWorkspace, BindSourceChecks: []sandbox.BindSourceCheck{{Root: p, Path: p, Identity: g}}}
	d := &Driver{cli: CLI{rt: RuntimeDocker}}
	_, err := d.Start(context.Background(), preparedWithoutPrepare(t, spec), sandbox.RunInfo{RunID: "private", WorkspacePath: t.TempDir()})
	if !g.called || err == nil || !strings.Contains(err.Error(), "private-original-replaced") {
		t.Fatalf("border was not identity-checked: %v called=%v", err, g.called)
	}
}
