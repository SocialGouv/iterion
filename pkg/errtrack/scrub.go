package errtrack

import (
	"fmt"
	"reflect"
	"regexp"
	"strings"

	sentry "github.com/getsentry/sentry-go"
)

// redacted replaces every value the scrubber refuses to send.
const redacted = "[redacted]"

// sensitiveKeys are field/header/tag name fragments whose VALUE is
// dropped wholesale, whatever it looks like. Matched case-insensitively
// as a substring, so "anthropic_api_key" and "X-Auth-Token" both hit.
// Deliberately NOT bare "auth": substring matching would eat
// "author"/"pr_author" — real, common fields — and in a filter an
// over-redaction destroys the event's value as surely as a leak.
// "authorization" and "token" cover the credential-bearing auth keys.
var sensitiveKeys = []string{
	"authorization", "cookie", "secret", "token", "password", "passwd",
	"api_key", "apikey", "credential", "private_key", "dsn", "session",
	"bearer",
}

// secretPatterns redact a secret embedded in an otherwise-useful
// string — a log message quoting a URL, an error text echoing a header.
// Kept narrow and anchored: over-redaction destroys the event's value,
// so each pattern targets a shape that is a credential and nothing else.
var secretPatterns = []*regexp.Regexp{
	// URL userinfo, which is exactly the shape of a Sentry DSN's key:
	// scheme://<key>[:<secret>]@host/…
	regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.-]*://)[^/\s:@]+(:[^/\s@]*)?@`),
	// Provider + iterion token prefixes.
	regexp.MustCompile(`\b(sk-[A-Za-z0-9_-]{8,}|xai-[A-Za-z0-9_-]{8,}|gh[pousr]_[A-Za-z0-9]{8,}|glpat-[A-Za-z0-9_-]{8,}|iap_[A-Za-z0-9_-]{8,}|iwh_[A-Za-z0-9_-]{8,})`),
	// Bearer/Basic credentials inside a header dump.
	regexp.MustCompile(`(?i)\b(bearer|basic)\s+[A-Za-z0-9._~+/=-]{8,}`),
	// iterion's own secret placeholders travel through prompts and
	// command lines; the name alone is enough to identify the binding.
	regexp.MustCompile(`__ITERION_SECRET_[A-Za-z0-9_]+__`),
	// Email addresses — the repo treats operator emails as personal data.
	regexp.MustCompile(`\b[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}\b`),
	// Slack tokens (xoxb-/xoxp-/xoxa-/xoxs-/xoxr-).
	regexp.MustCompile(`\bxox[abpsr]-[A-Za-z0-9-]{8,}`),
	// Google/Firebase API keys — fixed AIza prefix.
	regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{30,}`),
	// JWTs — three dot-joined base64url segments, eyJ = {" in base64.
	regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{4,}\.[A-Za-z0-9_-]{4,}\b`),
	// key=value credential echoes in prose, curl lines, query strings.
	// \b anchors the key name so "tokens=48657" (a count) stays intact.
	regexp.MustCompile(`(?i)\b(token|api[_-]?key|secret|password|passwd)=[^\s"'&]{6,}`),
}

// Redact returns s with every credential-shaped substring replaced. It
// is exported because call sites that build their own event text (the
// init-failure log line) want the same guarantee.
func Redact(s string) string {
	if s == "" {
		return s
	}
	out := s
	for i, re := range secretPatterns {
		if i == 0 {
			// Keep the scheme so "https://…@host" stays readable as a URL.
			out = re.ReplaceAllString(out, "${1}"+redacted+"@")
			continue
		}
		out = re.ReplaceAllString(out, redacted)
	}
	return out
}

// isSensitiveKey reports whether a field name means "never send the
// value".
func isSensitiveKey(k string) bool {
	lk := strings.ToLower(k)
	for _, frag := range sensitiveKeys {
		if strings.Contains(lk, frag) {
			return true
		}
	}
	return false
}

// maxScrubDepth bounds the recursive field walk. Past the cap the value
// is dropped to the redaction marker rather than rendered: fmt's %v does
// not detect cycles, so stringifying an arbitrarily deep (or cyclic)
// value would kill the process with an unrecoverable stack overflow —
// and "log lines never crash the producer" is this package's contract.
const maxScrubDepth = 8

// scrubFields copies fields with sensitive keys dropped and string
// values redacted, recursing into nested maps, slices and structs so a
// sensitive KEY is honoured at every level — a header dump, a nested
// payload or a struct with fielded credentials is exactly what the
// log→tracker seam carries. Returns nil for an empty input so callers
// can skip attaching an empty context.
func scrubFields(fields map[string]any) map[string]any {
	if len(fields) == 0 {
		return nil
	}
	out := make(map[string]any, len(fields))
	for k, v := range fields {
		if isSensitiveKey(k) {
			out[k] = redacted
			continue
		}
		out[k] = scrubValue(v, 0)
	}
	return out
}

// scrubValue redacts one field value. Strings and error texts go
// through Redact, scalars pass, and composites (maps, slices, exported
// struct fields) are walked with the key check applied at every level.
func scrubValue(v any, depth int) any {
	switch tv := v.(type) {
	case nil:
		return nil
	case string:
		return Redact(tv)
	case bool, int, int8, int16, int32, int64,
		uint, uint8, uint16, uint32, uint64, float32, float64:
		return tv
	case error:
		return Redact(tv.Error())
	case []byte:
		return Redact(string(tv))
	}
	if depth >= maxScrubDepth {
		// Too deep or cyclic: drop the value instead of rendering it
		// (see maxScrubDepth). Bounded loss beats a leak or a crash.
		return redacted
	}
	// fmt.Stringer before reflection so time.Time and friends keep
	// their readable form instead of an exported-field walk.
	if s, ok := v.(fmt.Stringer); ok {
		return Redact(s.String())
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Pointer, reflect.Interface:
		if rv.IsNil() {
			return nil
		}
		return scrubValue(rv.Elem().Interface(), depth+1)
	case reflect.Map:
		out := make(map[string]any, rv.Len())
		iter := rv.MapRange()
		for iter.Next() {
			key := fmt.Sprint(iter.Key().Interface())
			if isSensitiveKey(key) {
				out[key] = redacted
				continue
			}
			out[key] = scrubValue(iter.Value().Interface(), depth+1)
		}
		return out
	case reflect.Slice, reflect.Array:
		out := make([]any, rv.Len())
		for i := 0; i < rv.Len(); i++ {
			out[i] = scrubValue(rv.Index(i).Interface(), depth+1)
		}
		return out
	case reflect.Struct:
		rt := rv.Type()
		out := make(map[string]any, rt.NumField())
		for i := 0; i < rt.NumField(); i++ {
			f := rt.Field(i)
			if !f.IsExported() {
				continue
			}
			if isSensitiveKey(f.Name) {
				out[f.Name] = redacted
				continue
			}
			out[f.Name] = scrubValue(rv.Field(i).Interface(), depth+1)
		}
		return out
	default:
		// Leaf kinds with no walkable content (func, chan, complex, …).
		return Redact(fmt.Sprint(v))
	}
}

// scrubEvent is the SDK's BeforeSend AND BeforeSendTransaction hook:
// the last checkpoint before anything leaves the process. It walks the
// payload surfaces that can carry operator data — message, exception
// values, tags, contexts, request headers/query, spans — and redacts
// each in place.
func scrubEvent(event *sentry.Event, _ *sentry.EventHint) *sentry.Event {
	if event == nil {
		return nil
	}
	event.Message = Redact(event.Message)
	// Identity/meta surfaces are caller-supplied free-form strings that
	// ride on EVERY event — an operator building ServerName or
	// SENTRY_ENVIRONMENT from an unaudited env inherits a leak surface.
	// Redact only rewrites credential-shaped substrings, so the normal
	// values ("production", "iterion@v3.48.3+sha") pass unchanged.
	event.ServerName = Redact(event.ServerName)
	event.Environment = Redact(event.Environment)
	event.Release = Redact(event.Release)
	event.Dist = Redact(event.Dist)
	event.Transaction = Redact(event.Transaction)
	event.Logger = Redact(event.Logger)
	for i := range event.Fingerprint {
		event.Fingerprint[i] = Redact(event.Fingerprint[i])
	}
	for i := range event.Exception {
		event.Exception[i].Value = Redact(event.Exception[i].Value)
	}
	for k, v := range event.Tags {
		if isSensitiveKey(k) {
			event.Tags[k] = redacted
			continue
		}
		event.Tags[k] = Redact(v)
	}
	for name, ctx := range event.Contexts {
		event.Contexts[name] = scrubFields(ctx)
	}
	for _, b := range event.Breadcrumbs {
		scrubBreadcrumbInPlace(b)
	}
	// Transaction events carry their timing tree here. Mutating the
	// spans is safe: the SDK pre-serialises them only after the
	// BeforeSend* hooks have run.
	for _, s := range event.Spans {
		scrubSpanInPlace(s)
	}
	if event.Request != nil {
		for k := range event.Request.Headers {
			if isSensitiveKey(k) {
				event.Request.Headers[k] = redacted
				continue
			}
			event.Request.Headers[k] = Redact(event.Request.Headers[k])
		}
		event.Request.QueryString = Redact(event.Request.QueryString)
		event.Request.Cookies = redacted
		event.Request.URL = Redact(event.Request.URL)
		event.Request.Data = Redact(event.Request.Data)
	}
	// The user record is identity data we never need for triage.
	event.User = sentry.User{}
	return event
}

// scrubBreadcrumb is the SDK's BeforeBreadcrumb hook.
func scrubBreadcrumb(b *sentry.Breadcrumb, _ *sentry.BreadcrumbHint) *sentry.Breadcrumb {
	scrubBreadcrumbInPlace(b)
	return b
}

func scrubBreadcrumbInPlace(b *sentry.Breadcrumb) {
	if b == nil {
		return
	}
	b.Message = Redact(b.Message)
	if len(b.Data) > 0 {
		b.Data = scrubFields(b.Data)
	}
}
