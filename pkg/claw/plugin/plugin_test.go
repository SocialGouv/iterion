package plugin

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// --- PluginKind tests ---

func TestPluginKindString(t *testing.T) {
	tests := []struct {
		kind PluginKind
		want string
	}{
		{KindBuiltin, "builtin"},
		{KindBundled, "bundled"},
		{KindExternal, "external"},
	}
	for _, tt := range tests {
		if got := tt.kind.String(); got != tt.want {
			t.Errorf("PluginKind(%s).String() = %q, want %q", tt.kind, got, tt.want)
		}
	}
}

func TestPluginKindJSONRoundTrip(t *testing.T) {
	for _, kind := range []PluginKind{KindBuiltin, KindBundled, KindExternal} {
		data, err := json.Marshal(kind)
		if err != nil {
			t.Fatalf("Marshal(%v): %v", kind, err)
		}

		var got PluginKind
		if err := json.Unmarshal(data, &got); err != nil {
			t.Fatalf("Unmarshal(%s): %v", data, err)
		}
		if got != kind {
			t.Errorf("round-trip: got %v, want %v", got, kind)
		}
	}
}

func TestPluginKindJSONValues(t *testing.T) {
	tests := []struct {
		kind PluginKind
		json string
	}{
		{KindBuiltin, `"builtin"`},
		{KindBundled, `"bundled"`},
		{KindExternal, `"external"`},
	}
	for _, tt := range tests {
		data, _ := json.Marshal(tt.kind)
		if string(data) != tt.json {
			t.Errorf("Marshal(%v) = %s, want %s", tt.kind, data, tt.json)
		}
	}
}

func TestPluginKindUnmarshalInvalid(t *testing.T) {
	var k PluginKind
	if err := json.Unmarshal([]byte(`"bogus"`), &k); err == nil {
		t.Error("expected error for invalid kind")
	}
}

// --- Permission string values ---

func TestPluginPermissionValues(t *testing.T) {
	if PermissionRead != "read" {
		t.Errorf("PermissionRead = %q", PermissionRead)
	}
	if PermissionWrite != "write" {
		t.Errorf("PermissionWrite = %q", PermissionWrite)
	}
	if PermissionExecute != "execute" {
		t.Errorf("PermissionExecute = %q", PermissionExecute)
	}
}

func TestPluginToolPermissionValues(t *testing.T) {
	if ToolPermReadOnly != "read-only" {
		t.Errorf("ToolPermReadOnly = %q", ToolPermReadOnly)
	}
	if ToolPermWorkspaceWrite != "workspace-write" {
		t.Errorf("ToolPermWorkspaceWrite = %q", ToolPermWorkspaceWrite)
	}
	if ToolPermDangerFullAccess != "danger-full-access" {
		t.Errorf("ToolPermDangerFullAccess = %q", ToolPermDangerFullAccess)
	}
}

// --- LoadManifest tests ---

func TestLoadManifestValid(t *testing.T) {
	dir := t.TempDir()
	manifestJSON := `{
		"name": "test-plugin",
		"version": "1.0.0",
		"description": "A test plugin",
		"permissions": ["read", "write"],
		"defaultEnabled": true,
		"hooks": {
			"PreToolUse": ["echo pre"],
			"PostToolUse": ["echo post"],
			"PostToolUseFailure": []
		},
		"lifecycle": {
			"Init": ["echo init"],
			"Shutdown": ["echo shutdown"]
		},
		"tools": [
			{
				"name": "my-tool",
				"description": "Does things",
				"inputSchema": {"type": "object"},
				"command": "echo",
				"args": ["hello"],
				"requiredPermission": "read-only"
			}
		],
		"commands": [
			{
				"name": "my-cmd",
				"description": "A command",
				"command": "echo cmd"
			}
		]
	}`
	path := filepath.Join(dir, "plugin.json")
	os.WriteFile(path, []byte(manifestJSON), 0o644)

	m, err := LoadManifest(path)
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	if m.Name != "test-plugin" {
		t.Errorf("Name = %q", m.Name)
	}
	if m.Version != "1.0.0" {
		t.Errorf("Version = %q", m.Version)
	}
	if !m.DefaultEnabled {
		t.Error("DefaultEnabled should be true")
	}
	if len(m.Tools) != 1 {
		t.Fatalf("Tools len = %d", len(m.Tools))
	}
	if m.Tools[0].Name != "my-tool" {
		t.Errorf("Tool name = %q", m.Tools[0].Name)
	}
	if len(m.Hooks.PreToolUse) != 1 {
		t.Errorf("PreToolUse len = %d", len(m.Hooks.PreToolUse))
	}
	if len(m.Commands) != 1 {
		t.Errorf("Commands len = %d", len(m.Commands))
	}
	if m.RawJSON == nil {
		t.Error("RawJSON should be populated")
	}
}

func TestLoadManifestMissingFile(t *testing.T) {
	_, err := LoadManifest("/nonexistent/plugin.json")
	if err == nil {
		t.Fatal("expected error for missing file")
	}
	pe, ok := err.(*PluginError)
	if !ok {
		t.Fatalf("expected *PluginError, got %T", err)
	}
	if pe.Kind != ErrIO {
		t.Errorf("Kind = %q, want %q", pe.Kind, ErrIO)
	}
}

func TestLoadManifestInvalidJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "plugin.json")
	os.WriteFile(path, []byte(`{not json`), 0o644)

	_, err := LoadManifest(path)
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
	pe, ok := err.(*PluginError)
	if !ok {
		t.Fatalf("expected *PluginError, got %T", err)
	}
	if pe.Kind != ErrJSON {
		t.Errorf("Kind = %q, want %q", pe.Kind, ErrJSON)
	}
}

// --- ValidateManifest tests ---

func TestValidateEmptyFields(t *testing.T) {
	m := &PluginManifest{}
	errs := ValidateManifest(m, "", KindBuiltin)

	codes := make(map[string]int)
	for _, e := range errs {
		codes[e.Code]++
	}
	if codes["empty_field"] < 3 {
		t.Errorf("expected at least 3 empty_field errors, got %d: %v", codes["empty_field"], errs)
	}
}

func TestValidateDuplicatePermissions(t *testing.T) {
	m := &PluginManifest{
		Name:        "test",
		Version:     "1.0.0",
		Description: "test",
		Permissions: []PluginPermission{"read", "read"},
	}
	errs := ValidateManifest(m, "", KindBuiltin)

	found := false
	for _, e := range errs {
		if e.Code == "duplicate_permission" {
			found = true
		}
	}
	if !found {
		t.Error("expected duplicate_permission error")
	}
}

func TestValidateInvalidPermission(t *testing.T) {
	m := &PluginManifest{
		Name:        "test",
		Version:     "1.0.0",
		Description: "test",
		Permissions: []PluginPermission{"destroy"},
	}
	errs := ValidateManifest(m, "", KindBuiltin)

	found := false
	for _, e := range errs {
		if e.Code == "invalid_permission" {
			found = true
		}
	}
	if !found {
		t.Error("expected invalid_permission error")
	}
}

func TestValidateToolEmptyName(t *testing.T) {
	m := &PluginManifest{
		Name:        "test",
		Version:     "1.0.0",
		Description: "test",
		Tools: []PluginToolManifest{
			{Name: "", Command: "echo"},
		},
	}
	errs := ValidateManifest(m, "", KindBuiltin)

	found := false
	for _, e := range errs {
		if e.Code == "empty_entry_field" {
			found = true
		}
	}
	if !found {
		t.Error("expected empty_entry_field error for tool name")
	}
}

func TestValidateToolEmptyCommand(t *testing.T) {
	m := &PluginManifest{
		Name:        "test",
		Version:     "1.0.0",
		Description: "test",
		Tools: []PluginToolManifest{
			{Name: "foo", Command: ""},
		},
	}
	errs := ValidateManifest(m, "", KindBuiltin)

	found := false
	for _, e := range errs {
		if e.Code == "empty_entry_field" {
			found = true
		}
	}
	if !found {
		t.Error("expected empty_entry_field error for tool command")
	}
}

func TestValidateDuplicateToolName(t *testing.T) {
	m := &PluginManifest{
		Name:        "test",
		Version:     "1.0.0",
		Description: "test",
		Tools: []PluginToolManifest{
			{Name: "foo", Command: "echo"},
			{Name: "foo", Command: "echo"},
		},
	}
	errs := ValidateManifest(m, "", KindBuiltin)

	found := false
	for _, e := range errs {
		if e.Code == "duplicate_entry" {
			found = true
		}
	}
	if !found {
		t.Error("expected duplicate_entry error")
	}
}

func TestValidateInvalidInputSchema(t *testing.T) {
	m := &PluginManifest{
		Name:        "test",
		Version:     "1.0.0",
		Description: "test",
		Tools: []PluginToolManifest{
			{Name: "foo", Command: "echo", InputSchema: json.RawMessage(`"not an object"`)},
		},
	}
	errs := ValidateManifest(m, "", KindBuiltin)

	found := false
	for _, e := range errs {
		if e.Code == "invalid_tool_input_schema" {
			found = true
		}
	}
	if !found {
		t.Error("expected invalid_tool_input_schema error")
	}
}

func TestValidateInvalidToolPermission(t *testing.T) {
	m := &PluginManifest{
		Name:        "test",
		Version:     "1.0.0",
		Description: "test",
		Tools: []PluginToolManifest{
			{Name: "foo", Command: "echo", RequiredPermission: "admin"},
		},
	}
	errs := ValidateManifest(m, "", KindBuiltin)

	found := false
	for _, e := range errs {
		if e.Code == "invalid_tool_required_permission" {
			found = true
		}
	}
	if !found {
		t.Error("expected invalid_tool_required_permission error")
	}
}

func TestValidateUnsupportedContracts(t *testing.T) {
	dir := t.TempDir()
	raw := `{
		"name": "test",
		"version": "1.0.0",
		"description": "test",
		"skills": [],
		"mcpServers": {},
		"agents": []
	}`
	path := filepath.Join(dir, "plugin.json")
	os.WriteFile(path, []byte(raw), 0o644)

	m, err := LoadManifest(path)
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}

	errs := ValidateManifest(m, "", KindBuiltin)

	codes := make(map[string]int)
	for _, e := range errs {
		codes[e.Code]++
	}
	if codes["unsupported_contract"] != 3 {
		t.Errorf("expected 3 unsupported_contract errors, got %d: %v", codes["unsupported_contract"], errs)
	}
}

func TestValidateUnsupportedHookName(t *testing.T) {
	dir := t.TempDir()
	raw := `{
		"name": "test",
		"version": "1.0.0",
		"description": "test",
		"hooks": {
			"PreToolUse": ["cmd1"],
			"OnError": ["cmd2"],
			"BeforeAll": ["cmd3"]
		}
	}`
	path := filepath.Join(dir, "plugin.json")
	os.WriteFile(path, []byte(raw), 0o644)

	m, err := LoadManifest(path)
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}

	errs := ValidateManifest(m, "", KindBuiltin)

	unsupportedCount := 0
	for _, e := range errs {
		if e.Code == "unsupported_contract" {
			unsupportedCount++
		}
	}
	// OnError and BeforeAll are unsupported; PreToolUse is valid.
	if unsupportedCount != 2 {
		t.Errorf("expected 2 unsupported_contract errors for invalid hook names, got %d: %v", unsupportedCount, errs)
	}
}

func TestValidateStringCommands(t *testing.T) {
	// Rust validates raw JSON before deserialization, so string commands are
	// caught by the contract gap detector. In Go, LoadManifest would fail to
	// unmarshal strings into PluginCommandManifest, so we construct the manifest
	// manually with RawJSON set (mimicking the raw JSON path).
	rawJSON := `{
		"name": "test",
		"version": "1.0.0",
		"description": "test",
		"commands": ["glob-pattern", "another"]
	}`
	m := &PluginManifest{
		Name:        "test",
		Version:     "1.0.0",
		Description: "test",
		RawJSON:     json.RawMessage(rawJSON),
	}

	errs := ValidateManifest(m, "", KindBuiltin)

	found := false
	for _, e := range errs {
		if e.Code == "unsupported_contract" && e.Message == "commands array contains string entries; only object commands are supported" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected unsupported_contract error for string commands, got: %v", errs)
	}
}

func TestValidateObjectCommandsAllowed(t *testing.T) {
	// Object commands should NOT trigger the unsupported_contract error.
	dir := t.TempDir()
	raw := `{
		"name": "test",
		"version": "1.0.0",
		"description": "test",
		"commands": [{"name": "foo", "description": "a cmd", "command": "echo"}]
	}`
	path := filepath.Join(dir, "plugin.json")
	os.WriteFile(path, []byte(raw), 0o644)

	m, err := LoadManifest(path)
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}

	errs := ValidateManifest(m, "", KindBuiltin)

	for _, e := range errs {
		if e.Code == "unsupported_contract" && e.Message == "commands array contains string entries; only object commands are supported" {
			t.Errorf("object commands should not trigger string-command rejection, got: %v", e)
		}
	}
}

func TestValidateMissingToolPath(t *testing.T) {
	// Use a relative path (./...) so it's NOT treated as a literal shell command.
	m := &PluginManifest{
		Name:        "test",
		Version:     "1.0.0",
		Description: "test",
		Tools: []PluginToolManifest{
			{Name: "foo", Description: "d", Command: "./nonexistent-binary-xyz"},
		},
	}
	errs := ValidateManifest(m, t.TempDir(), KindExternal)

	found := false
	for _, e := range errs {
		if e.Code == "missing_path" {
			found = true
		}
	}
	if !found {
		t.Error("expected missing_path error for external plugin")
	}
}

func TestValidateToolLiteralCommandSkipsPathCheck(t *testing.T) {
	// Literal shell commands should not be path-validated.
	m := &PluginManifest{
		Name:        "test",
		Version:     "1.0.0",
		Description: "test",
		Tools: []PluginToolManifest{
			{Name: "foo", Description: "d", Command: "some-binary", InputSchema: []byte(`{}`)},
		},
	}
	errs := ValidateManifest(m, t.TempDir(), KindExternal)

	for _, e := range errs {
		if e.Code == "missing_path" {
			t.Errorf("literal command should not produce missing_path, got: %s", e.Message)
		}
	}
}

// --- PluginRegistry tests ---

func makeBuiltin(id string, enabled bool, hooks PluginHooks, tools []PluginToolDefinition) RegisteredPlugin {
	return RegisteredPlugin{
		Plugin: &BuiltinPlugin{
			Meta: PluginMetadata{
				ID:   id,
				Name: id,
			},
			HooksDef: hooks,
			ToolsDef: tools,
		},
		Enabled: enabled,
	}
}

func TestRegistrySortsByID(t *testing.T) {
	reg := NewPluginRegistry([]RegisteredPlugin{
		makeBuiltin("charlie", true, PluginHooks{}, nil),
		makeBuiltin("alpha", true, PluginHooks{}, nil),
		makeBuiltin("bravo", true, PluginHooks{}, nil),
	})

	plugins := reg.Plugins()
	if len(plugins) != 3 {
		t.Fatalf("len = %d", len(plugins))
	}
	ids := []string{
		plugins[0].Plugin.Metadata().ID,
		plugins[1].Plugin.Metadata().ID,
		plugins[2].Plugin.Metadata().ID,
	}
	if ids[0] != "alpha" || ids[1] != "bravo" || ids[2] != "charlie" {
		t.Errorf("not sorted: %v", ids)
	}
}

func TestRegistryGetAndContains(t *testing.T) {
	reg := NewPluginRegistry([]RegisteredPlugin{
		makeBuiltin("foo", true, PluginHooks{}, nil),
		makeBuiltin("bar", true, PluginHooks{}, nil),
	})

	if rp := reg.Get("foo"); rp == nil {
		t.Error("Get(foo) returned nil")
	}
	if rp := reg.Get("baz"); rp != nil {
		t.Error("Get(baz) should return nil")
	}
	if !reg.Contains("bar") {
		t.Error("Contains(bar) should be true")
	}
	if reg.Contains("nope") {
		t.Error("Contains(nope) should be false")
	}
}

func TestRegistryAggregatedHooks(t *testing.T) {
	reg := NewPluginRegistry([]RegisteredPlugin{
		makeBuiltin("a", true, PluginHooks{PreToolUse: []string{"cmd1"}}, nil),
		makeBuiltin("b", true, PluginHooks{PreToolUse: []string{"cmd2"}, PostToolUse: []string{"cmd3"}}, nil),
		makeBuiltin("c", false, PluginHooks{PreToolUse: []string{"cmd4"}}, nil), // disabled
	})

	h, err := reg.AggregatedHooks()
	if err != nil {
		t.Fatalf("AggregatedHooks: %v", err)
	}
	if len(h.PreToolUse) != 2 {
		t.Errorf("PreToolUse len = %d, want 2", len(h.PreToolUse))
	}
	if len(h.PostToolUse) != 1 {
		t.Errorf("PostToolUse len = %d, want 1", len(h.PostToolUse))
	}
}

func TestRegistryAggregatedToolsConflict(t *testing.T) {
	reg := NewPluginRegistry([]RegisteredPlugin{
		makeBuiltin("a", true, PluginHooks{}, []PluginToolDefinition{
			{Name: "shared-tool", Description: "from a"},
		}),
		makeBuiltin("b", true, PluginHooks{}, []PluginToolDefinition{
			{Name: "shared-tool", Description: "from b"},
			{Name: "unique-tool", Description: "only b"},
		}),
	})

	_, err := reg.AggregatedTools()
	if err == nil {
		t.Fatal("expected error on tool name conflict")
	}
	pe, ok := err.(*PluginError)
	if !ok {
		t.Fatalf("expected *PluginError, got %T", err)
	}
	if pe.Kind != ErrInvalidManifest {
		t.Errorf("Kind = %q, want %q", pe.Kind, ErrInvalidManifest)
	}
}

func TestRegistryAggregatedToolsNoConflict(t *testing.T) {
	reg := NewPluginRegistry([]RegisteredPlugin{
		makeBuiltin("a", true, PluginHooks{}, []PluginToolDefinition{
			{Name: "tool-a", Description: "from a"},
		}),
		makeBuiltin("b", true, PluginHooks{}, []PluginToolDefinition{
			{Name: "tool-b", Description: "from b"},
		}),
	})

	tools, err := reg.AggregatedTools()
	if err != nil {
		t.Fatalf("AggregatedTools: %v", err)
	}
	if len(tools) != 2 {
		t.Errorf("tools len = %d, want 2", len(tools))
	}
}

func TestRegistryAggregatedToolsDisabledSkipped(t *testing.T) {
	reg := NewPluginRegistry([]RegisteredPlugin{
		makeBuiltin("a", true, PluginHooks{}, []PluginToolDefinition{
			{Name: "tool-a"},
		}),
		makeBuiltin("b", false, PluginHooks{}, []PluginToolDefinition{
			{Name: "tool-b"},
		}),
	})

	tools, err := reg.AggregatedTools()
	if err != nil {
		t.Fatalf("AggregatedTools: %v", err)
	}
	if len(tools) != 1 {
		t.Errorf("tools len = %d, want 1", len(tools))
	}
}

func TestRegistryInitializeShutdownBuiltin(t *testing.T) {
	reg := NewPluginRegistry([]RegisteredPlugin{
		makeBuiltin("a", true, PluginHooks{}, nil),
		makeBuiltin("b", true, PluginHooks{}, nil),
	})

	if err := reg.Initialize(); err != nil {
		t.Errorf("Initialize: %v", err)
	}
	if err := reg.Shutdown(); err != nil {
		t.Errorf("Shutdown: %v", err)
	}
}

func TestRegistrySummaries(t *testing.T) {
	reg := NewPluginRegistry([]RegisteredPlugin{
		makeBuiltin("x", true, PluginHooks{}, nil),
		makeBuiltin("y", false, PluginHooks{}, nil),
	})

	summaries := reg.Summaries()
	if len(summaries) != 2 {
		t.Fatalf("len = %d", len(summaries))
	}
	if summaries[0].Metadata.ID != "x" || !summaries[0].Enabled {
		t.Errorf("summaries[0] = %+v", summaries[0])
	}
	if summaries[1].Metadata.ID != "y" || summaries[1].Enabled {
		t.Errorf("summaries[1] = %+v", summaries[1])
	}
}

// --- BuiltinPlugin tests ---

func TestBuiltinPluginNoOps(t *testing.T) {
	p := &BuiltinPlugin{
		Meta: PluginMetadata{ID: "builtin-test", Name: "builtin-test"},
	}
	if err := p.Validate(); err != nil {
		t.Errorf("Validate: %v", err)
	}
	if err := p.Initialize(); err != nil {
		t.Errorf("Initialize: %v", err)
	}
	if err := p.Shutdown(); err != nil {
		t.Errorf("Shutdown: %v", err)
	}
}

// --- InstalledPluginRegistry JSON round-trip ---

func TestInstalledPluginRegistryRoundTrip(t *testing.T) {
	reg := InstalledPluginRegistry{
		Plugins: map[string]InstalledPluginRecord{
			"my-plugin": {
				Kind:          KindExternal,
				ID:            "my-plugin",
				Name:          "my-plugin",
				Version:       "2.0.0",
				Description:   "A plugin",
				InstallPath:   "/path/to/plugin",
				Source:        LocalPathSource("/source"),
				InstalledAtMs: 1000,
				UpdatedAtMs:   2000,
			},
		},
	}

	data, err := json.Marshal(reg)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var got InstalledPluginRegistry
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	p, ok := got.Plugins["my-plugin"]
	if !ok {
		t.Fatal("missing my-plugin")
	}
	if p.Kind != KindExternal {
		t.Errorf("Kind = %v", p.Kind)
	}
	if p.Version != "2.0.0" {
		t.Errorf("Version = %q", p.Version)
	}
	if p.InstalledAtMs != 1000 {
		t.Errorf("InstalledAtMs = %d", p.InstalledAtMs)
	}
	if p.Source.Type != "local_path" {
		t.Errorf("Source.Type = %q, want local_path", p.Source.Type)
	}
	if p.Source.Path != "/source" {
		t.Errorf("Source.Path = %q, want /source", p.Source.Path)
	}
}

func TestPluginInstallSourceJSON(t *testing.T) {
	// Test local_path round-trip
	local := LocalPathSource("/home/user/plugin")
	data, err := json.Marshal(local)
	if err != nil {
		t.Fatal(err)
	}
	var decoded PluginInstallSource
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Type != "local_path" || decoded.Path != "/home/user/plugin" {
		t.Errorf("local round-trip: %+v", decoded)
	}

	// Test git_url round-trip
	git := GitURLSource("https://github.com/example/plugin.git")
	data, err = json.Marshal(git)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Type != "git_url" || decoded.URL != "https://github.com/example/plugin.git" {
		t.Errorf("git round-trip: %+v", decoded)
	}
}

func TestPluginInstallSourcePath(t *testing.T) {
	local := LocalPathSource("/path")
	if local.SourcePath() != "/path" {
		t.Errorf("SourcePath() = %q", local.SourcePath())
	}
	git := GitURLSource("https://example.com")
	if git.SourcePath() != "" {
		t.Errorf("SourcePath() = %q, want empty for git_url", git.SourcePath())
	}
}

// --- PluginError tests ---

func TestPluginErrorWithCause(t *testing.T) {
	cause := &PluginError{Kind: ErrIO, Message: "inner"}
	outer := &PluginError{Kind: ErrJSON, Message: "outer", Cause: cause}

	if outer.Unwrap() != cause {
		t.Error("Unwrap should return cause")
	}
	s := outer.Error()
	if s == "" {
		t.Error("Error() should not be empty")
	}
}

func TestPluginErrorWithoutCause(t *testing.T) {
	e := &PluginError{Kind: ErrNotFound, Message: "not found"}
	if e.Unwrap() != nil {
		t.Error("Unwrap should return nil")
	}
}

func TestValidationErrorString(t *testing.T) {
	e := &ValidationError{Code: "empty_field", Message: "name is required"}
	s := e.Error()
	if s != "[empty_field] name is required" {
		t.Errorf("Error() = %q", s)
	}
}

func TestLoadFailureString(t *testing.T) {
	f := &LoadFailure{
		PluginRoot: "/path",
		Kind:       KindExternal,
		Source:     "dir",
		Err:        &PluginError{Kind: ErrIO, Message: "read failed"},
	}
	s := f.Error()
	if s == "" {
		t.Error("Error() should not be empty")
	}
}

// --- PluginManager tests ---

func TestPluginManagerDiscoverBundled(t *testing.T) {
	dir := t.TempDir()
	bundledRoot := filepath.Join(dir, "bundled")
	pluginDir := filepath.Join(bundledRoot, "my-bundled")
	os.MkdirAll(pluginDir, 0o755)
	os.WriteFile(filepath.Join(pluginDir, "plugin.json"), []byte(`{
		"name": "my-bundled",
		"version": "1.0.0",
		"description": "bundled plugin",
		"defaultEnabled": true
	}`), 0o644)

	mgr, err := NewPluginManager(PluginManagerConfig{
		ConfigHome:  filepath.Join(dir, "config"),
		BundledRoot: bundledRoot,
	})
	if err != nil {
		t.Fatalf("NewPluginManager: %v", err)
	}

	plugins, failures := mgr.DiscoverPlugins()
	if len(failures) != 0 {
		t.Errorf("failures: %v", failures)
	}
	if len(plugins) != 1 {
		t.Fatalf("plugins len = %d", len(plugins))
	}
	if plugins[0].Plugin.Metadata().Name != "my-bundled" {
		t.Errorf("Name = %q", plugins[0].Plugin.Metadata().Name)
	}
	if plugins[0].Plugin.Metadata().ID != "my-bundled@bundled" {
		t.Errorf("ID = %q, want %q", plugins[0].Plugin.Metadata().ID, "my-bundled@bundled")
	}
	if !plugins[0].Enabled {
		t.Error("should be enabled by default")
	}
}

func TestPluginManagerInstallLocal(t *testing.T) {
	dir := t.TempDir()
	srcDir := filepath.Join(dir, "source-plugin")
	os.MkdirAll(srcDir, 0o755)
	os.WriteFile(filepath.Join(srcDir, "plugin.json"), []byte(`{
		"name": "local-plugin",
		"version": "0.1.0",
		"description": "a local plugin"
	}`), 0o644)

	configHome := filepath.Join(dir, "config")
	mgr, err := NewPluginManager(PluginManagerConfig{
		ConfigHome: configHome,
	})
	if err != nil {
		t.Fatalf("NewPluginManager: %v", err)
	}

	outcome, err := mgr.Install(srcDir)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	expectedID := "local-plugin@external"
	if outcome.PluginID != expectedID {
		t.Errorf("PluginID = %q, want %q", outcome.PluginID, expectedID)
	}
	if outcome.Version != "0.1.0" {
		t.Errorf("Version = %q", outcome.Version)
	}

	// Verify auto-enabled after install (matching Rust behavior).
	if !mgr.config.EnabledPlugins[expectedID] {
		t.Error("plugin should be auto-enabled after install")
	}

	// Verify install directory uses sanitized ID.
	expectedDir := filepath.Join(configHome, "plugins", "installed", "local-plugin-external")
	if outcome.InstallPath != expectedDir {
		t.Errorf("InstallPath = %q, want %q", outcome.InstallPath, expectedDir)
	}

	// Verify registry persisted
	mgr2, err := NewPluginManager(PluginManagerConfig{
		ConfigHome: configHome,
	})
	if err != nil {
		t.Fatalf("NewPluginManager reload: %v", err)
	}
	if _, ok := mgr2.registry.Plugins[expectedID]; !ok {
		t.Error("plugin not in registry after reload")
	}
}

func TestPluginManagerInstallGitUnsupported(t *testing.T) {
	mgr, _ := NewPluginManager(PluginManagerConfig{
		ConfigHome: t.TempDir(),
	})
	_, err := mgr.Install("https://github.com/example/plugin.git")
	if err == nil {
		t.Fatal("expected error for git install")
	}
}

func TestPluginManagerUninstall(t *testing.T) {
	dir := t.TempDir()
	srcDir := filepath.Join(dir, "src")
	os.MkdirAll(srcDir, 0o755)
	os.WriteFile(filepath.Join(srcDir, "plugin.json"), []byte(`{
		"name": "rm-me",
		"version": "1.0.0",
		"description": "removable"
	}`), 0o644)

	mgr, _ := NewPluginManager(PluginManagerConfig{ConfigHome: filepath.Join(dir, "config")})
	mgr.Install(srcDir)

	rmID := "rm-me@external"
	if err := mgr.Uninstall(rmID); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	if _, ok := mgr.registry.Plugins[rmID]; ok {
		t.Error("plugin should be removed from registry")
	}
}

func TestPluginManagerUninstallNotFound(t *testing.T) {
	mgr, _ := NewPluginManager(PluginManagerConfig{ConfigHome: t.TempDir()})
	err := mgr.Uninstall("nonexistent")
	if err == nil {
		t.Fatal("expected error for uninstalling nonexistent plugin")
	}
}

func TestPluginManagerEnableDisable(t *testing.T) {
	mgr, _ := NewPluginManager(PluginManagerConfig{ConfigHome: t.TempDir()})

	mgr.Enable("foo")
	if !mgr.config.EnabledPlugins["foo"] {
		t.Error("foo should be enabled")
	}
	mgr.Disable("foo")
	if mgr.config.EnabledPlugins["foo"] {
		t.Error("foo should be disabled")
	}
}

func TestPluginManagerEnableDisablePersistence(t *testing.T) {
	configHome := filepath.Join(t.TempDir(), "config")

	// Create first manager and enable a plugin.
	mgr1, err := NewPluginManager(PluginManagerConfig{ConfigHome: configHome})
	if err != nil {
		t.Fatalf("NewPluginManager: %v", err)
	}
	if err := mgr1.Enable("my-plugin"); err != nil {
		t.Fatalf("Enable: %v", err)
	}
	if err := mgr1.Disable("other-plugin"); err != nil {
		t.Fatalf("Disable: %v", err)
	}

	// Create second manager from the same config — state should survive.
	mgr2, err := NewPluginManager(PluginManagerConfig{ConfigHome: configHome})
	if err != nil {
		t.Fatalf("NewPluginManager reload: %v", err)
	}
	if !mgr2.config.EnabledPlugins["my-plugin"] {
		t.Error("my-plugin should be enabled after reload")
	}
	if mgr2.config.EnabledPlugins["other-plugin"] {
		t.Error("other-plugin should be disabled after reload")
	}
}

func TestPluginManagerUpdate(t *testing.T) {
	dir := t.TempDir()
	srcDir := filepath.Join(dir, "source-plugin")
	os.MkdirAll(srcDir, 0o755)
	os.WriteFile(filepath.Join(srcDir, "plugin.json"), []byte(`{
		"name": "updatable",
		"version": "1.0.0",
		"description": "v1"
	}`), 0o644)

	configHome := filepath.Join(dir, "config")
	mgr, err := NewPluginManager(PluginManagerConfig{ConfigHome: configHome})
	if err != nil {
		t.Fatalf("NewPluginManager: %v", err)
	}

	// Install v1.
	_, err = mgr.Install(srcDir)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	updateID := "updatable@external"
	if mgr.registry.Plugins[updateID].Version != "1.0.0" {
		t.Fatalf("expected version 1.0.0, got %s", mgr.registry.Plugins[updateID].Version)
	}

	// Update source to v2.
	os.WriteFile(filepath.Join(srcDir, "plugin.json"), []byte(`{
		"name": "updatable",
		"version": "2.0.0",
		"description": "v2"
	}`), 0o644)

	outcome, err := mgr.Update(updateID)
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if outcome.OldVersion != "1.0.0" {
		t.Errorf("OldVersion = %q, want 1.0.0", outcome.OldVersion)
	}
	if outcome.NewVersion != "2.0.0" {
		t.Errorf("NewVersion = %q, want 2.0.0", outcome.NewVersion)
	}

	// Verify registry was persisted.
	mgr2, err := NewPluginManager(PluginManagerConfig{ConfigHome: configHome})
	if err != nil {
		t.Fatalf("NewPluginManager reload: %v", err)
	}
	if mgr2.registry.Plugins[updateID].Version != "2.0.0" {
		t.Errorf("persisted version = %q, want 2.0.0", mgr2.registry.Plugins[updateID].Version)
	}
}

func TestPluginManagerUpdateNotFound(t *testing.T) {
	mgr, _ := NewPluginManager(PluginManagerConfig{ConfigHome: t.TempDir()})
	_, err := mgr.Update("nonexistent")
	if err == nil {
		t.Fatal("expected error for updating nonexistent plugin")
	}
}

func TestPluginManagerSyncBundled(t *testing.T) {
	dir := t.TempDir()
	bundledRoot := filepath.Join(dir, "bundled")
	configHome := filepath.Join(dir, "config")

	// Create two bundled plugins.
	for _, name := range []string{"alpha", "bravo"} {
		pluginDir := filepath.Join(bundledRoot, name)
		os.MkdirAll(pluginDir, 0o755)
		os.WriteFile(filepath.Join(pluginDir, "plugin.json"), []byte(`{
			"name": "`+name+`",
			"version": "1.0.0",
			"description": "bundled `+name+`"
		}`), 0o644)
	}

	mgr, err := NewPluginManager(PluginManagerConfig{
		ConfigHome:  configHome,
		BundledRoot: bundledRoot,
	})
	if err != nil {
		t.Fatalf("NewPluginManager: %v", err)
	}

	// Sync — both should be added.
	if err := mgr.SyncBundledPlugins(); err != nil {
		t.Fatalf("SyncBundledPlugins: %v", err)
	}
	if len(mgr.registry.Plugins) != 2 {
		t.Fatalf("registry len = %d, want 2", len(mgr.registry.Plugins))
	}
	if mgr.registry.Plugins["alpha@bundled"].Version != "1.0.0" {
		t.Errorf("alpha version = %q", mgr.registry.Plugins["alpha@bundled"].Version)
	}

	// Update bravo to v2.0.0 on disk.
	os.WriteFile(filepath.Join(bundledRoot, "bravo", "plugin.json"), []byte(`{
		"name": "bravo",
		"version": "2.0.0",
		"description": "bundled bravo v2"
	}`), 0o644)

	if err := mgr.SyncBundledPlugins(); err != nil {
		t.Fatalf("SyncBundledPlugins v2: %v", err)
	}
	if mgr.registry.Plugins["bravo@bundled"].Version != "2.0.0" {
		t.Errorf("bravo version after sync = %q, want 2.0.0", mgr.registry.Plugins["bravo@bundled"].Version)
	}

	// Remove alpha from disk.
	os.RemoveAll(filepath.Join(bundledRoot, "alpha"))

	if err := mgr.SyncBundledPlugins(); err != nil {
		t.Fatalf("SyncBundledPlugins prune: %v", err)
	}
	if _, exists := mgr.registry.Plugins["alpha@bundled"]; exists {
		t.Error("alpha should have been pruned from registry")
	}
	if _, exists := mgr.registry.Plugins["bravo@bundled"]; !exists {
		t.Error("bravo should still be in registry")
	}

	// Verify registry was persisted.
	mgr2, err := NewPluginManager(PluginManagerConfig{
		ConfigHome:  configHome,
		BundledRoot: bundledRoot,
	})
	if err != nil {
		t.Fatalf("NewPluginManager reload: %v", err)
	}
	if _, exists := mgr2.registry.Plugins["alpha@bundled"]; exists {
		t.Error("alpha should still be absent after reload")
	}
	if mgr2.registry.Plugins["bravo@bundled"].Version != "2.0.0" {
		t.Errorf("bravo version after reload = %q", mgr2.registry.Plugins["bravo@bundled"].Version)
	}
}

func TestPluginManagerDiscoverClaudePluginFallback(t *testing.T) {
	dir := t.TempDir()
	bundledRoot := filepath.Join(dir, "bundled")
	pluginDir := filepath.Join(bundledRoot, "packaged-plugin")
	claudeDir := filepath.Join(pluginDir, ".claude-plugin")
	os.MkdirAll(claudeDir, 0o755)
	os.WriteFile(filepath.Join(claudeDir, "plugin.json"), []byte(`{
		"name": "packaged",
		"version": "1.0.0",
		"description": "uses .claude-plugin path"
	}`), 0o644)

	mgr, err := NewPluginManager(PluginManagerConfig{
		ConfigHome:  filepath.Join(dir, "config"),
		BundledRoot: bundledRoot,
	})
	if err != nil {
		t.Fatalf("NewPluginManager: %v", err)
	}

	plugins, failures := mgr.DiscoverPlugins()
	if len(failures) != 0 {
		t.Errorf("failures: %v", failures)
	}
	if len(plugins) != 1 {
		t.Fatalf("plugins len = %d, want 1", len(plugins))
	}
	if plugins[0].Plugin.Metadata().Name != "packaged" {
		t.Errorf("Name = %q", plugins[0].Plugin.Metadata().Name)
	}
}

// --- FIX-5 tests: pluginID and sanitizePluginID ---

func TestPluginIDFormat(t *testing.T) {
	id := pluginID("my-plugin", "external")
	if id != "my-plugin@external" {
		t.Errorf("pluginID = %q, want my-plugin@external", id)
	}
}

func TestSanitizePluginID(t *testing.T) {
	tests := []struct {
		input, want string
	}{
		{"simple", "simple"},
		{"my-plugin@external", "my-plugin-external"},
		{"path/to\\plugin:v1", "path-to-plugin-v1"},
		{"no@slash/back\\colon:", "no-slash-back-colon-"},
	}
	for _, tt := range tests {
		got := sanitizePluginID(tt.input)
		if got != tt.want {
			t.Errorf("sanitizePluginID(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

// --- FIX-6 test: external plugin enabled default ---

func TestDiscoverExternalPluginDefaultDisabled(t *testing.T) {
	dir := t.TempDir()
	externalDir := filepath.Join(dir, "external")
	pluginDir := filepath.Join(externalDir, "ext-plugin")
	os.MkdirAll(pluginDir, 0o755)
	os.WriteFile(filepath.Join(pluginDir, "plugin.json"), []byte(`{
		"name": "ext-plugin",
		"version": "1.0.0",
		"description": "external plugin",
		"defaultEnabled": true
	}`), 0o644)

	mgr, err := NewPluginManager(PluginManagerConfig{
		ConfigHome:   filepath.Join(dir, "config"),
		ExternalDirs: []string{externalDir},
	})
	if err != nil {
		t.Fatalf("NewPluginManager: %v", err)
	}

	plugins, _ := mgr.DiscoverPlugins()
	if len(plugins) != 1 {
		t.Fatalf("plugins len = %d", len(plugins))
	}
	// External plugins default to disabled regardless of defaultEnabled
	if plugins[0].Enabled {
		t.Error("external plugin should default to disabled, even with defaultEnabled=true")
	}
	// Verify ID uses name@marketplace format
	if plugins[0].Plugin.Metadata().ID != "ext-plugin@external" {
		t.Errorf("ID = %q, want %q", plugins[0].Plugin.Metadata().ID, "ext-plugin@external")
	}
}

// --- FIX-8 test: tool description validation ---

func TestValidateToolDescriptionEmpty(t *testing.T) {
	m := &PluginManifest{
		Name:        "test",
		Version:     "1.0.0",
		Description: "test",
		Tools: []PluginToolManifest{
			{Name: "foo", Description: "", Command: "echo"},
		},
	}
	errs := ValidateManifest(m, "", KindBuiltin)

	found := false
	for _, e := range errs {
		if e.Code == "empty_entry_field" && e.Message == `tool "foo" description is required` {
			found = true
		}
	}
	if !found {
		t.Errorf("expected empty_entry_field error for empty tool description, got: %v", errs)
	}
}

// --- FIX-10 test: hook and lifecycle path validation ---

func TestValidateHookPathMissing(t *testing.T) {
	m := &PluginManifest{
		Name:        "test",
		Version:     "1.0.0",
		Description: "test",
		Hooks: PluginHooks{
			PreToolUse: []string{"./nonexistent-hook-script.sh"},
		},
	}
	errs := ValidateManifest(m, t.TempDir(), KindExternal)

	found := false
	for _, e := range errs {
		if e.Code == "missing_path" {
			found = true
		}
	}
	if !found {
		t.Error("expected missing_path error for non-existent hook command path")
	}
}

func TestValidateLifecyclePathMissing(t *testing.T) {
	m := &PluginManifest{
		Name:        "test",
		Version:     "1.0.0",
		Description: "test",
		Lifecycle: PluginLifecycle{
			Init: []string{"./nonexistent-init.sh"},
		},
	}
	errs := ValidateManifest(m, t.TempDir(), KindBundled)

	found := false
	for _, e := range errs {
		if e.Code == "missing_path" {
			found = true
		}
	}
	if !found {
		t.Error("expected missing_path error for non-existent lifecycle command path")
	}
}

func TestValidateHookPathNotCheckedForBuiltin(t *testing.T) {
	m := &PluginManifest{
		Name:        "test",
		Version:     "1.0.0",
		Description: "test",
		Hooks: PluginHooks{
			PreToolUse: []string{"./nonexistent-hook.sh"},
		},
	}
	errs := ValidateManifest(m, t.TempDir(), KindBuiltin)

	for _, e := range errs {
		if e.Code == "missing_path" {
			t.Error("should not check hook paths for builtin plugins")
		}
	}
}

func TestValidateLiteralCommandSkipsPathCheck(t *testing.T) {
	// Shell commands like "echo hello" should NOT be validated as paths.
	m := &PluginManifest{
		Name:        "test",
		Version:     "1.0.0",
		Description: "test",
		Hooks: PluginHooks{
			PreToolUse: []string{"echo hello"},
		},
		Lifecycle: PluginLifecycle{
			Init: []string{"python -c 'print(1)'"},
		},
		Tools: []PluginToolManifest{
			{Name: "tool1", Description: "d", Command: "my-cli-tool", InputSchema: []byte(`{}`)},
		},
	}
	errs := ValidateManifest(m, t.TempDir(), KindExternal)

	for _, e := range errs {
		if e.Code == "missing_path" {
			t.Errorf("literal shell commands should not produce missing_path errors, got: %s", e.Message)
		}
	}
}

func TestValidateRelativePathStillChecked(t *testing.T) {
	// Commands starting with ./ or ../ should still be validated as paths.
	m := &PluginManifest{
		Name:        "test",
		Version:     "1.0.0",
		Description: "test",
		Hooks: PluginHooks{
			PreToolUse: []string{"./nonexistent.sh"},
		},
	}
	errs := ValidateManifest(m, t.TempDir(), KindExternal)

	found := false
	for _, e := range errs {
		if e.Code == "missing_path" {
			found = true
		}
	}
	if !found {
		t.Error("relative path commands (./...) should still be validated")
	}
}

func TestPluginKindMarketplace(t *testing.T) {
	tests := []struct {
		kind PluginKind
		want string
	}{
		{KindBuiltin, "builtin"},
		{KindBundled, "bundled"},
		{KindExternal, "external"},
	}
	for _, tt := range tests {
		if got := tt.kind.Marketplace(); got != tt.want {
			t.Errorf("PluginKind(%s).Marketplace() = %q, want %q", tt.kind, got, tt.want)
		}
	}
}

func TestDiscoverPluginIDFormat(t *testing.T) {
	// Verify discovered plugins use name@marketplace ID format.
	dir := t.TempDir()
	bundledRoot := filepath.Join(dir, "bundled")
	pluginDir := filepath.Join(bundledRoot, "test-plugin")
	os.MkdirAll(pluginDir, 0o755)
	os.WriteFile(filepath.Join(pluginDir, "plugin.json"), []byte(`{
		"name": "test-plugin",
		"version": "1.0.0",
		"description": "test"
	}`), 0o644)

	mgr, _ := NewPluginManager(PluginManagerConfig{
		ConfigHome:  filepath.Join(dir, "config"),
		BundledRoot: bundledRoot,
	})
	plugins, _ := mgr.DiscoverPlugins()
	if len(plugins) != 1 {
		t.Fatalf("expected 1 plugin, got %d", len(plugins))
	}
	if plugins[0].Plugin.Metadata().ID != "test-plugin@bundled" {
		t.Errorf("ID = %q, want %q", plugins[0].Plugin.Metadata().ID, "test-plugin@bundled")
	}
}

// --- validatePaths tests (BUGFIX-2) ---

func TestValidatePathsLiteralCommand(t *testing.T) {
	// Literal commands (e.g. "python my_script.py") should be skipped by validatePaths.
	tools := []PluginToolManifest{
		{Name: "run-py", Command: "python my_script.py"},
	}
	err := validatePaths("/nonexistent/root", tools)
	if err != nil {
		t.Errorf("validatePaths should skip literal commands, got: %v", err)
	}
}

func TestValidatePathsRelativePathMissing(t *testing.T) {
	// Relative path starting with "./" should error if not found.
	tools := []PluginToolManifest{
		{Name: "run-sh", Command: "./nonexistent/script.sh"},
	}
	err := validatePaths(t.TempDir(), tools)
	if err == nil {
		t.Error("validatePaths should error for missing relative path")
	}
}

func TestValidatePathsRelativePathExists(t *testing.T) {
	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "script.sh")
	if err := os.WriteFile(scriptPath, []byte("#!/bin/sh\necho ok"), 0o755); err != nil {
		t.Fatal(err)
	}
	tools := []PluginToolManifest{
		{Name: "run-sh", Command: "./script.sh"},
	}
	err := validatePaths(dir, tools)
	if err != nil {
		t.Errorf("validatePaths should pass for existing relative path, got: %v", err)
	}
}

func TestPluginManagerSyncBundledEmptyRoot(t *testing.T) {
	mgr, _ := NewPluginManager(PluginManagerConfig{ConfigHome: t.TempDir()})
	// No BundledRoot set — should be a no-op.
	if err := mgr.SyncBundledPlugins(); err != nil {
		t.Fatalf("SyncBundledPlugins: %v", err)
	}
}

// FuzzPluginManifestDecode verifies that manifest JSON decoding never panics.
func FuzzPluginManifestDecode(f *testing.F) {
	// Seed corpus with representative JSON inputs.
	f.Add(`{"name":"test","version":"1.0","description":"test plugin","permissions":["read"],"defaultEnabled":true}`)
	f.Add(`{"name":"","version":"","description":""}`)
	f.Add(`{}`)
	f.Add(`null`)
	f.Add(`[]`)
	f.Add(`not json at all`)
	f.Add(`{"name":"p","version":"1","description":"d","hooks":{"PreToolUse":["./a.sh"]}}`)
	f.Add(`{"name":"p","version":"1","description":"d","tools":[{"name":"t","description":"d","inputSchema":{},"command":"./t.sh"}]}`)
	f.Add(`{"name":"p","version":"1","description":"d","skills":["oops"]}`)

	f.Fuzz(func(t *testing.T, input string) {
		// Write the input to a temp file and try to load it.
		dir := t.TempDir()
		path := filepath.Join(dir, "plugin.json")
		if err := os.WriteFile(path, []byte(input), 0644); err != nil {
			t.Skip("cannot write temp file")
		}

		// Must not panic — errors are fine.
		m, err := LoadManifest(path)
		if err != nil {
			return // expected for most fuzz inputs
		}

		// If parsing succeeded, validate doesn't panic.
		_ = ValidateManifest(m, "", KindExternal)
	})
}

func TestPluginManifestGoldenFixture(t *testing.T) {
	goldenPath := "../testdata/golden/plugin_manifest.json"
	m, err := LoadManifest(goldenPath)
	if err != nil {
		t.Fatalf("load golden manifest: %v", err)
	}

	if m.Name != "test-plugin" {
		t.Errorf("name: got %q, want %q", m.Name, "test-plugin")
	}
	if m.Version != "1.0.0" {
		t.Errorf("version: got %q, want %q", m.Version, "1.0.0")
	}
	if m.Description != "A test plugin" {
		t.Errorf("description: got %q", m.Description)
	}
	if len(m.Permissions) != 2 {
		t.Fatalf("permissions: got %d, want 2", len(m.Permissions))
	}
	if !m.DefaultEnabled {
		t.Error("defaultEnabled should be true")
	}
	if len(m.Hooks.PreToolUse) != 1 || m.Hooks.PreToolUse[0] != "./guard.sh" {
		t.Errorf("hooks.PreToolUse: got %v", m.Hooks.PreToolUse)
	}
	if len(m.Tools) != 1 {
		t.Fatalf("tools: got %d, want 1", len(m.Tools))
	}
	if m.Tools[0].Name != "test-tool" {
		t.Errorf("tool name: got %q", m.Tools[0].Name)
	}
	if m.Tools[0].RequiredPermission != ToolPermReadOnly {
		t.Errorf("tool permission: got %q, want %q", m.Tools[0].RequiredPermission, ToolPermReadOnly)
	}
}
