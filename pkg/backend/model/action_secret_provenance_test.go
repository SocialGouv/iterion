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

// Provenance, not spelling: an action argument materialises the author's own
// `{{secrets.NAME}}` references and nothing else. Each renders to a marker of
// that one rendering (crypto/rand, never shown), which alone leaf resolves —
// so a placeholder DATA spells, however it arrives (the exact token, a JSON
// escape that decodes into it, two references that join into it), stays the
// text it is and never becomes the value.
//
// Mutations seen red (each restored): leaf resolving canonical placeholders
// through MaterializeFor(params) — every data case below leaked the value to
// the vendor; the untyped-list and object cases left on
// secretguard.MaterializeLeaves with the marker leaf (the escaped
// interpolation materialised after the decode — the marker leaf never saw
// the joined text).
func TestAPlaceholderDataSpellsNeverMaterialises(t *testing.T) {
	t.Parallel()
	const value = "db-password-7f3a9c2e1b4d"
	ph := secretguard.PlaceholderForName("PW")
	// The JSON escape of the first underscore: decodes to the exact token.
	esc := `\u005f` + ph[1:]
	for _, tc := range []struct {
		name            string
		declared, items string
		value           string
		input           map[string]any
	}{
		{name: "the literal token in a whole-value list", declared: "array", items: "string", value: "{{input.labels}}", input: map[string]any{"labels": []any{ph}}},
		{name: "the literal token interpolated", declared: "array", items: "string", value: `["{{input.x}}"]`, input: map[string]any{"x": ph}},
		{name: "a JSON escape in a string holding a list", declared: "array", items: "string", value: "{{input.labels}}", input: map[string]any{"labels": `["` + esc + `"]`}},
		{name: "a JSON escape interpolated into a list", declared: "array", items: "string", value: `["{{input.x}}"]`, input: map[string]any{"x": esc}},
		{name: "a JSON escape interpolated into an untyped list", declared: "array", value: `["{{input.x}}"]`, input: map[string]any{"x": esc}},
		{name: "a JSON escape interpolated into an object", declared: "object", value: `{"k":"{{input.x}}"}`, input: map[string]any{"x": esc}},
		{name: "two references joined into the token on a string", declared: "string", value: `{{input.a}}{{input.b}}`, input: map[string]any{"a": ph[:10], "b": ph[10:]}},
		{name: "two references joined into the token in a list", declared: "array", items: "string", value: `["{{input.a}}{{input.b}}"]`, input: map[string]any{"a": ph[:10], "b": ph[10:]}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			// The secret GRANTS the params sink: what is asserted is not the
			// grant but the provenance — the value must not reach the vendor
			// from data even where the author's own reference would deliver it.
			guard := secretguard.New([]secretguard.Secret{{Name: "PW", Value: value, Sinks: []string{"params"}}}, secretguard.DefaultConfig())
			rec, run := narrowingNode(t, tc.declared, tc.items, tc.value, model.WithSecretGuard(guard))
			if err := run(tc.input); err != nil {
				t.Fatalf("execute: %v", err)
			}
			got, calls := rec.last(t)
			if calls != 1 {
				t.Fatalf("the vendor was called %d times, want 1", calls)
			}
			if strings.Contains(got, value) {
				t.Errorf("LEAK: a secret reached the vendor from data: %s", got)
			}
			if !strings.Contains(got, "ITERION_SECRET_PW") {
				t.Errorf("the placeholder must stay the text it is, got %s", got)
			}
		})
	}
}

// The authored reference still resolves: the same secret, the same declared
// types, the reference the author wrote.
//
// Mutation seen red (restored): the HTTP leaf resolving a marker to its
// canonical text rather than through the params grant (the vendor received
// the placeholder).
func TestAnAuthoredSecretReferenceStillMaterialisesOverHTTP(t *testing.T) {
	t.Parallel()
	const value = "db-password-7f3a9c2e1b4d"
	for _, tc := range []struct {
		name            string
		declared, items string
		value           string
		want            string
	}{
		{name: "an element of a string list", declared: "array", items: "string", value: `["{{secrets.PW}}"]`, want: `["` + value + `"]`},
		{name: "an object value", declared: "object", value: `{"k":"{{secrets.PW}}"}`, want: `{"k":"` + value + `"}`},
		{name: "a string argument", declared: "string", value: `bearer {{secrets.PW}}`, want: `"bearer ` + value + `"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			guard := secretguard.New([]secretguard.Secret{{Name: "PW", Value: value, Sinks: []string{"params"}}}, secretguard.DefaultConfig())
			rec, run := narrowingNode(t, tc.declared, tc.items, tc.value, model.WithSecretGuard(guard))
			if err := run(nil); err != nil {
				t.Fatalf("execute: %v", err)
			}
			if got, _ := rec.last(t); got != tc.want {
				t.Errorf("the vendor received %s, want %s", got, tc.want)
			}
		})
	}
}

// A map key is sent as written, so a secret may not stand in one: an authored
// `{{secrets.K}}` in a key is a typed refusal on EVERY declaration — the
// object and untyped readings of the same text cannot disagree — naming the
// key as its placeholder, with the marker and the value in neither the
// message nor the fields. A placeholder DATA carries into a key is not
// refused: it is text, sent as written, and never becomes the value.
//
// Mutations seen red (each restored): the key check dropped from
// authoredSecrets.leaves (the object-declared key reached the vendor as its
// marker — the refusal gone); the key check dropped from leafText (the
// untyped and string declarations materialised the value into the key); the
// refused key shown without shown() (the message carried the marker).
func TestASecretReferenceInAKeyIsRefusedOnEveryDeclaration(t *testing.T) {
	t.Parallel()
	const value = "keyname-not-real-91"
	ph := secretguard.PlaceholderForName("K")
	for _, tc := range []struct {
		name     string
		declared string
		items    string
		value    string
		item     int
	}{
		{name: "an object declaration", declared: "object", value: `{"{{secrets.K}}":"v"}`, item: -1},
		{name: "an untyped declaration", declared: "", value: `{"{{secrets.K}}":"v"}`, item: -1},
		{name: "a string declaration", declared: "string", value: `{"{{secrets.K}}":"v"}`, item: -1},
		{name: "nested in an object value", declared: "object", value: `{"a":{"{{secrets.K}}":"v"}}`, item: -1},
		{name: "an element of object items", declared: "array", items: "object", value: `[{"ok":1},{"{{secrets.K}}":"v"}]`, item: 1},
		{name: "an unknown parameter", declared: "string", value: `{"{{secrets.K}}":"v"}`, item: -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			guard := secretguard.New([]secretguard.Secret{{Name: "K", Value: value}}, secretguard.DefaultConfig())
			key := "body"
			if tc.name == "an unknown parameter" {
				key = "zz"
			}
			rec, run := narrowingNodeFor(t, key, tc.declared, tc.items, tc.value, model.WithSecretGuard(guard))
			err := run(nil)
			var refused *model.ActionParamError
			if !errors.As(err, &refused) {
				t.Fatalf("want a typed *model.ActionParamError, got %T: %v", err, err)
			}
			if refused.SecretKey != ph {
				t.Errorf("SecretKey = %q, want the reference's placeholder %q", refused.SecretKey, ph)
			}
			if refused.Item != tc.item || refused.Key != key {
				t.Errorf("refusal = {Key:%q Item:%d}, want {Key:%q Item:%d}", refused.Key, refused.Item, key, tc.item)
			}
			if strings.Contains(err.Error(), "ITERION_AUTHORED") || strings.Contains(refused.SecretKey, "ITERION_AUTHORED") {
				t.Errorf("the refusal shows a marker: %v", err)
			}
			if strings.Contains(err.Error(), value) {
				t.Errorf("the refusal carries the value: %v", err)
			}
			if _, calls := rec.last(t); calls != 0 {
				t.Errorf("the vendor was called %d time(s) with a refused argument", calls)
			}
		})
	}
}

// A placeholder DATA carries into a key is text: not refused, not resolved —
// sent as written.
func TestAPlaceholderDataCarriesIntoAKeyStaysText(t *testing.T) {
	t.Parallel()
	const value = "keyname-not-real-91"
	ph := secretguard.PlaceholderForName("K")
	guard := secretguard.New([]secretguard.Secret{{Name: "K", Value: value}}, secretguard.DefaultConfig())
	rec, run := narrowingNode(t, "object", "", "{{input.doc}}", model.WithSecretGuard(guard))
	if err := run(map[string]any{"doc": map[string]any{ph: "v"}}); err != nil {
		t.Fatalf("a data-carried placeholder key is text, not a refusal: %v", err)
	}
	got, _ := rec.last(t)
	if strings.Contains(got, value) {
		t.Errorf("LEAK: the key materialised from data: %s", got)
	}
	if !strings.Contains(got, ph) {
		t.Errorf("the key must be sent as written, got %s", got)
	}
}

// A secret reference outside a JSON string — `[{{secrets.N}}]`, `{"id":
// {{secrets.N}}}` — asks for the secret's VALUE as syntax, and a secret is a
// string: the refusal is typed (BareSecret), quotes the text with the
// reference as its placeholder, and names the escape.
//
// Mutations seen red (each restored): the outsideString check dropped from
// the array case (the refusal fell back to the generic "does not read as an
// array", BareSecret false); the same in the object case.
func TestASecretReferenceOutsideAStringIsATypedRefusal(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		declared string
		value    string
		text     string
		escape   string
	}{
		{name: "an element of an array", declared: "array", value: `[{{secrets.N}}]`, text: "[" + secretguard.PlaceholderForName("N") + "]", escape: "items: integer"},
		{name: "an object value", declared: "object", value: `{"id": {{secrets.N}}}`, text: `{"id": ` + secretguard.PlaceholderForName("N") + `}`, escape: "quote"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			guard := secretguard.New([]secretguard.Secret{{Name: "N", Value: "42424"}}, secretguard.DefaultConfig())
			rec, run := narrowingNode(t, tc.declared, "", tc.value, model.WithSecretGuard(guard))
			err := run(nil)
			var refused *model.ActionParamError
			if !errors.As(err, &refused) {
				t.Fatalf("want a typed *model.ActionParamError, got %T: %v", err, err)
			}
			if !refused.BareSecret {
				t.Errorf("BareSecret = false, want the typed bare-reference refusal: %v", err)
			}
			if refused.Text != tc.text {
				t.Errorf("the refusal's Text = %q, want %q", refused.Text, tc.text)
			}
			if !strings.Contains(err.Error(), tc.escape) {
				t.Errorf("the message must name the escape %q: %v", tc.escape, err)
			}
			if strings.Contains(err.Error(), "ITERION_AUTHORED") {
				t.Errorf("the refusal shows a marker: %v", err)
			}
			if _, calls := rec.last(t); calls != 0 {
				t.Errorf("the vendor was called %d time(s) with a refused argument", calls)
			}
		})
	}
}

// scalarTransport is a native transport whose one operation declares `arg`
// at whatever type a case needs: the board facade declares strings and
// string lists only, so the native scalar refusals have no facade to read.
// It records the arguments a call received — the store-side oracle.
type scalarTransport struct {
	declared, items string
	calls           int
	got             map[string]any
}

func (s *scalarTransport) Connector() string { return "probescalar" }

func (s *scalarTransport) Package() *spec.Package {
	op := spec.Operation{
		ID: "probescalar.arg.set", Resource: "arg", Verb: "set",
		Deterministic: true, Effect: spec.EffectUpdate, Anonymous: true,
		Params: []spec.Param{{Key: "arg", Name: "arg", In: spec.InQuery, Type: s.declared, Items: s.items, Required: true}},
	}
	return &spec.Package{
		Connector: spec.Connector{
			SchemaVersion: spec.SchemaVersion, ID: "probescalar", Version: "1.0.0",
			Maturity: spec.MaturityQualified,
		},
		Ops: []spec.OpsFile{{
			SchemaVersion: spec.SchemaVersion, Connector: "probescalar", Domain: "arg",
			Operations: []spec.Operation{op},
		}},
	}
}

func (s *scalarTransport) Call(_ context.Context, _ spec.Operation, params map[string]any, _ []string) (any, error) {
	s.calls++
	s.got = params
	return map[string]any{"ok": true}, nil
}

// scalarNativeNode runs one `arg: <value>` param against a native transport
// declaring it with the given type and items.
func scalarNativeNode(t *testing.T, declared, items, value string, opts ...model.ClawExecutorOption) (*scalarTransport, func(map[string]any) error) {
	t.Helper()
	transport := &scalarTransport{declared: declared, items: items}
	refs, err := ir.ParseRefs(value)
	if err != nil {
		t.Fatalf("ParseRefs(%q): %v", value, err)
	}
	node := &ir.ToolNode{
		BaseNode: ir.BaseNode{ID: "n"}, Action: "probescalar.arg.set",
		Params: []ir.ActionParam{{Key: "arg", Value: value, Refs: refs}},
	}
	opts = append(opts, model.WithActionTransports(transport))
	e := model.NewClawExecutor(model.NewRegistry(), &ir.Workflow{}, opts...)
	return transport, func(input map[string]any) error {
		_, err := e.Execute(context.Background(), node, input)
		return err
	}
}

// A native transport never holds a secret's value, so an authored
// `{{secrets.N}}` on an integer, number or boolean field is REFUSED there —
// the placeholder parses as no spelling — while the same authored reference
// reaches the same field over HTTP as its value's spelling. That gap is the
// documented one: both transports narrow the same marker form, and only the
// HTTP leaf resolves it. The placeholder, never the marker, is what the
// refusal quotes.
//
// Mutation seen red (restored): the native transport's leaf resolving
// markers through the params grant like the HTTP one (the store received
// 42424 three times).
func TestANativeActionRefusesASecretOnAScalarField(t *testing.T) {
	t.Parallel()
	ph := secretguard.PlaceholderForName("N")
	for _, tc := range []struct {
		declared, value string
	}{
		{"integer", "42424"},
		{"number", "42.5"},
		{"boolean", "true"},
	} {
		t.Run(tc.declared, func(t *testing.T) {
			t.Parallel()
			// MinLen 2: the guard skips a value shorter than 5 runes, and a
			// boolean's spelling is four — the narrowing, not the length
			// policy, is what is witnessed here.
			cfg := secretguard.DefaultConfig()
			cfg.MinLen = 2
			guard := secretguard.New([]secretguard.Secret{{Name: "N", Value: tc.value}}, cfg)

			transport, runNative := scalarNativeNode(t, tc.declared, "", "{{secrets.N}}", model.WithSecretGuard(guard))
			nerr := runNative(nil)
			var refused *model.ActionParamError
			if !errors.As(nerr, &refused) {
				t.Fatalf("native: want a typed *model.ActionParamError, got %T: %v", nerr, nerr)
			}
			if refused.Declared != tc.declared || refused.Text != ph {
				t.Errorf("native: refusal = {Declared:%q Text:%q}, want {%q %q}", refused.Declared, refused.Text, tc.declared, ph)
			}
			if strings.Contains(nerr.Error(), tc.value) || strings.Contains(nerr.Error(), "ITERION_AUTHORED") {
				t.Errorf("native: the refusal shows the value or a marker: %v", nerr)
			}
			if transport.calls != 0 {
				t.Errorf("native: the transport was called %d time(s) with a refused argument", transport.calls)
			}

			rec, runHTTP := narrowingNode(t, tc.declared, "", "{{secrets.N}}", model.WithSecretGuard(guard))
			if err := runHTTP(nil); err != nil {
				t.Fatalf("http: the authored reference must reach the field as its spelling: %v", err)
			}
			if got, _ := rec.last(t); got != tc.value {
				t.Errorf("http: the vendor received %s, want %s", got, tc.value)
			}
		})
	}
}

// The key refusal is the transports' shared rule: a native transport refuses
// an authored reference in a key exactly as the HTTP call does.
func TestANativeActionRefusesASecretReferenceInAKey(t *testing.T) {
	t.Parallel()
	guard := secretguard.New([]secretguard.Secret{{Name: "K", Value: "keyname-not-real-91"}}, secretguard.DefaultConfig())
	transport, run := scalarNativeNode(t, "object", "", `{"{{secrets.K}}":"v"}`, model.WithSecretGuard(guard))
	err := run(nil)
	var refused *model.ActionParamError
	if !errors.As(err, &refused) {
		t.Fatalf("want a typed *model.ActionParamError, got %T: %v", err, err)
	}
	if refused.SecretKey != secretguard.PlaceholderForName("K") {
		t.Errorf("SecretKey = %q, want the reference's placeholder", refused.SecretKey)
	}
	if transport.calls != 0 {
		t.Errorf("the transport was called %d time(s) with a refused argument", transport.calls)
	}
}

// narrowingNodeFor is narrowingNode with the argument's key named, for the
// unknown-parameter case: the operation declares `body`; the argument goes
// under `zz`.
func narrowingNodeFor(t *testing.T, key, declared, items, value string, opts ...model.ClawExecutorOption) (*bodyRecorder, func(map[string]any) error) {
	t.Helper()
	if key == "body" {
		return narrowingNode(t, declared, items, value, opts...)
	}
	rec := &bodyRecorder{}
	srv := httptest.NewServer(http.HandlerFunc(rec.serve))
	t.Cleanup(srv.Close)

	pkg, op := mutatingPackage(srv.URL)
	op.Params[0].Type = declared
	op.Params[0].Items = items
	pkg.Ops[0].Operations[0] = op

	refs, err := ir.ParseRefs(value)
	if err != nil {
		t.Fatalf("ParseRefs(%q): %v", value, err)
	}
	node := &ir.ToolNode{
		BaseNode: ir.BaseNode{ID: "n"}, Action: "probe.issue.comment", Connection: "main",
		Params: []ir.ActionParam{{Key: key, Value: value, Refs: refs}},
	}
	opts = append(opts, model.WithConnectors(&stubResolver{pkg: pkg, op: op, baseURL: srv.URL}, exec.MarkGuarded(srv.Client())))
	e := model.NewClawExecutor(model.NewRegistry(), &ir.Workflow{}, opts...)
	return rec, func(input map[string]any) error {
		_, err := e.Execute(context.Background(), node, input)
		return err
	}
}
