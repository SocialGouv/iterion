package errtrack

import (
	"fmt"
	"regexp"
	"strings"

	sentry "github.com/getsentry/sentry-go"
)

// redacted replaces every value the scrubber refuses to send.
const redacted = "[redacted]"

// sensitiveKeys are field/header/tag name fragments whose VALUE is
// dropped wholesale, whatever it looks like. Matched case-insensitively
// as a substring, so "anthropic_api_key" and "X-Auth-Token" both hit.
var sensitiveKeys = []string{
	"authorization", "cookie", "secret", "token", "password", "passwd",
	"api_key", "apikey", "credential", "private_key", "dsn", "session",
	"bearer", "auth",
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

// scrubFields copies fields with sensitive keys dropped and string
// values redacted. Returns nil for an empty input so callers can skip
// attaching an empty context. Non-string scalars pass through; nested
// maps/slices are rendered with %v and then redacted, which keeps the
// payload shallow (Sentry contexts are displayed flat anyway) and
// guarantees a secret nested three levels down still gets caught.
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
		switch tv := v.(type) {
		case string:
			out[k] = Redact(tv)
		case bool, int, int8, int16, int32, int64,
			uint, uint8, uint16, uint32, uint64, float32, float64:
			out[k] = tv
		case error:
			out[k] = Redact(tv.Error())
		default:
			out[k] = Redact(fmt.Sprintf("%v", tv))
		}
	}
	return out
}

// scrubEvent is the SDK's BeforeSend hook: the last checkpoint before
// anything leaves the process. It walks the payload surfaces that can
// carry operator data — message, exception values, tags, contexts,
// request headers/query — and redacts each in place.
func scrubEvent(event *sentry.Event, _ *sentry.EventHint) *sentry.Event {
	if event == nil {
		return nil
	}
	event.Message = Redact(event.Message)
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
