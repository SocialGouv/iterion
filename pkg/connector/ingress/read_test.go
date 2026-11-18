package ingress

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/connector/spec"
)

// Every signature below comes from outside this package: the vendors' own
// documented test vectors (Slack, GitHub, Jira), or openssl over a body
// written here (Linear, Sentry, Forgejo). None is computed by the code under
// test.

// slackBody is Slack's documented example request (docs.slack.dev,
// "Verifying requests from Slack").
const slackBody = "token=xyzz0WbapA4vBCDEFasx0q6G&team_id=T1DC2JH3J&team_domain=testteamnow&channel_id=G8PSS9T3V&channel_name=foobar&user_id=U2CERLKJA&user_name=roadrunner&command=%2Fwebhook-collect&text=&response_url=https%3A%2F%2Fhooks.slack.com%2Fcommands%2FT1DC2JH3J%2F397700885554%2F96rGlfmibIGlgcZRskXaIFfN&trigger_id=398738663015.47445629121.803a0bc887a14d10d2c447fce8b6703c"

const (
	slackSecret    = "8f742231b10e8888abcd99yyyzzz85a5"
	slackTimestamp = "1531420618"
	slackSignature = "v0=a2114d57b48eac39b9ad189dd8316235a7b4a8d21a10bd27519666489c69b503"
	// The form's legacy verification token: the canary of the redaction.
	slackFormToken = "xyzz0WbapA4vBCDEFasx0q6G"
)

func slackIngress() *spec.Ingress {
	return &spec.Ingress{
		Body: "form",
		Credential: spec.IngressCredential{Scheme: spec.SchemeHMACSHA256, In: "header", Name: "X-Slack-Signature",
			Encoding: "hex", Prefix: "v0=", Signed: "v0:{timestamp}:{body}", Secret: "vendor"},
		Timestamp:    &spec.IngressTimestamp{In: "header", Name: "X-Slack-Request-Timestamp", Unit: "s", Tolerance: "5m"},
		Installation: "payload.team_id",
		Event:        spec.IngressLocator{From: "payload.command"},
		Headers:      []string{"X-Slack-Request-Timestamp"},
		Redact:       []string{"token"},
	}
}

var slackTriggers = []spec.Trigger{{
	ID: "slack.command.invoked", Event: "/webhook-collect",
	Outputs: []spec.TriggerOutput{{Name: "actor", From: "payload.user_name"}, {Name: "thread", From: "payload.channel_id"}},
}}

func slackRequest(mutate func(*Request)) Request {
	h := http.Header{}
	h.Set("X-Slack-Signature", slackSignature)
	h.Set("X-Slack-Request-Timestamp", slackTimestamp)
	req := Request{
		Ingress: slackIngress(), Triggers: slackTriggers, Secret: []byte(slackSecret), Installation: "T1DC2JH3J",
		Body: []byte(slackBody), Header: h, Now: time.Unix(1531420618, 0).Add(30 * time.Second),
	}
	if mutate != nil {
		mutate(&req)
	}
	return req
}

func eventOf(t *testing.T, d Delivery) map[string]any {
	t.Helper()
	var ev map[string]any
	if err := json.Unmarshal(d.Event, &ev); err != nil {
		t.Fatalf("event %s: %v", d.Event, err)
	}
	return ev
}

// Slack's documented request is believed, read as its trigger, and stored
// without the form's own verification token.
func TestReadSlackDocumentedRequest(t *testing.T) {
	d, err := Read(slackRequest(nil))
	if err != nil {
		t.Fatalf("Read(Slack's documented request) = %v", err)
	}
	if d.TriggerID != "slack.command.invoked" || d.EventName != "/webhook-collect" || d.Filtered != "" {
		t.Fatalf("delivery = %+v, want the command trigger", d)
	}
	ev := eventOf(t, d)
	outputs, _ := ev["outputs"].(map[string]any)
	if outputs["actor"] != "roadrunner" || outputs["thread"] != "G8PSS9T3V" {
		t.Fatalf("outputs = %v", outputs)
	}
	if strings.Contains(string(d.Event), slackFormToken) {
		t.Fatalf("the redacted form token reached the event: %s", d.Event)
	}
	if strings.Contains(string(d.Event), strings.TrimPrefix(slackSignature, "v0=")) {
		t.Fatalf("the signature reached the event: %s", d.Event)
	}
}

// Each way a Slack delivery fails to prove its sender is a 401-class refusal,
// one at a time.
func TestReadSlackRefusals(t *testing.T) {
	for name, mutate := range map[string]func(*Request){
		"wrong secret":            func(r *Request) { r.Secret = []byte("not-the-signing-secret") },
		"signature missing":       func(r *Request) { r.Header.Del("X-Slack-Signature") },
		"signature repeated":      func(r *Request) { r.Header.Add("X-Slack-Signature", slackSignature) },
		"body changed":            func(r *Request) { r.Body = append(append([]byte(nil), r.Body...), "&x=1"...) },
		"timestamp missing":       func(r *Request) { r.Header.Del("X-Slack-Request-Timestamp") },
		"timestamp too old":       func(r *Request) { r.Now = time.Unix(1531420618, 0).Add(6 * time.Minute) },
		"timestamp in the future": func(r *Request) { r.Now = time.Unix(1531420618, 0).Add(-6 * time.Minute) },
		"another installation":    func(r *Request) { r.Installation = "T0THER" },
		"no installation bound":   func(r *Request) { r.Installation = "" },
		"no secret held":          func(r *Request) { r.Secret = nil },
		// Without a secret, a forger signs with the empty key: the same
		// HMAC the lane would compute. Signed with crypto/hmac here.
		"empty-key forgery": func(r *Request) {
			r.Secret = nil
			mac := hmac.New(sha256.New, nil)
			mac.Write([]byte("v0:" + slackTimestamp + ":" + slackBody))
			r.Header.Set("X-Slack-Signature", "v0="+hex.EncodeToString(mac.Sum(nil)))
		},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Read(slackRequest(mutate)); !errors.Is(err, ErrUnauthenticated) {
				t.Fatalf("Read = %v, want ErrUnauthenticated", err)
			}
		})
	}
}

// GitHub's and Jira's documented vectors pass the HMAC check over their raw
// bytes — the body is no JSON object, so the authenticated delivery is then
// malformed, never unauthenticated — and fail it under another secret.
func TestReadDocumentedHMACVectors(t *testing.T) {
	for name, c := range map[string]struct {
		header, prefix, body, signature string
	}{
		"github (Forgejo's scheme)": {"X-Forgejo-Signature", "", "Hello, World!", "757107ea0eb2509fc211221cce984b8a37570b6d7586c22c46f4379c8b043e17"},
		"jira":                      {"X-Hub-Signature", "sha256=", "Hello World!", "sha256=a4771c39fbe90f317c7824e83ddef3caae9cb3d976c214ace1f2937e133263c9"},
	} {
		t.Run(name, func(t *testing.T) {
			in := &spec.Ingress{
				Credential: spec.IngressCredential{Scheme: spec.SchemeHMACSHA256, In: "header", Name: c.header, Prefix: c.prefix, Secret: "minted"},
				Event:      spec.IngressLocator{Header: "X-Event"},
			}
			req := func(secret string) Request {
				h := http.Header{}
				h.Set(c.header, c.signature)
				return Request{Ingress: in, Secret: []byte(secret), Body: []byte(c.body), Header: h, Now: time.Now()}
			}
			if _, err := Read(req("It's a Secret to Everybody")); !errors.Is(err, ErrMalformed) || errors.Is(err, ErrUnauthenticated) {
				t.Fatalf("the documented vector = %v, want authenticated then malformed", err)
			}
			if _, err := Read(req("another secret")); !errors.Is(err, ErrUnauthenticated) {
				t.Fatalf("the vector under another secret = %v, want ErrUnauthenticated", err)
			}
		})
	}
}

const (
	linearBody = `{"action":"create","type":"Issue","data":{"id":"abc","identifier":"ENG-42","title":"Fix it"},"webhookTimestamp":1763190000000}`
	linearSig  = "a2261165d4ebac1b9402295d43f614b67a41342338e34963af45ba91c7a7edcb"
)

// Linear signs its body, and its timestamp lives IN the body, in
// milliseconds: the window reads that unit.
func TestReadLinearBodyTimestampInMilliseconds(t *testing.T) {
	in := &spec.Ingress{
		Credential: spec.IngressCredential{Scheme: spec.SchemeHMACSHA256, In: "header", Name: "Linear-Signature", Secret: "minted"},
		Timestamp:  &spec.IngressTimestamp{In: "body", Name: "webhookTimestamp", Unit: "ms", Tolerance: "60s"},
		Event:      spec.IngressLocator{From: `payload.type + "." + payload.action`},
	}
	trig := []spec.Trigger{{ID: "linear.issue.created", Event: "Issue.create", Outputs: []spec.TriggerOutput{{Name: "subject", From: "payload.data.identifier"}}}}
	sent := time.UnixMilli(1763190000000)
	req := func(now time.Time, unit string) Request {
		in := *in
		ts := *in.Timestamp
		ts.Unit = unit
		in.Timestamp = &ts
		h := http.Header{}
		h.Set("Linear-Signature", linearSig)
		return Request{Ingress: &in, Triggers: trig, Secret: []byte("linear-secret-xyz"), Body: []byte(linearBody), Header: h, Now: now}
	}
	d, err := Read(req(sent.Add(10*time.Second), "ms"))
	if err != nil || d.TriggerID != "linear.issue.created" {
		t.Fatalf("Read = (%+v, %v)", d, err)
	}
	if out := eventOf(t, d)["outputs"].(map[string]any); out["subject"] != "ENG-42" {
		t.Fatalf("outputs = %v", out)
	}
	if _, err := Read(req(sent.Add(2*time.Minute), "ms")); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("two minutes late = %v, want ErrUnauthenticated", err)
	}
	if _, err := Read(req(sent.Add(10*time.Second), "s")); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("the same instant read in seconds = %v, want it out of the window", err)
	}
}

const (
	sentryBody = `{"action":"created","data":{"issue":{"id":"123","shortId":"PROJ-1","title":"boom"}},"installation":{"uuid":"inst-1"}}`
	sentrySig  = "938ad0ee61e94c388c42ad6fafe7ba0544ea6facc5cc679b318a318c393b1a01"
)

// A delivery signed by the right secret for another installation is refused:
// the URL says which ingress was hit, only the signed body whose event it is.
func TestReadSentryInstallation(t *testing.T) {
	in := &spec.Ingress{
		Credential:   spec.IngressCredential{Scheme: spec.SchemeHMACSHA256, In: "header", Name: "Sentry-Hook-Signature", Secret: "vendor"},
		Installation: "payload.installation.uuid",
		Event:        spec.IngressLocator{From: "payload.action"},
	}
	req := func(bound string) Request {
		h := http.Header{}
		h.Set("Sentry-Hook-Signature", sentrySig)
		return Request{Ingress: in, Triggers: []spec.Trigger{{ID: "sentry.issue.created", Event: "created"}},
			Secret: []byte("sentry-secret"), Installation: bound, Body: []byte(sentryBody), Header: h, Now: time.Now()}
	}
	if d, err := Read(req("inst-1")); err != nil || d.TriggerID != "sentry.issue.created" {
		t.Fatalf("the bound installation = (%+v, %v)", d, err)
	}
	if _, err := Read(req("inst-2")); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("another installation = %v, want ErrUnauthenticated", err)
	}
	// An ingress bound to no value believes nothing — not even a delivery
	// whose installation is empty too. Signed with crypto/hmac here.
	empty := `{"action":"created","installation":{"uuid":""}}`
	mac := hmac.New(sha256.New, []byte("sentry-secret"))
	mac.Write([]byte(empty))
	r := req("")
	r.Body = []byte(empty)
	r.Header.Set("Sentry-Hook-Signature", hex.EncodeToString(mac.Sum(nil)))
	if _, err := Read(r); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("an empty installation against an empty binding = %v, want ErrUnauthenticated", err)
	}
}

const (
	forgejoBody = `{"action":"created","comment":{"body":"hello","user":{"login":"alice"}},"issue":{"number":7}}`
	forgejoSig  = "7fe41ea77a319679ce7e201bd1c0b1c9299c966f7e543ededb297f9e221d2728"
	canary      = "canary-7f3a9c"
)

func forgejoRequest(mutate func(*Request)) Request {
	h := http.Header{}
	h.Set("X-Forgejo-Signature", forgejoSig)
	h.Set("X-Forgejo-Event", "issue_comment")
	h.Set("X-Forgejo-Delivery", "d-1")
	h.Set("X-Canary", canary)
	h.Set("Authorization", "Bearer "+canary)
	req := Request{
		Ingress: &spec.Ingress{
			Credential: spec.IngressCredential{Scheme: spec.SchemeHMACSHA256, In: "header", Name: "X-Forgejo-Signature", Secret: "minted"},
			Event:      spec.IngressLocator{Header: "X-Forgejo-Event"},
			DeliveryID: &spec.IngressLocator{Header: "X-Forgejo-Delivery"},
			Headers:    []string{"X-Forgejo-Event", "X-Forgejo-Delivery"},
		},
		Triggers: []spec.Trigger{{ID: "forgejo.comment.created", Event: "issue_comment", Outputs: []spec.TriggerOutput{
			{Name: "actor", From: "payload.comment.user.login"},
			{Name: "subject", From: "payload.issue.number", Type: "int"},
			// An output naming an unlisted header reads nothing.
			{Name: "leak", From: `headers["X-Canary"]`},
		}}},
		Secret: []byte("forgejo-secret"), Body: []byte(forgejoBody), Header: h, Now: time.Now(),
	}
	if mutate != nil {
		mutate(&req)
	}
	return req
}

// The canary (ADR-168 "The canary"): a value in the credential header, in an
// unlisted header and in Authorization reaches no expression and no event.
func TestReadTheCanaryReachesNothing(t *testing.T) {
	d, err := Read(forgejoRequest(nil))
	if err != nil {
		t.Fatalf("Read = %v", err)
	}
	ev := eventOf(t, d)
	outputs := ev["outputs"].(map[string]any)
	if outputs["actor"] != "alice" || outputs["subject"] != float64(7) {
		t.Fatalf("outputs = %v", outputs)
	}
	if _, ok := outputs["leak"]; ok {
		t.Fatalf("an output read an unlisted header: %v", outputs)
	}
	for _, secret := range []string{canary, forgejoSig} {
		if strings.Contains(string(d.Event), secret) {
			t.Fatalf("%q reached the event: %s", secret, d.Event)
		}
	}
	if h := ev["headers"].(map[string]any); len(h) != 2 || h["X-Forgejo-Event"] != "issue_comment" {
		t.Fatalf("event headers = %v, want the two listed ones", h)
	}
	if d.DeliveryID != "d-1" {
		t.Fatalf("delivery id = %q", d.DeliveryID)
	}
}

// A package that lists a credential header — refused by its validation, read
// here without it — still hands none to an expression or to the event.
func TestReadAListedCredentialHeaderStaysUnreadable(t *testing.T) {
	d, err := Read(forgejoRequest(func(r *Request) {
		r.Ingress.Headers = append(r.Ingress.Headers, "X-Forgejo-Signature", "authorization")
		r.Triggers[0].Outputs = append(r.Triggers[0].Outputs,
			spec.TriggerOutput{Name: "sig", From: `headers["X-Forgejo-Signature"]`},
			spec.TriggerOutput{Name: "auth", From: `headers["Authorization"]`})
	}))
	if err != nil {
		t.Fatalf("Read = %v", err)
	}
	for _, secret := range []string{canary, forgejoSig} {
		if strings.Contains(string(d.Event), secret) {
			t.Fatalf("%q reached the event: %s", secret, d.Event)
		}
	}
	outputs := eventOf(t, d)["outputs"].(map[string]any)
	if _, ok := outputs["sig"]; ok {
		t.Fatalf("an output read the credential header: %v", outputs)
	}
	if _, ok := outputs["auth"]; ok {
		t.Fatalf("an output read Authorization: %v", outputs)
	}
}

// The dedup identity is the signed material: a vendor's delivery-id header is
// unsigned, so changing it on a captured request opens no second intake.
func TestReadDedupIgnoresTheUnsignedDeliveryID(t *testing.T) {
	a, err := Read(forgejoRequest(nil))
	if err != nil {
		t.Fatal(err)
	}
	b, err := Read(forgejoRequest(func(r *Request) { r.Header.Set("X-Forgejo-Delivery", "d-forged") }))
	if err != nil {
		t.Fatal(err)
	}
	if a.DedupKey != b.DedupKey || b.DeliveryID != "d-forged" {
		t.Fatalf("dedup keys %s / %s (delivery id %q): want one identity for one signed body", a.DedupKey, b.DedupKey, b.DeliveryID)
	}
	// A timestamp signed apart from the body is not part of the identity:
	// Slack re-signs a retry at its own instant, and the retry is the same
	// delivery. The re-signature is computed with crypto/hmac, not by the
	// code under test.
	s1, err := Read(slackRequest(nil))
	if err != nil {
		t.Fatal(err)
	}
	retry, err := Read(slackRequest(func(r *Request) {
		const ts = "1531420648"
		mac := hmac.New(sha256.New, []byte(slackSecret))
		mac.Write([]byte("v0:" + ts + ":" + slackBody))
		r.Header.Set("X-Slack-Request-Timestamp", ts)
		r.Header.Set("X-Slack-Signature", "v0="+hex.EncodeToString(mac.Sum(nil)))
	}))
	if err != nil {
		t.Fatalf("the re-signed retry = %v", err)
	}
	body := sha256.Sum256([]byte(slackBody))
	if s1.DedupKey != retry.DedupKey || s1.DedupKey != hex.EncodeToString(body[:]) {
		t.Fatalf("dedup keys %s / %s, want both the body's digest %x: a re-signed retry is the same delivery", s1.DedupKey, retry.DedupKey, body)
	}
}

// A token in the body (Mattermost's outgoing webhooks) proves the sender and is
// removed from the event; a token that is missing, repeated or nested is not.
func TestReadTokenInTheBody(t *testing.T) {
	in := &spec.Ingress{
		Body:       "form",
		Credential: spec.IngressCredential{Scheme: spec.SchemeToken, In: "body", Name: "token", Secret: "vendor"},
		Event:      spec.IngressLocator{From: "payload.trigger_word"},
	}
	trig := []spec.Trigger{{ID: "mattermost.message.posted", Event: "!iterion"}}
	read := func(body string) (Delivery, error) {
		return Read(Request{Ingress: in, Triggers: trig, Secret: []byte(canary), Body: []byte(body), Header: http.Header{}, Now: time.Now()})
	}
	d, err := read("token=" + canary + "&trigger_word=%21iterion&text=hi")
	if err != nil || d.TriggerID != "mattermost.message.posted" {
		t.Fatalf("Read = (%+v, %v)", d, err)
	}
	if strings.Contains(string(d.Event), canary) {
		t.Fatalf("the body token reached the event: %s", d.Event)
	}
	for name, c := range map[string][2]string{
		"missing":  {"trigger_word=%21iterion", "missing"},
		"repeated": {"token=" + canary + "&token=" + canary + "&trigger_word=%21iterion", "not a scalar"},
		"wrong":    {"token=nope&trigger_word=%21iterion", "does not match"},
	} {
		// Read by its words: a repeated value is refused as such, not
		// left to fail a comparison it would never pass.
		if _, err := read(c[0]); !errors.Is(err, ErrUnauthenticated) || !strings.Contains(err.Error(), c[1]) {
			t.Errorf("%s token = %v, want ErrUnauthenticated saying %q", name, err, c[1])
		}
	}
	jsonIn := *in
	jsonIn.Body = "json"
	if _, err := Read(Request{Ingress: &jsonIn, Triggers: trig, Secret: []byte(canary),
		Body: []byte(`{"token":"other","token":"` + canary + `","trigger_word":"!iterion"}`), Header: http.Header{}, Now: time.Now()}); !errors.Is(err, ErrUnauthenticated) || !strings.Contains(err.Error(), "repeated") {
		t.Errorf("a JSON body repeating its token key = %v, want ErrUnauthenticated saying it is repeated", err)
	}
	if _, err := Read(Request{Ingress: &jsonIn, Triggers: trig, Secret: []byte(canary),
		Body: []byte(`{"token":{"v":"` + canary + `"},"trigger_word":"!iterion"}`), Header: http.Header{}, Now: time.Now()}); !errors.Is(err, ErrUnauthenticated) || !strings.Contains(err.Error(), "not a scalar") {
		t.Errorf("a nested token = %v, want ErrUnauthenticated saying it is not a scalar", err)
	}
}

// Bearer and basic credentials travel in Authorization, compared in constant
// time; another value, or another scheme's spelling, is refused.
func TestReadBearerAndBasic(t *testing.T) {
	read := func(scheme, authorization string) error {
		in := &spec.Ingress{Credential: spec.IngressCredential{Scheme: scheme, Secret: "minted"}, Event: spec.IngressLocator{From: "payload.kind"}}
		h := http.Header{}
		if authorization != "" {
			h.Set("Authorization", authorization)
		}
		_, err := Read(Request{Ingress: in, Triggers: []spec.Trigger{{ID: "am.alert.firing", Event: "alert"}},
			Secret: []byte("user:s3cret"), Body: []byte(`{"kind":"alert"}`), Header: h, Now: time.Now()})
		return err
	}
	basic := "Basic " + base64.StdEncoding.EncodeToString([]byte("user:s3cret"))
	if err := read(spec.SchemeBearer, "Bearer user:s3cret"); err != nil {
		t.Fatalf("bearer = %v", err)
	}
	if err := read(spec.SchemeBasic, basic); err != nil {
		t.Fatalf("basic = %v", err)
	}
	for name, c := range map[string][2]string{
		"bearer, wrong value":    {spec.SchemeBearer, "Bearer nope"},
		"bearer, basic spelling": {spec.SchemeBearer, basic},
		"bearer, missing":        {spec.SchemeBearer, ""},
		// Another scheme of the bearer prefix's length: the prefix is read,
		// not skipped.
		"bearer, another scheme": {spec.SchemeBearer, "Token: user:s3cret"},
		"basic, wrong value":     {spec.SchemeBasic, "Basic " + base64.StdEncoding.EncodeToString([]byte("user:nope"))},
		"basic, bearer spelling": {spec.SchemeBasic, "Bearer user:s3cret"},
		"basic, not base64":      {spec.SchemeBasic, "Basic !!!"},
	} {
		if err := read(c[0], c[1]); !errors.Is(err, ErrUnauthenticated) {
			t.Errorf("%s = %v, want ErrUnauthenticated", name, err)
		}
	}
}

// An authenticated delivery answers a handshake and reads no trigger; an
// event no trigger declares, and one its loop guard drops, are answered and
// filtered.
func TestReadHandshakeAndFilters(t *testing.T) {
	in := &spec.Ingress{
		Credential: spec.IngressCredential{Scheme: spec.SchemeToken, In: "query", Name: "t", Secret: "minted"},
		Event:      spec.IngressLocator{From: "payload.type"},
		Handshake:  &spec.IngressHandshake{When: `payload.type == "url_verification"`, Respond: "payload.challenge"},
	}
	trig := []spec.Trigger{{ID: "chat.message.posted", Event: "message", Ignore: `payload.bot == true`}}
	read := func(body string) (Delivery, error) {
		return Read(Request{Ingress: in, Triggers: trig, Secret: []byte("tok"), Body: []byte(body),
			Header: http.Header{}, Query: url.Values{"t": {"tok"}}, Now: time.Now()})
	}
	d, err := read(`{"type":"url_verification","challenge":"c-123"}`)
	if err != nil || d.Handshake == nil || *d.Handshake != "c-123" || d.TriggerID != "" {
		t.Fatalf("handshake = (%+v, %v)", d, err)
	}
	if d, err := read(`{"type":"reaction"}`); err != nil || d.Filtered == "" || d.TriggerID != "" {
		t.Fatalf("an undeclared event = (%+v, %v), want filtered", d, err)
	}
	if d, err := read(`{"type":"message","bot":true}`); err != nil || d.Filtered == "" || d.TriggerID != "chat.message.posted" {
		t.Fatalf("a bot's own message = (%+v, %v), want dropped by the loop guard", d, err)
	}
	if d, err := read(`{"type":"message","bot":false}`); err != nil || d.Filtered != "" || d.Event == nil {
		t.Fatalf("a message = (%+v, %v), want it read", d, err)
	}
	// A body of two JSON values is two deliveries to two parsers.
	if _, err := read(`{"type":"message","bot":false}{"type":"message","bot":true}`); !errors.Is(err, ErrMalformed) {
		t.Fatalf("two JSON values = %v, want ErrMalformed", err)
	}
	// The declared ack keeps its body; a status left unstated is 200.
	in.Ack = &spec.IngressAck{Body: "ok"}
	if d, err := read(`{"type":"message","bot":false}`); err != nil || d.Ack.Status != http.StatusOK || d.Ack.Body != "ok" {
		t.Fatalf("an ack without a status = (%+v, %v), want 200 and its body", d.Ack, err)
	}
	if _, err := Read(Request{Ingress: in, Triggers: trig, Secret: []byte("tok"), Body: []byte(`{"type":"message"}`),
		Header: http.Header{}, Query: url.Values{"t": {"nope"}}, Now: time.Now()}); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("a wrong query token = %v, want ErrUnauthenticated", err)
	}
}

// A body that names one key twice reads as two different deliveries to two
// parsers: refused, at any depth.
func TestReadRefusesRepeatedKeys(t *testing.T) {
	if err := refuseRepeatedKeys([]byte(`{"a":{"b":1,"b":2}}`)); !errors.Is(err, ErrMalformed) {
		t.Fatalf("a nested repeated key = %v, want ErrMalformed", err)
	}
	if err := refuseRepeatedKeys([]byte(`{"a":[{"b":1},{"b":2}],"c":{"b":3}}`)); err != nil {
		t.Fatalf("the same key in sibling objects = %v, want accepted", err)
	}
	deep := strings.Repeat("[", maxDepth+2) + strings.Repeat("]", maxDepth+2)
	if err := refuseRepeatedKeys([]byte(deep)); !errors.Is(err, ErrMalformed) {
		t.Fatalf("a body nested past the bound = %v, want ErrMalformed", err)
	}
}

// --- the first adversarial round's witnesses ---

// linearIngress keeps its timestamp in the body, signed by the body itself.
func linearIngress() *spec.Ingress {
	return &spec.Ingress{
		Credential: spec.IngressCredential{Scheme: spec.SchemeHMACSHA256, In: "header", Name: "Linear-Signature", Encoding: "hex", Secret: "minted"},
		Timestamp:  &spec.IngressTimestamp{In: "body", Name: "webhookTimestamp", Unit: "ms", Tolerance: "60s"},
		Event:      spec.IngressLocator{From: "payload.type"},
	}
}

// allocatedBy measures the bytes f allocates.
func allocatedBy(f func()) uint64 {
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	f()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}

// Before the proof nothing decodes the body into a tree: an unsigned 4 MiB
// body costs a scan, whether the timestamp or the credential lives in it.
// Decoded first, the same body allocated ~70 times its size.
func TestReadAnUnauthenticatedBodyIsNeverDecodedIntoATree(t *testing.T) {
	big := []byte(`{"a":[` + strings.Repeat("1.5,", 1<<20) + `1.5],"webhookTimestamp":1,"token":"x"}`)
	bound := uint64(8 * len(big))
	h := http.Header{}
	h.Set("Linear-Signature", strings.Repeat("0", 64))
	if got := allocatedBy(func() {
		if _, err := Read(Request{Ingress: linearIngress(), Secret: []byte("s"), Body: big, Header: h, Now: time.Now()}); !errors.Is(err, ErrUnauthenticated) {
			t.Errorf("a forged Linear delivery = %v, want ErrUnauthenticated", err)
		}
	}); got > bound {
		t.Errorf("a body timestamp: an unsigned %d-byte body allocated %d bytes before its refusal (bound %d)", len(big), got, bound)
	}
	token := &spec.Ingress{Credential: spec.IngressCredential{Scheme: spec.SchemeToken, In: "body", Name: "token", Secret: "vendor"},
		Event: spec.IngressLocator{From: "payload.type"}}
	if got := allocatedBy(func() {
		if _, err := Read(Request{Ingress: token, Secret: []byte("the-token"), Body: big, Header: http.Header{}, Now: time.Now()}); !errors.Is(err, ErrUnauthenticated) {
			t.Errorf("a wrong body token = %v, want ErrUnauthenticated", err)
		}
	}); got > bound {
		t.Errorf("a body credential: an unsigned %d-byte body allocated %d bytes before its refusal (bound %d)", len(big), got, bound)
	}
}

// Slack's documented url_verification (docs.slack.dev, Events API "Request
// URL" verification) carries no team_id: an ingress that reads the
// installation still answers it. Signed here with crypto/hmac.
func TestReadSlackURLVerificationAnswersBeforeTheInstallation(t *testing.T) {
	const body = `{"token":"Jhj5dZrVaK7ZwHHjRyZWjbDl","challenge":"3eZbrw1aBm2rZgRNFdxV2595E9CY3gmdALWMmHkvFXO7tYXAYM8P","type":"url_verification"}`
	in := &spec.Ingress{
		Credential: spec.IngressCredential{Scheme: spec.SchemeHMACSHA256, In: "header", Name: "X-Slack-Signature",
			Encoding: "hex", Prefix: "v0=", Signed: "v0:{timestamp}:{body}", Secret: "vendor"},
		Timestamp:    &spec.IngressTimestamp{In: "header", Name: "X-Slack-Request-Timestamp", Unit: "s", Tolerance: "5m"},
		Installation: "payload.team_id",
		Event:        spec.IngressLocator{From: "payload.event.type"},
		Redact:       []string{"token"},
		Handshake:    &spec.IngressHandshake{When: `payload.type == "url_verification"`, Respond: "payload.challenge"},
	}
	mac := hmac.New(sha256.New, []byte(slackSecret))
	mac.Write([]byte("v0:" + slackTimestamp + ":" + body))
	h := http.Header{}
	h.Set("X-Slack-Request-Timestamp", slackTimestamp)
	h.Set("X-Slack-Signature", "v0="+hex.EncodeToString(mac.Sum(nil)))
	d, err := Read(Request{Ingress: in, Secret: []byte(slackSecret), Installation: "T1DC2JH3J", Body: []byte(body), Header: h,
		Now: time.Unix(1531420618, 0)})
	if err != nil || d.Handshake == nil || *d.Handshake != "3eZbrw1aBm2rZgRNFdxV2595E9CY3gmdALWMmHkvFXO7tYXAYM8P" {
		t.Fatalf("Slack's url_verification = (%+v, %v), want its challenge answered", d, err)
	}
}

// A body credential named with a dot is read by its literal key and removed
// by that same key.
func TestReadADottedBodyCredentialNeverReachesTheEvent(t *testing.T) {
	in := &spec.Ingress{Credential: spec.IngressCredential{Scheme: spec.SchemeToken, In: "body", Name: "auth.token", Secret: "vendor"},
		Event: spec.IngressLocator{From: "payload.kind"}}
	for _, c := range []struct{ encoding, body string }{
		{"json", `{"auth.token":"` + canary + `","kind":"msg"}`},
		{"form", "auth.token=" + canary + "&kind=msg"},
	} {
		in.Body = c.encoding
		d, err := Read(Request{Ingress: in, Triggers: []spec.Trigger{{ID: "t", Event: "msg", Outputs: []spec.TriggerOutput{{Name: "kind", From: "payload.kind"}}}},
			Secret: []byte(canary), Body: []byte(c.body), Header: http.Header{}, Now: time.Now()})
		if err != nil {
			t.Fatalf("%s: Read = %v", c.encoding, err)
		}
		if strings.Contains(string(d.Event), canary) {
			t.Fatalf("%s: the credential reached the event: %s", c.encoding, d.Event)
		}
	}
}

// A header locator never reads a header that carries a credential: the event
// name and the delivery id are logged and stored.
func TestReadAHeaderLocatorNeverReadsACredential(t *testing.T) {
	for _, header := range []string{"Authorization", "Cookie", "X-Forgejo-Signature"} {
		d, err := Read(forgejoRequest(func(r *Request) {
			r.Header.Set("Cookie", "sid="+canary)
			r.Ingress.DeliveryID = &spec.IngressLocator{Header: header}
		}))
		if err != nil {
			t.Fatalf("delivery_id from %s: Read = %v", header, err)
		}
		if d.DeliveryID != "" {
			t.Fatalf("delivery_id read %s: %q", header, d.DeliveryID)
		}
		_, err = Read(forgejoRequest(func(r *Request) {
			r.Header.Set("Cookie", "sid="+canary)
			r.Ingress.Event = spec.IngressLocator{Header: header}
		}))
		if !errors.Is(err, ErrMalformed) || strings.Contains(err.Error(), canary) || strings.Contains(err.Error(), forgejoSig) {
			t.Fatalf("event from %s = %v, want ErrMalformed naming no credential", header, err)
		}
	}
}

// What a delivery carries reaches an error — and so the log — bounded.
func TestReadErrorsQuoteTheDeliveryBounded(t *testing.T) {
	long := strings.Repeat("\xff", 1<<16)
	cases := map[string]Request{
		"a repeated long key": {Ingress: &spec.Ingress{Credential: spec.IngressCredential{Scheme: spec.SchemeToken, In: "query", Name: "t", Secret: "minted"},
			Event: spec.IngressLocator{From: "payload.type"}},
			Secret: []byte("tok"), Query: url.Values{"t": {"tok"}}, Header: http.Header{},
			Body: []byte(`{"` + long + `":1,"` + long + `":1}`)},
		"a long timestamp": slackRequest(func(r *Request) { r.Header.Set("X-Slack-Request-Timestamp", long) }),
		"a long number": {Ingress: &spec.Ingress{Credential: spec.IngressCredential{Scheme: spec.SchemeToken, In: "query", Name: "t", Secret: "minted"},
			Event: spec.IngressLocator{From: "payload.type"}},
			Secret: []byte("tok"), Query: url.Values{"t": {"tok"}}, Header: http.Header{},
			Body: []byte(`{"type":` + strings.Repeat("9", 1<<16) + `x}`)},
	}
	for name, req := range cases {
		if req.Now.IsZero() {
			req.Now = time.Now()
		}
		_, err := Read(req)
		if err == nil || len(err.Error()) > 1024 {
			t.Errorf("%s: an error of %d bytes (%v), want one bounded under 1 KiB", name, len(fmt.Sprint(err)), err == nil)
		}
	}
}

// The event is stored as written — not HTML-escaped six bytes per "<" — and
// one past MaxEventBytes is refused, never stored to fail its row.
func TestReadTheEventIsEncodedPlainAndBounded(t *testing.T) {
	in := &spec.Ingress{Credential: spec.IngressCredential{Scheme: spec.SchemeToken, In: "query", Name: "t", Secret: "minted"},
		Event: spec.IngressLocator{From: "payload.type"}}
	read := func(body string, outputs int) (Delivery, error) {
		trig := spec.Trigger{ID: "t", Event: "x"}
		for i := 0; i < outputs; i++ {
			trig.Outputs = append(trig.Outputs, spec.TriggerOutput{Name: fmt.Sprintf("o%d", i), From: "payload.v"})
		}
		return Read(Request{Ingress: in, Triggers: []spec.Trigger{trig}, Secret: []byte("tok"), Query: url.Values{"t": {"tok"}},
			Header: http.Header{}, Body: []byte(body), Now: time.Now()})
	}
	d, err := read(`{"type":"x","v":"<a&b>"}`, 1)
	if err != nil || !strings.Contains(string(d.Event), `"<a&b>"`) {
		t.Fatalf("event = %s (%v), want the value as written", d.Event, err)
	}
	if _, err := read(`{"type":"x","v":"`+strings.Repeat("a", 4<<20)+`"}`, 3); !errors.Is(err, ErrEventTooLarge) {
		t.Fatalf("a 16 MiB event = %v, want ErrEventTooLarge", err)
	}
}

// Nothing may follow the body's one JSON value — not a stray closer either.
func TestReadNothingFollowsTheBody(t *testing.T) {
	in := &spec.Ingress{Credential: spec.IngressCredential{Scheme: spec.SchemeToken, In: "query", Name: "t", Secret: "minted"},
		Event: spec.IngressLocator{From: "payload.type"}}
	for _, body := range []string{`{"type":"x"}]{"type":"y"}`, `{"type":"x"}}garbage`, `{"type":"x"} {"type":"y"}`} {
		if _, err := Read(Request{Ingress: in, Triggers: []spec.Trigger{{ID: "t", Event: "x"}}, Secret: []byte("tok"),
			Query: url.Values{"t": {"tok"}}, Header: http.Header{}, Body: []byte(body), Now: time.Now()}); !errors.Is(err, ErrMalformed) {
			t.Errorf("%s = %v, want ErrMalformed", body, err)
		}
	}
}

// The installation is the signed body's word: a header naming one is never
// read, and an id past int64 keeps its literal.
func TestReadTheInstallationIsTheSignedBodysWord(t *testing.T) {
	read := func(installation, body, bound string, h http.Header) error {
		in := &spec.Ingress{Credential: spec.IngressCredential{Scheme: spec.SchemeToken, In: "query", Name: "t", Secret: "minted"},
			Installation: installation, Event: spec.IngressLocator{From: "payload.type"}, Headers: []string{"X-Team"}}
		_, err := Read(Request{Ingress: in, Triggers: []spec.Trigger{{ID: "t", Event: "x"}}, Secret: []byte("tok"), Installation: bound,
			Query: url.Values{"t": {"tok"}}, Header: h, Body: []byte(body), Now: time.Now()})
		return err
	}
	h := http.Header{}
	h.Set("X-Team", "T-VICTIM")
	if err := read(`headers["X-Team"]`, `{"type":"x"}`, "T-VICTIM", h); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("an installation read from a header = %v, want ErrUnauthenticated", err)
	}
	const id = `{"type":"x","inst":18446744073709551615}`
	if err := read("payload.inst", id, "18446744073709551615", http.Header{}); err != nil {
		t.Fatalf("the exact id past int64 = %v", err)
	}
	if err := read("payload.inst", `{"type":"x","inst":18446744073709551614}`, "18446744073709551615", http.Header{}); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("a neighbouring id past int64 = %v, want ErrUnauthenticated", err)
	}
}

// A redact path indexes lists as an expression does.
func TestReadARedactPathIndexesLists(t *testing.T) {
	in := &spec.Ingress{Credential: spec.IngressCredential{Scheme: spec.SchemeToken, In: "query", Name: "t", Secret: "minted"},
		Event: spec.IngressLocator{From: "payload.type"}, Redact: []string{"items.0.secret"}}
	d, err := Read(Request{Ingress: in, Triggers: []spec.Trigger{{ID: "t", Event: "x", Outputs: []spec.TriggerOutput{{Name: "leak", From: "payload.items[0].secret"}}}},
		Secret: []byte("tok"), Query: url.Values{"t": {"tok"}}, Header: http.Header{},
		Body: []byte(`{"type":"x","items":[{"secret":"` + canary + `","keep":1}]}`), Now: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(d.Event), canary) || !strings.Contains(string(d.Event), `"keep":1`) {
		t.Fatalf("event = %s, want the list item's secret removed and the rest kept", d.Event)
	}
}

// The pre-proof scan walks a bounded number of top-level members: a body of
// millions of members costs the forger a refusal, not the reader's CPU.
func TestReadThePreProofScanIsBounded(t *testing.T) {
	var b strings.Builder
	b.WriteString("{")
	for i := 0; i <= maxBodyFieldMembers; i++ {
		fmt.Fprintf(&b, `"k%d":0,`, i)
	}
	b.WriteString(`"token":"` + canary + `"}`)
	in := &spec.Ingress{Credential: spec.IngressCredential{Scheme: spec.SchemeToken, In: "body", Name: "token", Secret: "vendor"},
		Event: spec.IngressLocator{From: "payload.type"}}
	_, err := Read(Request{Ingress: in, Secret: []byte(canary), Body: []byte(b.String()), Header: http.Header{}, Now: time.Now()})
	if !errors.Is(err, ErrUnauthenticated) || !strings.Contains(err.Error(), "top-level members") {
		t.Fatalf("a body past the member bound = %v, want ErrUnauthenticated naming the bound", err)
	}
}

// A body timestamp is the vendor's volatile member: a re-signed retry — a
// new instant — must meet the row its first delivery opened, so the dedup
// digest elides it. The signature still covers the whole body, so only the
// vendor can re-sign; a body otherwise different is another delivery.
func TestIngress_ABodyTimestampResignedRetryOpensNoSecondIntake(t *testing.T) {
	body := func(ts string, identifier string) []byte {
		return []byte(`{"action":"create","type":"Issue","data":{"id":"abc","identifier":"` + identifier + `","title":"Fix it"},"webhookTimestamp":` + ts + `}`)
	}
	sign := func(b []byte) string {
		mac := hmac.New(sha256.New, []byte("linear-secret-xyz"))
		mac.Write(b)
		return hex.EncodeToString(mac.Sum(nil))
	}
	in := &spec.Ingress{
		Credential: spec.IngressCredential{Scheme: spec.SchemeHMACSHA256, In: "header", Name: "Linear-Signature", Secret: "minted"},
		Timestamp:  &spec.IngressTimestamp{In: "body", Name: "webhookTimestamp", Unit: "ms", Tolerance: "60s"},
		Event:      spec.IngressLocator{From: `payload.type + "." + payload.action`},
	}
	read := func(b []byte) Delivery {
		h := http.Header{}
		h.Set("Linear-Signature", sign(b))
		d, err := Read(Request{Ingress: in, Triggers: []spec.Trigger{{ID: "linear.issue.created", Event: "Issue.create"}},
			Secret: []byte("linear-secret-xyz"), Body: b, Header: h, Now: time.UnixMilli(1763190000000).Add(10 * time.Second)})
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
		return d
	}
	d1 := read(body("1763190000000", "ENG-42"))
	d2 := read(body("1763190005000", "ENG-42"))
	if d1.DedupKey != d2.DedupKey {
		t.Fatalf("dedup %s != %s: a re-signed retry of the same event opened a second identity", d1.DedupKey, d2.DedupKey)
	}
	if d3 := read(body("1763190000000", "ENG-99")); d3.DedupKey == d1.DedupKey {
		t.Fatal("a different delivery shared the dedup identity")
	}
	// The digest is the body's own, minus the volatile member — computed
	// here by its own JSON surgery, not by calling the helper.
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body("1763190000000", "ENG-42"), &m); err != nil {
		t.Fatal(err)
	}
	delete(m, "webhookTimestamp")
	canonical, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if want := hex.EncodeToString(func() []byte { sum := sha256.Sum256(canonical); return sum[:] }()); d1.DedupKey != want {
		t.Fatalf("dedup = %s, want the body's digest without its timestamp (%s)", d1.DedupKey, want)
	}
}

// A polled trigger declares no event — its poller names it — so a delivery
// that names no event at all must not reach it: the poller is its only feed.
func TestReadAnUnnamedDeliveryNeverReachesAPolledTrigger(t *testing.T) {
	d, err := Read(forgejoRequest(func(r *Request) {
		r.Header.Del("X-Forgejo-Event")
		r.Triggers = append(r.Triggers, spec.Trigger{
			ID: "forgejo.issue.polled", Delivery: spec.DeliveryPoll,
			Outputs: []spec.TriggerOutput{{Name: "actor", From: "payload.sender.login"}},
		})
	}))
	if err != nil {
		t.Fatalf("an authenticated delivery with no event: %v", err)
	}
	if d.TriggerID != "" || d.Filtered == "" {
		t.Fatalf("the unnamed delivery read as trigger %q (filtered %q) — a polled trigger is fed by its poller only", d.TriggerID, d.Filtered)
	}
}
