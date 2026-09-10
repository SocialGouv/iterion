package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/forge"
	"github.com/SocialGouv/iterion/pkg/webhooks"
	"github.com/SocialGouv/iterion/pkg/webhooks/gitlab"
)

// The reply-in-thread (converse) lane used to launch on the NOTE payload's
// own pair — the event project's clone URL plus the MR's source branch name.
// On a fork merge request that pair names two different projects: the base
// project, and a branch that lives in the fork. The checkout then misses, or
// hits a same-named branch on the base and the bot answers grounded in the
// wrong code under its own identity.
//
// Every other GitLab lane already proves the head project before launching
// (the command lane, the auto lane, the relaunch); this one did not, and it is
// the field the #728 plumbing added — PullRef.HeadCloneURL — that it has to
// read the day forks are served.
func TestGitLabConverseLane_ProvesTheHeadProjectBeforeLaunching(t *testing.T) {
	converseCfg := func() webhooks.Config {
		cfg := glConfig()
		cfg.BotIDs = []string{"review-pr", "revi-converse"}
		return cfg
	}
	replyBody := strings.Replace(glNoteRevi, `"note": "/revi"`, `"note": "Can you expand on the SSRF fix?"`, 1)

	newServer := func(t *testing.T) *Server {
		t.Helper()
		s := newWebhookTestServer(t)
		s.cfg.Bots.Paths = []string{botsDirAbs(t)}
		s.webhookNoteGate = func(context.Context, webhooks.Config, gitlab.ParsedNote, string) (bool, bool, string, string, error) {
			return true, true, "@revi (you, the bot):\nthe fix pins the host.", "reply", nil
		}
		return s
	}

	t.Run("a fork merge request never launches", func(t *testing.T) {
		s := newServer(t)
		s.webhookGitLabPRResolver = func(_ context.Context, _ webhooks.Config, p gitlab.ParsedNote, _ string) (forge.PullRef, error) {
			return forge.PullRef{
				State: "open", SourceBranch: p.SourceBranch, TargetBranch: p.TargetBranch,
				HeadRepoFullName: "mallory/widgets",
				HeadCloneURL:     "https://gitlab.com/mallory/widgets.git",
			}, nil
		}
		launched := 0
		s.webhookLaunchBot = func(context.Context, string, map[string]string, string, string, string, map[string]string, map[string]string) (string, error) {
			launched++
			return "run-x", nil
		}
		w := httptest.NewRecorder()
		s.handleGitLabWebhook(w, glNoteReq(gitlabCtx(converseCfg()), replyBody))
		if w.Code != http.StatusOK {
			t.Fatalf("a refused fork reply is a clean 200/filtered (a 4xx gets the hook disabled): code=%d body=%s", w.Code, w.Body.String())
		}
		if launched != 0 {
			t.Fatalf("launched %d run(s) on a fork merge request — the pair would be the BASE clone URL and a fork-chosen branch name", launched)
		}
	})

	t.Run("a same-project merge request launches on the proven head", func(t *testing.T) {
		s := newServer(t)
		s.webhookGitLabPRResolver = func(_ context.Context, _ webhooks.Config, p gitlab.ParsedNote, _ string) (forge.PullRef, error) {
			return forge.PullRef{
				State: "open", SourceBranch: p.SourceBranch, TargetBranch: p.TargetBranch,
				HeadRepoFullName: p.ProjectPath,
				HeadCloneURL:     "https://gitlab.com/acme/widgets.git",
			}, nil
		}
		var gotURL, gotRef, gotBot string
		launched := 0
		s.webhookLaunchBot = func(_ context.Context, botID string, _ map[string]string, repoURL, repoRef, _ string, _, _ map[string]string) (string, error) {
			launched++
			gotBot, gotURL, gotRef = botID, repoURL, repoRef
			return "run-reply", nil
		}
		w := httptest.NewRecorder()
		s.handleGitLabWebhook(w, glNoteReq(gitlabCtx(converseCfg()), replyBody))
		if w.Code != http.StatusAccepted || launched != 1 {
			t.Fatalf("a same-project reply must still launch: code=%d launched=%d body=%s", w.Code, launched, w.Body.String())
		}
		if gotBot != "revi-converse" {
			t.Fatalf("bot = %q", gotBot)
		}
		if gotURL != "https://gitlab.com/acme/widgets.git" || gotRef != "feature/x" {
			t.Fatalf("the launch must ride the RESOLVED head, not the note payload's project: url=%q ref=%q", gotURL, gotRef)
		}
	})

	t.Run("an unresolvable merge request is a visible launch error", func(t *testing.T) {
		s := newServer(t)
		s.webhookGitLabPRResolver = func(context.Context, webhooks.Config, gitlab.ParsedNote, string) (forge.PullRef, error) {
			return forge.PullRef{}, context.DeadlineExceeded
		}
		launched := 0
		s.webhookLaunchBot = func(context.Context, string, map[string]string, string, string, string, map[string]string, map[string]string) (string, error) {
			launched++
			return "run-x", nil
		}
		w := httptest.NewRecorder()
		s.handleGitLabWebhook(w, glNoteReq(gitlabCtx(converseCfg()), replyBody))
		if w.Code != http.StatusBadGateway {
			t.Fatalf("a head that could not be proven is an explicit failure, never a launch on a guess: code=%d body=%s", w.Code, w.Body.String())
		}
		if launched != 0 {
			t.Fatalf("launched %d run(s) on an unresolved head", launched)
		}
	})
}
