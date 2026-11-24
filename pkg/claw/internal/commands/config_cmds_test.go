package commands

import (
	"testing"
)

type mockConfigSwitcher struct {
	model          string
	permissionMode string
}

func (m *mockConfigSwitcher) CurrentModel() string          { return m.model }
func (m *mockConfigSwitcher) SetModel(model string) error   { m.model = model; return nil }
func (m *mockConfigSwitcher) CurrentPermissionMode() string { return m.permissionMode }
func (m *mockConfigSwitcher) SetPermissionMode(mode string) error {
	m.permissionMode = mode
	return nil
}

func TestConfigEnvSubcommand(t *testing.T) {
	r := NewRegistry()
	RegisterConfigCommands(r)

	handled, err := r.Execute("/config env", nil)
	if !handled {
		t.Error("expected command to be handled")
	}
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestConfigModelSubcommand(t *testing.T) {
	r := NewRegistry()
	RegisterConfigCommands(r)

	mock := &mockConfigSwitcher{model: "claude-sonnet-4-6"}

	handled, err := r.Execute("/config model", mock)
	if !handled {
		t.Error("expected command to be handled")
	}
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestConfigUnknownSubcommand(t *testing.T) {
	r := NewRegistry()
	RegisterConfigCommands(r)

	handled, err := r.Execute("/config unknown", nil)
	if !handled {
		t.Error("expected command to be handled")
	}
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestConfigHelpSubcommand(t *testing.T) {
	r := NewRegistry()
	RegisterConfigCommands(r)

	handled, err := r.Execute("/config", nil)
	if !handled {
		t.Error("expected command to be handled")
	}
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestModelCommand(t *testing.T) {
	r := NewRegistry()
	RegisterConfigCommands(r)

	mock := &mockConfigSwitcher{model: "claude-sonnet-4-6"}

	// Show current model
	handled, err := r.Execute("/model", mock)
	if !handled || err != nil {
		t.Errorf("show model: handled=%v, err=%v", handled, err)
	}

	// Set new model
	handled, err = r.Execute("/model claude-opus-4", mock)
	if !handled || err != nil {
		t.Errorf("set model: handled=%v, err=%v", handled, err)
	}
	if mock.model != "claude-opus-4" {
		t.Errorf("expected model='claude-opus-4', got %q", mock.model)
	}
}

func TestPermissionsCommand(t *testing.T) {
	r := NewRegistry()
	RegisterConfigCommands(r)

	mock := &mockConfigSwitcher{permissionMode: "default"}

	handled, err := r.Execute("/permissions workspace-write", mock)
	if !handled || err != nil {
		t.Errorf("set permissions: handled=%v, err=%v", handled, err)
	}
	if mock.permissionMode != "workspace-write" {
		t.Errorf("expected mode='workspace-write', got %q", mock.permissionMode)
	}
}

func TestCompactCommand(t *testing.T) {
	r := NewRegistry()
	RegisterConfigCommands(r)

	// Without compactor — should output fallback
	handled, err := r.Execute("/compact", "not a compactor")
	if !handled {
		t.Error("expected command to be handled")
	}
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

// mockTempController implements tempController for testing.
type mockTempController struct {
	temp float64
}

func (m *mockTempController) GetTemperature() float64        { return m.temp }
func (m *mockTempController) SetTemperature(t float64) error { m.temp = t; return nil }

func TestTemperatureCommandShow(t *testing.T) {
	r := NewRegistry()
	RegisterConfigCommands(r)

	mock := &mockTempController{temp: 0.7}

	handled, err := r.Execute("/temperature", mock)
	if !handled || err != nil {
		t.Errorf("show temp: handled=%v, err=%v", handled, err)
	}
}

func TestTemperatureCommandSet(t *testing.T) {
	r := NewRegistry()
	RegisterConfigCommands(r)

	mock := &mockTempController{temp: 0.7}

	handled, err := r.Execute("/temperature 1.5", mock)
	if !handled || err != nil {
		t.Errorf("set temp: handled=%v, err=%v", handled, err)
	}
	if mock.temp != 1.5 {
		t.Errorf("expected temp=1.5, got %f", mock.temp)
	}
}

func TestTemperatureCommandInvalidRange(t *testing.T) {
	r := NewRegistry()
	RegisterConfigCommands(r)

	mock := &mockTempController{temp: 0.7}

	_, err := r.Execute("/temperature 3.0", mock)
	if err == nil {
		t.Error("expected error for temperature > 2")
	}
}

func TestTemperatureAlias(t *testing.T) {
	r := NewRegistry()
	RegisterConfigCommands(r)

	mock := &mockTempController{temp: 0.5}

	handled, err := r.Execute("/temp", mock)
	if !handled || err != nil {
		t.Errorf("temp alias: handled=%v, err=%v", handled, err)
	}
}

func TestTemperatureNoInterface(t *testing.T) {
	r := NewRegistry()
	RegisterConfigCommands(r)

	handled, err := r.Execute("/temperature 0.5", "not a controller")
	if !handled {
		t.Error("expected command to be handled")
	}
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

// mockTokenController implements tokenController for testing.
type mockTokenController struct {
	maxTokens int
}

func (m *mockTokenController) GetMaxTokens() int        { return m.maxTokens }
func (m *mockTokenController) SetMaxTokens(n int) error { m.maxTokens = n; return nil }

func TestMaxTokensCommandShow(t *testing.T) {
	r := NewRegistry()
	RegisterConfigCommands(r)

	mock := &mockTokenController{maxTokens: 4096}

	handled, err := r.Execute("/max-tokens", mock)
	if !handled || err != nil {
		t.Errorf("show max-tokens: handled=%v, err=%v", handled, err)
	}
}

func TestMaxTokensCommandSet(t *testing.T) {
	r := NewRegistry()
	RegisterConfigCommands(r)

	mock := &mockTokenController{maxTokens: 4096}

	handled, err := r.Execute("/max-tokens 8192", mock)
	if !handled || err != nil {
		t.Errorf("set max-tokens: handled=%v, err=%v", handled, err)
	}
	if mock.maxTokens != 8192 {
		t.Errorf("expected maxTokens=8192, got %d", mock.maxTokens)
	}
}

func TestMaxTokensCommandInvalid(t *testing.T) {
	r := NewRegistry()
	RegisterConfigCommands(r)

	mock := &mockTokenController{maxTokens: 4096}

	_, err := r.Execute("/max-tokens abc", mock)
	if err == nil {
		t.Error("expected error for non-numeric token count")
	}
}

// mockSysPromptManager implements sysPromptManager for testing.
type mockSysPromptManager struct {
	prompt string
}

func (m *mockSysPromptManager) GetSystemPrompt() string        { return m.prompt }
func (m *mockSysPromptManager) SetSystemPrompt(p string) error { m.prompt = p; return nil }

func TestSystemPromptShow(t *testing.T) {
	r := NewRegistry()
	RegisterConfigCommands(r)

	mock := &mockSysPromptManager{prompt: "Be helpful."}

	handled, err := r.Execute("/system-prompt show", mock)
	if !handled || err != nil {
		t.Errorf("show sys prompt: handled=%v, err=%v", handled, err)
	}
}

func TestSystemPromptSet(t *testing.T) {
	r := NewRegistry()
	RegisterConfigCommands(r)

	mock := &mockSysPromptManager{}

	handled, err := r.Execute("/system-prompt set You are a coding assistant.", mock)
	if !handled || err != nil {
		t.Errorf("set sys prompt: handled=%v, err=%v", handled, err)
	}
	if mock.prompt != "You are a coding assistant." {
		t.Errorf("expected prompt set, got %q", mock.prompt)
	}
}

func TestSystemPromptAlias(t *testing.T) {
	r := NewRegistry()
	RegisterConfigCommands(r)

	mock := &mockSysPromptManager{prompt: "test"}

	handled, err := r.Execute("/sysprompt", mock)
	if !handled || err != nil {
		t.Errorf("sysprompt alias: handled=%v, err=%v", handled, err)
	}
}

func TestSystemPromptNoInterface(t *testing.T) {
	r := NewRegistry()
	RegisterConfigCommands(r)

	handled, err := r.Execute("/system-prompt", "not a manager")
	if !handled {
		t.Error("expected command to be handled")
	}
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

// mockProfileManager implements profileManager for testing.
type mockProfileManager struct {
	current  string
	profiles []string
	switched string
}

func (m *mockProfileManager) CurrentProfile() string          { return m.current }
func (m *mockProfileManager) SwitchProfile(name string) error { m.switched = name; return nil }
func (m *mockProfileManager) ListProfiles() []string          { return m.profiles }

func TestProfileCommandShow(t *testing.T) {
	r := NewRegistry()
	RegisterConfigCommands(r)

	mock := &mockProfileManager{current: "default", profiles: []string{"default", "dev"}}

	handled, err := r.Execute("/profile", mock)
	if !handled || err != nil {
		t.Errorf("show profile: handled=%v, err=%v", handled, err)
	}
}

func TestProfileCommandSwitch(t *testing.T) {
	r := NewRegistry()
	RegisterConfigCommands(r)

	mock := &mockProfileManager{current: "default"}

	handled, err := r.Execute("/profile dev", mock)
	if !handled || err != nil {
		t.Errorf("switch profile: handled=%v, err=%v", handled, err)
	}
	if mock.switched != "dev" {
		t.Errorf("expected switched='dev', got %q", mock.switched)
	}
}

// mockLanguageManager implements languageManager for testing.
type mockLanguageManager struct {
	lang string
}

func (m *mockLanguageManager) GetLanguage() string        { return m.lang }
func (m *mockLanguageManager) SetLanguage(l string) error { m.lang = l; return nil }

func TestLanguageCommandShow(t *testing.T) {
	r := NewRegistry()
	RegisterConfigCommands(r)

	mock := &mockLanguageManager{lang: "en"}

	handled, err := r.Execute("/language", mock)
	if !handled || err != nil {
		t.Errorf("show language: handled=%v, err=%v", handled, err)
	}
}

func TestLanguageCommandSet(t *testing.T) {
	r := NewRegistry()
	RegisterConfigCommands(r)

	mock := &mockLanguageManager{lang: "en"}

	handled, err := r.Execute("/language fr", mock)
	if !handled || err != nil {
		t.Errorf("set language: handled=%v, err=%v", handled, err)
	}
	if mock.lang != "fr" {
		t.Errorf("expected lang='fr', got %q", mock.lang)
	}
}

func TestLanguageAlias(t *testing.T) {
	r := NewRegistry()
	RegisterConfigCommands(r)

	mock := &mockLanguageManager{lang: "en"}

	handled, err := r.Execute("/lang", mock)
	if !handled || err != nil {
		t.Errorf("lang alias: handled=%v, err=%v", handled, err)
	}
}

// mockUltraPlanController implements the planController interface for ultraplan.
type mockUltraPlanController struct {
	planMode       bool
	reasoningLevel string
}

func (m *mockUltraPlanController) PlanMode() bool      { return m.planMode }
func (m *mockUltraPlanController) SetPlanMode(on bool) { m.planMode = on }
func (m *mockUltraPlanController) SetReasoningEffort(level string) error {
	m.reasoningLevel = level
	return nil
}

func TestUltraplanCommand(t *testing.T) {
	r := NewRegistry()
	RegisterConfigCommands(r)

	mock := &mockUltraPlanController{}

	handled, err := r.Execute("/ultraplan", mock)
	if !handled || err != nil {
		t.Errorf("ultraplan: handled=%v, err=%v", handled, err)
	}
	if !mock.planMode {
		t.Error("expected plan mode to be enabled")
	}
	if mock.reasoningLevel != "high" {
		t.Errorf("expected reasoning level='high', got %q", mock.reasoningLevel)
	}
}

func TestUltraplanNoInterface(t *testing.T) {
	r := NewRegistry()
	RegisterConfigCommands(r)

	handled, err := r.Execute("/ultraplan", "not a controller")
	if !handled {
		t.Error("expected command to be handled")
	}
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}
