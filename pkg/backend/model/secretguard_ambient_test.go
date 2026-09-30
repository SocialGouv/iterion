package model

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

func TestAmbientSecretGloballySafe(t *testing.T) {
	for _, value := range []string{"shorts", "password123", "abcdefghijklmnopqrst"} {
		if ambientSecretGloballySafe(value) {
			t.Errorf("ambientSecretGloballySafe(%q) = true; common value would over-redact", value)
		}
	}
	for _, value := range []string{"Abcdef1234!?", "correct-horse-battery-staple", "sk_test_FAKE_0123456789abcdef"} {
		if !ambientSecretGloballySafe(value) {
			t.Errorf("ambientSecretGloballySafe(%q) = false; distinctive secret should be tainted", value)
		}
	}
}

func TestBuildSecretGuard_DoesNotGloballyRedactShortAmbientPassword(t *testing.T) {
	t.Setenv("ITERION_TEST_PASSWORD", "shorts")
	g := BuildSecretGuard(t.Context(), &ir.Workflow{}, nil, nil)
	if got := g.Redact("shorts_pipeline/shorts and more shorts"); got != "shorts_pipeline/shorts and more shorts" {
		t.Fatalf("short ambient password poisoned ordinary project text: %q", got)
	}
}

func TestBuildSecretGuard_RedactsDistinctiveAmbientSecret(t *testing.T) {
	const value = "Abcdef1234!?"
	t.Setenv("ITERION_TEST_PASSWORD", value)
	g := BuildSecretGuard(t.Context(), &ir.Workflow{}, nil, nil)
	got := g.Redact("password=" + value)
	if strings.Contains(got, value) || (!strings.Contains(got, "__ITERION_SECRET_env_ITERION_TEST_PASSWORD__") && !strings.Contains(got, "[redacted]")) {
		t.Fatalf("distinctive ambient secret was not redacted: %q", got)
	}
}

// A final newline makes no ambient value more distinctive: a weak one stays
// out of the guard — the guard would register it without the newline too, and
// rewrite the word everywhere.
func TestBuildSecretGuard_ANewlineDoesNotMakeAWeakAmbientValueSafe(t *testing.T) {
	t.Setenv("ITERION_TEST_PASSWORD", "password123\n")
	g := BuildSecretGuard(t.Context(), &ir.Workflow{}, nil)
	for _, text := range []string{"reset password123 to a new value", "password123\n"} {
		if got := g.Unmaterialize(text); got != text {
			t.Fatalf("Unmaterialize(%q) = %q: a weak ambient value poisoned ordinary text", text, got)
		}
	}
}
