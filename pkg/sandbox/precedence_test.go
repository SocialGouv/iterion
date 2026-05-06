package sandbox

import (
	"reflect"
	"testing"
)

func TestResolvePrecedence(t *testing.T) {
	cases := []struct {
		name     string
		global   *Spec
		workflow *Spec
		node     *Spec
		want     *Spec
	}{
		{
			name: "all-nil-yields-explicit-none",
			want: &Spec{Mode: ModeNone},
		},
		{
			name:   "global-only",
			global: &Spec{Mode: ModeAuto},
			want:   &Spec{Mode: ModeAuto},
		},
		{
			name:     "workflow-overrides-global",
			global:   &Spec{Mode: ModeNone},
			workflow: &Spec{Mode: ModeAuto},
			want:     &Spec{Mode: ModeAuto},
		},
		{
			name:     "node-overrides-workflow-with-none",
			workflow: &Spec{Mode: ModeAuto},
			node:     &Spec{Mode: ModeNone},
			want:     &Spec{Mode: ModeNone},
		},
		{
			name:     "node-inherit-keeps-workflow",
			workflow: &Spec{Mode: ModeAuto},
			node:     &Spec{Mode: ModeInherit},
			want:     &Spec{Mode: ModeAuto},
		},
		{
			name:     "global-inherit-falls-through-to-workflow",
			global:   &Spec{Mode: ModeInherit},
			workflow: &Spec{Mode: ModeAuto},
			want:     &Spec{Mode: ModeAuto},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Resolve(c.global, c.workflow, c.node)
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("Resolve() = %#v, want %#v", got, c.want)
			}
		})
	}
}

func TestResolveNetworkMerge(t *testing.T) {
	workflow := &Spec{
		Mode: ModeAuto,
		Network: &Network{
			Mode:  NetworkModeAllowlist,
			Rules: []string{"api.anthropic.com", "**.github.com"},
		},
	}

	t.Run("default-merge-appends-node-rules", func(t *testing.T) {
		node := &Spec{
			Mode: ModeInherit,
			Network: &Network{
				Rules: []string{"api.openai.com"},
			},
		}
		got := Resolve(nil, workflow, node)
		if got.Network == nil {
			t.Fatal("got.Network is nil")
		}
		want := []string{"api.anthropic.com", "**.github.com", "api.openai.com"}
		if !reflect.DeepEqual(got.Network.Rules, want) {
			t.Errorf("Rules = %v, want %v", got.Network.Rules, want)
		}
		// Mode preserved from parent (node didn't set it).
		if got.Network.Mode != NetworkModeAllowlist {
			t.Errorf("Mode = %v, want allowlist", got.Network.Mode)
		}
	})

	t.Run("replace-discards-parent-rules", func(t *testing.T) {
		node := &Spec{
			Mode: ModeInherit,
			Network: &Network{
				Inherit: InheritReplace,
				Mode:    NetworkModeDenylist,
				Rules:   []string{"**", "!evil.site"},
			},
		}
		got := Resolve(nil, workflow, node)
		if got.Network == nil {
			t.Fatal("got.Network is nil")
		}
		want := []string{"**", "!evil.site"}
		if !reflect.DeepEqual(got.Network.Rules, want) {
			t.Errorf("Rules = %v, want %v", got.Network.Rules, want)
		}
		if got.Network.Mode != NetworkModeDenylist {
			t.Errorf("Mode = %v, want denylist", got.Network.Mode)
		}
		// Inherit marker stripped.
		if got.Network.Inherit != InheritMerge {
			t.Errorf("Inherit = %v, want merge (stripped post-merge)", got.Network.Inherit)
		}
	})

	t.Run("node-mode-overrides-parent-when-set", func(t *testing.T) {
		node := &Spec{
			Mode: ModeInherit,
			Network: &Network{
				Mode:  NetworkModeOpen,
				Rules: nil,
			},
		}
		got := Resolve(nil, workflow, node)
		if got.Network.Mode != NetworkModeOpen {
			t.Errorf("Mode = %v, want open", got.Network.Mode)
		}
	})
}

func TestResolveImmutability(t *testing.T) {
	workflow := &Spec{
		Mode:  ModeAuto,
		Image: "alpine",
		Network: &Network{
			Mode:  NetworkModeAllowlist,
			Rules: []string{"a.com"},
		},
	}
	node := &Spec{
		Mode: ModeInherit,
		Network: &Network{
			Rules: []string{"b.com"},
		},
	}

	resolved := Resolve(nil, workflow, node)
	resolved.Image = "MUTATED"
	resolved.Network.Rules[0] = "MUTATED"

	if workflow.Image != "alpine" {
		t.Errorf("workflow.Image leaked: %q", workflow.Image)
	}
	if workflow.Network.Rules[0] != "a.com" {
		t.Errorf("workflow.Network.Rules leaked: %q", workflow.Network.Rules[0])
	}
}
