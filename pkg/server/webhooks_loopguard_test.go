package server

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/forge"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/webhooks"
	"github.com/SocialGouv/iterion/pkg/webhooks/gitlab"
	"github.com/SocialGouv/iterion/pkg/webhooks/prforge"
)

// A self-comment loop guard that cannot read the bot's own identity has
// decided nothing. Since the App client stopped inventing a login (#711), an
// unreachable `GET /app` is an ERROR — and the old `err == nil &&` shape read
// that as "not the bot", walked the rest of the gate, and let the bot's own
// comment authorize itself with nothing written anywhere.
type loopGuardAPI struct {
	login    string
	whoErr   error
	perm     string
	permErr  error
	permHits int
}

func (f *loopGuardAPI) WhoAmI(context.Context) (forge.Identity, error) {
	if f.whoErr != nil {
		return forge.Identity{}, f.whoErr // what a real client returns on a failed read
	}
	return forge.Identity{Login: f.login}, nil
}

func (f *loopGuardAPI) CollaboratorPermission(context.Context, string, string) (string, error) {
	f.permHits++
	return f.perm, f.permErr
}

func (f *loopGuardAPI) GetPullRequest(context.Context, string, int) (forge.PullRef, error) {
	return forge.PullRef{}, nil
}

func TestPRForgeCommandGate_FailsClosedWhenTheBotIdentityIsUnreadable(t *testing.T) {
	cfg := webhooks.Config{ID: "w1", TenantID: "t1", MinReplierRole: "write"}
	// The dangerous shape: the commenter IS the bot, and it would clear the
	// role gate on its own repo permission.
	p := prforge.ParsedNote{ProjectPath: "acme/widgets", IssueNumber: 7, AuthorLogin: "iterion-bot[bot]"}
	route := webhooks.CommandRoute{BotID: "branch-improve-loop"}

	t.Run("an unreachable identity refuses the delivery", func(t *testing.T) {
		api := &loopGuardAPI{whoErr: errors.New("GET /app: 503"), perm: "admin"}
		outcome, reason, err := prforgeCommandGateWithAPI(context.Background(), cfg, p, route, api)
		if err != nil {
			t.Fatalf("a guard that cannot decide is a refusal, not an infra error: %v", err)
		}
		if outcome == gateAuthorized {
			t.Fatal("the bot's own comment authorized itself while the loop guard was blind")
		}
		if outcome != gateUnevaluable {
			t.Fatalf("outcome = %v, want gateUnevaluable (a maintainer has to fix the identity resolution)", outcome)
		}
		if !strings.Contains(reason, loopGuardUnevaluable) || !strings.Contains(reason, "503") {
			t.Fatalf("the delivery row must carry WHY the guard could not decide: %q", reason)
		}
		if api.permHits != 0 {
			t.Fatalf("the gate must stop at the blind guard, not walk on to the role read (%d permission calls)", api.permHits)
		}
	})

	t.Run("an identity the forge does not name refuses too", func(t *testing.T) {
		api := &loopGuardAPI{login: "", perm: "admin"}
		outcome, reason, _ := prforgeCommandGateWithAPI(context.Background(), cfg, p, route, api)
		if outcome != gateUnevaluable || !strings.Contains(reason, loopGuardUnevaluable) {
			t.Fatalf("an empty login decides nothing either: outcome=%v reason=%q", outcome, reason)
		}
	})

	t.Run("a readable identity still gates normally", func(t *testing.T) {
		api := &loopGuardAPI{login: "iterion-bot[bot]"}
		if outcome, reason, _ := prforgeCommandGateWithAPI(context.Background(), cfg, p, route, api); outcome != gateRefused || reason != "self comment (loop-guard)" {
			t.Fatalf("the bot's own comment is refused in silence: outcome=%v reason=%q", outcome, reason)
		}
		human := prforge.ParsedNote{ProjectPath: "acme/widgets", IssueNumber: 7, AuthorLogin: "dev-dan"}
		api2 := &loopGuardAPI{login: "iterion-bot[bot]", perm: "write"}
		if outcome, reason, _ := prforgeCommandGateWithAPI(context.Background(), cfg, human, route, api2); outcome != gateAuthorized {
			t.Fatalf("a write-permission human must still be authorized: outcome=%v reason=%q", outcome, reason)
		}
	})
}

// gitlabLoopGuardAPI is the GitLab half of the same class: CurrentUser is the
// only self-note guard, so an error there must refuse, not fall through.
type gitlabLoopGuardAPI struct {
	id      int64
	curErr  error
	perm    int
	permErr error
	members int
}

func (f *gitlabLoopGuardAPI) CurrentUser(context.Context) (gitlab.User, error) {
	if f.curErr != nil {
		return gitlab.User{}, f.curErr // what a real client returns on a failed read
	}
	return gitlab.User{ID: f.id}, nil
}

func (f *gitlabLoopGuardAPI) Discussion(context.Context, int64, int64, string) ([]gitlab.DiscussionNote, error) {
	return nil, nil
}

func (f *gitlabLoopGuardAPI) MemberAccessLevel(context.Context, int64, int64) (int, bool, error) {
	f.members++
	return f.perm, true, f.permErr
}

func TestGitLabCommandGate_FailsClosedWhenTheBotIdentityIsUnreadable(t *testing.T) {
	s := New(Config{}, iterlog.New(iterlog.LevelError, nil))
	cfg := webhooks.Config{ID: "w1", TenantID: "t1", MinReplierRole: "developer"}
	p := gitlab.ParsedNote{ProjectPath: "acme/widgets", ProjectID: 42, MRIID: 7, AuthorID: 9, AuthorUsername: "iterion-bot"}
	route := webhooks.CommandRoute{BotID: "branch-improve-loop"}

	api := &gitlabLoopGuardAPI{id: 9, curErr: errors.New("GET /user: 503"), perm: 50}
	outcome, reason, err := s.gitlabCommandGateWithAPI(context.Background(), cfg, p, route, api)
	if err != nil {
		t.Fatalf("a guard that cannot decide is a refusal, not an infra error: %v", err)
	}
	if outcome == gateAuthorized {
		t.Fatal("the bot's own note authorized itself while the loop guard was blind")
	}
	if outcome != gateUnevaluable || !strings.Contains(reason, loopGuardUnevaluable) {
		t.Fatalf("outcome=%v reason=%q, want an unevaluable gate naming the loop guard", outcome, reason)
	}
	if api.members != 0 {
		t.Fatalf("the gate must stop at the blind guard (%d role reads)", api.members)
	}

	// The working path is unchanged: the bot's own note is refused in silence,
	// a human clears the role gate.
	ok := &gitlabLoopGuardAPI{id: 9, perm: 50}
	if outcome, _, _ := s.gitlabCommandGateWithAPI(context.Background(), cfg, p, route, ok); outcome != gateRefused {
		t.Fatalf("the bot's own note must stay refused, got %v", outcome)
	}
	human := p
	human.AuthorID, human.AuthorUsername = 11, "dev-dan"
	if outcome, reason, _ := s.gitlabCommandGateWithAPI(context.Background(), cfg, human, route, &gitlabLoopGuardAPI{id: 9, perm: 50}); outcome != gateAuthorized {
		t.Fatalf("a developer must still be authorized: outcome=%v reason=%q", outcome, reason)
	}
}

// The review-thread gate has a SECOND identity source (the connection's [bot]
// slug), so an unreadable token identity does not blind it — it narrows it.
// That degradation used to happen with nothing said anywhere, which is exactly
// the shape a self-answering conversation hides in.
func TestReviewReplyGate_SaysSoWhenTheTokenIdentityIsUnreadable(t *testing.T) {
	var logbuf bytes.Buffer
	s := New(Config{}, iterlog.New(iterlog.LevelWarn, &logbuf))
	// The connection-derived half of the guard is present (an App's [bot]
	// slug), so the lane keeps deciding — it just decides on less.
	s.webhookIterionBotAuthor = func(_ context.Context, _ webhooks.Config, login string) bool {
		return login == "iterion-bot[bot]"
	}
	cfg := webhooks.Config{ID: "w1", TenantID: "t1", MinReplierRole: "write"}
	p := prforge.ParsedReviewComment{ProjectPath: "acme/widgets", PRNumber: 7, AuthorLogin: "dev-dan", CommentID: 5, ThreadRootID: 4}
	api := &fakeThreadAPI{whoErr: errors.New("GET /app: 503")}

	if _, _, _, err := s.reviewReplyGateWithAPI(context.Background(), cfg, p, api); err != nil {
		t.Fatalf("the connection identity still decides — the lane must stay live: %v", err)
	}
	if !strings.Contains(logbuf.String(), "token identity is unreadable") {
		t.Fatalf("a narrowed loop guard must say so, log was: %q", logbuf.String())
	}
}
