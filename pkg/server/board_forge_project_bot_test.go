package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/audit"
	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
)

// The board lane on a connection that posts as project bots: a card pushed
// to an issue, a pull request opened or merged from a card, reach the forge
// under the repository's MINTED bot token — the writer the review lane uses
// — never under the grant of the person who connected. Reads keep the grant.
// A repository with no minted bot is refused, not posted as the person
// (#2681). Recorded on the bearer of every call the real GitLab client makes.
func TestBoardForge_ProjectBotConnectionWritesAsTheBot(t *testing.T) {
	gl := &projectBotGitLab{}
	back := gl.server()
	defer back.Close()
	backURL, _ := url.Parse(back.URL)
	proxy := httputil.NewSingleHostReverseProxy(backURL)

	var mu sync.Mutex
	var calls, bodies []string
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.EscapedPath()
		if !strings.Contains(p, "/merge_requests") && !strings.Contains(p, "/issues") {
			proxy.ServeHTTP(w, r)
			return
		}
		mu.Lock()
		calls = append(calls, r.Method+" "+p+" bearer="+strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost {
			raw, _ := io.ReadAll(r.Body)
			mu.Lock()
			bodies = append(bodies, string(raw))
			mu.Unlock()
		}
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(p, "/merge_requests"):
			_, _ = w.Write([]byte(`[]`))
		case r.Method == http.MethodGet && strings.Contains(p, "/merge_requests/"):
			_, _ = w.Write([]byte(`{"iid":5,"state":"opened","web_url":"https://gl/group/api/-/merge_requests/5","sha":"cafe1234cafe1234cafe1234cafe1234cafe1234","source_branch":"f","target_branch":"main","diff_refs":{"base_sha":"a","head_sha":"cafe1234cafe1234cafe1234cafe1234cafe1234","start_sha":"c"}}`))
		case r.Method == http.MethodPut && strings.HasSuffix(p, "/merge"):
			_, _ = w.Write([]byte(`{"iid":5,"state":"merged","web_url":"https://gl/group/api/-/merge_requests/5","sha":"cafe1234cafe1234cafe1234cafe1234cafe1234","source_branch":"f","target_branch":"main"}`))
		case r.Method == http.MethodPost && strings.HasSuffix(p, "/merge_requests"):
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"iid":5,"state":"opened","web_url":"https://gl/group/api/-/merge_requests/5","sha":"cafe1234cafe1234cafe1234cafe1234cafe1234","source_branch":"f","target_branch":"main"}`))
		case r.Method == http.MethodPost && strings.HasSuffix(p, "/issues"):
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"iid":7,"state":"opened","web_url":"https://gl/group/api/-/issues/7","title":"card"}`))
		case r.Method == http.MethodPut && strings.Contains(p, "/issues/"):
			_, _ = w.Write([]byte(`{"iid":3,"state":"opened","web_url":"https://gl/group/api/-/issues/3","title":"card"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer front.Close()

	s := newProjectBotTestServer(t)
	s.auditStore = audit.NewMemoryStore()
	board := newTestBoard(t)
	fb := &fixFailingBoard{BoardStore: board}
	s.cfg.CloudBoardFor = func(string) native.BoardStore { return fb }
	ctx := superAdminCtx()
	w := httptest.NewRecorder()
	s.handleAdminRegisterForgeOAuthApp(w, forgeReq(ctx, "POST", "/api/admin/forge/oauth-apps",
		`{"provider":"gitlab","forge_base_url":"`+front.URL+`","client_id":"cid","client_secret":"csecret"}`, ""))
	if w.Code != http.StatusOK {
		t.Fatalf("register platform app: code=%d body=%s", w.Code, w.Body.String())
	}
	conn := oauthConnectProjectBot(t, s, front.URL)
	w = httptest.NewRecorder()
	s.handleEnableForgeRepoBots(w, forgeReq(ctx, "POST", "/api/teams/t1/forge/repo-bots",
		`{"connection_id":"`+conn.ID+`","repo":"group/api","bot_ids":["review-pr"]}`, "t1"))
	if w.Code != http.StatusOK {
		t.Fatalf("enable: code=%d body=%s", w.Code, w.Body.String())
	}
	unlinked, err := board.Create(native.Issue{Title: "card", Body: "body"})
	if err != nil {
		t.Fatal(err)
	}
	linked, err := board.Create(native.Issue{Title: "linked", Body: "body", External: &native.ExternalRef{Provider: "gitlab", ConnectionID: conn.ID, Repo: "group/api", Number: 3}})
	if err != nil {
		t.Fatal(err)
	}

	lastCall := func() string {
		mu.Lock()
		defer mu.Unlock()
		if len(calls) == 0 {
			return ""
		}
		return calls[len(calls)-1]
	}
	callsSince := func(n int) int {
		mu.Lock()
		defer mu.Unlock()
		return len(calls) - n
	}
	// The board handlers read the caller's ACTIVE team: the board is the
	// team's, and the connection is checked against it.
	boardCtx := auth.WithIdentity(context.Background(), auth.Identity{UserID: "admin", IsSuperAdmin: true, TeamID: "t1"})
	drive := func(name string, handler http.HandlerFunc, method, path, body string, pathValues map[string]string) *httptest.ResponseRecorder {
		t.Helper()
		w := httptest.NewRecorder()
		req := forgeReq(boardCtx, method, path, body, "t1")
		for k, v := range pathValues {
			req.SetPathValue(k, v)
		}
		handler(w, req)
		t.Logf("%s: code=%d body=%s", name, w.Code, strings.TrimSpace(w.Body.String()))
		return w
	}
	wantBot := func(name, verb string, w *httptest.ResponseRecorder) {
		t.Helper()
		if w.Code != http.StatusOK {
			t.Fatalf("%s answered %d: %s", name, w.Code, w.Body.String())
		}
		c := lastCall()
		if !strings.HasPrefix(c, verb+" ") {
			t.Fatalf("%s: the last forge call is %q, want a %s", name, c, verb)
		}
		if bearer := c[strings.LastIndex(c, "bearer=")+7:]; bearer != pbMinted {
			t.Errorf("%s reached the forge as %q — the minted project bot is %q, the human's grant is %q", name, bearer, pbMinted, pbGrant)
		}
	}

	// The forge accepted the issue, then the card link failed: the write is
	// audited all the same.
	orphan, err := board.Create(native.Issue{Title: "orphan", Body: "body"})
	if err != nil {
		t.Fatal(err)
	}
	fb.fail.Store(true)
	w = drive("push, link fails", s.handlePushIssueToForge, "POST", "/api/board/issues/"+orphan.ID+"/push", `{"connection_id":"`+conn.ID+`","repo":"group/api"}`, map[string]string{"id": orphan.ID})
	fb.fail.Store(false)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("a failed card link answered %d, want 500", w.Code)
	}
	// A card pushed to a new issue, and a linked card pushed as an update.
	w = drive("push unlinked card", s.handlePushIssueToForge, "POST", "/api/board/issues/"+unlinked.ID+"/push", `{"connection_id":" `+conn.ID+` ","repo":" group/api "}`, map[string]string{"id": unlinked.ID})
	wantBot("the issue created from a card", "POST", w)
	if pushed, err := board.Get(unlinked.ID); err != nil || pushed.External == nil || pushed.External.Number != 7 || pushed.External.Repo != "group/api" {
		t.Fatalf("the pushed card is not linked to the issue it created: %+v, %v", pushed, err)
	}
	w = drive("push linked card", s.handlePushIssueToForge, "POST", "/api/board/issues/"+linked.ID+"/push", "", map[string]string{"id": linked.ID})
	wantBot("the issue updated from a card", "PUT", w)
	// A pull request opened from a card, and one merged from a card. The
	// opened one carries the board marker: the webhook lane reviews it as
	// human work although its author is the bot.
	w = drive("create pull", s.handleCreateIssuePull, "POST", "/api/board/issues/"+unlinked.ID+"/pulls", `{"source_branch":"f","target_branch":"main","title":"t","body":"please review","connection_id":"`+conn.ID+`","repo":"group/api"}`, map[string]string{"id": unlinked.ID})
	wantBot("the pull request opened from a card", "POST", w)
	lastDescription := func() string {
		t.Helper()
		mu.Lock()
		opened := bodies[len(bodies)-1]
		mu.Unlock()
		var got struct {
			Description *string `json:"description"`
		}
		if err := json.Unmarshal([]byte(opened), &got); err != nil {
			t.Fatalf("the create request is not JSON: %v: %s", err, opened)
		}
		if got.Description == nil {
			t.Fatalf("the create request carries no description: %s", opened)
		}
		return *got.Description
	}
	var sent struct{ Description string }
	sent.Description = lastDescription()
	if !strings.HasSuffix(sent.Description, "\n\n"+boardOpenedPullMarker) || !strings.HasPrefix(sent.Description, "please review") {
		t.Errorf("the pull request opened from a card was sent without the board marker after its body: %q", sent.Description)
	}
	// The Studio's own shape sends no body: the description is the marker
	// alone — the path production uses, so the guard's exemption holds there.
	w = drive("create pull, Studio shape", s.handleCreateIssuePull, "POST", "/api/board/issues/"+unlinked.ID+"/pulls", `{"source_branch":"f","target_branch":"main","connection_id":"`+conn.ID+`","repo":"group/api"}`, map[string]string{"id": unlinked.ID})
	wantBot("the pull request opened from a card without a body", "POST", w)
	sent.Description = lastDescription()
	if sent.Description != boardOpenedPullMarker {
		t.Errorf("a pull request opened without a body was sent with description %q, want the marker alone", sent.Description)
	}
	// A body that already carries the marker is sent as is, not marked twice.
	w = drive("create pull, body already marked", s.handleCreateIssuePull, "POST", "/api/board/issues/"+unlinked.ID+"/pulls", `{"source_branch":"f","target_branch":"main","body":"again\n\n`+boardOpenedPullMarker+`","connection_id":"`+conn.ID+`","repo":"group/api"}`, map[string]string{"id": unlinked.ID})
	wantBot("the pull request opened from a marked body", "POST", w)
	sent.Description = lastDescription()
	if strings.Count(sent.Description, boardOpenedPullMarker) != 1 {
		t.Errorf("a body already carrying the marker was sent with it %d times: %q", strings.Count(sent.Description, boardOpenedPullMarker), sent.Description)
	}
	w = drive("merge pull", s.handleMergeIssuePull, "POST", "/api/board/issues/"+linked.ID+"/pulls/5/merge", `{"method":"merge"}`, map[string]string{"id": linked.ID, "number": "5"})
	wantBot("the pull request merged from a card", "PUT", w)

	// Reads keep the grant: listing the card's pull requests and reading one
	// are the person's.
	drive("list pulls", s.handleListIssuePulls, "GET", "/api/board/issues/"+linked.ID+"/pulls", "", map[string]string{"id": linked.ID})
	if c := lastCall(); !strings.HasPrefix(c, "GET ") || !strings.HasSuffix(c, "bearer="+pbGrant) {
		t.Errorf("listing pull requests reached the forge as %q, want the grant %q (reads keep it)", c, pbGrant)
	}
	before := callsSince(0)
	drive("pull CI", s.handleIssuePullCI, "GET", "/api/board/issues/"+linked.ID+"/pulls/5/ci", "", map[string]string{"id": linked.ID, "number": "5"})
	mu.Lock()
	ciCalls := append([]string(nil), calls[before:]...)
	mu.Unlock()
	if len(ciCalls) == 0 || !strings.HasPrefix(ciCalls[0], "GET ") || !strings.HasSuffix(ciCalls[0], "bearer="+pbGrant) {
		t.Errorf("reading a pull request reached the forge as %v, want a GET with the grant %q", ciCalls, pbGrant)
	}

	// The person who acted is in the team's audit log, with what posted.
	var events []audit.Event
	waitForCond(t, 5*time.Second, "the four board writes in the audit log", func() bool {
		var err error
		events, err = s.auditStore.ListByTenant(context.Background(), "t1", audit.Page{Limit: 100})
		if err != nil {
			t.Fatalf("audit: %v", err)
		}
		n := 0
		for _, e := range events {
			if strings.HasPrefix(e.Action, "forge.board.") {
				n++
			}
		}
		return n >= 7
	})
	seen := map[string]bool{}
	wantNumber := map[string]string{"forge.board.issue_pushed": "7", "forge.board.issue_updated": "3", "forge.board.pull_opened": "5", "forge.board.pull_merged": "5"}
	for _, e := range events {
		if !strings.HasPrefix(e.Action, "forge.board.") {
			continue
		}
		seen[e.Action] = true
		if e.ActorID != "admin" || e.Target != "board_card" || e.Meta["posted_as"] != "project_bot" || e.Meta["repo"] != "group/api" || e.Meta["connection_id"] != conn.ID || fmt.Sprint(e.Meta["number"]) != wantNumber[e.Action] {
			t.Errorf("audit event %s = actor %q target %q meta %v, want the acting user, board_card, posted_as project_bot on group/api, connection %s, number %s", e.Action, e.ActorID, e.Target, e.Meta, conn.ID, wantNumber[e.Action])
		}
	}
	orphanAudited := false
	for _, e := range events {
		if e.Action == "forge.board.issue_pushed" && e.TargetID == orphan.ID {
			orphanAudited = true
		}
	}
	if !orphanAudited {
		t.Errorf("the issue created for a card whose link then failed is not in the audit log")
	}
	for _, want := range []string{"forge.board.issue_pushed", "forge.board.issue_updated", "forge.board.pull_opened", "forge.board.pull_merged"} {
		if !seen[want] {
			t.Errorf("audit log lacks %s: %v", want, seen)
		}
	}

	// A repository with no minted bot: refused before the forge is asked,
	// never posted as the person. A fresh card: the push above linked the
	// first one to group/api, and a linked card names its own repository.
	fresh, err := board.Create(native.Issue{Title: "fresh", Body: "body"})
	if err != nil {
		t.Fatal(err)
	}
	before = callsSince(0)
	w = drive("create pull on an unenabled repo", s.handleCreateIssuePull, "POST", "/api/board/issues/"+fresh.ID+"/pulls", `{"source_branch":"f","target_branch":"main","connection_id":"`+conn.ID+`","repo":"group/other"}`, map[string]string{"id": fresh.ID})
	if w.Code != http.StatusConflict {
		t.Errorf("a write on a repository with no minted bot answered %d, want 409: %s", w.Code, w.Body.String())
	}
	if n := callsSince(before); n != 0 {
		t.Errorf("the refused write still reached the forge: %d call(s), last %q", n, lastCall())
	}
}

type fixFailingBoard struct {
	native.BoardStore
	fail atomic.Bool
}

func (b *fixFailingBoard) Update(id string, p native.Patch) (*native.Issue, error) {
	if b.fail.Load() {
		return nil, fmt.Errorf("board store write refused")
	}
	return b.BoardStore.Update(id, p)
}
