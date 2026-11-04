package model

import (
	"os"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/stdiotest"
)

func TestMalformedCorrectionBudgetWarns(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("TMPDIR", t.TempDir())
	for _, value := range []string{"O", "-1", "unlimited"} {
		t.Run(value, func(t *testing.T) {
			t.Setenv(EnvOutputCorrectionBudget, value)
			warning := stdiotest.Run(t, &os.Stderr, func() {
				if got := DefaultOutputCorrectionBudget(); got != 2 {
					t.Fatalf("fallback=%d", got)
				}
			})
			if !strings.Contains(warning, "OUTPUT_CORRECTION_BUDGET_INVALID") || !strings.Contains(warning, EnvOutputCorrectionBudget) || !strings.Contains(warning, value) {
				t.Fatalf("explicit invalid budget silently replaced: value=%q warning=%q", value, warning)
			}
		})
	}
}
