package server

import (
	"context"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dispatcher/boardmongo"
	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	"github.com/SocialGouv/iterion/pkg/webhooks"
	"github.com/SocialGouv/iterion/pkg/webhooks/prforge"
)

// TestForkLaneConfig_AnIssueLabelNeverCardsATrustedTarget: "a fork-lane
// config never launches a same-repo target" holds on EVERY entry of the
// config, not only the gesture. The issue lane (an `issues: labeled`
// delivery, or a zero-touch open) composes a TRUSTED target on the base's
// default branch and hands it to dispatchInvocation, whose board branch
// writes a card: with a coordinator a Ready card the dispatcher later
// launches — a branch that never reaches the launch tail — and without one
// a tracking card written before the tail refuses. The branch answers the
// tail's provenance precondition (webhookProvenanceRefusal) first, so the
// delivery is filtered with the lane-mismatch reason and no card exists,
// either way.
//
// Mutation: drop the webhookProvenanceRefusal call from the board branch →
// the coordinator row answers 202 "carded" with a Ready card, and the
// no-coordinator row leaves a tracking card for the launch the tail refuses
// (each red on its own assertion).
func TestForkLaneConfig_AnIssueLabelNeverCardsATrustedTarget(t *testing.T) {
	for _, withCoordinator := range []bool{true, false} {
		name := "a board coordinator (the card is the launch)"
		if !withCoordinator {
			name = "no coordinator (a tracking card beside the launch)"
		}
		t.Run(name, func(t *testing.T) {
			s := newWebhookTestServer(t)
			board, err := native.NewStore(t.TempDir())
			if err != nil {
				t.Fatalf("native.NewStore: %v", err)
			}
			s.cfg.CloudBoardFor = func(string) native.BoardStore { return board }
			if withCoordinator {
				s.cfg.CloudBoardCoordinator = &boardmongo.Coordinator{}
			}
			launched := 0
			s.webhookLaunchBot = func(context.Context, string, map[string]string, string, string, string, map[string]string, map[string]string) (string, error) {
				launched++
				return "run-x", nil
			}
			cfg, pt := issueConfig(t, s)
			cfg.ForkLane = true

			w := httptest.NewRecorder()
			s.handleGitHubWebhook(w, ghReq(ghCtx(cfg), ghLabeledIssue, prforge.EventHeaderIssues, pt))

			cards, err := board.List(native.ListFilter{})
			if err != nil {
				t.Fatal(err)
			}
			if len(cards) != 0 {
				t.Fatalf("a fork-lane config carded %d trusted target(s) (response %d %s) — the lane serves the gesture only", len(cards), w.Code, w.Body.String())
			}
			if launched != 0 {
				t.Fatalf("launched %d run(s)", launched)
			}
			if w.Code != http.StatusOK {
				t.Fatalf("code=%d body=%s — a refusal is a 200/filtered verdict", w.Code, w.Body.String())
			}
			var resp map[string]string
			if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
				t.Fatalf("body=%s: %v", w.Body.String(), err)
			}
			if resp["status"] != webhooks.StatusFiltered || !strings.Contains(resp["reason"], "fork-lane mismatch") {
				t.Fatalf("response = %v, want a filtered lane-mismatch verdict", resp)
			}
			rows, err := s.webhookDeliveries.ListByWebhook(context.Background(), cfg.TenantID, cfg.ID, 10)
			if err != nil || len(rows) != 1 {
				t.Fatalf("delivery rows=%+v err=%v, want the refusal recorded once", rows, err)
			}
			if rows[0].Status != webhooks.StatusFiltered || !strings.Contains(rows[0].Error, "fork-lane mismatch") {
				t.Fatalf("delivery row = %s %q, want filtered naming the lane mismatch", rows[0].Status, rows[0].Error)
			}
		})
	}
}

// TestWebhookProvenanceRefusal_IsTheOnePredicate keeps the class closed as
// the package grows: the lane-disjointness predicate is spelled once, in
// webhookProvenanceRefusal, and every launch from a config crosses that
// function. A site that re-spells `cfg.ForkLane && !…Trust…` inline is a
// second guard that will drift — the board card branch was exactly that
// shape, checking one direction of the two.
func TestWebhookProvenanceRefusal_IsTheOnePredicate(t *testing.T) {
	inline := regexp.MustCompile(`ForkLane\s*&&\s*!\s*[A-Za-z_.]*Trust`)
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, rerr := os.ReadFile(filepath.Clean(name))
		if rerr != nil {
			t.Fatal(rerr)
		}
		if !inline.Match(src) {
			continue
		}
		f, perr := parser.ParseFile(fset, name, src, 0)
		if perr != nil {
			t.Fatal(perr)
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Name.Name == "webhookProvenanceRefusal" {
				continue
			}
			body := src[fset.Position(fn.Pos()).Offset:fset.Position(fn.End()).Offset]
			if inline.Match(body) {
				t.Errorf("%s: %s spells the lane-disjointness predicate inline — call webhookProvenanceRefusal, the one precondition every launch from a config crosses", name, fn.Name.Name)
			}
		}
	}
}
