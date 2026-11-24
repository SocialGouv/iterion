package runtime

import (
	"strings"
	"testing"
)

func TestActiveSkill_FilterAllowedTools(t *testing.T) {
	var l SkillStateLock
	l.Set(&ActiveSkill{
		Name:         "myplugin:myskill",
		AllowedTools: []string{"read_file", "bash"},
	})
	if err := l.CheckAllowed("read_file"); err != nil {
		t.Errorf("read_file should be allowed: %v", err)
	}
	if err := l.CheckAllowed("bash"); err != nil {
		t.Errorf("bash should be allowed: %v", err)
	}
	err := l.CheckAllowed("web_fetch")
	if err == nil {
		t.Fatal("web_fetch should be denied")
	}
	if !strings.Contains(err.Error(), "web_fetch") || !strings.Contains(err.Error(), "myplugin:myskill") {
		t.Errorf("error should name the tool and active skill, got %q", err.Error())
	}
}

func TestActiveSkill_NoListPermitsAll(t *testing.T) {
	var l SkillStateLock
	l.Set(&ActiveSkill{Name: "x:y"}) // empty AllowedTools
	if err := l.CheckAllowed("anything"); err != nil {
		t.Errorf("empty allowed-tools should permit everything: %v", err)
	}
}

func TestActiveSkill_AlwaysAllowed(t *testing.T) {
	var l SkillStateLock
	l.Set(&ActiveSkill{
		Name:         "x:y",
		AllowedTools: []string{"read_file"}, // tight whitelist
	})
	for _, tool := range []string{"skill", "tool_search", "todo_write"} {
		if err := l.CheckAllowed(tool); err != nil {
			t.Errorf("%s should be always allowed: %v", tool, err)
		}
	}
}

func TestActiveSkill_ClearOnUserTurn(t *testing.T) {
	var l SkillStateLock
	l.Set(&ActiveSkill{Name: "x:y", AllowedTools: []string{"read_file"}})
	if got := l.Get(); got == nil {
		t.Fatal("expected active skill to be set")
	}
	l.Clear()
	if got := l.Get(); got != nil {
		t.Errorf("expected nil after Clear, got %+v", got)
	}
	if err := l.CheckAllowed("anything"); err != nil {
		t.Errorf("after Clear all tools allowed: %v", err)
	}
}

func TestActiveSkill_OverrideOnReinvoke(t *testing.T) {
	var l SkillStateLock
	l.Set(&ActiveSkill{Name: "first", AllowedTools: []string{"read_file"}})
	l.Set(&ActiveSkill{Name: "second", AllowedTools: []string{"bash"}})
	got := l.Get()
	if got == nil || got.Name != "second" {
		t.Fatalf("expected second active, got %+v", got)
	}
	if err := l.CheckAllowed("bash"); err != nil {
		t.Errorf("bash should be allowed under second skill: %v", err)
	}
	if err := l.CheckAllowed("read_file"); err == nil {
		t.Errorf("read_file should NOT be allowed under second skill")
	}
}

func TestNormalizeToolName(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"BashTool", "bash"},
		{"Bash", "bash"},
		{"read_file", "readfile"},
		{"ReadFile", "readfile"},
		{"web_fetch_tool", "webfetch"},
	}
	for _, c := range cases {
		if got := normalizeToolName(c.in); got != c.want {
			t.Errorf("normalizeToolName(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
