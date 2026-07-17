package permission

import "testing"

func TestParseMode(t *testing.T) {
	cases := map[string]Mode{
		"":        ModeOff,
		"off":     ModeOff,
		"bypass":  ModeOff,
		"ask":     ModeAsk,
		"default": ModeAsk,
		"deny":    ModeDeny,
		"dontAsk": ModeDeny,
	}
	for in, want := range cases {
		got, err := ParseMode(in)
		if err != nil {
			t.Fatalf("ParseMode(%q) error: %v", in, err)
		}
		if got != want {
			t.Errorf("ParseMode(%q) = %v, want %v", in, got, want)
		}
	}
	if _, err := ParseMode("askk"); err == nil {
		t.Error("ParseMode(askk) should error on typo")
	}
}

func TestResolveModeSourced(t *testing.T) {
	cases := []struct {
		name                              string
		override, node, workflow, envDflt string
		wantMode                          Mode
		wantSource                        string
		wantErr                           bool
	}{
		{"all unset → off from default", "", "", "", "", ModeOff, "default", false},
		{"override wins over everything", "deny", "ask", "off", "ask", ModeDeny, "run_override", false},
		{"node wins below override", "", "ask", "deny", "deny", ModeAsk, "node", false},
		{"workflow wins below node", "", "", "deny", "ask", ModeDeny, "workflow", false},
		{"env wins below workflow", "", "", "", "ask", ModeAsk, "env", false},
		{"whitespace-only level is unset", " ", "\t", "", "deny", ModeDeny, "env", false},
		// First NON-EMPTY level wins even on a parse error: the error
		// names the level the operator actually set, no silent
		// fall-through to a lower level.
		{"typo at winning level surfaces, not skipped", "", "askk", "deny", "", ModeOff, "node", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mode, source, err := ResolveModeSourced(tc.override, tc.node, tc.workflow, tc.envDflt)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if source != tc.wantSource {
				t.Fatalf("source = %q, want %q", source, tc.wantSource)
			}
			if !tc.wantErr && mode != tc.wantMode {
				t.Fatalf("mode = %v, want %v", mode, tc.wantMode)
			}
		})
	}
}

func mustPolicy(t *testing.T, mode Mode, allow, ask, deny []string) *Policy {
	t.Helper()
	p, err := NewPolicy(mode, allow, ask, deny)
	if err != nil {
		t.Fatalf("NewPolicy: %v", err)
	}
	return p
}

func TestEvaluate_PrecedenceAndModes(t *testing.T) {
	type tc struct {
		name  string
		tool  string
		input map[string]any
		want  Decision
	}
	p := mustPolicy(t,
		ModeAsk,
		[]string{"Read(**)", "Bash(go test:*)"},
		[]string{"Bash(git push:*)"},
		[]string{"Bash(rm -rf:*)", "WebFetch(domain:evil.example)"},
	)
	cases := []tc{
		{"deny wins on rm", "Bash", map[string]any{"command": "rm -rf /"}, Deny},
		{"ask rule git push", "Bash", map[string]any{"command": "git push origin main"}, Ask},
		{"allow go test", "Bash", map[string]any{"command": "go test ./..."}, Allow},
		{"allow read any", "Read", map[string]any{"file_path": "/etc/hosts"}, Allow},
		{"unmatched bash -> ask mode default", "Bash", map[string]any{"command": "curl http://x"}, Ask},
		{"deny webfetch evil by domain", "WebFetch", map[string]any{"url": "https://evil.example/steal"}, Deny},
		{"unmatched webfetch -> ask", "WebFetch", map[string]any{"url": "https://good.example"}, Ask},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, _ := p.Evaluate(c.tool, c.input)
			if got != c.want {
				t.Errorf("Evaluate(%s) = %v, want %v", c.tool, got, c.want)
			}
		})
	}
}

func TestEvaluate_DenyModeUnmatchedIsDeny(t *testing.T) {
	p := mustPolicy(t, ModeDeny, []string{"Read(**)"}, nil, nil)
	if got, _ := p.Evaluate("Read", map[string]any{"file_path": "x"}); got != Allow {
		t.Errorf("allowed read = %v, want Allow", got)
	}
	if got, _ := p.Evaluate("Bash", map[string]any{"command": "ls"}); got != Deny {
		t.Errorf("unmatched bash in deny mode = %v, want Deny", got)
	}
}

func TestEvaluate_OffModeIsAllowAll(t *testing.T) {
	p := mustPolicy(t, ModeOff, nil, nil, []string{"Bash(rm:*)"})
	if p.Enabled() {
		t.Fatal("off-mode policy should be disabled")
	}
	if got, _ := p.Evaluate("Bash", map[string]any{"command": "rm -rf /"}); got != Allow {
		t.Errorf("off mode = %v, want Allow (gate disabled)", got)
	}
}

// Parity: the SAME rule must gate the matching tool on both backends —
// claude_code TitleCase names and claw snake_case names.
func TestEvaluate_CrossBackendParity(t *testing.T) {
	p := mustPolicy(t, ModeAsk, []string{"Edit(pkg/**)"}, nil, []string{"Bash(curl:*)"})

	for _, name := range []string{"Bash", "bash", "shell"} {
		if got, _ := p.Evaluate(name, map[string]any{"command": "curl http://x"}); got != Deny {
			t.Errorf("%s curl = %v, want Deny", name, got)
		}
	}
	for _, edit := range []string{"Edit", "edit_file", "file_edit", "MultiEdit", "Write", "write_file"} {
		// Edit alias covers Write too in our table? Write is separate; test edits only.
		_ = edit
	}
	for _, name := range []string{"Edit", "edit_file", "file_edit"} {
		if got, _ := p.Evaluate(name, map[string]any{"file_path": "pkg/foo/bar.go"}); got != Allow {
			t.Errorf("%s pkg edit = %v, want Allow", name, got)
		}
		if got, _ := p.Evaluate(name, map[string]any{"file_path": "cmd/main.go"}); got != Ask {
			t.Errorf("%s cmd edit = %v, want Ask (unmatched)", name, got)
		}
	}
}

func TestEvaluate_BareToolAndGlobs(t *testing.T) {
	p := mustPolicy(t, ModeAsk,
		[]string{"Grep", "mcp__github__get_*"},
		nil,
		[]string{"*"}, // deny-all glob still overridden? no: deny wins. Use carefully.
	)
	// deny-all `*` makes everything Deny (deny precedence). Grep allow is shadowed.
	if got, _ := p.Evaluate("Grep", map[string]any{"pattern": "TODO"}); got != Deny {
		t.Errorf("grep with deny-all = %v, want Deny", got)
	}

	// Without the deny-all, bare allow + mcp glob work.
	p2 := mustPolicy(t, ModeAsk, []string{"Grep", "mcp__github__get_*"}, nil, nil)
	if got, _ := p2.Evaluate("Grep", map[string]any{"pattern": "TODO"}); got != Allow {
		t.Errorf("bare Grep allow = %v, want Allow", got)
	}
	if got, _ := p2.Evaluate("mcp__github__get_issue", map[string]any{}); got != Allow {
		t.Errorf("mcp glob allow = %v, want Allow", got)
	}
	if got, _ := p2.Evaluate("mcp__github__create_issue", map[string]any{}); got != Ask {
		t.Errorf("mcp non-matching = %v, want Ask", got)
	}
}

func TestNewPolicy_MalformedRule(t *testing.T) {
	if _, err := NewPolicy(ModeAsk, []string{"Bash(unterminated"}, nil, nil); err == nil {
		t.Error("expected error on missing ')'")
	}
}

// TestIsInfrastructureTool_AllBackendSpellings guards the namespace
// exemption against the cross-backend FQN spellings the registration sites
// actually emit. If a new internal tool/server is added outside this set,
// add its real registered name here — a failure means the gate would block
// iterion's own plumbing (deadlocking `ask` mode).
func TestIsInfrastructureTool_AllBackendSpellings(t *testing.T) {
	exempt := []string{
		// interaction (claw bare name + claude_code MCP FQN)
		"ask_user", "mcp__iterion__ask_user", "send_user_message",
		// structured-output emission tool (claude_code + claw spellings)
		"StructuredOutput", "structured_output",
		// board (claude_code double-underscore, claw single-underscore, dotted)
		"mcp__iterion_board__board.create", "mcp_iterion_board_create",
		"mcp.iterion_board.create",
		// other internal MCP servers
		"mcp__iterion_control__x", "__mcp-board",
	}
	for _, n := range exempt {
		if !IsInfrastructureTool(n) {
			t.Errorf("IsInfrastructureTool(%q) = false, want true (internal tool must be gate-exempt)", n)
		}
	}
	// A real agent action must NOT be exempt, even if it mentions mcp.
	for _, n := range []string{"Bash", "Edit", "mcp__github__create_issue", "mcp_slack_post"} {
		if IsInfrastructureTool(n) {
			t.Errorf("IsInfrastructureTool(%q) = true, want false (third-party/agent tool must be gated)", n)
		}
	}
}

func TestMarkExempt(t *testing.T) {
	p := mustPolicy(t, ModeAsk, nil, nil, nil)
	// A bare custom internal tool outside the namespace is gated by default…
	if dec, _ := p.Evaluate("my_internal_tool", nil); dec != Ask {
		t.Fatalf("pre-mark = %v, want Ask", dec)
	}
	// …until the runtime marks it exempt.
	p.MarkExempt("my_internal_tool")
	if dec, _ := p.Evaluate("my_internal_tool", nil); dec != Allow {
		t.Errorf("post-mark = %v, want Allow", dec)
	}
}

func TestAddAllowRule_AllowAlways(t *testing.T) {
	p := mustPolicy(t, ModeAsk, nil, nil, nil)
	if got, _ := p.Evaluate("Bash", map[string]any{"command": "go build"}); got != Ask {
		t.Fatalf("pre-grant = %v, want Ask", got)
	}
	p.AddAllowRule("Bash(go build:*)")
	if got, _ := p.Evaluate("Bash", map[string]any{"command": "go build ./..."}); got != Allow {
		t.Errorf("post-grant = %v, want Allow", got)
	}
}
