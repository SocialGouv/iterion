package model_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/backend/secretguard"
	"github.com/SocialGouv/iterion/pkg/connector/exec"
	"github.com/SocialGouv/iterion/pkg/connector/spec"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// queryTokenNode wires a GET whose `token` query parameter the package does
// NOT mark secret — the ordinary case, since a generated package inherits that
// flag from a vendor description that almost never sets one. The run, however,
// materialises a real `{{secrets.X}}` into it, which is the whole gap:
// exec's redaction set is what the PACKAGE declared, the run's Guard knows
// what was actually materialised.
func queryTokenNode(t *testing.T, base, secretValue string) (*model.ClawExecutor, *ir.ToolNode) {
	t.Helper()
	pkg, op := mutatingPackage(base)
	op.HTTP.Method, op.HTTP.RequestBody, op.Effect = "GET", "", spec.EffectRead
	op.Params = []spec.Param{{Key: "token", Name: "token", In: spec.InQuery, Type: "string"}}
	op.Results = []spec.ResultCase{{Status: 200}}
	pkg.Ops[0].Operations[0] = op

	guard := secretguard.New([]secretguard.Secret{{Name: "MY_SECRET", Value: secretValue}},
		secretguard.DefaultConfig())
	node := &ir.ToolNode{
		BaseNode: ir.BaseNode{ID: "list"}, Action: "probe.issue.comment", Connection: "main",
		Params: []ir.ActionParam{{
			Key:   "token",
			Value: "{{secrets.MY_SECRET}}",
			Refs:  []*ir.Ref{{Kind: ir.RefSecrets, Path: []string{"MY_SECRET"}, Raw: "{{secrets.MY_SECRET}}"}},
		}},
	}
	e := model.NewClawExecutor(model.NewRegistry(), &ir.Workflow{},
		model.WithSecretGuard(guard),
		model.WithConnectors(&stubResolver{pkg: pkg, op: op, baseURL: base}, exec.MarkGuarded(&http.Client{})))
	return e, node
}

// TestAMaterialisedSecretIsRedactedOutOfTheNodeError.
//
// A transport failure puts the *url.Error's own text — which prints the full
// request URL, query string and all — straight into Error.Message
// (exec.transportError). exec redacts it against the package's declared
// secrets plus the connection credential, and a second credential passed as an
// ARGUMENT is in neither set, so it left verbatim in the node's error, which
// travels to the run's events, the tool hooks and error tracking.
func TestAMaterialisedSecretIsRedactedOutOfTheNodeError(t *testing.T) {
	const secretValue = "gh-pat-not-a-real-token-9f2c"
	// A server that is up long enough to hand out a URL and then refuses the
	// connection: the request is BUILT (so the secret is in the query string)
	// and then fails at the transport, which is the path that copies the URL
	// into the message.
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	base := srv.URL
	srv.Close()

	e, node := queryTokenNode(t, base, secretValue)
	_, err := e.Execute(context.Background(), node, nil)
	if err == nil {
		t.Fatal("a refused connection must fail the node")
	}
	if strings.Contains(err.Error(), secretValue) {
		t.Errorf("the secret travelled verbatim in the node error: %v", err)
	}
	// Redacted to the guard's PLACEHOLDER, not merely dropped: an operator has
	// to be able to read WHICH secret travelled without reading its value.
	if !strings.Contains(err.Error(), "__ITERION_SECRET_MY_SECRET__") {
		t.Errorf("the redaction must name the secret it replaced: %v", err)
	}
	// Load-bearing: the redaction must not have flattened the typed error, or
	// the engine's recovery dispatcher — which classifies by type — loses the
	// ambiguous-effect no-retry guarantee that finishAction's `%w` exists for.
	var typed *exec.Error
	if !errors.As(err, &typed) {
		t.Fatalf("the typed *exec.Error must survive the redaction: %v", err)
	}
	if typed.Class != spec.ErrTransport {
		t.Errorf("Class = %q, want %q", typed.Class, spec.ErrTransport)
	}
}

// The same gap on the vendor's OWN words: a gateway echoing the credential it
// rejected has that text copied into the message.
func TestAMaterialisedSecretIsRedactedOutOfAVendorEcho(t *testing.T) {
	const secretValue = "gh-pat-not-a-real-token-7b31"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		// What a real gateway does: name the token it refused.
		_, _ = w.Write([]byte(`{"message": "rejected credential ` + r.URL.Query().Get("token") + `"}`))
	}))
	defer srv.Close()

	e, node := queryTokenNode(t, srv.URL, secretValue)
	_, err := e.Execute(context.Background(), node, nil)
	if err == nil {
		t.Fatal("a 403 must fail the node")
	}
	if strings.Contains(err.Error(), secretValue) {
		t.Errorf("the vendor's echo carried the secret into the node error: %v", err)
	}
	var typed *exec.Error
	if !errors.As(err, &typed) {
		t.Fatalf("the typed *exec.Error must survive the redaction: %v", err)
	}
}

// An error that carries no secret must come back EXACTLY as it went in, so the
// sentinel checks the rest of the engine makes still work. Flattening
// unconditionally is how a redaction turns into a second defect.
func TestAnErrorWithNoSecretIsNotRebuilt(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	e, node := queryTokenNode(t, srv.URL, "a-secret-the-vendor-never-mentions")
	_, err := e.Execute(context.Background(), node, nil)
	if err == nil {
		t.Fatal("a 404 must fail the node")
	}
	var typed *exec.Error
	if !errors.As(err, &typed) {
		t.Fatalf("the typed error must reach the engine untouched: %v", err)
	}
	if typed.Status != http.StatusNotFound {
		t.Errorf("Status = %d, want %d", typed.Status, http.StatusNotFound)
	}
}

// TestASecretReachesTheVendorEXACTLY.
//
// A whole-value reference is rendered as a JSON LITERAL — that is what lets
// `index: "{{outputs.pick.number}}"` reach an integer field as a number — and
// the secret was materialised INTO that literal, after the encoder had run and
// before the coercion decodes it. So the credential's own bytes were read as
// JSON syntax: a `"` or a `\` in it made the decode fail and the value reached
// the vendor WITH its surrounding quotes, while a literal `\n` two-character
// sequence arrived as a newline. Every one of those is a 401 on a perfectly
// valid credential — the symptom materialising was added to remove — and the
// shipped test could not see it, its own secret containing neither character.
//
// The oracle is the request the server received.
func TestASecretReachesTheVendorEXACTLY(t *testing.T) {
	for _, tc := range []struct{ name, secret string }{
		{"ordinary", "gh-pat-not-a-real-token-7b31"},
		{"a backslash", `back\slash-token`},
		{"a quote", `has"quote-token`},
		{"an escape sequence", `nl\nliteral-token`},
		{"a brace", `{"looks":"like json"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				got = r.URL.Query().Get("token")
				_, _ = w.Write([]byte(`{}`))
			}))
			defer srv.Close()

			e, node := queryTokenNode(t, srv.URL, tc.secret)
			if _, err := e.Execute(context.Background(), node, nil); err != nil {
				t.Fatalf("execute: %v", err)
			}
			if got != tc.secret {
				t.Errorf("the vendor received %q, want the credential byte for byte", got)
			}
		})
	}
}
