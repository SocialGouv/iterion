package spec

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/dsl/expr"
)

// The verification schemes an ingress may declare. Cryptography is code, so
// the set is closed and parameterised by data: a vendor that fits none of
// them adds a scheme here, argued on its own (ADR-168 §3).
const (
	SchemeHMACSHA256 = "hmac-sha256"
	SchemeHMACSHA1   = "hmac-sha1"
	SchemeToken      = "token"
	SchemeBearer     = "bearer"
	SchemeBasic      = "basic"
)

// The namespaces an expression evaluated against a delivery may read: the
// decoded body and the headers the package lists. Every other root is
// refused at validation and is an error at evaluation.
const (
	NamespacePayload = "payload"
	NamespaceHeaders = "headers"
)

// Ingress is how a delivery for a package is read and authenticated — the
// document-level `ingress:` block of triggers.yaml. Every reader of a
// delivery reads it, so the lane that serves it holds no vendor's name.
type Ingress struct {
	// Body is how the body is decoded, once, under the body cap: json or
	// form. Empty means json.
	Body       string            `yaml:"body,omitempty" json:"body,omitempty"`
	Credential IngressCredential `yaml:"credential" json:"credential"`
	// Timestamp is the optional replay window.
	Timestamp *IngressTimestamp `yaml:"timestamp,omitempty" json:"timestamp,omitempty"`
	// Installation is an expression over the authenticated body naming the
	// installation a delivery belongs to (a Slack team_id): binding records
	// the operator's value and a delivery carrying another is refused.
	Installation string `yaml:"installation,omitempty" json:"installation,omitempty"`
	// Event locates the vendor's event name, matched against each
	// trigger's Event.
	Event IngressLocator `yaml:"event" json:"event"`
	// DeliveryID locates the vendor's delivery id. It is metadata only:
	// unsigned, so never trusted for dedup.
	DeliveryID *IngressLocator `yaml:"delivery_id,omitempty" json:"delivery_id,omitempty"`
	// Headers are the only headers an expression may read.
	Headers []string `yaml:"headers,omitempty" json:"headers,omitempty"`
	// Redact lists dotted body paths removed from the event; a credential
	// carried in the body is always removed as well.
	Redact    []string          `yaml:"redact,omitempty" json:"redact,omitempty"`
	Handshake *IngressHandshake `yaml:"handshake,omitempty" json:"handshake,omitempty"`
	Ack       *IngressAck       `yaml:"ack,omitempty" json:"ack,omitempty"`
}

// IngressCredential is what proves the sender.
type IngressCredential struct {
	Scheme string `yaml:"scheme" json:"scheme"`
	// In is where the credential travels: header, body or query.
	In   string `yaml:"in,omitempty" json:"in,omitempty"`
	Name string `yaml:"name,omitempty" json:"name,omitempty"`
	// Encoding (hmac only) is the signature's encoding: hex or base64.
	Encoding string `yaml:"encoding,omitempty" json:"encoding,omitempty"`
	// Prefix is stripped from the presented value ("v0=", "sha256=").
	Prefix string `yaml:"prefix,omitempty" json:"prefix,omitempty"`
	// Signed (hmac only) is the signed-string template over {body} and
	// {timestamp}. Empty means "{body}".
	Signed string `yaml:"signed,omitempty" json:"signed,omitempty"`
	// Secret is the secret's origin: minted (iterion generates it) or vendor
	// (the vendor generates it and the operator pastes it).
	Secret string `yaml:"secret" json:"secret"`
}

// IngressTimestamp is a replay window read from a header or the body.
type IngressTimestamp struct {
	In        string `yaml:"in" json:"in"`
	Name      string `yaml:"name" json:"name"`
	Unit      string `yaml:"unit" json:"unit"`
	Tolerance string `yaml:"tolerance" json:"tolerance"`
}

// IngressLocator reads one value of a delivery: a header, or an expression
// over the delivery. Exactly one of the two is set.
type IngressLocator struct {
	Header string `yaml:"header,omitempty" json:"header,omitempty"`
	From   string `yaml:"from,omitempty" json:"from,omitempty"`
}

// IngressHandshake answers a vendor's endpoint verification after the
// delivery is authenticated and before any trigger is read.
type IngressHandshake struct {
	When    string `yaml:"when" json:"when"`
	Respond string `yaml:"respond" json:"respond"`
}

// IngressAck is the answer to an accepted delivery.
type IngressAck struct {
	Status int    `yaml:"status" json:"status"`
	Body   string `yaml:"body,omitempty" json:"body,omitempty"`
}

// TriggerOutput is a named value extracted from a delivery.
type TriggerOutput struct {
	Name        string `yaml:"name" json:"name"`
	From        string `yaml:"from" json:"from"`
	Description string `yaml:"description,omitempty" json:"description,omitempty"`
	// Type is string, int, bool, list or map. Empty means string.
	Type string `yaml:"type,omitempty" json:"type,omitempty"`
}

// TriggerFilter is a value an operator may filter a trigger's deliveries on.
type TriggerFilter struct {
	ID   string `yaml:"id" json:"id"`
	From string `yaml:"from" json:"from"`
	// Type is string, enum or bool.
	Type    string   `yaml:"type" json:"type"`
	Options []string `yaml:"options,omitempty" json:"options,omitempty"`
	Default []string `yaml:"default,omitempty" json:"default,omitempty"`
	// Selector names an operation of the same package whose result fills
	// the studio's picker for this filter.
	Selector string `yaml:"selector,omitempty" json:"selector,omitempty"`
}

// SampleDelivery is a recorded delivery a package ships under samples/: the
// headers and the decoded body, secrets removed.
type SampleDelivery struct {
	Headers map[string]string `json:"headers"`
	Payload map[string]any    `json:"payload"`
}

// reservedOutputs are the output names that fill the trigger.Event envelope
// and the launch, each with its fixed type (ADR-168 §5).
var reservedOutputs = map[string]string{
	"subject": "string", "subject_state": "string", "actor": "string", "repo": "string",
	"thread": "string", "repo_url": "string", "repo_ref": "string", "labels": "list",
}

var (
	outputTypes  = []string{"string", "int", "bool", "list", "map"}
	filterTypes  = []string{"string", "enum", "bool"}
	identifierRe = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
	placeholder  = regexp.MustCompile(`\{[^}]*\}`)
	// neverListable are headers no expression may read, whatever a package
	// lists: they carry credentials.
	neverListable = []string{"Authorization", "Cookie", "Proxy-Authorization"}
)

// NewDeliveryContext is the one evaluation context a delivery's expressions
// run under — validation against a sample, the preview and the lane share it.
// Only the listed headers are visible, under their canonical names.
func NewDeliveryContext(payload map[string]any, headers http.Header, listed []string) *expr.Context {
	visible := map[string]any{}
	for _, h := range listed {
		k := http.CanonicalHeaderKey(h)
		if v := headers.Get(k); v != "" {
			visible[k] = v
		}
	}
	return &expr.Context{Named: map[string]func([]string) any{
		NamespacePayload: func(p []string) any { return descend(payload, p) },
		NamespaceHeaders: func(p []string) any { return descend(visible, p) },
	}}
}

// descend walks maps (and lists, by index) along a dotted path.
func descend(v any, p []string) any {
	for _, seg := range p {
		switch c := v.(type) {
		case map[string]any:
			v = c[seg]
		case []any:
			i, err := strconv.Atoi(seg)
			if err != nil || i < 0 || i >= len(c) {
				return nil
			}
			v = c[i]
		default:
			return nil
		}
	}
	return v
}

// checkExpr parses src and refuses any namespace outside allowed. A bare
// unknown root would otherwise read the implicit `input` namespace, which no
// delivery has.
func checkExpr(where, src string, allowed ...string) error {
	ast, err := expr.Parse(src)
	if err != nil {
		return fmt.Errorf("%s: %w", where, err)
	}
	for _, r := range ast.Refs() {
		if !slices.Contains(allowed, r.Namespace) {
			return fmt.Errorf("%s: %q reads namespace %q — this site may read %s only", where, src, r.Namespace, strings.Join(allowed, ", "))
		}
	}
	return nil
}

func evalExpr(src string, ctx *expr.Context) (any, error) {
	ast, err := expr.Parse(src)
	if err != nil {
		return nil, err
	}
	return ast.Eval(ctx)
}

// validateIngress refuses an ingress block no lane could honour safely. Each
// refusal names what it prevents: a verification declared wrong is a
// delivery believed without proof.
func validateIngress(p *Package) error {
	in := p.Ingress
	if in == nil {
		return nil
	}
	id := p.Connector.ID
	where := func(f string, a ...any) error {
		return fmt.Errorf("connector %q: ingress: "+f, append([]any{id}, a...)...)
	}
	if in.Body != "" && in.Body != "json" && in.Body != "form" {
		return where("body %q is not json or form", in.Body)
	}
	c := in.Credential
	switch c.Secret {
	case "minted", "vendor":
	default:
		return where("credential.secret %q is not minted or vendor — the secret's origin decides who generates it", c.Secret)
	}
	hmacScheme := c.Scheme == SchemeHMACSHA256 || c.Scheme == SchemeHMACSHA1
	switch c.Scheme {
	case SchemeHMACSHA256, SchemeHMACSHA1:
		if c.In != "header" && c.In != "query" {
			return where("an hmac signature travels in a header or the query, not %q", c.In)
		}
		if c.Encoding != "" && c.Encoding != "hex" && c.Encoding != "base64" {
			return where("credential.encoding %q is not hex or base64", c.Encoding)
		}
	case SchemeToken:
		if c.In != "header" && c.In != "body" && c.In != "query" {
			return where("a token travels in a header, the body or the query, not %q", c.In)
		}
	case SchemeBearer, SchemeBasic:
		if c.In != "" && c.In != "header" {
			return where("a %s credential travels in the Authorization header, not %q", c.Scheme, c.In)
		}
	default:
		return where("credential.scheme %q is not one of %s, %s, %s, %s, %s — a delivery nobody can authenticate is never believed",
			c.Scheme, SchemeHMACSHA256, SchemeHMACSHA1, SchemeToken, SchemeBearer, SchemeBasic)
	}
	if (hmacScheme || c.Scheme == SchemeToken) && strings.TrimSpace(c.Name) == "" {
		return where("credential.name is empty — the lane would not know where to read the %s", c.Scheme)
	}
	if !hmacScheme && (c.Encoding != "" || c.Signed != "") {
		return where("credential.encoding and credential.signed apply to hmac schemes only")
	}
	if hmacScheme {
		if err := checkSignedTemplate(c.Signed, in.Timestamp); err != nil {
			return where("%v", err)
		}
	}
	if t := in.Timestamp; t != nil {
		if t.In != "header" && t.In != "body" {
			return where("timestamp.in %q is not header or body", t.In)
		}
		if strings.TrimSpace(t.Name) == "" {
			return where("timestamp.name is empty")
		}
		if t.Unit != "s" && t.Unit != "ms" {
			return where("timestamp.unit %q is not s or ms", t.Unit)
		}
		if d, err := time.ParseDuration(t.Tolerance); err != nil || d <= 0 {
			return where("timestamp.tolerance %q is not a positive duration", t.Tolerance)
		}
	}
	if err := checkLocator("event", &in.Event, true); err != nil {
		return where("%v", err)
	}
	if err := checkLocator("delivery_id", in.DeliveryID, false); err != nil {
		return where("%v", err)
	}
	if in.Installation != "" {
		if err := checkExpr("installation", in.Installation, NamespacePayload, NamespaceHeaders); err != nil {
			return where("%v", err)
		}
	}
	for _, h := range in.Headers {
		k := http.CanonicalHeaderKey(strings.TrimSpace(h))
		if k == "" {
			return where("headers lists an empty name")
		}
		// Prevents: a credential reaching an expression, a preview, the
		// intake store or a run.
		if slices.Contains(neverListable, k) || (c.In == "header" && k == http.CanonicalHeaderKey(c.Name)) ||
			(c.Scheme == SchemeBearer || c.Scheme == SchemeBasic) && k == "Authorization" {
			return where("headers lists %q, which carries a credential and is never readable", k)
		}
	}
	for _, r := range in.Redact {
		if strings.TrimSpace(r) == "" || slices.Contains(strings.Split(r, "."), "") {
			return where("redact path %q is not a dotted body path", r)
		}
	}
	if h := in.Handshake; h != nil {
		if h.When == "" || h.Respond == "" {
			return where("handshake needs both when and respond")
		}
		if err := checkExpr("handshake.when", h.When, NamespacePayload, NamespaceHeaders); err != nil {
			return where("%v", err)
		}
		if err := checkExpr("handshake.respond", h.Respond, NamespacePayload, NamespaceHeaders); err != nil {
			return where("%v", err)
		}
	}
	if a := in.Ack; a != nil && (a.Status < 200 || a.Status > 299) {
		return where("ack.status %d is not a 2xx — a vendor reads any other status as a failed delivery", a.Status)
	}
	return nil
}

// checkSignedTemplate validates an hmac signed-string template: {body} is
// required, {timestamp} is required exactly when the timestamp travels in a
// header (a body timestamp is already authenticated by {body}), and no other
// placeholder exists.
func checkSignedTemplate(signed string, ts *IngressTimestamp) error {
	if signed == "" {
		signed = "{body}"
	}
	for _, ph := range placeholder.FindAllString(signed, -1) {
		if ph != "{body}" && ph != "{timestamp}" {
			return fmt.Errorf("credential.signed %q holds %s — only {body} and {timestamp} exist", signed, ph)
		}
	}
	// Prevents: a signature that does not cover the body, so a forged
	// body would verify.
	if !strings.Contains(signed, "{body}") {
		return fmt.Errorf("credential.signed %q does not cover {body}", signed)
	}
	headerTS := ts != nil && ts.In == "header"
	if headerTS != strings.Contains(signed, "{timestamp}") {
		if headerTS {
			// Prevents: a replay window on an unsigned timestamp, which an
			// attacker rewrites at will.
			return fmt.Errorf("credential.signed %q does not cover {timestamp}, which travels in a header", signed)
		}
		return fmt.Errorf("credential.signed %q names {timestamp}, but no timestamp travels in a header", signed)
	}
	return nil
}

func checkLocator(name string, l *IngressLocator, required bool) error {
	if l == nil || (l.Header == "" && l.From == "") {
		if required {
			return fmt.Errorf("%s locates nothing — it needs a header or a from expression", name)
		}
		return nil
	}
	if l.Header != "" && l.From != "" {
		return fmt.Errorf("%s sets both header and from — one value has one source", name)
	}
	if l.From != "" {
		return checkExpr(name+".from", l.From, NamespacePayload, NamespaceHeaders)
	}
	return nil
}

// locate reads a locator's value from a delivery.
func locate(l IngressLocator, payload map[string]any, headers http.Header, listed []string) (string, error) {
	if l.Header != "" {
		return headers.Get(l.Header), nil
	}
	v, err := evalExpr(l.From, NewDeliveryContext(payload, headers, listed))
	if err != nil {
		return "", err
	}
	if v == nil {
		return "", nil
	}
	return fmt.Sprint(v), nil
}

// validateTriggerExtraction checks one trigger's outputs, filters, loop guard
// and sample against the package's ingress.
func validateTriggerExtraction(p *Package, t Trigger) error {
	id := p.Connector.ID
	declares := len(t.Outputs) > 0 || len(t.Filters) > 0 || t.Ignore != "" || t.Sample != ""
	if declares && p.Ingress == nil {
		// Prevents: declarations nothing reads — the ingress lane is their
		// only reader.
		return fmt.Errorf("connector %q: trigger %q declares outputs, filters, an ignore or a sample, but the package declares no ingress: block to read deliveries with", id, t.ID)
	}
	names := map[string]bool{}
	for _, o := range t.Outputs {
		if !identifierRe.MatchString(o.Name) {
			return fmt.Errorf("connector %q: trigger %q: output name %q is not a lowercase identifier", id, t.ID, o.Name)
		}
		if names[o.Name] {
			return fmt.Errorf("connector %q: trigger %q: duplicate output %q", id, t.ID, o.Name)
		}
		names[o.Name] = true
		typ := outputType(o)
		if !slices.Contains(outputTypes, typ) {
			return fmt.Errorf("connector %q: trigger %q: output %q has type %q, not one of %s", id, t.ID, o.Name, typ, strings.Join(outputTypes, ", "))
		}
		if want, ok := reservedOutputs[o.Name]; ok && typ != want {
			return fmt.Errorf("connector %q: trigger %q: output %q is reserved as a %s", id, t.ID, o.Name, want)
		}
		if err := checkExpr(fmt.Sprintf("trigger %s output %s", t.ID, o.Name), o.From, NamespacePayload, NamespaceHeaders); err != nil {
			return fmt.Errorf("connector %q: %w", id, err)
		}
	}
	fids := map[string]bool{}
	for _, f := range t.Filters {
		if !identifierRe.MatchString(f.ID) {
			return fmt.Errorf("connector %q: trigger %q: filter id %q is not a lowercase identifier", id, t.ID, f.ID)
		}
		if fids[f.ID] {
			return fmt.Errorf("connector %q: trigger %q: duplicate filter %q", id, t.ID, f.ID)
		}
		fids[f.ID] = true
		if !slices.Contains(filterTypes, f.Type) {
			return fmt.Errorf("connector %q: trigger %q: filter %q has type %q, not one of %s", id, t.ID, f.ID, f.Type, strings.Join(filterTypes, ", "))
		}
		if f.Type == "enum" {
			if len(f.Options) == 0 {
				return fmt.Errorf("connector %q: trigger %q: enum filter %q declares no options", id, t.ID, f.ID)
			}
			for _, d := range f.Default {
				if !slices.Contains(f.Options, d) {
					return fmt.Errorf("connector %q: trigger %q: filter %q defaults to %q, which is not an option", id, t.ID, f.ID, d)
				}
			}
		}
		if f.Selector != "" {
			if _, ok := p.Operation(f.Selector); !ok {
				return fmt.Errorf("connector %q: trigger %q: filter %q selector %q is not an operation of this package", id, t.ID, f.ID, f.Selector)
			}
		}
		if err := checkExpr(fmt.Sprintf("trigger %s filter %s", t.ID, f.ID), f.From, NamespacePayload, NamespaceHeaders); err != nil {
			return fmt.Errorf("connector %q: %w", id, err)
		}
	}
	if t.Ignore != "" {
		if err := checkExpr(fmt.Sprintf("trigger %s ignore", t.ID), t.Ignore, NamespacePayload, NamespaceHeaders); err != nil {
			return fmt.Errorf("connector %q: %w", id, err)
		}
	}
	if t.Sample != "" {
		return checkSample(p, t)
	}
	return nil
}

func outputType(o TriggerOutput) string {
	if o.Type == "" {
		return "string"
	}
	return o.Type
}

// checkSample proves the trigger's declarations against the recorded
// delivery it ships: the ingress locates this trigger's event in it, the
// loop guard keeps it, and every output and filter reads it with its
// declared type.
func checkSample(p *Package, t Trigger) error {
	id := p.Connector.ID
	if t.SampleBody == nil {
		return fmt.Errorf("connector %q: trigger %q: sample %q was not loaded", id, t.ID, t.Sample)
	}
	var s SampleDelivery
	if err := json.Unmarshal(t.SampleBody, &s); err != nil {
		return fmt.Errorf("connector %q: trigger %q: sample %q is not a {headers, payload} JSON delivery: %w", id, t.ID, t.Sample, err)
	}
	if s.Payload == nil {
		return fmt.Errorf("connector %q: trigger %q: sample %q has no payload object", id, t.ID, t.Sample)
	}
	headers := http.Header{}
	for k, v := range s.Headers {
		headers.Set(k, v)
	}
	listed := p.Ingress.Headers
	event, err := locate(p.Ingress.Event, s.Payload, headers, listed)
	if err != nil {
		return fmt.Errorf("connector %q: trigger %q: the ingress cannot read the sample's event: %w", id, t.ID, err)
	}
	if event != t.Event {
		return fmt.Errorf("connector %q: trigger %q: the sample's event is %q, not the trigger's %q", id, t.ID, event, t.Event)
	}
	ctx := NewDeliveryContext(s.Payload, headers, listed)
	if t.Ignore != "" {
		v, err := evalExpr(t.Ignore, ctx)
		if err != nil {
			return fmt.Errorf("connector %q: trigger %q: ignore fails on the sample: %w", id, t.ID, err)
		}
		if v == true {
			return fmt.Errorf("connector %q: trigger %q: the trigger's own ignore drops its sample — a sample must show a delivery the trigger keeps", id, t.ID)
		}
	}
	for _, o := range t.Outputs {
		v, err := evalExpr(o.From, ctx)
		if err != nil {
			return fmt.Errorf("connector %q: trigger %q: output %q fails on the sample: %w", id, t.ID, o.Name, err)
		}
		if v != nil && !hasType(v, outputType(o)) {
			return fmt.Errorf("connector %q: trigger %q: output %q reads %T from the sample, not a %s", id, t.ID, o.Name, v, outputType(o))
		}
	}
	for _, f := range t.Filters {
		if _, err := evalExpr(f.From, ctx); err != nil {
			return fmt.Errorf("connector %q: trigger %q: filter %q fails on the sample: %w", id, t.ID, f.ID, err)
		}
	}
	return nil
}

func hasType(v any, typ string) bool {
	switch typ {
	case "string":
		_, ok := v.(string)
		return ok
	case "int":
		switch n := v.(type) {
		case int64:
			return true
		case float64:
			return n == math.Trunc(n)
		}
		return false
	case "bool":
		_, ok := v.(bool)
		return ok
	case "list":
		_, ok := v.([]any)
		return ok
	case "map":
		_, ok := v.(map[string]any)
		return ok
	}
	return false
}

// validSamplePath accepts samples/<name>.json and nothing that could leave
// the package.
func validSamplePath(s string) bool {
	return path.Clean(s) == s && strings.HasPrefix(s, "samples/") && strings.HasSuffix(s, ".json") && !strings.Contains(s, "..")
}
