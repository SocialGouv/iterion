package permissions

import (
	"context"
	"testing"
)

func TestIsReadOnlyBashCommandAllows(t *testing.T) {
	allowed := []string{
		"ls -la",
		"cat go.mod",
		"git status",
		"git log --oneline -20",
		"git diff HEAD~1 -- pkg/",
		"git branch -a",
		"git branch --show-current",
		"git tag -l",
		"git remote -v",
		"git stash list",
		"git worktree list",
		"git config --get user.name",
		"gh pr view 42 --json title",
		"gh api repos/o/r/pulls/1",
		"gh api -X GET repos/o/r",
		"gh search code foo",
		"gh status",
		"docker ps -a",
		"docker logs mycontainer",
		"grep -rn TODO internal/",
		"find . -name '*.go' -newer go.mod",
		"git status && git diff",
		"cat a.txt | wc -l",
		"sort file.txt | uniq -c",
		"pwd",
		"node --version",
		"go version",
		"ip addr",
		"which rg; type go",
	}
	for _, cmd := range allowed {
		if !IsReadOnlyBashCommand(cmd) {
			t.Errorf("should be read-only: %q", cmd)
		}
	}
}

func TestIsReadOnlyBashCommandRejects(t *testing.T) {
	rejected := []string{
		"",
		"rm -rf /",
		"git push",
		"git commit -m x",
		"git branch new-feature",
		"git branch -D old",
		"git tag v1.0.0",
		"git remote add origin url",
		"git stash pop",
		"git config user.name evil",
		"git checkout main",
		"git -C /elsewhere status",
		"git log --output=/tmp/f",
		"gh api -X POST repos/o/r/issues",
		"gh api repos/o/r/issues -f title=x",
		"gh pr merge 42",
		"gh pr create",
		"docker run alpine",
		"docker exec -it c sh",
		"sort -o out.txt in.txt",
		"find . -name '*.tmp' -delete",
		"find . -exec rm {} \\;",
		"cat a > b",
		"echo hi >> log.txt",
		"cat $(echo /etc/passwd)",
		"cat `whoami`",
		"cat <<EOF\nhi\nEOF",
		"ls & rm -rf /",
		"git status && rm -rf /",
		"PATH=/evil cat x",
		"./cat file",
		"/bin/rm x",
		"pwd extra-arg",
		"python --version --extra",
		"npm install",
		"curl https://example.com",
	}
	for _, cmd := range rejected {
		if IsReadOnlyBashCommand(cmd) {
			t.Errorf("must NOT be read-only: %q", cmd)
		}
	}
}

func TestRuleClassifierReadOnlyBash(t *testing.T) {
	rc := NewRuleClassifier()

	d, err := rc.Classify(context.Background(), "bash", map[string]any{"command": "git status"})
	if err != nil || d != DecisionAllow {
		t.Errorf("git status via classifier: d=%v err=%v, want allow", d, err)
	}
	// Raw-subject fallback (non-JSON input parsed into __subject).
	d, err = rc.Classify(context.Background(), "bash", parseToolArgs("git log --oneline"))
	if err != nil || d != DecisionAllow {
		t.Errorf("raw subject via classifier: d=%v err=%v, want allow", d, err)
	}
	d, err = rc.Classify(context.Background(), "bash", map[string]any{"command": "rm -rf /"})
	if err != nil || d != DecisionAsk {
		t.Errorf("rm via classifier: d=%v err=%v, want ask", d, err)
	}
}

func TestCheckLegacyReadOnlyBashInPromptMode(t *testing.T) {
	m := NewManager(ModePrompt, nil)
	if d := m.Check("bash", "git status"); d != DecisionAllow {
		t.Errorf("prompt mode git status: %v, want allow", d)
	}
	if d := m.Check("bash", "git push origin main"); d != DecisionAsk {
		t.Errorf("prompt mode git push: %v, want ask", d)
	}
	// Explicit deny rules still win over the built-in allow.
	rules := &Ruleset{Rules: []Rule{{Tool: "bash", Pattern: "git status", Decision: DecisionDeny, RawDecision: "deny"}}}
	m = NewManager(ModePrompt, rules)
	if d := m.Check("bash", "git status"); d != DecisionDeny {
		t.Errorf("deny rule must beat built-in read-only allow: %v", d)
	}
	// Plan mode still denies everything.
	m = NewManager(ModePlan, nil)
	if d := m.Check("bash", "git status"); d != DecisionDeny {
		t.Errorf("plan mode must stay deny-all: %v", d)
	}
}
