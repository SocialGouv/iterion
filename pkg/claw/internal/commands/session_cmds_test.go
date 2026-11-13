package commands

import (
	"strings"
	"testing"
)

// mockSessionManager implements SessionManager for testing.
type mockSessionManager struct {
	sessions []string
	forked   string
	loaded   string
	deleted  string
}

func (m *mockSessionManager) ListSessions() ([]string, error) {
	return m.sessions, nil
}

func (m *mockSessionManager) ForkSession(name string) (string, error) {
	m.forked = name
	return "fork-123", nil
}

func (m *mockSessionManager) LoadSession(id string) error {
	m.loaded = id
	return nil
}

func (m *mockSessionManager) DeleteSession(id string) error {
	m.deleted = id
	return nil
}

func TestSessionListCommand(t *testing.T) {
	r := NewRegistry()
	RegisterSessionCommands(r)

	mock := &mockSessionManager{
		sessions: []string{"session-1", "session-2", "session-3"},
	}

	handled, err := r.Execute("/session list", mock)
	if !handled {
		t.Error("expected command to be handled")
	}
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestSessionForkCommand(t *testing.T) {
	r := NewRegistry()
	RegisterSessionCommands(r)

	mock := &mockSessionManager{}

	handled, err := r.Execute("/session fork my-branch", mock)
	if !handled {
		t.Error("expected command to be handled")
	}
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if mock.forked != "my-branch" {
		t.Errorf("expected forked='my-branch', got %q", mock.forked)
	}
}

func TestSessionDeleteCommand(t *testing.T) {
	r := NewRegistry()
	RegisterSessionCommands(r)

	mock := &mockSessionManager{}

	handled, err := r.Execute("/session delete old-session", mock)
	if !handled {
		t.Error("expected command to be handled")
	}
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if mock.deleted != "old-session" {
		t.Errorf("expected deleted='old-session', got %q", mock.deleted)
	}
}

func TestSessionSwitchCommand(t *testing.T) {
	r := NewRegistry()
	RegisterSessionCommands(r)

	mock := &mockSessionManager{}

	handled, err := r.Execute("/session switch target-id", mock)
	if !handled {
		t.Error("expected command to be handled")
	}
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if mock.loaded != "target-id" {
		t.Errorf("expected loaded='target-id', got %q", mock.loaded)
	}
}

func TestSessionDefaultSubcommand(t *testing.T) {
	r := NewRegistry()
	RegisterSessionCommands(r)

	mock := &mockSessionManager{
		sessions: []string{"s1"},
	}

	// No args should default to "list"
	handled, err := r.Execute("/session", mock)
	if !handled {
		t.Error("expected command to be handled")
	}
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestSessionWithoutManager(t *testing.T) {
	r := NewRegistry()
	RegisterSessionCommands(r)

	// Pass a non-SessionManager — should output fallback message
	handled, err := r.Execute("/session list", "not a session manager")
	if !handled {
		t.Error("expected command to be handled")
	}
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestResumeCommand(t *testing.T) {
	r := NewRegistry()
	RegisterSessionCommands(r)

	mock := &mockSessionManager{}
	handled, err := r.Execute("/resume path/to/session.json", mock)
	if !handled {
		t.Error("expected command to be handled")
	}
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if !strings.Contains(mock.loaded, "path/to/session.json") {
		t.Errorf("expected loaded path, got %q", mock.loaded)
	}
}

// mockHistoryViewer implements historyViewer for testing.
type mockHistoryViewer struct {
	entries []string
	count   int
}

func (m *mockHistoryViewer) ShowHistory(count int) ([]string, error) {
	m.count = count
	return m.entries, nil
}

func TestHistoryCommand(t *testing.T) {
	r := NewRegistry()
	RegisterSessionCommands(r)

	mock := &mockHistoryViewer{
		entries: []string{"Turn 1: Hello", "Turn 2: World"},
	}

	handled, err := r.Execute("/history", mock)
	if !handled {
		t.Error("expected command to be handled")
	}
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if mock.count != 10 {
		t.Errorf("expected default count=10, got %d", mock.count)
	}
}

func TestHistoryCommandWithCount(t *testing.T) {
	r := NewRegistry()
	RegisterSessionCommands(r)

	mock := &mockHistoryViewer{entries: []string{"entry"}}

	handled, err := r.Execute("/history 5", mock)
	if !handled || err != nil {
		t.Errorf("history 5: handled=%v, err=%v", handled, err)
	}
	if mock.count != 5 {
		t.Errorf("expected count=5, got %d", mock.count)
	}
}

func TestHistoryCommandNoInterface(t *testing.T) {
	r := NewRegistry()
	RegisterSessionCommands(r)

	handled, err := r.Execute("/history", "not a history viewer")
	if !handled {
		t.Error("expected command to be handled")
	}
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestWorkspaceCommand(t *testing.T) {
	r := NewRegistry()
	RegisterSessionCommands(r)

	handled, err := r.Execute("/workspace", nil)
	if !handled {
		t.Error("expected command to be handled")
	}
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestWorkspaceAlias(t *testing.T) {
	r := NewRegistry()
	RegisterSessionCommands(r)

	handled, err := r.Execute("/ws", nil)
	if !handled {
		t.Error("expected /ws alias to be handled")
	}
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

// mockFocusManager implements focusManager for testing.
type mockFocusManager struct {
	focused   string
	unfocused string
}

func (m *mockFocusManager) AddFocus(path string) error {
	m.focused = path
	return nil
}

func (m *mockFocusManager) RemoveFocus(path string) error {
	m.unfocused = path
	return nil
}

func TestFocusCommand(t *testing.T) {
	r := NewRegistry()
	RegisterSessionCommands(r)

	mock := &mockFocusManager{}

	handled, err := r.Execute("/focus src/main.go", mock)
	if !handled || err != nil {
		t.Errorf("focus: handled=%v, err=%v", handled, err)
	}
	if mock.focused != "src/main.go" {
		t.Errorf("expected focused='src/main.go', got %q", mock.focused)
	}
}

func TestFocusCommandNoArgs(t *testing.T) {
	r := NewRegistry()
	RegisterSessionCommands(r)

	mock := &mockFocusManager{}

	handled, err := r.Execute("/focus", mock)
	if !handled || err != nil {
		t.Errorf("focus no args: handled=%v, err=%v", handled, err)
	}
	if mock.focused != "" {
		t.Errorf("expected no focus set, got %q", mock.focused)
	}
}

func TestFocusCommandNoInterface(t *testing.T) {
	r := NewRegistry()
	RegisterSessionCommands(r)

	handled, err := r.Execute("/focus src/", "not a focus manager")
	if !handled {
		t.Error("expected command to be handled")
	}
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestUnfocusCommand(t *testing.T) {
	r := NewRegistry()
	RegisterSessionCommands(r)

	mock := &mockFocusManager{}

	handled, err := r.Execute("/unfocus src/main.go", mock)
	if !handled || err != nil {
		t.Errorf("unfocus: handled=%v, err=%v", handled, err)
	}
	if mock.unfocused != "src/main.go" {
		t.Errorf("expected unfocused='src/main.go', got %q", mock.unfocused)
	}
}

func TestUnfocusCommandNoArgs(t *testing.T) {
	r := NewRegistry()
	RegisterSessionCommands(r)

	mock := &mockFocusManager{}

	handled, err := r.Execute("/unfocus", mock)
	if !handled || err != nil {
		t.Errorf("unfocus no args: handled=%v, err=%v", handled, err)
	}
}
