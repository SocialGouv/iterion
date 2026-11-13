package permissions

import (
	"context"
	"errors"
	"testing"
)

// failOnPrompt is a PermissionPrompter that fails the test if it is ever
// invoked. Used to assert that ModeDontAsk never reaches the prompt path.
type failOnPrompt struct {
	t *testing.T
}

func (f *failOnPrompt) Decide(req *PermissionRequest) PermissionPromptDecision {
	f.t.Helper()
	f.t.Fatalf("prompter must not be called under ModeDontAsk; got request for %q", req.ToolName)
	return PermissionPromptDecision{}
}

func TestModeDontAsk_DeniesUnlistedTool(t *testing.T) {
	policy := NewPermissionPolicy(ModeDontAsk)

	outcome := policy.Authorize("read_file", "{}", nil)
	if outcome.Allowed {
		t.Errorf("expected Deny for unlisted tool under ModeDontAsk, got Allow")
	}
	if outcome.Reason == "" {
		t.Error("denial under ModeDontAsk should include a reason")
	}

	outcome = policy.Authorize("bash", `{"command":"ls"}`, nil)
	if outcome.Allowed {
		t.Errorf("expected Deny for bash under ModeDontAsk")
	}
}

func TestModeDontAsk_AllowsListedTool(t *testing.T) {
	policy := NewPermissionPolicy(ModeDontAsk).
		WithToolRequirement("read_file", ModeReadOnly)

	outcome := policy.Authorize("read_file", `{"path":"main.go"}`, nil)
	if !outcome.Allowed {
		t.Errorf("expected Allow for listed read_file under ModeDontAsk, got Deny: %s", outcome.Reason)
	}

	// A different tool that is not listed must still be denied.
	outcome = policy.Authorize("write_file", `{"path":"main.go"}`, nil)
	if outcome.Allowed {
		t.Error("expected Deny for non-listed write_file under ModeDontAsk")
	}
}

func TestModeDontAsk_AllowsViaAllowRule(t *testing.T) {
	// Verifies the allow-list also accepts entries provided through allow rules.
	policy := NewPermissionPolicy(ModeDontAsk).
		WithPermissionRules([]string{"bash(git:*)"}, nil, nil)

	outcome := policy.Authorize("bash", `{"command":"git status"}`, nil)
	if !outcome.Allowed {
		t.Errorf("expected Allow via allow-rule under ModeDontAsk, got Deny: %s", outcome.Reason)
	}

	outcome = policy.Authorize("bash", `{"command":"rm -rf /"}`, nil)
	if outcome.Allowed {
		t.Error("expected Deny for non-matching bash invocation under ModeDontAsk")
	}
}

func TestModeDontAsk_DoesNotPrompt(t *testing.T) {
	policy := NewPermissionPolicy(ModeDontAsk).
		WithToolRequirement("bash", ModeDangerFullAccess)

	prompter := &failOnPrompt{t: t}

	// Listed tool — must Allow without consulting the prompter.
	outcome := policy.Authorize("bash", `{"command":"echo hi"}`, prompter)
	if !outcome.Allowed {
		t.Errorf("expected Allow for listed tool, got Deny: %s", outcome.Reason)
	}

	// Unlisted tool — must Deny without consulting the prompter.
	outcome = policy.Authorize("write_file", `{"path":"x"}`, prompter)
	if outcome.Allowed {
		t.Error("expected Deny for unlisted tool")
	}
}

func TestModeDontAsk_DenyRulesStillShortCircuit(t *testing.T) {
	// Deny rules must take precedence even over an explicit allow-list entry.
	policy := NewPermissionPolicy(ModeDontAsk).
		WithToolRequirement("bash", ModeDangerFullAccess).
		WithPermissionRules(nil, []string{"bash(rm -rf:*)"}, nil)

	outcome := policy.Authorize("bash", `{"command":"rm -rf /"}`, nil)
	if outcome.Allowed {
		t.Error("deny rule must take precedence over ModeDontAsk allow-list")
	}
}

func TestModeAuto_DefaultClassifierAllowsRead(t *testing.T) {
	policy := NewPermissionPolicy(ModeAuto)

	prompter := &recordingPrompter{allow: true}
	for _, tool := range []string{"read_file", "glob", "grep"} {
		outcome := policy.Authorize(tool, `{"path":"main.go"}`, prompter)
		if !outcome.Allowed {
			t.Errorf("expected Allow for %s under ModeAuto default classifier, got Deny: %s",
				tool, outcome.Reason)
		}
	}

	if len(prompter.seen) != 0 {
		t.Errorf("default classifier should permit reads without prompting; got %d prompts",
			len(prompter.seen))
	}
}

func TestModeAuto_DefaultClassifierAllowsHTTPSWebFetch(t *testing.T) {
	policy := NewPermissionPolicy(ModeAuto)

	outcome := policy.Authorize("web_fetch", `{"url":"https://example.com"}`, nil)
	if !outcome.Allowed {
		t.Errorf("expected Allow for https web_fetch, got Deny: %s", outcome.Reason)
	}

	// http:// should not be allowed without a prompter.
	outcome = policy.Authorize("web_fetch", `{"url":"http://example.com"}`, nil)
	if outcome.Allowed {
		t.Error("expected http web_fetch to fall through to prompt path (denied without prompter)")
	}
}

func TestModeAuto_DefaultClassifierPromptsWrite(t *testing.T) {
	policy := NewPermissionPolicy(ModeAuto)

	for _, tool := range []string{"write_file", "bash"} {
		// Without a prompter, the classifier's Ask outcome should result in Deny.
		outcome := policy.Authorize(tool, `{"path":"x"}`, nil)
		if outcome.Allowed {
			t.Errorf("expected Deny without prompter for %s under ModeAuto", tool)
		}

		// With a prompter, the prompt path is taken.
		prompter := &recordingPrompter{allow: true}
		outcome = policy.Authorize(tool, `{"path":"x"}`, prompter)
		if !outcome.Allowed {
			t.Errorf("expected Allow after prompt for %s, got Deny: %s", tool, outcome.Reason)
		}
		if len(prompter.seen) != 1 {
			t.Errorf("expected exactly 1 prompt for %s, got %d", tool, len(prompter.seen))
		}
	}
}

// stubClassifier is a Classifier whose Decision is configurable per call.
type stubClassifier struct {
	decision Decision
	err      error
	calls    int
}

func (s *stubClassifier) Classify(_ context.Context, _ string, _ map[string]any) (Decision, error) {
	s.calls++
	return s.decision, s.err
}

func TestModeAuto_CustomClassifierWins(t *testing.T) {
	// A custom classifier returning Allow should overrule the default's Ask
	// for write_file.
	allowAll := &stubClassifier{decision: DecisionAllow}
	policy := NewPermissionPolicy(ModeAuto).WithClassifier(allowAll)

	outcome := policy.Authorize("write_file", `{"path":"x"}`, nil)
	if !outcome.Allowed {
		t.Errorf("custom classifier Allow should permit write_file, got Deny: %s", outcome.Reason)
	}
	if allowAll.calls != 1 {
		t.Errorf("expected classifier to be called once, got %d", allowAll.calls)
	}

	// A custom classifier returning Deny should overrule the default's Allow
	// for read_file.
	denyAll := &stubClassifier{decision: DecisionDeny}
	policy = NewPermissionPolicy(ModeAuto).WithClassifier(denyAll)

	outcome = policy.Authorize("read_file", `{"path":"x"}`, nil)
	if outcome.Allowed {
		t.Error("custom classifier Deny should block read_file under ModeAuto")
	}
	if outcome.Reason == "" {
		t.Error("classifier deny should include reason")
	}
}

func TestModeAuto_ClassifierErrorFallsThroughToPrompt(t *testing.T) {
	// A classifier returning a non-nil error should be treated as Ask.
	broken := &stubClassifier{decision: DecisionAllow, err: errors.New("boom")}
	policy := NewPermissionPolicy(ModeAuto).WithClassifier(broken)

	prompter := &recordingPrompter{allow: true}
	outcome := policy.Authorize("read_file", `{"path":"x"}`, prompter)
	if !outcome.Allowed {
		t.Errorf("expected Allow after prompt when classifier errors, got Deny: %s", outcome.Reason)
	}
	if len(prompter.seen) != 1 {
		t.Errorf("expected 1 prompt for classifier error, got %d", len(prompter.seen))
	}
}

func TestModeAuto_AskRuleOverridesClassifierAllow(t *testing.T) {
	// Even when the classifier permits a tool, an ask rule must force a prompt.
	allowAll := &stubClassifier{decision: DecisionAllow}
	policy := NewPermissionPolicy(ModeAuto).
		WithClassifier(allowAll).
		WithPermissionRules(nil, nil, []string{"read_file(*)"})

	prompter := &recordingPrompter{allow: true}
	outcome := policy.Authorize("read_file", `{"path":"x"}`, prompter)
	if !outcome.Allowed {
		t.Errorf("expected Allow after ask-rule prompt, got Deny: %s", outcome.Reason)
	}
	if len(prompter.seen) != 1 {
		t.Errorf("ask rule should force a prompt under ModeAuto Allow; got %d prompts", len(prompter.seen))
	}
}

func TestModeStrings_DontAskAndAuto(t *testing.T) {
	if got := ModeDontAsk.String(); got != "dont-ask" {
		t.Errorf("ModeDontAsk.String() = %q, want \"dont-ask\"", got)
	}
	if got := ModeAuto.String(); got != "auto" {
		t.Errorf("ModeAuto.String() = %q, want \"auto\"", got)
	}

	m, err := ParsePermissionMode("dont-ask")
	if err != nil || m != ModeDontAsk {
		t.Errorf("ParsePermissionMode(\"dont-ask\") = (%v, %v), want (ModeDontAsk, nil)", m, err)
	}
	m, err = ParsePermissionMode("auto")
	if err != nil || m != ModeAuto {
		t.Errorf("ParsePermissionMode(\"auto\") = (%v, %v), want (ModeAuto, nil)", m, err)
	}
}
