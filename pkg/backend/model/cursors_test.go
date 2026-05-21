package model

import (
	"os"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

func TestResolveCursorFragments_EnumLookup(t *testing.T) {
	decls := map[string]*ir.CursorDef{
		"ambition": {
			Name: "ambition",
			Values: []ir.CursorValue{
				{Name: "cautious", Prompt: "Stay focused."},
				{Name: "ambitious", Prompt: "Surface improvements."},
			},
		},
	}
	inv := &ir.CursorInvocation{
		Enabled:  true,
		Settings: []ir.CursorSetting{{Key: "ambition", Value: "ambitious"}},
	}
	got := resolveCursorFragments(inv, decls)
	if len(got) != 1 {
		t.Fatalf("expected 1 fragment, got %d", len(got))
	}
	if !strings.Contains(got[0], "**Ambition:** Surface improvements.") {
		t.Fatalf("unexpected fragment: %q", got[0])
	}
}

func TestResolveCursorFragments_NumericBand(t *testing.T) {
	decls := map[string]*ir.CursorDef{
		"depth": {
			Name: "depth",
			Bands: []ir.CursorBandSpec{
				{Lo: 0.0, Hi: 0.5, Prompt: "Skim."},
				{Lo: 0.5, Hi: 1.0, Prompt: "Deep dive."},
			},
		},
	}
	inv := &ir.CursorInvocation{
		Enabled:  true,
		Settings: []ir.CursorSetting{{Key: "depth", Value: "0.8"}},
	}
	got := resolveCursorFragments(inv, decls)
	if len(got) != 1 || !strings.Contains(got[0], "Deep dive.") {
		t.Fatalf("expected upper band fragment, got %v", got)
	}
}

func TestResolveCursorFragments_NumericSnapToEnumPosition(t *testing.T) {
	decls := map[string]*ir.CursorDef{
		"ambition": {
			Name: "ambition",
			Values: []ir.CursorValue{
				{Name: "cautious", Prompt: "A"},
				{Name: "balanced", Prompt: "B"},
				{Name: "ambitious", Prompt: "C"},
			},
		},
	}
	cases := []struct {
		value string
		want  string
	}{
		{"0.0", "A"}, // floor(0 * 3) = 0
		{"0.4", "B"}, // floor(0.4 * 3) = 1
		{"0.8", "C"}, // floor(0.8 * 3) = 2
		{"1.0", "C"}, // clamped to len-1
	}
	for _, tc := range cases {
		inv := &ir.CursorInvocation{
			Enabled:  true,
			Settings: []ir.CursorSetting{{Key: "ambition", Value: tc.value}},
		}
		got := resolveCursorFragments(inv, decls)
		if len(got) != 1 || !strings.Contains(got[0], tc.want) {
			t.Errorf("value=%s: expected %q, got %v", tc.value, tc.want, got)
		}
	}
}

func TestResolveCursorFragments_DisabledSkipsAll(t *testing.T) {
	decls := map[string]*ir.CursorDef{
		"ambition": {Values: []ir.CursorValue{{Name: "a", Prompt: "x"}}},
	}
	inv := &ir.CursorInvocation{
		Enabled:  false,
		Settings: []ir.CursorSetting{{Key: "ambition", Value: "a"}},
	}
	got := resolveCursorFragments(inv, decls)
	if got != nil {
		t.Fatalf("expected nil when enabled=false, got %v", got)
	}
}

func TestResolveCursorFragments_SortsAlphabetically(t *testing.T) {
	decls := map[string]*ir.CursorDef{
		"zenith": {Values: []ir.CursorValue{{Name: "a", Prompt: "Z"}}},
		"alpha":  {Values: []ir.CursorValue{{Name: "a", Prompt: "A"}}},
		"middle": {Values: []ir.CursorValue{{Name: "a", Prompt: "M"}}},
	}
	inv := &ir.CursorInvocation{
		Enabled: true,
		Settings: []ir.CursorSetting{
			{Key: "zenith", Value: "a"},
			{Key: "alpha", Value: "a"},
			{Key: "middle", Value: "a"},
		},
	}
	got := resolveCursorFragments(inv, decls)
	if len(got) != 3 {
		t.Fatalf("expected 3 fragments, got %d", len(got))
	}
	if !strings.HasPrefix(got[0], "**Alpha:**") ||
		!strings.HasPrefix(got[1], "**Middle:**") ||
		!strings.HasPrefix(got[2], "**Zenith:**") {
		t.Fatalf("not alphabetically sorted: %v", got)
	}
}

func TestResolveCursorFragments_EnvVarSubstitution(t *testing.T) {
	t.Setenv("ITERION_TEST_CURSOR_VAL", "ambitious")
	decls := map[string]*ir.CursorDef{
		"ambition": {
			Name: "ambition",
			Values: []ir.CursorValue{
				{Name: "cautious", Prompt: "stay focused"},
				{Name: "ambitious", Prompt: "expand scope"},
			},
		},
	}
	inv := &ir.CursorInvocation{
		Enabled:  true,
		Settings: []ir.CursorSetting{{Key: "ambition", Value: "${ITERION_TEST_CURSOR_VAL}"}},
	}
	got := resolveCursorFragments(inv, decls)
	if len(got) != 1 || !strings.Contains(got[0], "expand scope") {
		t.Fatalf("env-substituted value should resolve to ambitious: %v", got)
	}
}

func TestResolveCursorFragments_EnvVarDefault(t *testing.T) {
	os.Unsetenv("ITERION_TEST_CURSOR_UNSET")
	decls := map[string]*ir.CursorDef{
		"ambition": {
			Name: "ambition",
			Values: []ir.CursorValue{
				{Name: "cautious", Prompt: "stay focused"},
				{Name: "ambitious", Prompt: "expand scope"},
			},
		},
	}
	inv := &ir.CursorInvocation{
		Enabled: true,
		Settings: []ir.CursorSetting{
			{Key: "ambition", Value: "${ITERION_TEST_CURSOR_UNSET:-cautious}"},
		},
	}
	got := resolveCursorFragments(inv, decls)
	if len(got) != 1 || !strings.Contains(got[0], "stay focused") {
		t.Fatalf("env default should resolve to cautious: %v", got)
	}
}

func TestResolveCursorFragments_UnknownCursorSilentlyDropped(t *testing.T) {
	decls := map[string]*ir.CursorDef{
		"ambition": {Values: []ir.CursorValue{{Name: "a", Prompt: "p"}}},
	}
	inv := &ir.CursorInvocation{
		Enabled: true,
		Settings: []ir.CursorSetting{
			{Key: "ambition", Value: "a"},
			{Key: "nonexistent", Value: "x"},
		},
	}
	got := resolveCursorFragments(inv, decls)
	if len(got) != 1 {
		t.Fatalf("unknown cursor should be silently skipped at runtime (compile-time warned via C083): %v", got)
	}
}
