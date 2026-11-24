package commands

import (
	"testing"
)

// mockThemeLoop implements themeLoop for testing.
type mockThemeLoop struct {
	current string
	themes  []string
}

func (m *mockThemeLoop) SetTheme(name string) error { m.current = name; return nil }
func (m *mockThemeLoop) CurrentTheme() string       { return m.current }
func (m *mockThemeLoop) ListThemes() []string       { return m.themes }

// mockEffortLoop implements effortLoop for testing.
type mockEffortLoop struct {
	level string
}

func (m *mockEffortLoop) SetEffort(level string) error { m.level = level; return nil }
func (m *mockEffortLoop) GetEffort() string            { return m.level }

// mockToggleLoop implements toggleLoop for testing.
type mockToggleLoop struct {
	toggles map[string]bool
}

func (m *mockToggleLoop) GetToggle(name string) bool        { return m.toggles[name] }
func (m *mockToggleLoop) SetToggle(name string, value bool) { m.toggles[name] = value }

func TestThemeCommand(t *testing.T) {
	r := NewRegistry()
	RegisterUXCommands(r)

	ml := &mockThemeLoop{current: "dark", themes: []string{"dark", "light", "solarized"}}

	// Set theme
	handled, err := r.Execute("/theme light", ml)
	if !handled {
		t.Fatal("expected /theme to be handled")
	}
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ml.current != "light" {
		t.Fatalf("expected theme 'light', got %q", ml.current)
	}

	// Show current theme (no arg)
	handled, err = r.Execute("/theme", ml)
	if !handled || err != nil {
		t.Fatalf("expected handled with no error, got handled=%v err=%v", handled, err)
	}
}

func TestEffortCommand(t *testing.T) {
	r := NewRegistry()
	RegisterUXCommands(r)

	ml := &mockEffortLoop{level: "medium"}

	t.Run("valid effort", func(t *testing.T) {
		_, err := r.Execute("/effort high", ml)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if ml.level != "high" {
			t.Fatalf("expected effort 'high', got %q", ml.level)
		}
	})

	t.Run("invalid effort", func(t *testing.T) {
		_, err := r.Execute("/effort ultra", ml)
		if err == nil {
			t.Fatal("expected error for invalid effort level")
		}
	})

	t.Run("show current effort", func(t *testing.T) {
		_, err := r.Execute("/effort", ml)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})
}

func TestVimToggle(t *testing.T) {
	r := NewRegistry()
	RegisterUXCommands(r)

	ml := &mockToggleLoop{toggles: map[string]bool{"vim": false}}

	// Enable vim mode
	_, err := r.Execute("/vim", ml)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ml.toggles["vim"] {
		t.Fatal("expected vim toggle to be true")
	}

	// Disable vim mode
	_, err = r.Execute("/vim", ml)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ml.toggles["vim"] {
		t.Fatal("expected vim toggle to be false")
	}
}
