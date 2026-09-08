package server

import (
	"context"
	"strings"

	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	"github.com/SocialGouv/iterion/pkg/webhooks"
)

// wireNativeBoardCommands installs the slash-command resolver on the native
// board store so a "/command" typed into a board-issue comment launches the
// bot — the native/local twin of the GitLab/GitHub issue-comment trigger. The
// store records every comment regardless; the resolver fires only when the
// comment leads with a known command whose bot is enabled on the issue surface.
// Wired once at server start (no-op when the server has no native store).
func (s *Server) wireNativeBoardCommands() {
	if s.cfg.NativeTrackerStore == nil {
		return
	}
	s.cfg.NativeTrackerStore.SetCommentDispatcher(s.resolveBoardComment)
}

// resolveBoardComment is the native.CommentDispatcher: it turns a board-issue
// comment body into a bot launch. It returns ok=false (record the comment,
// launch nothing) unless the body leads with a "/command" that resolves — via
// the same registry the forge webhooks use — to an ENABLED bot whose command
// allows the "issue" surface. For an opens-MR command it stamps open_mr +
// source_issue_ref="native:<id>" (this card) into bot_args, so the routed bot
// opens an MR and posts the URL back onto this very card through the board
// comment tool — exactly the forge issue-comment behaviour, on the local board.
//
// The command args land in the route's args_var (the feature/improvement
// prompt); the issue title+body become scope_notes. The issue moves to
// StateReady so the polling dispatcher claims it. Mirrors ensureBoardCard's
// stamp + buildSpec's BotArgs precedence so the local board and the forge
// webhooks behave identically.
func (s *Server) resolveBoardComment(iss native.Issue, body string) (bot string, botArgs map[string]string, transitionTo string, ok bool) {
	cmd, args := webhooks.ParseSlashCommand(body)
	if cmd == "" {
		return "", nil, "", false
	}
	// The native board is a single local store with no tenancy — the launch
	// it triggers resolves platform-over-baked too, so the discovery matches
	// it by passing no team rather than inventing one.
	route, found := s.cmdDiscoveryFor(context.Background(), "").LookupCommand(cmd)
	if !found || !route.AllowsScope("issue") {
		return "", nil, "", false
	}
	botArgs = map[string]string{}
	if route.ArgsVar != "" {
		if a := strings.TrimSpace(args); a != "" {
			botArgs[route.ArgsVar] = a
		}
	}
	if sn := strings.TrimSpace(iss.Title + "\n\n" + iss.Body); sn != "" {
		botArgs["scope_notes"] = sn
	}
	// opens_mr stamp (gated on the command, like ensureBoardCard): the back-link
	// target is THIS card. iss.ID is already prefixed "native:<uuid>", which the
	// forge-mr-create skill recognises and posts the opened MR URL onto via the
	// board comment tool.
	if route.OpensMR {
		botArgs["open_mr"] = "true"
		botArgs["source_issue_ref"] = iss.ID
	}
	return route.BotID, botArgs, native.StateReady, true
}
