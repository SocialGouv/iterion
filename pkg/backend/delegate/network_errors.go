package delegate

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"syscall"
)

// ---------------------------------------------------------------------------
// Transient network-failure classification.
//
// A momentary internet/API outage (DNS flap, TCP reset, TLS handshake
// timeout, upstream 5xx/overload) must not abort a long multi-node run.
// The executor already retries with exponential backoff — but only for
// errors it recognises as transient. CLI delegates (claude_code, codex)
// surface a connectivity drop as an opaque "session ended without result
// message (cli_exit_code=N)", whose only network evidence is on the
// subprocess stderr; in-process backends (claw) surface a raw net error
// the legacy *APIError check misses. This classifier covers both so the
// retry loop can ride the blip out.
//
// Lives in the low-level delegate package (not model) so claude_code.go can
// re-type its own errors without an import cycle — model already imports
// delegate, never the reverse.
// ---------------------------------------------------------------------------

// networkErrorSignatures are case-insensitive substrings marking a transient
// connectivity failure. Matched against an error message — the fallback for
// failures that cross a process or SDK boundary as plain text, notably the
// claude_code CLI whose Node/undici stack reports "fetch failed",
// "ECONNRESET", "socket hang up", "getaddrinfo ENOTFOUND", and Anthropic
// API "overloaded"/5xx bodies.
//
// Deliberately broad: a false positive costs one bounded, backed-off retry;
// a false negative aborts an entire run on a momentary blip. That asymmetry
// is exactly what this guards against.
var networkErrorSignatures = []string{
	"connection refused", "connection reset", "reset by peer",
	"broken pipe", "no such host", "network is unreachable",
	"host is unreachable", "no route to host", "i/o timeout",
	"tls handshake timeout", "remote error: tls", "unexpected eof",
	"connection timed out", "operation timed out", "timeout awaiting",
	"dial tcp", "dial udp", "read tcp", "write tcp",
	"fetch failed", "socket hang up", "econnreset", "econnrefused",
	"econnaborted", "etimedout", "enotfound", "eai_again", "epipe",
	"getaddrinfo", "temporary failure in name resolution",
	"other side closed", "network error", "connection error",
	"connection aborted", "connection closed", "upstream connect error",
	"service unavailable", "bad gateway", "gateway timeout",
	"overloaded", "internal server error",
	// CLI-verbatim + HTTP/2 connectivity markers. These are the phrases
	// the run-level recovery.Classify already treats as NETWORK_TRANSIENT;
	// mirroring them here lets the FAST in-executor retry loop absorb the
	// blip instead of it only being caught by the slower run-level recovery
	// dispatch. False positives cost one bounded backed-off retry (see the
	// asymmetry note above) — the gap they close is a missed fast retry.
	"unable to connect to api", "failedtoopensocket", "http: eof",
	"http2: timeout", "http2: server sent goaway",
	"server closed idle connection",
	"stream disconnected",
}

// MatchesNetworkSignature reports whether s (an error message or a captured
// stderr line) contains a known transient-connectivity marker. Exposed so
// CLI delegates can re-type opaque "session ended without result" failures
// whose only network evidence is on the subprocess's stderr. Self-inflicted
// "context canceled" is excluded — it is never a network fault.
func MatchesNetworkSignature(s string) bool {
	return matchesAnySignature(s, networkErrorSignatures)
}

// transportErrorSignatures is the subset of networkErrorSignatures whose
// tokens are emitted by the runtime or transport layer itself — syscall
// errno names, undici's "fetch failed", Go's net/http and HTTP/2 markers —
// and is therefore safe to match against a CLI agent's RAW stderr. The
// signatures left out ("unexpected eof", "network error", "connection
// error", "overloaded", "internal server error", …) are prose an
// APPLICATION-level error also produces — measured on opencode 1.1.19: a
// credential-less host dies deterministically with "JSON Parse error:
// Unexpected EOF" — so against free-form stderr they are not evidence of a
// transport fault. They still count against an error the process itself
// returned (MatchesNetworkSignature above), whose text is the caller's
// own. Mid-stream provider failures keep their structured channel where
// one exists: pi re-types from its error metadata (piClassifyFailure,
// full list included) and opencode's stream error events reach
// IsNetworkError's full list as plain executor-visible errors. kimi and
// grok carry no error channel at all, so a crash-rendered 5xx on their
// stderr is now surfaced fast rather than retried — the deliberate price
// of distrusting free-form stderr.
var transportErrorSignatures = []string{
	"connection refused", "connection reset", "reset by peer",
	"broken pipe", "no such host", "network is unreachable",
	"host is unreachable", "no route to host", "i/o timeout",
	"tls handshake timeout", "remote error: tls",
	"connection timed out", "operation timed out", "timeout awaiting",
	"dial tcp", "dial udp", "read tcp", "write tcp",
	"fetch failed", "socket hang up", "econnreset", "econnrefused",
	"econnaborted", "etimedout", "enotfound", "eai_again", "epipe",
	"getaddrinfo", "temporary failure in name resolution",
	"other side closed", "upstream connect error",
	"unable to connect to api", "failedtoopensocket", "http: eof",
	"http2: timeout", "http2: server sent goaway",
	"server closed idle connection",
	"stream disconnected",
}

// transportStderrMaxLine caps the lines matchesTransportStderr reads.
// Node's uncaught-exception render echoes the THROWING SOURCE LINE at
// column 0 — file:line, then the source verbatim, then a caret — and for
// a minified single-line bundle that echo holds nearly every string
// literal in the program, so the column-0 filter alone would read quoted
// source as the CLI's own report. Genuine transport error headers are
// short (the longest shapes above — a dial line with a max-length DNS
// name — reach ~300 chars); 500 leaves ample headroom while a minified
// source echo is orders of magnitude past it. Same construction as the
// length caps in claude_code_errmap.go.
const transportStderrMaxLine = 500

// matchesTransportStderr reports whether a CLI agent's stderr carries a
// transport-level failure marker on a line that is the CLI's OWN error
// report. A JavaScript crash dump is free-form text — its codeframes and
// Node's column-0 source echo quote arbitrary source lines, which can
// contain any signature verbatim — so only short, unindented lines (the
// runtime's error headers) and the `[cause]:` chain Node indents under
// them are read, and only the transport-explicit signatures match. Stack
// frames, codeframe excerpts and minified source echoes are skipped.
func matchesTransportStderr(stderr string) bool {
	for _, line := range strings.Split(stderr, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || len(trimmed) > transportStderrMaxLine {
			continue
		}
		if (line[0] == ' ' || line[0] == '\t') && !strings.HasPrefix(trimmed, "[cause]:") {
			continue
		}
		if matchesAnySignature(trimmed, transportErrorSignatures) {
			return true
		}
	}
	return false
}

func matchesAnySignature(s string, sigs []string) bool {
	s = strings.ToLower(s)
	if strings.Contains(s, "context canceled") {
		return false
	}
	for _, sig := range sigs {
		if strings.Contains(s, sig) {
			return true
		}
	}
	return false
}

// IsNetworkError reports whether err is a transient network/connectivity
// failure that a bounded, backed-off retry can plausibly recover from.
//
// Layered, strongest signal first:
//  1. net.Error with Timeout() — the canonical Go timeout interface.
//  2. Wrapped syscall errno (ECONNRESET, ECONNREFUSED, ETIMEDOUT, …).
//  3. io.ErrUnexpectedEOF — a stream cut mid-flight.
//  4. Substring match on the message (networkErrorSignatures) — the
//     fallback for errors that crossed a process / SDK boundary as text.
//
// Returns false for nil and for the run's own context errors (Canceled /
// DeadlineExceeded), which the caller's ctx.Done path already handles and
// must never retry — note context.DeadlineExceeded itself satisfies
// net.Error with Timeout()==true, so it MUST be screened out before the
// net.Error check below. Genuine network timeouts that are not the run
// deadline (net.OpError timeouts, http.Client.Timeout, "i/o timeout"
// messages) are still caught downstream.
func IsNetworkError(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	// DNS failures typed, not by message: resolver flaps surface as
	// *net.DNSError (temporary SERVFAIL, timeout, or NXDOMAIN from a
	// half-broken resolver). NotFound is included deliberately — per the
	// asymmetry note on networkErrorSignatures, a genuinely-bad host
	// costs one bounded retry; a resolver blip aborting an overnight run
	// costs the run.
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return true
	}
	for _, errno := range []syscall.Errno{
		syscall.ECONNRESET, syscall.ECONNREFUSED, syscall.ECONNABORTED,
		syscall.ETIMEDOUT, syscall.EPIPE, syscall.ENETUNREACH,
		syscall.EHOSTUNREACH, syscall.ENETDOWN, syscall.ENETRESET,
		syscall.EHOSTDOWN,
	} {
		if errors.Is(err, errno) {
			return true
		}
	}
	if errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	return MatchesNetworkSignature(err.Error())
}
