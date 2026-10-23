package dispatcher

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

// The dispatcher's pre-claim route check reads each bot's resolved
// visibility: the manifest default composed with the workspace overlay. An
// overlay its reader refuses — dense past the read budget, past the size
// cap, not YAML — holds the operator's decisions unread, and routes NO bot:
// answered as "no overlay" it re-enabled every bot the operator disabled,
// and routed it (r4b F1).
func TestAnOverlayItsReaderRefusesRoutesNoBot(t *testing.T) {
	t.Setenv(parser.ReadBudgetEnv, "200")
	decisions := "bots:\n  secret-bot:\n    enabled: false\n  other-bot:\n    enabled: false\n"
	for _, c := range []struct {
		name    string
		overlay string
		routed  bool
	}{
		{"no overlay", "", true},
		{"the decisions read", decisions, false},
		{"dense past the read budget", decisions + "pad: {" + strings.Repeat("a,", 300) + "a}\n", false},
		{"past the size cap", decisions + "# " + strings.Repeat("x", 10<<20) + "\n", false},
		{"not YAML", decisions + "\t\tnot: [yaml\n", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			work := t.TempDir()
			write := func(rel, text string) {
				full := filepath.Join(work, filepath.FromSlash(rel))
				if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(full, []byte(text), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			for _, b := range []string{"secret-bot", "other-bot"} {
				write("bots/"+b+".bot", "## ---\n## name: "+b+"\n## ---\nagent a:\n  model: \"m\"\nworkflow w:\n  entry: a\n")
			}
			if c.overlay != "" {
				write(".iterion/bot-overrides.yaml", c.overlay)
			}
			rr := &RoutingRunner{BotsPaths: []string{filepath.Join(work, "bots")}}
			if got := rr.HasRoute("secret-bot"); got != c.routed {
				t.Errorf("HasRoute(secret-bot) = %v, want %v", got, c.routed)
			}
		})
	}
}
