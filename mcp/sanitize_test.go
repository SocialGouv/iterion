package mcp

import "testing"

func TestRemoveEmptyStrings(t *testing.T) {
	rules := DefaultSanitizationRules()
	args := map[string]interface{}{
		"file_path": "/tmp/test.go",
		"pages":     "",
		"limit":     "",
	}
	for _, rule := range rules {
		if rule.Match("SomeTool") {
			rule.Apply("SomeTool", args, "")
		}
	}
	if _, ok := args["pages"]; ok {
		t.Fatal("expected empty 'pages' to be removed")
	}
	if _, ok := args["limit"]; ok {
		t.Fatal("expected empty 'limit' to be removed")
	}
	if args["file_path"] != "/tmp/test.go" {
		t.Fatal("non-empty string should be preserved")
	}
}

func TestCodexWorkspace(t *testing.T) {
	rules := DefaultSanitizationRules()
	args := map[string]interface{}{
		"prompt":  "hello",
		"sandbox": "read-only",
	}
	for _, rule := range rules {
		if rule.Match("codex") {
			rule.Apply("codex", args, "/workspace")
		}
	}
	if args["cwd"] != "/workspace" {
		t.Fatalf("expected cwd=/workspace, got %v", args["cwd"])
	}
	if args["approval-policy"] != "never" {
		t.Fatalf("expected approval-policy=never, got %v", args["approval-policy"])
	}
	if args["sandbox"] != "danger-full-access" {
		t.Fatalf("expected sandbox=danger-full-access, got %v", args["sandbox"])
	}
}

func TestCodexWorkspaceNoWorkDir(t *testing.T) {
	rules := DefaultSanitizationRules()
	args := map[string]interface{}{"prompt": "hello"}
	for _, rule := range rules {
		if rule.Match("codex") {
			rule.Apply("codex", args, "")
		}
	}
	if _, ok := args["cwd"]; ok {
		t.Fatal("cwd should not be set when workDir is empty")
	}
}

func TestReadLimitCap(t *testing.T) {
	rules := DefaultSanitizationRules()

	t.Run("no limit set", func(t *testing.T) {
		args := map[string]interface{}{"file_path": "/tmp/f.go"}
		for _, rule := range rules {
			if rule.Match("Read") {
				rule.Apply("Read", args, "")
			}
		}
		if args["limit"] != float64(500) {
			t.Fatalf("expected limit=500, got %v", args["limit"])
		}
	})

	t.Run("limit too high", func(t *testing.T) {
		args := map[string]interface{}{"file_path": "/tmp/f.go", "limit": float64(9999)}
		for _, rule := range rules {
			if rule.Match("Read") {
				rule.Apply("Read", args, "")
			}
		}
		if args["limit"] != float64(500) {
			t.Fatalf("expected limit capped to 500, got %v", args["limit"])
		}
	})

	t.Run("limit within range", func(t *testing.T) {
		args := map[string]interface{}{"file_path": "/tmp/f.go", "limit": float64(100)}
		for _, rule := range rules {
			if rule.Match("Read") {
				rule.Apply("Read", args, "")
			}
		}
		if args["limit"] != float64(100) {
			t.Fatalf("expected limit=100 preserved, got %v", args["limit"])
		}
	})

	t.Run("pages set skips limit", func(t *testing.T) {
		args := map[string]interface{}{"file_path": "/tmp/f.pdf", "pages": "1-5"}
		for _, rule := range rules {
			if rule.Match("Read") {
				rule.Apply("Read", args, "")
			}
		}
		if _, ok := args["limit"]; ok {
			t.Fatal("limit should not be set when pages is present")
		}
	})
}

func TestCustomRules(t *testing.T) {
	custom := []SanitizationRule{
		{
			Name:  "test-rule",
			Match: func(name string) bool { return name == "MyTool" },
			Apply: func(_ string, args map[string]interface{}, _ string) {
				args["injected"] = true
			},
		},
	}
	mgr := NewManager(map[string]*ServerConfig{}, WithSanitizationRules(custom))
	if len(mgr.sanitizationRules) != 1 || mgr.sanitizationRules[0].Name != "test-rule" {
		t.Fatal("expected custom rules to replace defaults")
	}
}
