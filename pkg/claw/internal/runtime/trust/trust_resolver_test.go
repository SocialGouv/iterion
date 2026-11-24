package trust

import (
	"sync"
	"testing"
)

func TestDetectTrustPrompt(t *testing.T) {
	tests := []struct {
		name       string
		screenText string
		want       bool
	}{
		{
			name:       "full trust prompt with options",
			screenText: "Do you trust the files in this folder?\n1. Yes, proceed\n2. No",
			want:       true,
		},
		{
			name:       "trust this folder variant",
			screenText: "Would you like to trust this folder?",
			want:       true,
		},
		{
			name:       "allow and continue",
			screenText: "Please allow and continue",
			want:       true,
		},
		{
			name:       "yes proceed",
			screenText: "? Yes, proceed",
			want:       true,
		},
		{
			name:       "case insensitive matching",
			screenText: "DO YOU TRUST THE FILES IN THIS FOLDER?",
			want:       true,
		},
		{
			name:       "no trust prompt",
			screenText: "Ready for your input\n>",
			want:       false,
		},
		{
			name:       "empty text",
			screenText: "",
			want:       false,
		},
		{
			name:       "unrelated text",
			screenText: "Compiling project... 42 modules compiled",
			want:       false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := DetectTrustPrompt(tt.screenText)
			if got != tt.want {
				t.Errorf("DetectTrustPrompt(%q) = %v, want %v", tt.screenText, got, tt.want)
			}
		})
	}
}

func TestResolve_NoTrustPrompt(t *testing.T) {
	resolver := NewTrustResolver(NewTrustConfig().WithAllowlisted("/tmp/worktrees"))

	decision := resolver.Resolve("/tmp/worktrees/repo-a", "Ready for your input\n>")

	if decision.IsRequired() {
		t.Error("expected NotRequired when no trust prompt present")
	}
	if decision.Policy() != nil {
		t.Error("expected nil policy for NotRequired")
	}
	if len(decision.Events()) != 0 {
		t.Errorf("expected 0 events, got %d", len(decision.Events()))
	}
}

func TestResolve_AutoTrustAllowlisted(t *testing.T) {
	resolver := NewTrustResolver(NewTrustConfig().WithAllowlisted("/tmp/worktrees"))
	prompt := "Do you trust the files in this folder?\n1. Yes, proceed\n2. No"

	decision := resolver.Resolve("/tmp/worktrees/repo-a", prompt)

	if !decision.IsRequired() {
		t.Fatal("expected Required decision")
	}
	if p := decision.Policy(); p == nil || *p != AutoTrust {
		t.Errorf("expected AutoTrust policy, got %v", p)
	}
	events := decision.Events()
	if len(events) != 2 {
		t.Fatalf("expected 2 events, got %d", len(events))
	}
	if events[0].Kind != TrustRequired || events[0].Cwd != "/tmp/worktrees/repo-a" {
		t.Errorf("unexpected first event: %+v", events[0])
	}
	if events[1].Kind != TrustResolved || events[1].Cwd != "/tmp/worktrees/repo-a" {
		t.Errorf("unexpected second event: %+v", events[1])
	}
	if events[1].Policy == nil || *events[1].Policy != AutoTrust {
		t.Errorf("expected AutoTrust in resolved event, got %v", events[1].Policy)
	}
}

func TestResolve_RequireApprovalUnknown(t *testing.T) {
	resolver := NewTrustResolver(NewTrustConfig().WithAllowlisted("/tmp/worktrees"))
	prompt := "Do you trust the files in this folder?\n1. Yes, proceed\n2. No"

	decision := resolver.Resolve("/tmp/other/repo-b", prompt)

	if !decision.IsRequired() {
		t.Fatal("expected Required decision")
	}
	if p := decision.Policy(); p == nil || *p != RequireApproval {
		t.Errorf("expected RequireApproval, got %v", p)
	}
	events := decision.Events()
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Kind != TrustRequired || events[0].Cwd != "/tmp/other/repo-b" {
		t.Errorf("unexpected event: %+v", events[0])
	}
}

func TestResolve_DeniedTakesPrecedence(t *testing.T) {
	resolver := NewTrustResolver(
		NewTrustConfig().
			WithAllowlisted("/tmp/worktrees").
			WithDenied("/tmp/worktrees/repo-c"),
	)
	prompt := "Do you trust the files in this folder?\n1. Yes, proceed\n2. No"

	decision := resolver.Resolve("/tmp/worktrees/repo-c", prompt)

	if !decision.IsRequired() {
		t.Fatal("expected Required decision")
	}
	if p := decision.Policy(); p == nil || *p != Deny {
		t.Errorf("expected Deny, got %v", p)
	}
	events := decision.Events()
	if len(events) != 2 {
		t.Fatalf("expected 2 events, got %d", len(events))
	}
	if events[0].Kind != TrustRequired {
		t.Errorf("expected TrustRequired first, got %v", events[0].Kind)
	}
	if events[1].Kind != TrustDenied {
		t.Errorf("expected TrustDenied second, got %v", events[1].Kind)
	}
	if events[1].Reason != "cwd matches denied trust root: /tmp/worktrees/repo-c" {
		t.Errorf("unexpected reason: %s", events[1].Reason)
	}
}

func TestPathMatches_SiblingPrefixDoesNotMatch(t *testing.T) {
	// /tmp/worktrees-other/repo-d must NOT match /tmp/worktrees
	if PathMatchesTrustedRoot("/tmp/worktrees-other/repo-d", "/tmp/worktrees") {
		t.Error("sibling prefix should not match trusted root")
	}
}

func TestTrusts(t *testing.T) {
	resolver := NewTrustResolver(
		NewTrustConfig().
			WithAllowlisted("/tmp/worktrees").
			WithDenied("/tmp/worktrees/repo-c"),
	)

	tests := []struct {
		cwd  string
		want bool
	}{
		{"/tmp/worktrees/repo-a", true},
		{"/tmp/worktrees/repo-c", false}, // denied takes precedence
		{"/tmp/other/repo", false},       // unknown
	}

	for _, tt := range tests {
		t.Run(tt.cwd, func(t *testing.T) {
			got := resolver.Trusts(tt.cwd)
			if got != tt.want {
				t.Errorf("Trusts(%q) = %v, want %v", tt.cwd, got, tt.want)
			}
		})
	}
}

func TestTrustPolicyJSON(t *testing.T) {
	for _, p := range []TrustPolicy{AutoTrust, RequireApproval, Deny} {
		data, err := p.MarshalJSON()
		if err != nil {
			t.Fatalf("MarshalJSON(%v) error: %v", p, err)
		}
		var p2 TrustPolicy
		if err := p2.UnmarshalJSON(data); err != nil {
			t.Fatalf("UnmarshalJSON(%s) error: %v", data, err)
		}
		if p2 != p {
			t.Errorf("round-trip failed: %v → %s → %v", p, data, p2)
		}
	}
}

// TestTrustResolverConcurrent verifies that concurrent Resolve calls on a
// shared resolver produce correct results without data races.
func TestTrustResolverConcurrent(t *testing.T) {
	resolver := NewTrustResolver(
		NewTrustConfig().
			WithAllowlisted("/tmp/worktrees").
			WithDenied("/tmp/worktrees/deny"),
	)
	prompt := "Do you trust the files in this folder?"

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			var cwd string
			switch i % 3 {
			case 0:
				cwd = "/tmp/worktrees/repo"
			case 1:
				cwd = "/tmp/worktrees/deny/sub"
			case 2:
				cwd = "/tmp/unknown/dir"
			}
			d := resolver.Resolve(cwd, prompt)
			if !d.IsRequired() {
				t.Errorf("expected Required for prompt-present resolve")
			}
		}(i)
	}
	wg.Wait()
}
