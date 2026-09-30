package runtime

import (
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// A `vars:` default's ${ITERION_*:-default} resolves through the bot-var
// overlay like the node fields beside it (ADR-093). It read the process env
// alone, so a stored value never reached a var default.
func TestVarDefaultsReadTheBotVarOverlay(t *testing.T) {
	t.Setenv("ITERION_TEST_VAR_DIAL", "")
	ir.SetEnvOverlay(func(name string) (string, bool) {
		if name == "ITERION_TEST_VAR_DIAL" {
			return "999", true
		}
		return "", false
	})
	defer ir.SetEnvOverlay(nil)

	e := &Engine{}
	if got := ir.ExpandWithDefault("${ITERION_TEST_VAR_DIAL:-120}", e.varExpandFn()); got != "999" {
		t.Errorf("var default expanded to %q, want 999 — the stored bot var never reached it", got)
	}
	// Non-ITERION_ names keep the process env alone.
	t.Setenv("PLAIN_TEST_VAR", "env")
	if got := ir.ExpandWithDefault("${PLAIN_TEST_VAR:-d}", e.varExpandFn()); got != "env" {
		t.Errorf("plain var expanded to %q, want env", got)
	}
}
