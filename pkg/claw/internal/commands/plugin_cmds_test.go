package commands

import (
	"fmt"
	"testing"
)

// mockPluginManager implements pluginManagerLoop for testing.
type mockPluginManager struct {
	plugins     []string
	installed   string
	enabled     string
	disabled    string
	uninstalled string
	updated     string
}

func (m *mockPluginManager) PluginList() ([]string, error) { return m.plugins, nil }
func (m *mockPluginManager) PluginInstall(name string) error {
	m.installed = name
	return nil
}
func (m *mockPluginManager) PluginEnable(name string) error {
	m.enabled = name
	return nil
}
func (m *mockPluginManager) PluginDisable(name string) error {
	m.disabled = name
	return nil
}
func (m *mockPluginManager) PluginUninstall(name string) error {
	m.uninstalled = name
	return nil
}
func (m *mockPluginManager) PluginUpdate(name string) error {
	m.updated = name
	return nil
}

// mockTaskManager implements taskManagerLoop for testing.
type mockTaskManager struct {
	tasks   []string
	gotID   string
	stopped string
}

func (m *mockTaskManager) TaskList() ([]string, error) { return m.tasks, nil }
func (m *mockTaskManager) TaskGet(id string) (string, error) {
	m.gotID = id
	return fmt.Sprintf("task %s info", id), nil
}
func (m *mockTaskManager) TaskStop(id string) error {
	m.stopped = id
	return nil
}

func TestPluginListCommand(t *testing.T) {
	r := NewRegistry()
	RegisterPluginCommands(r)

	mock := &mockPluginManager{
		plugins: []string{"plugin-a", "plugin-b"},
	}

	handled, err := r.Execute("/plugin list", mock)
	if !handled {
		t.Error("expected command to be handled")
	}
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestPluginDefaultSubcommand(t *testing.T) {
	r := NewRegistry()
	RegisterPluginCommands(r)

	mock := &mockPluginManager{plugins: []string{"p1"}}

	handled, err := r.Execute("/plugin", mock)
	if !handled {
		t.Error("expected command to be handled")
	}
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestPluginSubcommands(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		checkFn func(*mockPluginManager) string
		want    string
	}{
		{
			name:    "install",
			input:   "/plugin install my-plugin",
			checkFn: func(m *mockPluginManager) string { return m.installed },
			want:    "my-plugin",
		},
		{
			name:    "enable",
			input:   "/plugin enable my-plugin",
			checkFn: func(m *mockPluginManager) string { return m.enabled },
			want:    "my-plugin",
		},
		{
			name:    "disable",
			input:   "/plugin disable my-plugin",
			checkFn: func(m *mockPluginManager) string { return m.disabled },
			want:    "my-plugin",
		},
		{
			name:    "uninstall",
			input:   "/plugin uninstall my-plugin",
			checkFn: func(m *mockPluginManager) string { return m.uninstalled },
			want:    "my-plugin",
		},
		{
			name:    "update",
			input:   "/plugin update my-plugin",
			checkFn: func(m *mockPluginManager) string { return m.updated },
			want:    "my-plugin",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := NewRegistry()
			RegisterPluginCommands(r)
			mock := &mockPluginManager{}

			handled, err := r.Execute(tt.input, mock)
			if !handled {
				t.Error("expected command to be handled")
			}
			if err != nil {
				t.Errorf("unexpected error: %v", err)
			}
			if got := tt.checkFn(mock); got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestPluginSubcommandMissingArg(t *testing.T) {
	tests := []struct {
		name  string
		input string
	}{
		{"install no arg", "/plugin install"},
		{"enable no arg", "/plugin enable"},
		{"disable no arg", "/plugin disable"},
		{"uninstall no arg", "/plugin uninstall"},
		{"update no arg", "/plugin update"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := NewRegistry()
			RegisterPluginCommands(r)
			mock := &mockPluginManager{}

			handled, err := r.Execute(tt.input, mock)
			if !handled {
				t.Error("expected command to be handled")
			}
			if err != nil {
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
}

func TestPluginAliases(t *testing.T) {
	r := NewRegistry()
	RegisterPluginCommands(r)

	mock := &mockPluginManager{plugins: []string{"p1"}}

	for _, alias := range []string{"/plugins", "/marketplace"} {
		handled, err := r.Execute(alias, mock)
		if !handled {
			t.Errorf("expected %s to be handled", alias)
		}
		if err != nil {
			t.Errorf("unexpected error for %s: %v", alias, err)
		}
	}
}

func TestPluginNilGuard(t *testing.T) {
	r := NewRegistry()
	RegisterPluginCommands(r)

	// Pass a non-pluginManagerLoop — should print fallback, no error.
	handled, err := r.Execute("/plugin list", "not a plugin manager")
	if !handled {
		t.Error("expected command to be handled")
	}
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestPluginNilLoop(t *testing.T) {
	r := NewRegistry()
	RegisterPluginCommands(r)

	// nil loop should print fallback, no error.
	handled, err := r.Execute("/plugin list", nil)
	if !handled {
		t.Error("expected command to be handled")
	}
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestTasksListCommand(t *testing.T) {
	r := NewRegistry()
	RegisterPluginCommands(r)

	mock := &mockTaskManager{tasks: []string{"task-1", "task-2"}}

	handled, err := r.Execute("/tasks list", mock)
	if !handled {
		t.Error("expected command to be handled")
	}
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestTasksGetCommand(t *testing.T) {
	r := NewRegistry()
	RegisterPluginCommands(r)

	mock := &mockTaskManager{}

	handled, err := r.Execute("/tasks get abc-123", mock)
	if !handled {
		t.Error("expected command to be handled")
	}
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if mock.gotID != "abc-123" {
		t.Errorf("expected gotID='abc-123', got %q", mock.gotID)
	}
}

func TestTasksStopCommand(t *testing.T) {
	r := NewRegistry()
	RegisterPluginCommands(r)

	mock := &mockTaskManager{}

	handled, err := r.Execute("/tasks stop abc-123", mock)
	if !handled {
		t.Error("expected command to be handled")
	}
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if mock.stopped != "abc-123" {
		t.Errorf("expected stopped='abc-123', got %q", mock.stopped)
	}
}

func TestTasksNilGuard(t *testing.T) {
	r := NewRegistry()
	RegisterPluginCommands(r)

	handled, err := r.Execute("/tasks", "not a task manager")
	if !handled {
		t.Error("expected command to be handled")
	}
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestTasksMissingArg(t *testing.T) {
	r := NewRegistry()
	RegisterPluginCommands(r)

	mock := &mockTaskManager{}

	tests := []struct {
		name  string
		input string
	}{
		{"get no arg", "/tasks get"},
		{"stop no arg", "/tasks stop"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handled, err := r.Execute(tt.input, mock)
			if !handled {
				t.Error("expected command to be handled")
			}
			if err != nil {
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
}

func TestMemoryCommand(t *testing.T) {
	r := NewRegistry()
	RegisterPluginCommands(r)

	// Memory command does not require a loop interface.
	handled, err := r.Execute("/memory", nil)
	if !handled {
		t.Error("expected command to be handled")
	}
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestSandboxCommand(t *testing.T) {
	r := NewRegistry()
	RegisterPluginCommands(r)

	handled, err := r.Execute("/sandbox", nil)
	if !handled {
		t.Error("expected command to be handled")
	}
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestInitCommand(t *testing.T) {
	r := NewRegistry()
	RegisterPluginCommands(r)

	// Without a projectInitializer, should print fallback.
	handled, err := r.Execute("/init", nil)
	if !handled {
		t.Error("expected command to be handled")
	}
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestUpgradeCommand(t *testing.T) {
	r := NewRegistry()
	RegisterPluginCommands(r)

	// Without an upgradeChecker, should print placeholder message.
	handled, err := r.Execute("/upgrade", nil)
	if !handled {
		t.Error("expected command to be handled")
	}
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestSkillsAlias(t *testing.T) {
	r := NewRegistry()
	RegisterPluginCommands(r)

	handled, err := r.Execute("/skill list", nil)
	if !handled {
		t.Error("expected /skill alias to be handled")
	}
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestParseTwoQuoted(t *testing.T) {
	tests := []struct {
		input   string
		first   string
		second  string
		wantErr bool
	}{
		{`"*/5 * * * *" "check status"`, "*/5 * * * *", "check status", false},
		{`"a" "b"`, "a", "b", false},
		{`no quotes`, "", "", true},
		{`"only one"`, "", "", true},
		{``, "", "", true},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			first, second, err := parseTwoQuoted(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Error("expected error")
				}
				return
			}
			if err != nil {
				t.Errorf("unexpected error: %v", err)
			}
			if first != tt.first {
				t.Errorf("first: got %q, want %q", first, tt.first)
			}
			if second != tt.second {
				t.Errorf("second: got %q, want %q", second, tt.second)
			}
		})
	}
}
