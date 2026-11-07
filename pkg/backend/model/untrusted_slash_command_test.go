package model

import (
	"bytes"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

// TestSlashCommand_TheCheckoutDoesNotWriteAnUntrustedNodesPrompt is the
// ADR-162 U3 witness for the prompt half of the workspace slash-command
// capability: a workspace `.claude/commands/<name>.md` is substituted into
// the prompt of a claw node whose text opens with `/<name>`, and on a
// checkout its tenant did not write that body is the fork's — the same
// surface as a planted skill or hook, reached through the prompt.
//
// The trust comes from the ENGINE's push (SetWorkspaceTrust, the field
// stampWorkspace reads), never set by hand beside a workspace the executor
// does not name: a floor fed by a field nobody populates is green in a test
// and dead in production.
//
// Mutation: dropping the `!e.WorkspaceTrust().Trusted()` abstention from
// expandWorkspaceSlashCommand reddens the fork row on its own "the prompt
// carries the checkout's command body" assertion.
func TestSlashCommand_TheCheckoutDoesNotWriteAnUntrustedNodesPrompt(t *testing.T) {
	t.Parallel()

	const body = "COMMAND-BODY-FROM-THE-CHECKOUT\n"
	ws := commandWorkspace(t, "review.md", body)

	for _, row := range []struct {
		name     string
		trust    store.RunTrust
		expanded bool
	}{
		{"a trusted workspace's command is loaded", store.RunTrustDefault, true},
		{"a fork's command is not", store.RunTrustFork, false},
	} {
		t.Run(row.name, func(t *testing.T) {
			var log bytes.Buffer
			e := slashTestExecutor(ws, iterlog.New(iterlog.LevelInfo, &log))
			// The engine's own seam, the one it calls once per run.
			e.SetWorkspaceTrust(row.trust)

			got, hit := e.expandWorkspaceSlashCommand("/review the auth module", delegate.BackendClaw, "n", 0, &nodeBuildSession{})
			if hit != row.expanded {
				t.Fatalf("expanded = %v, want %v (prompt: %q)", hit, row.expanded, got)
			}
			if carried := strings.Contains(got, "COMMAND-BODY-FROM-THE-CHECKOUT"); carried != row.expanded {
				t.Errorf("the prompt carries the checkout's command body = %v, want %v: %q", carried, row.expanded, got)
			}
			if row.expanded {
				return
			}
			if got != "/review the auth module" {
				t.Errorf("the operator's own text was not sent unchanged: %q", got)
			}
			if !strings.Contains(log.String(), "not written by its tenant") {
				t.Errorf("the abstention was silent — nothing says why the command did not load: %q", log.String())
			}
		})
	}
}
