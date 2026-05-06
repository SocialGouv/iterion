package runtime

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/sandbox"
)

func TestPickMode(t *testing.T) {
	inlineWf := &ir.Workflow{
		Sandbox: &ir.SandboxSpec{
			Mode:  string(sandbox.ModeInline),
			Image: "alpine:3.20",
		},
	}
	autoWf := &ir.Workflow{
		Sandbox: &ir.SandboxSpec{
			Mode: string(sandbox.ModeAuto),
		},
	}
	emptyWf := &ir.Workflow{}

	cases := []struct {
		name       string
		wf         *ir.Workflow
		cli        string
		global     string
		wantMode   string
		wantSource string
	}{
		{"cli none beats workflow", inlineWf, "none", "", "none", "cli flag --sandbox"},
		{"cli auto loses to inline workflow block", inlineWf, "auto", "", "inline", "workflow sandbox: block (overrides --sandbox=auto)"},
		{"cli auto wins over auto workflow (no contradiction)", autoWf, "auto", "", "auto", "cli flag --sandbox"},
		{"cli auto on empty workflow", emptyWf, "auto", "", "auto", "cli flag --sandbox"},
		{"workflow inline wins when no cli", inlineWf, "", "auto", "inline", "workflow sandbox: block"},
		{"global default fallback", emptyWf, "", "auto", "auto", "ITERION_SANDBOX_DEFAULT"},
		{"nil workflow + cli", nil, "auto", "", "auto", "cli flag --sandbox"},
		{"nothing set", emptyWf, "", "", "", "default (no sandbox)"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gotMode, gotSource := pickMode(c.wf, c.cli, c.global)
			if gotMode != c.wantMode {
				t.Errorf("mode = %q, want %q", gotMode, c.wantMode)
			}
			if !strings.HasPrefix(gotSource, c.wantSource) {
				t.Errorf("source = %q, want prefix %q", gotSource, c.wantSource)
			}
		})
	}
}
