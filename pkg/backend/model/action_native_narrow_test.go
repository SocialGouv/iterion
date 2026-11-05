package model_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/backend/secretguard"
	"github.com/SocialGouv/iterion/pkg/connector/exec"
	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	"github.com/SocialGouv/iterion/pkg/dispatcher/native/boardops"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// A native action narrows its arguments through the conversion an HTTP call
// does (#2991 on the board seam, ADR-151). The board facade declares
// `labels` an array of strings; an HTTP operation declaring its `body` the
// same way is the twin. Each value below reaches the filed card as it
// reaches the vendor's JSON body, or both refuse it, typed and at the same
// element, before the store or the vendor is reached. The secrets mode is
// the one difference: both narrow the placeholder form, then a secret keeps
// its placeholder on the native path and is swapped into the narrowed
// strings on the HTTP one — so a secret with bytes JSON reads (a quote, a
// literal backslash-n, a comma) narrows alike on both, and a refusal's Text
// holds the placeholder on both.
//
// Mutations seen red (each restored): narrowing skipped for the native mode
// in renderActionParams (the store received every list as text and refused
// it, untyped); the secret materialised whatever the mode (the card carried
// the secret's value); the secret materialised before narrowing on the HTTP
// path (the quoted secret's element was refused over HTTP and filed on the
// board, `[{{secrets.SPLIT}}]` reached the vendor as two labels and was
// refused by the board).
func TestANativeActionNarrowsEachArgumentAsAnHTTPCallDoes(t *testing.T) {
	t.Parallel()
	const (
		secretValue = "ghp_synthetic_label_secret_2991"
		quoted      = `pa"ss\n-not-real-0001`
		split       = `11,22`
	)
	ph := secretguard.PlaceholderForName("GH_TOKEN")
	quotedJSON, err := json.Marshal([]string{quoted})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name     string
		value    string
		input    map[string]any
		native   []string // the card's labels; nil when the value is refused
		vendor   string   // the vendor's JSON body
		item     int      // the refused element, -1 for the argument itself
		declared string   // the type the refused value was narrowed to
		text     string   // the refusal's Text, when the value was text
	}{
		{name: "a string[] var seeded from launch text", value: "{{vars.labels}}", native: []string{"a11y", "rgaa"}, vendor: `["a11y","rgaa"]`},
		{name: "a literal JSON list", value: `["a11y","rgaa"]`, native: []string{"a11y", "rgaa"}, vendor: `["a11y","rgaa"]`},
		{name: "a string holding a JSON list", value: "{{input.labels}}", input: map[string]any{"labels": `["a11y"]`}, native: []string{"a11y"}, vendor: `["a11y"]`},
		{name: "number elements narrow to the declared string items", value: "{{input.labels}}", input: map[string]any{"labels": []any{1, 2}}, native: []string{"1", "2"}, vendor: `["1","2"]`},
		{name: "a secret element", value: `["{{secrets.GH_TOKEN}}"]`, native: []string{ph}, vendor: `["` + secretValue + `"]`},
		{name: "a secret element keeps the bytes JSON reads", value: `["{{secrets.QUOTED}}"]`, native: []string{secretguard.PlaceholderForName("QUOTED")}, vendor: string(quotedJSON)},
		{name: "a secret spells no list syntax", value: `[{{secrets.SPLIT}}]`, item: -1, declared: "array", text: "[" + secretguard.PlaceholderForName("SPLIT") + "]"},
		{name: "a comma list is refused", value: "a11y,rgaa", item: -1, declared: "array", text: "a11y,rgaa"},
		{name: "an object element is refused", value: `["a",{"k":1}]`, item: 1, declared: "string"},
		{name: "a whole-value secret is refused as text", value: "{{secrets.GH_TOKEN}}", item: -1, declared: "array", text: ph},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			refs, err := ir.ParseRefs(tc.value)
			if err != nil {
				t.Fatalf("ParseRefs(%q): %v", tc.value, err)
			}
			wf := &ir.Workflow{Vars: map[string]*ir.Var{"labels": {Name: "labels", Type: ir.VarStringArray}}}
			guard := secretguard.New([]secretguard.Secret{
				{Name: "GH_TOKEN", Value: secretValue},
				{Name: "QUOTED", Value: quoted},
				{Name: "SPLIT", Value: split},
			}, secretguard.DefaultConfig())
			launch := map[string]any{"labels": "a11y, rgaa"}

			// The native transport: the instance's board, the card the oracle.
			store := newBoardStore(t)
			ne := model.NewClawExecutor(model.NewRegistry(), wf,
				model.WithSecretGuard(guard),
				model.WithActionTransports(boardops.NewActionTransport(store, boardops.CallEnv{})))
			ne.SetLaunchVars(launch)
			nnode := boardActionNode("board.issue.create", []string{"board.create"})
			nnode.Params = []ir.ActionParam{
				{Key: "title", Value: "Critère 1.1 — images"},
				{Key: "labels", Value: tc.value, Refs: refs},
			}
			_, nerr := ne.Execute(context.Background(), nnode, tc.input)

			// The HTTP twin: the vendor's body the oracle.
			rec := &bodyRecorder{}
			srv := httptest.NewServer(http.HandlerFunc(rec.serve))
			t.Cleanup(srv.Close)
			pkg, op := mutatingPackage(srv.URL)
			op.Params[0].Type = "array"
			op.Params[0].Items = "string"
			pkg.Ops[0].Operations[0] = op
			he := model.NewClawExecutor(model.NewRegistry(), wf,
				model.WithSecretGuard(guard),
				model.WithConnectors(&stubResolver{pkg: pkg, op: op, baseURL: srv.URL}, exec.MarkGuarded(srv.Client())))
			he.SetLaunchVars(launch)
			hnode := &ir.ToolNode{
				BaseNode: ir.BaseNode{ID: "n"}, Action: "probe.issue.comment", Connection: "main",
				Params: []ir.ActionParam{{Key: "body", Value: tc.value, Refs: refs}},
			}
			_, herr := he.Execute(context.Background(), hnode, tc.input)

			if tc.native != nil {
				if nerr != nil {
					t.Fatalf("native: %v", nerr)
				}
				if herr != nil {
					t.Fatalf("http: %v", herr)
				}
				issues, err := store.List(native.ListFilter{})
				if err != nil || len(issues) != 1 {
					t.Fatalf("List: %d card(s), err %v — want the one card filed", len(issues), err)
				}
				if !slices.Equal(issues[0].Labels, tc.native) {
					t.Errorf("the card carries labels %q, want %q", issues[0].Labels, tc.native)
				}
				if got, _ := rec.last(t); got != tc.vendor {
					t.Errorf("the vendor received %s, want %s", got, tc.vendor)
				}
				return
			}

			for transport, err := range map[string]error{"native": nerr, "http": herr} {
				var refused *model.ActionParamError
				if !errors.As(err, &refused) {
					t.Errorf("%s: want a typed *model.ActionParamError, got %T: %v", transport, err, err)
					continue
				}
				if refused.Item != tc.item || refused.Declared != tc.declared {
					t.Errorf("%s: refusal = {Item:%d Declared:%q}, want {Item:%d Declared:%q}", transport, refused.Item, refused.Declared, tc.item, tc.declared)
				}
				if refused.Text != tc.text {
					t.Errorf("%s: the refusal's Text = %q, want %q", transport, refused.Text, tc.text)
				}
			}
			if cards := countCards(t, store); cards != 0 {
				t.Errorf("%d card(s) filed from a refused argument", cards)
			}
			if _, calls := rec.last(t); calls != 0 {
				t.Errorf("the vendor was called %d time(s) with a refused argument", calls)
			}
		})
	}
}
