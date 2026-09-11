package gen_test

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/connector/gen"
	"github.com/SocialGouv/iterion/pkg/connector/spec"
)

// This file covers the EXECUTION PROFILE: everything a package must carry for
// the executor to build the right request and read the answer correctly.
//
// It exists because three of an adversarial review's findings converged on the
// same point — that a reduced model which drops serialization, per-operation
// security and body-level failure signalling produces operations that LOOK
// executable and are not. Slack is the case that makes all three concrete, so
// the fixture below is its shape: form-urlencoded arguments, a credential
// declared as an ordinary parameter, and failure signalled by `ok: false`
// inside a 200.

// slackShapedFixture mirrors the Slack Web API's real Swagger 2.0 shape.
const slackShapedFixture = `{
  "swagger": "2.0",
  "info": {"title": "Probe Web API", "version": "1.0"},
  "host": "probe.example",
  "basePath": "/api",
  "consumes": ["application/x-www-form-urlencoded", "application/json"],
  "securityDefinitions": {
    "slackAuth": {
      "type": "oauth2", "flow": "accessCode",
      "authorizationUrl": "https://probe.example/oauth/authorize",
      "tokenUrl": "https://probe.example/api/oauth.access",
      "scopes": {"chat:write": "Post messages", "channels:read": "List channels", "files:write": "Upload"}
    }
  },
  "paths": {
    "/chat.postMessage": {
      "post": {
        "tags": ["chat"], "operationId": "chat_postMessage", "summary": "Post a message",
        "security": [{"slackAuth": ["chat:write"]}],
        "parameters": [
          {"name": "token", "in": "header", "required": true, "type": "string"},
          {"name": "channel", "in": "formData", "required": true, "type": "string"},
          {"name": "text", "in": "formData", "type": "string"},
          {"name": "attachments", "in": "formData", "type": "array", "items": {"type": "string"}, "collectionFormat": "multi"}
        ],
        "responses": {"200": {"description": "ok"}}
      }
    },
    "/api.test": {
      "get": {
        "tags": ["api"], "operationId": "api_test", "summary": "Public check",
        "security": [],
        "responses": {"200": {"description": "ok"}}
      }
    }
  }
}`

// TestFormEncodedBodyIsRecognised pins the encoding. Slack's whole Web API
// takes form-urlencoded arguments, and a generator that assumed JSON would
// send a body no Slack method reads — a request that is well-formed HTTP and
// wrong in every call.
func TestFormEncodedBodyIsRecognised(t *testing.T) {
	pkg := generate(t, slackShapedFixture)
	op, ok := pkg.Operation("probe.chat.post_message")
	if !ok {
		t.Fatalf("operations = %v", opIDs(pkg))
	}
	if op.HTTP.RequestBody != spec.BodyForm {
		t.Errorf("request body = %q, want form — the vendor lists form-urlencoded first in `consumes`", op.HTTP.RequestBody)
	}
	// A Swagger `formData` parameter is a BODY member: where it goes on the
	// wire is the operation's encoding, not the parameter's own location.
	channel, ok := findParam(op, "channel")
	if !ok {
		t.Fatalf("`channel` missing, got %v", paramNames(op))
	}
	if channel.In != spec.InBody {
		t.Errorf("channel.In = %q, want body", channel.In)
	}
	if !channel.Required {
		t.Error("channel is declared required and must stay so")
	}
	if !op.HasBodyParams() {
		t.Error("the operation must report that it carries a body")
	}
}

// TestSerializationSurvives pins the difference between `a,b` and `a&a=b` on
// the wire. Only one of them is what the vendor parses, and the description
// says which — dropping it is not a documentation loss.
func TestSerializationSurvives(t *testing.T) {
	pkg := generate(t, slackShapedFixture)
	op, _ := pkg.Operation("probe.chat.post_message")
	att, ok := findParam(op, "attachments")
	if !ok {
		t.Fatalf("`attachments` missing, got %v", paramNames(op))
	}
	if att.Style != spec.StyleForm {
		t.Errorf("style = %q, want form (collectionFormat: multi)", att.Style)
	}
	if !att.ExplodeOrDefault() {
		t.Error("collectionFormat `multi` means repeated k=v pairs, so explode must resolve true")
	}
	// A scalar carries no style: there is nothing to serialize, and recording
	// one would add bytes to every parameter of every package for no meaning.
	text, _ := findParam(op, "text")
	if text.Style != "" {
		t.Errorf("a scalar got style %q, want none", text.Style)
	}
}

// TestPerOperationSecurity pins what a binding check needs: which scheme, and
// which scopes THIS operation requires — not every scope the vendor offers.
func TestPerOperationSecurity(t *testing.T) {
	pkg := generate(t, slackShapedFixture)

	scheme, ok := pkg.Connector.AuthScheme("oauth")
	if !ok {
		t.Fatalf("no oauth scheme, got %v", authIDs(pkg))
	}
	if len(scheme.SupportedScopes) != 3 {
		t.Errorf("supported scopes = %v, want the three the vendor advertises", scheme.SupportedScopes)
	}
	// The advertised set must NOT become what a connection requests: that is
	// how an integration that posts one message ends up asking for file
	// upload and channel listing too.
	if len(scheme.DefaultScopes) != 0 {
		t.Errorf("default scopes = %v, want none — what to request is the overlay's decision", scheme.DefaultScopes)
	}

	op, _ := pkg.Operation("probe.chat.post_message")
	if len(op.Security) != 1 || len(op.Security[0].Terms) != 1 {
		t.Fatalf("security = %+v, want one requirement with one term", op.Security)
	}
	term := op.Security[0].Terms[0]
	if term.SchemeID != "oauth" {
		t.Errorf("term scheme = %q, want the DERIVED id, not the vendor's key", term.SchemeID)
	}
	if strings.Join(term.Scopes, ",") != "chat:write" {
		t.Errorf("required scopes = %v, want only chat:write", term.Scopes)
	}
}

// TestSatisfiedBy is the check that keeps a binding from being accepted at
// launch and failing mid-run against the vendor's 403.
func TestSatisfiedBy(t *testing.T) {
	pkg := generate(t, slackShapedFixture)
	op, _ := pkg.Operation("probe.chat.post_message")

	if ok, missing := op.SatisfiedBy("oauth", []string{"chat:write", "channels:read"}); !ok {
		t.Errorf("a connection holding chat:write must satisfy it, missing=%v", missing)
	}
	ok, missing := op.SatisfiedBy("oauth", []string{"channels:read"})
	if ok {
		t.Error("a connection without chat:write must NOT satisfy it")
	}
	if strings.Join(missing, ",") != "chat:write" {
		t.Errorf("missing = %v, want it to name chat:write — a refusal that does not say what is missing is unactionable", missing)
	}
	if ok, _ := op.SatisfiedBy("basic", []string{"chat:write"}); ok {
		t.Error("a different scheme must not satisfy the requirement")
	}
}

// TestExplicitlyAnonymousIsNotAbsentSecurity pins a distinction both formats
// make and a naive reader collapses: `security: []` says "no credential
// needed", while declaring nothing says "use the API's default". Collapsing
// them either demands a credential for a public endpoint or lets a protected
// one through unauthenticated.
func TestExplicitlyAnonymousIsNotAbsentSecurity(t *testing.T) {
	pkg := generate(t, slackShapedFixture)
	pub, ok := pkg.Operation("probe.api.test")
	if !ok {
		t.Fatalf("operations = %v", opIDs(pkg))
	}
	if !pub.Anonymous {
		t.Error("`security: []` must read as explicitly anonymous")
	}
	if got := pkg.EffectiveSecurity(pub); got != nil {
		t.Errorf("an anonymous operation resolves to %v, want no requirement", got)
	}
	if ok, _ := pub.SatisfiedBy("", nil); !ok {
		t.Error("an anonymous operation must be callable with no credential")
	}
}

// TestOutcomePolicyExpressesBodyLevelFailure is F16 made a test. Slack answers
// 200 with `{"ok": false, "error": "missing_scope"}`; a status-only model
// checkpoints that as a success, precisely where a workflow must branch. The
// generator cannot derive the policy (it is prose), but the SCHEMA must be
// able to carry it — which is what this asserts.
func TestOutcomePolicyExpressesBodyLevelFailure(t *testing.T) {
	pkg := generate(t, slackShapedFixture)
	// What an overlay would add.
	pkg.Connector.Outcome = &spec.OutcomePolicy{
		SuccessWhen:       "body.ok == true",
		ErrorCodeField:    "error",
		ErrorMessageField: "error",
		ErrorCodeMap: map[string]spec.ErrorClass{
			"missing_scope":     spec.ErrForbidden,
			"invalid_auth":      spec.ErrUnauthorized,
			"channel_not_found": spec.ErrNotFound,
			"ratelimited":       spec.ErrRateLimited,
		},
	}
	if err := pkg.Validate(); err != nil {
		t.Fatalf("a package with an outcome policy must validate: %v", err)
	}

	op, _ := pkg.Operation("probe.chat.post_message")
	policy := pkg.EffectiveOutcome(op)
	if policy == nil || policy.SuccessWhen == "" {
		t.Fatal("an operation must inherit the connector's outcome policy")
	}

	for code, want := range map[string]spec.ErrorClass{
		"missing_scope": spec.ErrForbidden,
		"invalid_auth":  spec.ErrUnauthorized,
		"ratelimited":   spec.ErrRateLimited,
	} {
		if got := policy.ClassifyCode(code, 200); got != want {
			t.Errorf("vendor code %q in a 200 = %q, want %q", code, got, want)
		}
	}
	// An unmapped code inside a 200 must NOT become a success. The status says
	// nothing (200 classifies to ""), so the fallback has to be a failure or
	// the whole policy leaks exactly the case it exists to catch.
	if got := policy.ClassifyCode("some_new_slack_code", 200); got != spec.ErrBadRequest {
		t.Errorf("an unmapped code = %q, want bad_request — never a success", got)
	}
	if got := policy.ClassifyCode("whatever", 503); got != spec.ErrUpstream {
		t.Errorf("an unmapped code with a 5xx = %q, want upstream", got)
	}
	// Retryability must follow the CLASS, so a `.bot` branching on it and the
	// executor deciding a retry cannot disagree.
	if !spec.ErrRateLimited.Retryable() || spec.ErrForbidden.Retryable() {
		t.Error("retryability must follow the class")
	}
}

// TestOperationOverridesTheConnectorOutcome pins the override direction: an
// API-wide policy is the norm, and one endpoint that behaves differently must
// be able to say so without changing every other operation.
func TestOperationOverridesTheConnectorOutcome(t *testing.T) {
	pkg := generate(t, slackShapedFixture)
	pkg.Connector.Outcome = &spec.OutcomePolicy{SuccessWhen: "body.ok == true"}

	ops := pkg.Ops
	for i := range ops {
		for j := range ops[i].Operations {
			if ops[i].Operations[j].ID == "probe.api.test" {
				ops[i].Operations[j].Outcome = &spec.OutcomePolicy{SuccessWhen: "status == 200"}
			}
		}
	}
	pub, _ := pkg.Operation("probe.api.test")
	if got := pkg.EffectiveOutcome(pub).SuccessWhen; got != "status == 200" {
		t.Errorf("override = %q, want the operation's own", got)
	}
	other, _ := pkg.Operation("probe.chat.post_message")
	if got := pkg.EffectiveOutcome(other).SuccessWhen; got != "body.ok == true" {
		t.Errorf("a non-overriding operation = %q, want the connector's", got)
	}
}

// TestMultipleSuccessVariantsSurvive pins the difference between "done" and
// "accepted, not finished". A workflow that treats a 202 as completion acts on
// work that has not happened.
func TestMultipleSuccessVariantsSurvive(t *testing.T) {
	const body = `{
  "openapi": "3.0.3",
  "info": {"title": "Probe", "version": "1.0"},
  "servers": [{"url": "https://probe.example"}],
  "components": {"securitySchemes": {"b": {"type": "http", "scheme": "bearer"}}},
  "paths": {
    "/exports": {
      "post": {
        "tags": ["export"], "operationId": "exportCreate", "summary": "Start an export",
        "responses": {
          "201": {"description": "created"},
          "202": {"description": "queued"},
          "409": {"description": "already running"}
        }
      }
    }
  }
}`
	pkg := generate(t, body)
	op, ok := pkg.Operation("probe.export.create")
	if !ok {
		t.Fatalf("operations = %v", opIDs(pkg))
	}
	if len(op.Results) != 2 {
		t.Fatalf("results = %+v, want both 201 and 202 kept", op.Results)
	}
	if op.PrimaryResult().Status != 201 {
		t.Errorf("primary = %d, want the lowest success status", op.PrimaryResult().Status)
	}
	if op.Results[0].Pending {
		t.Error("201 is not pending")
	}
	if !op.Results[1].Pending {
		t.Error("202 means accepted-not-finished and must be marked pending")
	}
	if len(op.Errors) != 1 || op.Errors[0].Class != spec.ErrConflict {
		t.Errorf("errors = %+v, want the 409 typed as conflict", op.Errors)
	}
}

// TestUnbuildableBodyBecomesACoverageGap pins the refusal. An operation whose
// body iterion cannot encode must not be published as executable: it would
// drop its payload and report success. It becomes a named gap instead.
func TestUnbuildableBodyBecomesACoverageGap(t *testing.T) {
	const body = `{
  "openapi": "3.0.3",
  "info": {"title": "Probe", "version": "1.0"},
  "servers": [{"url": "https://probe.example"}],
  "components": {"securitySchemes": {"b": {"type": "http", "scheme": "bearer"}}},
  "paths": {
    "/fine": {
      "post": {
        "tags": ["thing"], "operationId": "thingCreateFine", "summary": "JSON body",
        "requestBody": {"content": {"application/json": {"schema": {"type": "object", "properties": {"a": {"type": "string"}}}}}},
        "responses": {"201": {"description": "ok"}}
      }
    },
    "/xml": {
      "post": {
        "tags": ["thing"], "operationId": "thingCreateXML", "summary": "XML body",
        "requestBody": {"content": {"application/xml": {"schema": {"type": "object", "properties": {"a": {"type": "string"}}}}}},
        "responses": {"201": {"description": "ok"}}
      }
    }
  }
}`
	pkg, report, err := gen.Generate([]byte(body), gen.Options{ConnectorID: "probe"})
	if err != nil {
		t.Fatalf("an unbuildable body must not fail the generation: %v", err)
	}
	if _, ok := pkg.Operation("probe.thing.create_fine"); !ok {
		t.Errorf("the JSON operation was lost, got %v", opIDs(pkg))
	}
	if _, ok := pkg.Operation("probe.thing.create_xml"); ok {
		t.Error("an operation whose body iterion cannot encode must not be published")
	}
	if len(report.Skipped) != 1 {
		t.Fatalf("skipped = %+v, want the XML operation named", report.Skipped)
	}
	if !strings.Contains(report.Skipped[0].Reason, "application/xml") {
		t.Errorf("the reason must name the encoding that was refused, got %q", report.Skipped[0].Reason)
	}
}

// TestParamKeysAreUniqueAcrossLocations pins what one flat `params:` map
// needs: an operation legitimately carries `name` in its path and another
// `name` in its body, and a `.bot` must be able to set both.
func TestParamKeysAreUniqueAcrossLocations(t *testing.T) {
	const body = `{
  "swagger": "2.0",
  "info": {"title": "Probe", "version": "1.0"},
  "host": "probe.example",
  "consumes": ["application/json"],
  "securityDefinitions": {"tok": {"type": "apiKey", "name": "X-Token", "in": "header"}},
  "paths": {
    "/things/{name}": {
      "put": {
        "tags": ["thing"], "operationId": "thingRename", "summary": "Rename",
        "parameters": [
          {"name": "name", "in": "path", "required": true, "type": "string"},
          {"name": "body", "in": "body", "schema": {"type": "object", "required": ["name"], "properties": {"name": {"type": "string"}}}}
        ],
        "responses": {"200": {"description": "ok"}}
      }
    }
  }
}`
	pkg := generate(t, body)
	op, ok := pkg.Operation("probe.thing.rename")
	if !ok {
		t.Fatalf("operations = %v", opIDs(pkg))
	}
	if len(op.Params) != 2 {
		t.Fatalf("params = %v, want both the path and the body `name`", paramNames(op))
	}
	keys := map[string]spec.Param{}
	for _, p := range op.Params {
		if _, dup := keys[p.Key]; dup {
			t.Fatalf("two parameters share key %q — one could never be addressed", p.Key)
		}
		keys[p.Key] = p
	}
	// The path keeps the bare name: a `.bot` author reads that one off the URL.
	if p, ok := keys["name"]; !ok || p.In != spec.InPath {
		t.Errorf("key `name` = %+v, want the path parameter", p)
	}
	if p, ok := keys["body_name"]; !ok || p.In != spec.InBody {
		t.Errorf("key `body_name` = %+v, want the body member", p)
	}
	// The WIRE name is untouched by the key disambiguation — the vendor still
	// receives `name` in both places.
	for _, p := range op.Params {
		if p.Name != "name" {
			t.Errorf("wire name = %q, want `name` unchanged by key disambiguation", p.Name)
		}
	}
}
