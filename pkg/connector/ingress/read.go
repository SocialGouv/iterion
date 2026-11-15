// Package ingress reads one inbound delivery the way a connector package
// declares it (ADR-168 §3, §4): bounded extraction of the credential, the
// verification of the declared scheme over the raw bytes, the replay window,
// the authenticated installation — and only then the handshake, the event
// name, the loop guard and the outputs, all evaluated over the SANITIZED
// event, never the request. It holds no vendor's name and no server state:
// the lane hands it the package's ingress block, the ingress's secret and the
// raw request, and stores what it returns.
package ingress

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha1" //nolint:gosec // HMAC-SHA1 is a scheme vendors sign with, declared per package.
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/connector/spec"
)

// ErrUnauthenticated is the class of every refusal before the sender is
// proven: a missing, repeated or wrong credential, a timestamp out of its
// window, another installation. The lane answers 401 and writes nothing.
var ErrUnauthenticated = errors.New("ingress: delivery not authenticated")

// ErrMalformed is a delivery the package's contract cannot read once it is
// authenticated: a body that is not the declared encoding, an expression
// that fails on it.
var ErrMalformed = errors.New("ingress: malformed delivery")

// Request is one delivery as the lane received it.
type Request struct {
	Ingress  *spec.Ingress
	Triggers []spec.Trigger
	// Secret is the ingress's secret, unsealed: the HMAC key, the token, or
	// the bearer or basic credential.
	Secret []byte
	// Installation is the value bound to this ingress; empty when the
	// package declares none.
	Installation string
	// Body is the raw body, already bounded by the lane.
	Body   []byte
	Header http.Header
	Query  url.Values
	Now    time.Time
}

// Delivery is what an authenticated delivery is, read through its package.
type Delivery struct {
	// Handshake, when set, is the whole answer: the vendor verifies its
	// endpoint, and no trigger is read.
	Handshake *string
	// Filtered says why an authenticated delivery is answered and dropped:
	// an event no trigger declares, or the trigger's loop guard.
	Filtered string
	// TriggerID is the package trigger the event was read as.
	TriggerID string
	// EventName is the vendor's name for the event.
	EventName string
	// DeliveryID is the vendor's delivery id: metadata, never dedup.
	DeliveryID string
	// DedupKey is the digest of the signed body. A timestamp signed apart
	// from it is not part of it: a vendor re-signs its retry at a new
	// instant, and the retry must meet the row its first delivery opened.
	DedupKey string
	// Event is the sanitized event as JSON: outputs, payload without its
	// redacted fields and credential, listed headers.
	Event json.RawMessage
	// Ack is the declared answer to an accepted delivery.
	Ack spec.IngressAck
}

// MaxEventBytes bounds a stored event: Mongo's 16 MiB document bound, less
// the intake row's own fields. A delivery whose event would exceed it is
// refused, never stored truncated.
const MaxEventBytes = 12 << 20

// ErrEventTooLarge is an authenticated delivery whose sanitized event
// exceeds MaxEventBytes.
var ErrEventTooLarge = fmt.Errorf("%w: the event exceeds %d bytes", ErrMalformed, MaxEventBytes)

// Read authenticates req and reads it through its package.
//
// Before the sender is proven nothing decodes the body into a tree: a
// credential or a timestamp that lives in the body is read by a top-level
// scan that builds nothing (bodyField), so an unauthenticated body costs a
// scan, not the memory of its decoded tree. The body is decoded once, after
// the proof.
func Read(req Request) (Delivery, error) {
	in := req.Ingress
	if in == nil {
		return Delivery{}, fmt.Errorf("%w: the package declares no ingress", ErrMalformed)
	}
	timestamp, err := readTimestamp(in, req)
	if err != nil {
		return Delivery{}, err
	}
	if err := verify(in, req, timestamp); err != nil {
		return Delivery{}, err
	}
	payload, err := decodeBody(in.Body, req.Body)
	if err != nil {
		return Delivery{}, err
	}
	event := sanitize(in, payload)
	// One list serves the stored event and every expression: a package
	// that lists a credential header was refused by its validation, and a
	// reader that relied on that alone would hand it to an output.
	visible := visibleHeaders(in)
	headers := listedHeaders(visible, req.Header)
	ctx := spec.NewDeliveryContext(event, req.Header, visible)

	d := Delivery{DedupKey: dedupKey(req.Body), Ack: ack(in)}
	// The handshake comes before the installation: a vendor verifying its
	// endpoint (Slack's url_verification) carries no installation, the
	// signature already proved the sender, and a handshake routes no event.
	if h := in.Handshake; h != nil {
		when, err := spec.EvalDelivery(h.When, ctx)
		if err != nil {
			return Delivery{}, fmt.Errorf("%w: handshake.when: %v", ErrMalformed, err)
		}
		if when == true {
			respond, err := spec.EvalDelivery(h.Respond, ctx)
			if err != nil || respond == nil {
				return Delivery{}, fmt.Errorf("%w: handshake.respond: %v", ErrMalformed, err)
			}
			s := fmt.Sprint(respond)
			d.Handshake = &s
			return d, nil
		}
	}
	if in.Installation != "" {
		// Over the signed body only: a header is not authenticated.
		got, err := spec.EvalDelivery(in.Installation, spec.NewDeliveryContext(event, nil, nil))
		if err != nil {
			return Delivery{}, fmt.Errorf("%w: installation: %v", ErrUnauthenticated, err)
		}
		if got == nil || fmt.Sprint(got) != req.Installation || req.Installation == "" {
			return Delivery{}, fmt.Errorf("%w: the delivery belongs to installation %.64q, this ingress is bound to %.64q", ErrUnauthenticated, fmt.Sprint(got), req.Installation)
		}
	}
	name, err := spec.Locate(in, in.Event, event, req.Header, visible)
	if err != nil {
		return Delivery{}, fmt.Errorf("%w: event: %v", ErrMalformed, err)
	}
	d.EventName = name
	if in.DeliveryID != nil {
		if id, err := spec.Locate(in, *in.DeliveryID, event, req.Header, visible); err == nil {
			d.DeliveryID = id
		}
	}
	var trig *spec.Trigger
	for i := range req.Triggers {
		if req.Triggers[i].Event == name {
			trig = &req.Triggers[i]
			break
		}
	}
	if trig == nil {
		d.Filtered = fmt.Sprintf("the package declares no trigger for event %.64q", name)
		return d, nil
	}
	d.TriggerID = trig.ID
	if trig.Ignore != "" {
		drop, err := spec.EvalDelivery(trig.Ignore, ctx)
		if err != nil {
			return Delivery{}, fmt.Errorf("%w: %s.ignore: %v", ErrMalformed, trig.ID, err)
		}
		if drop == true {
			d.Filtered = fmt.Sprintf("trigger %s's loop guard dropped it", trig.ID)
			return d, nil
		}
	}
	outputs := map[string]any{}
	for _, o := range trig.Outputs {
		v, err := spec.EvalDelivery(o.From, ctx)
		if err != nil {
			return Delivery{}, fmt.Errorf("%w: %s output %s: %v", ErrMalformed, trig.ID, o.Name, err)
		}
		if v != nil {
			outputs[o.Name] = v
		}
	}
	raw, err := encodeEvent(map[string]any{"outputs": outputs, "payload": event, "headers": headers})
	if err != nil {
		return Delivery{}, err
	}
	d.Event = raw
	return d, nil
}

// encodeEvent encodes the event without HTML escaping — a "<" stored as
// six bytes would multiply the event past its bound — and refuses one past
// MaxEventBytes.
func encodeEvent(v any) (json.RawMessage, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, fmt.Errorf("%w: encode the event: %v", ErrMalformed, err)
	}
	raw := bytes.TrimSuffix(buf.Bytes(), []byte("\n"))
	if len(raw) > MaxEventBytes {
		return nil, fmt.Errorf("%w (%d bytes)", ErrEventTooLarge, len(raw))
	}
	return raw, nil
}

// decodeBody decodes the authenticated body once, as the package declares
// it: a JSON object whose keys are never repeated and that nothing follows
// (two parsers would read two different deliveries), or a form.
func decodeBody(encoding string, body []byte) (map[string]any, error) {
	if encoding == "form" {
		values, err := url.ParseQuery(string(body))
		if err != nil {
			return nil, fmt.Errorf("%w: the body is not a form", ErrMalformed)
		}
		out := make(map[string]any, len(values))
		for k, vs := range values {
			if len(vs) == 1 {
				out[k] = vs[0]
				continue
			}
			list := make([]any, len(vs))
			for i, v := range vs {
				list[i] = v
			}
			out[k] = list
		}
		return out, nil
	}
	if err := refuseRepeatedKeys(body); err != nil {
		return nil, err
	}
	var out map[string]any
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	if err := dec.Decode(&out); err != nil || out == nil {
		return nil, fmt.Errorf("%w: the body is not a JSON object", ErrMalformed)
	}
	if err := atEOF(dec); err != nil {
		return nil, err
	}
	normalizeNumbers(out)
	return out, nil
}

// atEOF refuses anything after the body's one JSON value — another value, a
// stray closer, garbage.
func atEOF(dec *json.Decoder) error {
	if _, err := dec.Token(); err != io.EOF {
		return fmt.Errorf("%w: the body holds more than one JSON value", ErrMalformed)
	}
	return nil
}

// maxDepth bounds the nesting the repeated-key walk follows — the bound the
// standard decoder applies to the same body.
const maxDepth = 10000

// refuseRepeatedKeys walks the JSON tokens and refuses an object that names
// one key twice, at any depth.
func refuseRepeatedKeys(body []byte) error {
	dec := json.NewDecoder(bytes.NewReader(body))
	return walkValue(dec, 0)
}

func walkValue(dec *json.Decoder, depth int) error {
	if depth > maxDepth {
		return fmt.Errorf("%w: the body nests deeper than %d", ErrMalformed, maxDepth)
	}
	tok, err := dec.Token()
	if err != nil {
		return notJSON(err)
	}
	d, ok := tok.(json.Delim)
	if !ok {
		return nil
	}
	switch d {
	case '{':
		seen := map[string]bool{}
		for dec.More() {
			kt, err := dec.Token()
			if err != nil {
				return notJSON(err)
			}
			k, _ := kt.(string)
			if seen[k] {
				return fmt.Errorf("%w: the body repeats the key %.64q", ErrMalformed, k)
			}
			seen[k] = true
			if err := walkValue(dec, depth+1); err != nil {
				return err
			}
		}
	case '[':
		for dec.More() {
			if err := walkValue(dec, depth+1); err != nil {
				return err
			}
		}
	}
	if _, err := dec.Token(); err != nil {
		return notJSON(err)
	}
	return nil
}

// notJSON names where a body stops being JSON without quoting it: a syntax
// error's text carries the offending bytes, and the error reaches the log.
func notJSON(err error) error {
	var syn *json.SyntaxError
	if errors.As(err, &syn) {
		return fmt.Errorf("%w: the body is not JSON (offset %d)", ErrMalformed, syn.Offset)
	}
	return fmt.Errorf("%w: the body is not JSON", ErrMalformed)
}

// normalizeNumbers turns the decoder's json.Number into int64 when integral,
// float64 otherwise, so expressions compare numbers as numbers. An integer
// past int64 keeps its literal: through float64 two ids would read as one.
func normalizeNumbers(v any) any {
	switch c := v.(type) {
	case map[string]any:
		for k, x := range c {
			c[k] = normalizeNumbers(x)
		}
	case []any:
		for i, x := range c {
			c[i] = normalizeNumbers(x)
		}
	case json.Number:
		if i, err := c.Int64(); err == nil {
			return i
		}
		if !strings.ContainsAny(c.String(), ".eE") {
			return c.String()
		}
		if f, err := c.Float64(); err == nil {
			return f
		}
		return c.String()
	}
	return v
}

// single reads a value that must be present exactly once and non-empty.
func single(values []string, where string) (string, error) {
	switch {
	case len(values) == 0 || values[0] == "":
		return "", fmt.Errorf("%w: %s is missing", ErrUnauthenticated, where)
	case len(values) > 1:
		return "", fmt.Errorf("%w: %s is repeated", ErrUnauthenticated, where)
	}
	return values[0], nil
}

// maxBodyFieldMembers bounds the top-level members the pre-proof scan walks:
// the parameter bound the standard form parser applies, far above any
// vendor's delivery.
const maxBodyFieldMembers = 10000

// bodyField reads one top-level field of the UNAUTHENTICATED body — a
// credential, a timestamp — without decoding the body into a tree: a form is
// parsed (its parameter count is bounded by the standard library), a JSON
// object is scanned at its top level, every other member skipped as raw
// bytes. The field must be present once, as one non-empty scalar; the
// literal name is the key, never a dotted path.
func bodyField(encoding string, body []byte, name, what string) (string, error) {
	missing := fmt.Errorf("%w: %s %q is missing from the body", ErrUnauthenticated, what, name)
	if encoding == "form" {
		values, err := url.ParseQuery(string(body))
		if err != nil {
			return "", fmt.Errorf("%w: the body is not a form", ErrUnauthenticated)
		}
		vs, ok := values[name]
		switch {
		case !ok:
			return "", missing
		case len(vs) > 1:
			return "", fmt.Errorf("%w: %s %q is not a scalar (repeated)", ErrUnauthenticated, what, name)
		case vs[0] == "":
			return "", fmt.Errorf("%w: %s %q is empty", ErrUnauthenticated, what, name)
		}
		return vs[0], nil
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return "", fmt.Errorf("%w: the body is not a JSON object", ErrUnauthenticated)
	}
	var value string
	found := false
	for members := 0; dec.More(); members++ {
		// The scan runs before the proof: bound the members it walks, as the
		// form parser bounds its parameters.
		if members >= maxBodyFieldMembers {
			return "", fmt.Errorf("%w: the body has more than %d top-level members", ErrUnauthenticated, maxBodyFieldMembers)
		}
		kt, err := dec.Token()
		if err != nil {
			return "", fmt.Errorf("%w: the body is not JSON", ErrUnauthenticated)
		}
		if k, _ := kt.(string); k != name {
			var skip json.RawMessage
			if err := dec.Decode(&skip); err != nil {
				return "", fmt.Errorf("%w: the body is not JSON", ErrUnauthenticated)
			}
			continue
		}
		if found {
			return "", fmt.Errorf("%w: %s %q is not a scalar (repeated)", ErrUnauthenticated, what, name)
		}
		found = true
		vt, err := dec.Token()
		if err != nil {
			return "", fmt.Errorf("%w: the body is not JSON", ErrUnauthenticated)
		}
		switch v := vt.(type) {
		case string:
			if v == "" {
				return "", fmt.Errorf("%w: %s %q is empty", ErrUnauthenticated, what, name)
			}
			value = v
		case json.Number:
			value = v.String()
		case bool:
			value = strconv.FormatBool(v)
		case nil:
			return "", missing
		default:
			return "", fmt.Errorf("%w: %s %q is not a scalar (nested)", ErrUnauthenticated, what, name)
		}
	}
	if !found {
		return "", missing
	}
	return value, nil
}

// readTimestamp reads and checks the replay window; it returns the raw value
// the signed template interpolates.
func readTimestamp(in *spec.Ingress, req Request) (string, error) {
	ts := in.Timestamp
	if ts == nil {
		return "", nil
	}
	var raw string
	var err error
	if ts.In == "header" {
		raw, err = single(req.Header.Values(ts.Name), "timestamp header "+ts.Name)
	} else {
		raw, err = bodyField(in.Body, req.Body, ts.Name, "timestamp")
	}
	if err != nil {
		return "", err
	}
	n, perr := strconv.ParseFloat(raw, 64)
	if perr != nil || math.IsNaN(n) || math.IsInf(n, 0) {
		return "", fmt.Errorf("%w: timestamp %.64q is not a number", ErrUnauthenticated, raw)
	}
	var at time.Time
	if ts.Unit == "ms" {
		at = time.UnixMilli(int64(n))
	} else {
		at = time.Unix(int64(n), 0)
	}
	tolerance, perr := time.ParseDuration(ts.Tolerance)
	if perr != nil {
		return "", fmt.Errorf("%w: timestamp tolerance %q: %v", ErrMalformed, ts.Tolerance, perr)
	}
	if d := req.Now.Sub(at); d > tolerance || d < -tolerance {
		return "", fmt.Errorf("%w: timestamp %.64s is %s from now, outside the %s window", ErrUnauthenticated, raw, d.Round(time.Second), tolerance)
	}
	return raw, nil
}

// verify proves the sender by the declared scheme, over the raw bytes.
func verify(in *spec.Ingress, req Request, timestamp string) error {
	c := in.Credential
	if len(req.Secret) == 0 {
		return fmt.Errorf("%w: the ingress holds no secret", ErrUnauthenticated)
	}
	switch c.Scheme {
	case spec.SchemeHMACSHA256, spec.SchemeHMACSHA1:
		presented, err := presentedValue(in, req)
		if err != nil {
			return err
		}
		presented = strings.TrimPrefix(presented, c.Prefix)
		var sig []byte
		if c.Encoding == "base64" {
			sig, err = base64.StdEncoding.DecodeString(presented)
		} else {
			sig, err = hex.DecodeString(strings.ToLower(presented))
		}
		if err != nil {
			return fmt.Errorf("%w: the signature is not %s", ErrUnauthenticated, encodingName(c.Encoding))
		}
		newHash := sha256.New
		if c.Scheme == spec.SchemeHMACSHA1 {
			newHash = func() hash.Hash { return sha1.New() } //nolint:gosec // declared by the package
		}
		mac := hmac.New(newHash, req.Secret)
		mac.Write(signedString(c.Signed, req.Body, timestamp))
		if !hmac.Equal(mac.Sum(nil), sig) {
			return fmt.Errorf("%w: the signature does not match", ErrUnauthenticated)
		}
		return nil
	case spec.SchemeToken:
		presented, err := presentedValue(in, req)
		if err != nil {
			return err
		}
		return equalSecret(presented, req.Secret)
	case spec.SchemeBearer:
		v, err := single(req.Header.Values("Authorization"), "the Authorization header")
		if err != nil {
			return err
		}
		const p = "bearer "
		if len(v) <= len(p) || !strings.EqualFold(v[:len(p)], p) {
			return fmt.Errorf("%w: the Authorization header is not a bearer credential", ErrUnauthenticated)
		}
		return equalSecret(v[len(p):], req.Secret)
	case spec.SchemeBasic:
		v, err := single(req.Header.Values("Authorization"), "the Authorization header")
		if err != nil {
			return err
		}
		const p = "basic "
		if len(v) <= len(p) || !strings.EqualFold(v[:len(p)], p) {
			return fmt.Errorf("%w: the Authorization header is not a basic credential", ErrUnauthenticated)
		}
		decoded, err := base64.StdEncoding.DecodeString(v[len(p):])
		if err != nil {
			return fmt.Errorf("%w: the basic credential is not base64", ErrUnauthenticated)
		}
		return equalSecret(string(decoded), req.Secret)
	}
	return fmt.Errorf("%w: scheme %.64q is not one the lane verifies", ErrUnauthenticated, c.Scheme)
}

func encodingName(e string) string {
	if e == "" {
		return "hex"
	}
	return e
}

func presentedValue(in *spec.Ingress, req Request) (string, error) {
	c := in.Credential
	switch c.In {
	case "", "header":
		return single(req.Header.Values(c.Name), "credential header "+c.Name)
	case "query":
		return single(req.Query[c.Name], "credential query parameter "+c.Name)
	case "body":
		return bodyField(in.Body, req.Body, c.Name, "credential")
	}
	return "", fmt.Errorf("%w: credential.in %.64q", ErrUnauthenticated, c.In)
}

func equalSecret(presented string, secret []byte) error {
	if subtle.ConstantTimeCompare([]byte(presented), secret) != 1 {
		return fmt.Errorf("%w: the credential does not match", ErrUnauthenticated)
	}
	return nil
}

// signedString renders the signed-string template over the raw body bytes.
func signedString(template string, body []byte, timestamp string) []byte {
	if template == "" {
		template = "{body}"
	}
	var out bytes.Buffer
	for {
		i := strings.IndexByte(template, '{')
		if i < 0 {
			out.WriteString(template)
			return out.Bytes()
		}
		out.WriteString(template[:i])
		rest := template[i:]
		switch {
		case strings.HasPrefix(rest, "{body}"):
			out.Write(body)
			template = rest[len("{body}"):]
		case strings.HasPrefix(rest, "{timestamp}"):
			out.WriteString(timestamp)
			template = rest[len("{timestamp}"):]
		default:
			out.WriteByte('{')
			template = rest[1:]
		}
	}
}

// sanitize returns the payload an expression and the stored event may see:
// the redacted paths and a credential carried in the body removed — the
// credential by the literal key it was read from (bodyField), a redact path
// as a dotted path.
func sanitize(in *spec.Ingress, payload map[string]any) map[string]any {
	if in.Credential.In == "body" && in.Credential.Name != "" {
		delete(payload, in.Credential.Name)
	}
	for _, p := range in.Redact {
		removePath(payload, strings.Split(p, "."))
	}
	return payload
}

// removePath removes a dotted body path. A numeric segment indexes a list,
// as an expression's path does; a list element at the end of the path is
// nulled, so the indices after it keep naming what they named.
func removePath(v any, path []string) {
	if len(path) == 0 {
		return
	}
	switch c := v.(type) {
	case map[string]any:
		if len(path) == 1 {
			delete(c, path[0])
			return
		}
		removePath(c[path[0]], path[1:])
	case []any:
		i, err := strconv.Atoi(path[0])
		if err != nil || i < 0 || i >= len(c) {
			return
		}
		if len(path) == 1 {
			c[i] = nil
			return
		}
		removePath(c[i], path[1:])
	}
}

// visibleHeaders is the package's header list under canonical names, without
// a header that carries a credential — the declared one, or one no package
// may list.
func visibleHeaders(in *spec.Ingress) []string {
	var out []string
	for _, name := range in.Headers {
		if in.CarriesCredential(name) {
			continue
		}
		out = append(out, http.CanonicalHeaderKey(name))
	}
	return out
}

// listedHeaders returns the visible headers the delivery carries.
func listedHeaders(visible []string, h http.Header) map[string]string {
	out := map[string]string{}
	for _, k := range visible {
		if v := h.Get(k); v != "" {
			out[k] = v
		}
	}
	return out
}

// dedupKey is the digest of the signed body. Neither the vendor's delivery
// id (unsigned) nor a timestamp signed apart (Slack re-signs every retry at
// its own instant — its third comes five minutes on, past a window that kept
// the first instant) is part of it: a replay inside the window and a
// vendor's retry both meet the row of the first delivery.
func dedupKey(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

// ack is the declared answer, 200 when the package states no status — the
// lane writes what this returns, so a status the validation would have
// refused never reaches it.
func ack(in *spec.Ingress) spec.IngressAck {
	a := spec.IngressAck{Status: http.StatusOK}
	if in.Ack != nil {
		a = *in.Ack
		if a.Status == 0 {
			a.Status = http.StatusOK
		}
	}
	return a
}
