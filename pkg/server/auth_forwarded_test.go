package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
	"slices"
	"strings"
	"testing"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// The mechanism is the OPERATOR's choice, not the client's. A reverse
// proxy that computes the true client into X-Real-IP and passes the
// client's own X-Forwarded-For through untouched — the canonical
// hand-written nginx snippet `proxy_set_header X-Real-IP $remote_addr;`
// alone, any HAProxy or CDN writing only RFC 7239 Forwarded — is the
// deployment where "read the first mechanism that carries any" loses to
// a header the client wrote: the client then names its own identity,
// rotating it buys a fresh rate-limit bucket per value, and the audit row
// carries an address it chose (#2673 through a header instead of a port).
func TestTheDeclaredHeaderIsReadAndNoOther(t *testing.T) {
	// The attacker's shape: the proxy's answer in the declared header,
	// the client's own line in every other mechanism.
	for _, declared := range []string{"X-Real-IP", "Forwarded", "CF-Connecting-IP"} {
		t.Run(declared, func(t *testing.T) {
			s := &Server{cfg: Config{TrustedProxyCIDRs: []string{"10.0.0.0/8"}, ForwardedHeader: declared}}
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			r.RemoteAddr = "10.1.1.1:443" // the proxy, inside the trusted range
			if declared == "Forwarded" {
				r.Header.Set(declared, "for=203.0.113.5;proto=https")
			} else {
				r.Header.Set(declared, "203.0.113.5")
			}
			r.Header.Set("X-Forwarded-For", "198.51.100.99")
			if got := s.clientIP(r); got != "203.0.113.5" {
				t.Errorf("got %q, want 203.0.113.5 — the client's own X-Forwarded-For shadowed the %s the proxy wrote", got, declared)
			}
		})
	}

	// And the undeclared mechanisms stay unread even with nothing else
	// to answer with: a fallback to "whatever is present" is the same
	// hole, reached one request later.
	t.Run("the undeclared mechanisms are not a fallback", func(t *testing.T) {
		s := &Server{cfg: Config{TrustedProxyCIDRs: []string{"10.0.0.0/8"}, ForwardedHeader: "X-Real-IP"}}
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.RemoteAddr = "10.1.1.1:443"
		r.Header.Set("X-Forwarded-For", "198.51.100.99")
		r.Header.Set("Forwarded", "for=198.51.100.98")
		if got := s.clientIP(r); got != "10.1.1.1" {
			t.Errorf("got %q, want the peer host 10.1.1.1 — a client-written mechanism became the identity", got)
		}
	})
}

// The same rule through the public path the limiter guards: ONE client
// behind an X-Real-IP proxy, rotating its own X-Forwarded-For. Each
// rotation that moved the key would be a fresh login bucket (5/min in
// production, one token here so a shared bucket shows in one request).
func TestAClientsOwnHeaderBuysNoBucketBehindADeclaredMechanism(t *testing.T) {
	s := &Server{
		cfg:         Config{TrustedProxyCIDRs: []string{"10.0.0.0/8"}, ForwardedHeader: "X-Real-IP"},
		authLimiter: frozenLimiter(),
	}
	handler := s.limitRoute("login", authBucketCfg{rate: 1.0 / 60.0, burst: 1}, nil)(
		func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })

	send := func(forged string) int {
		r := httptest.NewRequest(http.MethodPost, "/api/auth/login", nil)
		r.RemoteAddr = "10.1.1.1:443"
		r.Header.Set("X-Real-IP", "203.0.113.5") // what the proxy computed
		r.Header.Set("X-Forwarded-For", forged)  // what the client sent
		w := httptest.NewRecorder()
		handler(w, r)
		return w.Code
	}

	if got := send("198.51.100.1"); got != http.StatusNoContent {
		t.Fatalf("first request: got %d, want %d", got, http.StatusNoContent)
	}
	for i, forged := range []string{"198.51.100.2", "198.51.100.3", "198.51.100.4"} {
		if got := send(forged); got != http.StatusTooManyRequests {
			t.Errorf("rotation %d (%s): got %d, want %d — the client moved the limiter's key with its own header", i+1, forged, got, http.StatusTooManyRequests)
		}
	}
	// The rule still separates two real clients behind that proxy.
	other := func() int {
		r := httptest.NewRequest(http.MethodPost, "/api/auth/login", nil)
		r.RemoteAddr = "10.1.1.1:443"
		r.Header.Set("X-Real-IP", "198.51.100.7")
		w := httptest.NewRecorder()
		handler(w, r)
		return w.Code
	}
	if got := other(); got != http.StatusNoContent {
		t.Errorf("another real client: got %d, want %d — it must not pay for the first", got, http.StatusNoContent)
	}
}

// allocBytesPerCall is the heap one call allocates, as the MINIMUM over
// rounds: another goroutine's allocation can only ADD to the process
// counter, so the smallest round is the least polluted one. A ratio of
// two such measurements is the same on every machine — a byte count is
// not a duration.
func allocBytesPerCall(rounds, calls int, f func()) uint64 {
	best := ^uint64(0)
	for range rounds {
		var before, after runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&before)
		for range calls {
			f()
		}
		runtime.ReadMemStats(&after)
		if per := (after.TotalAlloc - before.TotalAlloc) / uint64(calls); per < best {
			best = per
		}
	}
	return best
}

// The hop bound must bound the COLLECTION, not only the walk. The header
// is client-writable up to net/http's default MaxHeaderBytes (1 MiB) and
// this runs before the limiter has a key to refuse the request with:
// splitting the whole value first and keeping 32 entries afterwards cost
// 10.8 MB of garbage and 3.5 ms of CPU per request — ~285 req/s saturate
// the 1-core server pod from one unauthenticated caller.
func TestALongForwardedHeaderCostsNoMoreThanTheHopBound(t *testing.T) {
	s := &Server{cfg: Config{TrustedProxyCIDRs: []string{"10.0.0.0/8"}}}
	call := func(header string) func() {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.RemoteAddr = "10.1.1.1:443"
		r.Header.Set("X-Forwarded-For", header)
		return func() { _ = s.clientIP(r) }
	}

	// A full chain the walk really does read, and a 1 MiB one: the first
	// is the work a real deployment pays, the second what one request can
	// ask for.
	atTheBound := strings.TrimSuffix(strings.Repeat("10.0.0.1,", maxForwardedHops), ",")
	oneMiB := strings.Repeat("10.0.0.1,", 1<<20/len("10.0.0.1,")+1)
	if len(oneMiB) < 1<<20 {
		t.Fatalf("the padded header is %d bytes, want at least %d — the witness needs MaxHeaderBytes' worth", len(oneMiB), 1<<20)
	}

	bound := allocBytesPerCall(3, 200, call(atTheBound))
	padded := allocBytesPerCall(3, 200, call(oneMiB))
	// Four times the bounded cost leaves room for a different allocator
	// shape; the defect this pins was 3800 times it.
	if padded > 4*bound {
		t.Errorf("a %d-byte X-Forwarded-For allocates %d B/call against %d B/call for %d hops (%.0fx): the collection is O(header), not O(hops) — one request pays for the whole header before the limiter can refuse it",
			len(oneMiB), padded, bound, maxForwardedHops, float64(padded)/float64(bound))
	}

	// The bound costing nothing would make the ratio meaningless.
	if bound == 0 {
		t.Fatalf("%d hops allocated nothing measurable: the ratio above proves nothing", maxForwardedHops)
	}

	// And the answer is still the right one: padding past the bound lands
	// on the proxy's own address, which is less reach and not more.
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "10.1.1.1:443"
	r.Header.Set("X-Forwarded-For", "203.0.113.5,"+oneMiB)
	if got := s.clientIP(r); got != "10.0.0.1" {
		t.Errorf("padded chain: got %q, want 10.0.0.1 — the rightmost hop inside the ranges, the furthest the bound can account for", got)
	}
}

// An RFC 7239 Forwarded header is as client-writable as X-Forwarded-For,
// and ONE element can carry a hundred thousand parameters: the bound has
// to hold on both cuts, the elements and their parameters.
func TestALongForwardedElementCostsNoMoreThanTheHopBound(t *testing.T) {
	s := &Server{cfg: Config{TrustedProxyCIDRs: []string{"10.0.0.0/8"}, ForwardedHeader: "Forwarded"}}
	call := func(header string) func() {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.RemoteAddr = "10.1.1.1:443"
		r.Header.Set("Forwarded", header)
		return func() { _ = s.clientIP(r) }
	}

	atTheBound := strings.TrimSuffix(strings.Repeat("for=10.0.0.1,", maxForwardedHops), ",")
	elements := strings.Repeat("for=10.0.0.1,", 1<<20/len("for=10.0.0.1,")+1)
	params := strings.Repeat(";for=10.0.0.1", 1<<20/len(";for=10.0.0.1")+1)

	bound := allocBytesPerCall(3, 200, call(atTheBound))
	if bound == 0 {
		t.Fatalf("%d elements allocated nothing measurable: the ratios below prove nothing", maxForwardedHops)
	}
	for name, header := range map[string]string{"a million bytes of elements": elements, "one element of a million bytes": params} {
		if padded := allocBytesPerCall(3, 200, call(header)); padded > 4*bound {
			t.Errorf("%s (%d bytes) allocates %d B/call against %d B/call for %d elements (%.0fx): the cut is O(header), not O(hops)",
				name, len(header), padded, bound, maxForwardedHops, float64(padded)/float64(bound))
		}
	}
}

// The hop bound bounds how MANY entries are read; nothing in it bounds
// how LONG one is. A client-writable header reaches MaxHeaderBytes
// (1 MiB, net/http's default and unset here) as ONE entry as easily as a
// million, and net.ParseIP walks every byte of what it is handed:
// measured on the pre-limiter path, a 1 MiB single entry cost 1.8 ms per
// call against 7 us for a full 32-hop chain — 245 times the bound, from
// one unauthenticated request.
//
// The allocation ratio above cannot see that shape: slicing a substring
// allocates nothing, so a megabyte in one entry moves that ratio the
// WRONG way (72 B/call against 1264 for the bound) and the witness stays
// green at any magnitude. These are signals instead — an entry no address
// could be is no hop, the reader stops looking past the bytes a full
// chain needs, and a window opening mid-entry invents no address —
// which no clock decides. After them every megabyte-sized shape costs
// 418-670 ns per call, under the 8 us a real 32-hop chain pays.
func TestAForwardedEntryTooLongToBeAnAddressIsNoHop(t *testing.T) {
	giant := strings.Repeat("A", 1<<20)
	chain := func(declared, value string) []string {
		s := &Server{cfg: Config{TrustedProxyCIDRs: []string{"10.0.0.0/8"}, ForwardedHeader: declared}}
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.RemoteAddr = "10.1.1.1:443"
		r.Header.Set(declared, value)
		return s.forwardedChain(r)
	}
	for _, c := range []struct {
		name, declared, value string
		want                  []string
	}{
		{"a megabyte with no separator", "X-Forwarded-For", giant, []string{}},
		{"a megabyte with one colon in it", "X-Forwarded-For", giant[:len(giant)-1] + ":", []string{}},
		// Past maxForwardedBytes the reader stops looking, so a
		// megabyte wedged between the hops buries what is BEHIND it —
		// the padding client loses its own entry and lands on the hops
		// nearest us, which is less reach and not more. The hop an
		// appending proxy writes is the last byte of the value, so no
		// padding can push that one out.
		{"a megabyte between the hops buries what is behind it", "X-Forwarded-For", "10.0.0.5," + giant + ",10.0.0.7", []string{"10.0.0.7"}},
		{"a megabyte on the far side leaves our end alone", "X-Forwarded-For", giant + ",10.0.0.5,10.0.0.7", []string{"10.0.0.5", "10.0.0.7"}},
		// An RFC 7239 element the window cannot delimit may be the
		// proxy's own, so it ends the chain as a hop that names no client
		// rather than vanishing (TestATruncatedProxyElementEndsTheRead).
		{"an RFC 7239 for= of a megabyte", "Forwarded", "for=" + giant, []string{unnamedHop}},
		{"the same beside a real element", "Forwarded", "for=" + giant + ",for=10.0.0.7", []string{unnamedHop, "10.0.0.7"}},
	} {
		got := chain(c.declared, c.value)
		if fmt.Sprint(got) == fmt.Sprint(c.want) {
			continue
		}
		// What is kept is reported by LENGTH: an unbounded entry is a
		// megabyte, and a failure must not become the log.
		sizes := []int{}
		for _, entry := range got {
			sizes = append(sizes, len(entry))
		}
		t.Errorf("%s: the chain keeps %d entries of %v bytes, want %v — net.ParseIP walks every byte of an entry, on a path that runs before the limiter has a key to refuse the request with", c.name, len(got), sizes, c.want)
	}

	// A chain of whole entries is read from our end, and every entry it
	// keeps is one a writer of the header delimited.
	padded := strings.Repeat("203.0.113.5,", maxForwardedBytes/len("203.0.113.5,")+4) + "10.0.0.7"
	windowed := chain("X-Forwarded-For", padded)
	if len(windowed) != maxForwardedHops {
		t.Errorf("a %d-byte chain of whole entries keeps %d of them, want %d", len(padded), len(windowed), maxForwardedHops)
	}
	for i, entry := range windowed {
		if entry != "203.0.113.5" && entry != "10.0.0.7" {
			t.Errorf("entry %d of the windowed chain is %q, which is neither entry of the header", i, entry)
		}
	}
	if len(windowed) > 0 && windowed[len(windowed)-1] != "10.0.0.7" {
		t.Errorf("the hop nearest us is %q, want 10.0.0.7 — the last byte of the value is the hop an appending proxy wrote", windowed[len(windowed)-1])
	}

	// The window's left edge lands wherever the arithmetic puts it, and
	// with few entries to its right it reaches a SHORT one: two bytes
	// into "203.0.113.5," it would hand the walk "3.0.113.5", an
	// address NOBODY wrote — outside the ranges, so it becomes the
	// rate-limit key and the audit row. The window opens at the first
	// separator instead. The edge is built from the constants so it
	// stays the edge when they move.
	head, tail := "203.0.113.5,", ",10.0.0.7"
	edge := head + strings.Repeat("B", maxForwardedBytes+2-len(head)-len(tail)) + tail
	if got := chain("X-Forwarded-For", edge); fmt.Sprint(got) != fmt.Sprint([]string{"10.0.0.7"}) {
		t.Errorf("the chain of a %d-byte value whose window opens two bytes into %q is %v, want [10.0.0.7]: an entry no writer of the header delimited", len(edge), head, got)
	}
	s := &Server{cfg: Config{TrustedProxyCIDRs: []string{"10.0.0.0/8"}}}
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "10.1.1.1:443"
	r.Header.Set("X-Forwarded-For", edge)
	if got := s.clientIP(r); got != "10.0.0.7" {
		t.Errorf("the identity is %q, want 10.0.0.7 — a truncated entry became the client", got)
	}

	// A bound that refuses an address would be the other defect. The
	// longest spelling canonicalIP accepts is an IPv4-mapped IPv6
	// address written out, bracketed and ported.
	longest := "[0000:0000:0000:0000:0000:ffff:255.255.255.255]:65535"
	if len(longest) > maxAddrLen {
		t.Fatalf("maxAddrLen is %d and the longest address form is %d bytes (%q): the bound refuses an address", maxAddrLen, len(longest), longest)
	}
	if got := canonicalIP(longest); got != "255.255.255.255" {
		t.Fatalf("canonicalIP(%q) = %q, want 255.255.255.255 — the length above is only a bound on addresses if this form is one", longest, got)
	}
	if got := chain("X-Forwarded-For", longest); fmt.Sprint(got) != fmt.Sprint([]string{longest}) {
		t.Errorf("the longest address form is dropped from the chain: got %v, want %v", got, []string{longest})
	}
}

// The boot warning fires only on an EMPTY list, and the outcome it exists
// to announce — every request keyed on the proxy's own address, one auth
// bucket for the whole instance — is identical when the list is non-empty
// but misses the real peer: a Service CIDR instead of a pod CIDR, a
// node-SNAT shape, an Istio-style sidecar whose inbound peer is
// 127.0.0.6. That is the likelier operator error now that the chart ships
// a default to narrow, and it is the one nothing in the logs shows.
func TestTheIgnoredForwardedAddressIsReportedOnce(t *testing.T) {
	warnings := func(out string) []string {
		lines := []string{}
		for _, line := range strings.Split(out, "\n") {
			if strings.Contains(line, "server: ") {
				lines = append(lines, line)
			}
		}
		return lines
	}

	t.Run("an out-of-range peer sending a forwarded header is named once", func(t *testing.T) {
		out := &lockedBuffer{}
		s := &Server{
			cfg:    Config{TrustedProxyCIDRs: []string{"10.0.0.0/8"}},
			logger: iterlog.New(iterlog.LevelWarn, out),
		}
		for range 2 {
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			r.RemoteAddr = "192.0.2.7:51000"
			r.Header.Set("X-Forwarded-For", "203.0.113.5")
			if got := s.clientIP(r); got != "192.0.2.7" {
				t.Fatalf("got %q, want the socket host 192.0.2.7", got)
			}
		}
		got := warnings(out.String())
		if len(got) != 1 {
			t.Fatalf("two such requests produced %d warning lines, want exactly 1:\n%s", len(got), strings.Join(got, "\n"))
		}
		for _, want := range []string{"192.0.2.7", "ITERION_AUTH_TRUSTED_PROXY_CIDRS", "10.0.0.0/8"} {
			if !strings.Contains(got[0], want) {
				t.Errorf("the warning does not name %s, so the operator cannot act on it:\n%s", want, got[0])
			}
		}
	})

	// The premise of that branch is "the request carries a forwarded
	// mechanism at all", and the mechanism this server reads is the
	// DECLARED one — which for a CDN deployment is on no candidate list.
	// Testing the three candidates alone left the diagnosis silent for
	// exactly the operator who had to declare a header: same
	// misconfiguration, same instance-wide bucket, no line.
	t.Run("the declared header counts as a forwarded mechanism", func(t *testing.T) {
		for _, declared := range []string{"CF-Connecting-IP", "True-Client-IP", "X-Forwarded-For"} {
			out := &lockedBuffer{}
			s := &Server{
				cfg:    Config{TrustedProxyCIDRs: []string{"10.0.0.0/8"}, ForwardedHeader: declared},
				logger: iterlog.New(iterlog.LevelWarn, out),
			}
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			r.RemoteAddr = "192.0.2.7:51000" // outside the ranges: the likely pod-CIDR mistake
			r.Header.Set(declared, "198.51.100.9")
			if got := s.clientIP(r); got != "192.0.2.7" {
				t.Fatalf("%s: got %q, want the socket host 192.0.2.7", declared, got)
			}
			got := warnings(out.String())
			if len(got) != 1 {
				t.Fatalf("a peer outside the ranges carrying the declared %s produced %d warning lines, want exactly 1:\n%s", declared, len(got), strings.Join(got, "\n"))
			}
			if !strings.Contains(got[0], "ITERION_AUTH_TRUSTED_PROXY_CIDRS") {
				t.Errorf("%s: the warning does not name the ranges:\n%s", declared, got[0])
			}
		}
	})

	// The twin cause: the ranges are right, the peer is ours, and the
	// header the operator declared is not the one the proxy writes. Every
	// ordinary request carries the diagnosis — the forged-header shape is
	// the rare one.
	t.Run("a proxy writing another mechanism is named once", func(t *testing.T) {
		out := &lockedBuffer{}
		s := &Server{
			cfg:    Config{TrustedProxyCIDRs: []string{"10.0.0.0/8"}},
			logger: iterlog.New(iterlog.LevelWarn, out),
		}
		for range 2 {
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			r.RemoteAddr = "10.1.1.1:443"
			r.Header.Set("X-Real-IP", "203.0.113.5")
			r.Header.Set("Forwarded", "for=203.0.113.5")
			if got := s.clientIP(r); got != "10.1.1.1" {
				t.Fatalf("got %q, want the peer host 10.1.1.1", got)
			}
		}
		got := warnings(out.String())
		if len(got) != 1 {
			t.Fatalf("two such requests produced %d warning lines, want exactly 1:\n%s", len(got), strings.Join(got, "\n"))
		}
		// Both mechanisms present, both named: picking one would be a
		// guess, and the operator has to choose the header their proxy
		// writes.
		for _, want := range []string{"10.1.1.1", "X-Real-IP", "Forwarded", "ITERION_AUTH_FORWARDED_HEADER"} {
			if !strings.Contains(got[0], want) {
				t.Errorf("the warning does not name %s:\n%s", want, got[0])
			}
		}
	})

	// The third cause, and the one that is on EVERY request rather than
	// the rare forged one: the ranges are right, the peer is ours, the
	// declared header is there — and it carries no address. The boot
	// validation accepts any field name on purpose (a CDN's
	// CF-Connecting-IP has to work), so a proxy writing a hostname, a
	// non-"Forwarded" RFC 7239 name and a mistyped neighbour like
	// X-Forwarded-Proto all pass it and land as the instance-wide
	// bucket. PRESENT is not READ.
	t.Run("a declared header that names no address is named once", func(t *testing.T) {
		for _, c := range []struct{ declared, value string }{
			{"X-Real-IP", "proxy-7.internal"},
			{"Forwarded-For", "for=203.0.113.5"},
			{"X-Forwarded-Proto", "https"},
		} {
			out := &lockedBuffer{}
			s := &Server{
				cfg:    Config{TrustedProxyCIDRs: []string{"10.0.0.0/8"}, ForwardedHeader: c.declared},
				logger: iterlog.New(iterlog.LevelWarn, out),
			}
			for range 2 {
				r := httptest.NewRequest(http.MethodGet, "/", nil)
				r.RemoteAddr = "10.1.1.1:443"
				r.Header.Set(c.declared, c.value)
				if got := s.clientIP(r); got != "10.1.1.1" {
					t.Fatalf("%s: got %q, want the peer host 10.1.1.1", c.declared, got)
				}
			}
			got := warnings(out.String())
			if len(got) != 1 {
				t.Fatalf("%s carrying %q: two such requests produced %d warning lines, want exactly 1:\n%s", c.declared, c.value, len(got), strings.Join(got, "\n"))
			}
			for _, want := range []string{"10.1.1.1", c.declared, c.value, "ITERION_AUTH_FORWARDED_HEADER"} {
				if !strings.Contains(got[0], want) {
					t.Errorf("the warning does not name %s, so the operator cannot act on it:\n%s", want, got[0])
				}
			}
		}
	})

	// A client cannot put a CRLF or a megabyte of its own into the log
	// line it triggers: the value is quoted and cut.
	t.Run("the value a client wrote reaches the line quoted and cut", func(t *testing.T) {
		out := &lockedBuffer{}
		s := &Server{
			cfg:    Config{TrustedProxyCIDRs: []string{"10.0.0.0/8"}, ForwardedHeader: "X-Real-IP"},
			logger: iterlog.New(iterlog.LevelWarn, out),
		}
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.RemoteAddr = "10.1.1.1:443"
		// Header.Set refuses a raw CRLF, so the forged line arrives the
		// way a non-net/http front end would hand it over.
		r.Header["X-Real-Ip"] = []string{"nope\r\nWARN server: forged line " + strings.Repeat("z", 1<<20)}
		s.clientIP(r)
		line := out.String()
		if strings.Count(strings.TrimRight(line, "\n"), "\n") != 0 {
			t.Errorf("the client's CRLF split the log into several lines:\n%q", line)
		}
		if len(line) > 1024 {
			t.Errorf("the line is %d bytes: a client sized the log", len(line))
		}
	})

	// The fourth cause, and the one the hop bound itself creates: a
	// SECOND appending proxy of the operator's own (a mesh sidecar, an
	// internal L7 proxy) that the declared count does not reach. The
	// walk stops at the hop nearest us, so every client behind the
	// undeclared one shares its bucket and its audit IP — the silence
	// this diagnosis exists to break.
	t.Run("an undeclared appending hop is named once", func(t *testing.T) {
		out := &lockedBuffer{}
		s := &Server{
			cfg:    Config{TrustedProxyCIDRs: []string{"10.0.0.0/8"}},
			logger: iterlog.New(iterlog.LevelWarn, out),
		}
		for range 2 {
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			r.RemoteAddr = "10.1.1.1:443"
			r.Header.Set("X-Forwarded-For", "203.0.113.5, 10.0.0.7, 10.0.0.8")
			if got := s.clientIP(r); got != "10.0.0.8" {
				t.Fatalf("got %q, want the hop the one declared count reaches, 10.0.0.8", got)
			}
		}
		got := warnings(out.String())
		if len(got) != 1 {
			t.Fatalf("two such requests produced %d warning lines, want exactly 1:\n%s", len(got), strings.Join(got, "\n"))
		}
		for _, want := range []string{"10.1.1.1", "10.0.0.7", "ITERION_AUTH_TRUSTED_PROXY_HOPS", "ITERION_AUTH_TRUSTED_PROXY_CIDRS"} {
			if !strings.Contains(got[0], want) {
				t.Errorf("the warning does not name %s, so the operator cannot act on it:\n%s", want, got[0])
			}
		}
	})

	// And THAT diagnosis stays silent for the shape it must not be
	// confused with: an in-range client whose forged entry is outside
	// the ranges. The bound refusing a forgery is not a topology to
	// report, and a line naming the hop count per forging client would
	// send the operator raising it. The identity landing on one of our
	// own addresses is reported — once, below — because the two shapes
	// are indistinguishable by content, but never with that remedy.
	t.Run("a client's out-of-range entry past the bound names no hop count", func(t *testing.T) {
		out := &lockedBuffer{}
		s := &Server{
			cfg:    Config{TrustedProxyCIDRs: []string{"10.0.0.0/8"}},
			logger: iterlog.New(iterlog.LevelWarn, out),
		}
		for range 2 {
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			r.RemoteAddr = "10.1.1.1:443"
			r.Header.Set("X-Forwarded-For", "203.0.113.77, 10.2.2.2")
			if got := s.clientIP(r); got != "10.2.2.2" {
				t.Fatalf("got %q, want 10.2.2.2", got)
			}
		}
		got := warnings(out.String())
		if len(got) != 1 {
			t.Fatalf("two such requests produced %d warning lines, want exactly 1:\n%s", len(got), strings.Join(got, "\n"))
		}
		if strings.Contains(got[0], "ITERION_AUTH_TRUSTED_PROXY_HOPS declares, and that address is inside") {
			t.Errorf("a forged out-of-range entry was reported as an undeclared hop, which tells the operator to raise the count a forgery just exploited:\n%s", got[0])
		}
	})

	// A deployment that works says nothing, or the line is noise nobody
	// reads: the configured range holds the peer and the declared header
	// carries a client outside it.
	t.Run("the healthy shape stays silent", func(t *testing.T) {
		out := &lockedBuffer{}
		s := &Server{
			cfg:    Config{TrustedProxyCIDRs: []string{"10.0.0.0/8"}},
			logger: iterlog.New(iterlog.LevelWarn, out),
		}
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.RemoteAddr = "10.1.1.1:443"
		r.Header.Set("X-Forwarded-For", "203.0.113.5")
		s.clientIP(r)
		if got := warnings(out.String()); len(got) != 0 {
			t.Errorf("the declared header carrying the client warned %d time(s):\n%s", len(got), strings.Join(got, "\n"))
		}
	})

	// With no range configured the BOOT warning is the one that speaks,
	// and it already does: a second line per request would double it for
	// every deployment that trusts nothing on purpose.
	t.Run("an empty list leaves the boot warning to speak", func(t *testing.T) {
		out := &lockedBuffer{}
		s := &Server{cfg: Config{}, logger: iterlog.New(iterlog.LevelWarn, out)}
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.RemoteAddr = "192.0.2.7:51000"
		r.Header.Set("X-Forwarded-For", "203.0.113.5")
		s.clientIP(r)
		if got := warnings(out.String()); len(got) != 0 {
			t.Errorf("a server trusting no range warned per request:\n%s", strings.Join(got, "\n"))
		}
	})
}

// forwardedChain must read the header the operator named, whatever its
// case, and the RFC 7239 grammar only for that one name.
func TestForwardedChainReadsTheDeclaredHeader(t *testing.T) {
	cases := []struct {
		declared string
		headers  map[string]string
		want     []string
	}{
		{"", map[string]string{"X-Forwarded-For": "1.1.1.1, 2.2.2.2"}, []string{"1.1.1.1", "2.2.2.2"}},
		{"X-Forwarded-For", map[string]string{"X-Forwarded-For": "1.1.1.1"}, []string{"1.1.1.1"}},
		{"x-real-ip", map[string]string{"X-Real-IP": "3.3.3.3", "X-Forwarded-For": "9.9.9.9"}, []string{"3.3.3.3"}},
		{"forwarded", map[string]string{"Forwarded": "for=4.4.4.4;proto=https"}, []string{"4.4.4.4"}},
		{"X-Real-IP", map[string]string{"X-Forwarded-For": "9.9.9.9"}, []string{}},
	}
	for _, c := range cases {
		s := &Server{cfg: Config{ForwardedHeader: c.declared}}
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		for k, v := range c.headers {
			r.Header.Set(k, v)
		}
		got := s.forwardedChain(r)
		if fmt.Sprint(got) != fmt.Sprint(c.want) {
			t.Errorf("declared %q over %v: got %v, want %v", c.declared, c.headers, got, c.want)
		}
	}
}

// lastEntries keeps the entries NEAREST the server, in the order the
// header spells them, across the several lines a client and a proxy both
// wrote — and drops the empty ones without letting them spend the hop
// budget, since a header of separators alone names no hop.
func TestLastEntriesKeepsTheEntriesNearestUs(t *testing.T) {
	cases := []struct {
		name   string
		values []string
		keep   int
		want   []string
	}{
		{"two lines are one chain", []string{"9.9.9.9", "1.1.1.1, 2.2.2.2"}, 32, []string{"9.9.9.9", "1.1.1.1", "2.2.2.2"}},
		{"the bound keeps our end", []string{"1.1.1.1, 2.2.2.2, 3.3.3.3"}, 2, []string{"2.2.2.2", "3.3.3.3"}},
		{"empty entries are dropped", []string{", 1.1.1.1, , 2.2.2.2"}, 32, []string{"1.1.1.1", "2.2.2.2"}},
		{"separators alone name nobody", []string{" , , "}, 32, []string{}},
		{"no separator at all", []string{" 1.1.1.1 "}, 32, []string{"1.1.1.1"}},
		{"nothing at all", nil, 32, []string{}},
	}
	for _, c := range cases {
		b := newForwardedBudget()
		b.keep = c.keep
		if got := lastEntries(c.values, ',', maxAddrLen, plainCut, b); fmt.Sprint(got) != fmt.Sprint(c.want) {
			t.Errorf("%s: lastEntries(%q, keep=%d) = %v, want %v", c.name, c.values, c.keep, got, c.want)
		}
	}
}

// The hop budget and the work budget are different budgets, and a client
// writes what spends them. Conflated, the one that decides the identity
// is spent by entries that are no hops at all:
//
//   - a run of separators to the RIGHT of the only address in the value
//     (40 empties behind "203.0.113.5" is a malformed chain, never 40
//     hops) buried that address behind a budget of 32, while the same
//     run on the LEFT was harmless — an identity that depended on which
//     side of the hop the padding sat;
//   - a value whose window holds no separator yields nothing, so it
//     charged nothing: five hundred such lines cost five hundred window
//     scans on the path that runs BEFORE the limiter has a key to refuse
//     the request with.
//
// Both are witnessed as SIGNALS — which entries come back, and whether
// the far line is reached at all — because a verdict read off a clock
// measures the machine.
func TestTheHopBudgetIsNotSpentOnWhatIsNoHop(t *testing.T) {
	t.Run("empties behind the hop do not bury it, on either side", func(t *testing.T) {
		empties := strings.Repeat(",", maxForwardedHops+8)
		for name, value := range map[string]string{
			"the padding is to the right of the address": "203.0.113.5" + empties,
			"the padding is to the left of the address":  empties + "203.0.113.5",
		} {
			b := newForwardedBudget()
			got := lastEntries([]string{value}, ',', maxAddrLen, plainCut, b)
			if fmt.Sprint(got) != fmt.Sprint([]string{"203.0.113.5"}) {
				t.Errorf("%s: %d empty entries beside one address yield %v, want [203.0.113.5] — the empties spent the hop budget", name, maxForwardedHops+8, got)
			}
		}
	})

	// An entry too long to be an address is the same class: it is a
	// position looked at, not a hop kept, so a run of them must not push
	// the hop a proxy appended out of the budget either.
	t.Run("entries too long to be an address do not bury the hop", func(t *testing.T) {
		junk := strings.Repeat(strings.Repeat("A", maxAddrLen+1)+",", maxForwardedHops+8)
		b := newForwardedBudget()
		got := lastEntries([]string{junk + "203.0.113.5"}, ',', maxAddrLen, plainCut, b)
		if fmt.Sprint(got) != fmt.Sprint([]string{"203.0.113.5"}) {
			sizes := []int{}
			for _, entry := range got {
				sizes = append(sizes, len(entry))
			}
			t.Errorf("%d over-long entries beside one address yield %d entries of %v bytes, want [203.0.113.5]", maxForwardedHops+8, len(got), sizes)
		}
	})

	// The scan budget, witnessed by what it refuses to reach: past
	// maxForwardedScans lines the reader stops, so an address sitting
	// BEHIND more than that many separator-free windows is not read and
	// the identity falls back to the socket peer — the padding client
	// losing its own reach, as everywhere else. Uncharged, the walk
	// crossed every line of the value however many there were.
	t.Run("a separator-free line is a scan spent", func(t *testing.T) {
		line := strings.Repeat("A", maxForwardedBytes+20)
		for _, c := range []struct {
			name  string
			lines int
			want  []string
		}{
			{"under the scan budget the far address is still read", maxForwardedScans - 2, []string{"203.0.113.5"}},
			{"past it the reader stops before reaching it", 4 * maxForwardedScans, []string{}},
		} {
			values := append([]string{"203.0.113.5"}, slices.Repeat([]string{line}, c.lines)...)
			b := newForwardedBudget()
			if got := lastEntries(values, ',', maxAddrLen, plainCut, b); fmt.Sprint(got) != fmt.Sprint(c.want) {
				t.Errorf("%s: an address behind %d separator-free lines of %d bytes yields %v, want %v", c.name, c.lines, len(line), got, c.want)
			}
		}
	})
}

// The shape the chart's own default lands in, and the one nothing in the
// logs showed. With the Kubernetes-default `externalTrafficPolicy:
// Cluster` the Service SNATs the client address to the node, so the
// client the limiter keys on is the operator's own infrastructure:
//
//   - A: an ingress controller that REPLACES the chain (ingress-nginx
//     with `use-forwarded-headers: false`, its default) writes the node
//     address as the client. Narrowing trustedProxyCidrs cannot help —
//     the real client is not in the request at all.
//   - B: one that APPENDS (`use-forwarded-headers` with
//     `compute-full-forwarded-for`) writes `<what arrived>, <node>` —
//     the client's real address only when an L7 load balancer ahead of
//     the Service wrote it — and the hop bound left at 1 answers with
//     the node: the client IS in the request and is discarded.
//
// Both put the whole internet on one bucket per node and one audit IP.
// No diagnosis keyed on the chain's shape can see them: an in-cluster
// pod the NetworkPolicy admits produces identical requests. So the one
// that does is keyed on the ANSWER — if the identity finally read is
// itself inside the trusted ranges, this instance is keyed on its own
// infrastructure, whichever of the two causes it is — and it names both.
func TestAnIdentityInsideOurOwnRangesIsReportedOnce(t *testing.T) {
	warnings := func(out string) []string {
		lines := []string{}
		for _, line := range strings.Split(out, "\n") {
			if strings.Contains(line, "server: ") {
				lines = append(lines, line)
			}
		}
		return lines
	}
	// The chart's shipped default, verbatim: all of private space, one
	// appending hop.
	chartDefault := Config{TrustedProxyCIDRs: []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "fc00::/7"}, TrustedProxyHops: 1}

	for _, c := range []struct {
		name, chain, want string
	}{
		{"shape A: a replacing controller behind a SNAT Service", "10.5.0.7", "10.5.0.7"},
		{"shape B: an appending controller behind a writer of the client, one hop declared", "203.0.113.1, 10.5.0.7", "10.5.0.7"},
		{"the trusted peer forwards nothing at all", "", "10.244.0.5"},
	} {
		t.Run(c.name, func(t *testing.T) {
			out := &lockedBuffer{}
			s := &Server{cfg: chartDefault, logger: iterlog.New(iterlog.LevelWarn, out)}
			for range 3 {
				r := httptest.NewRequest(http.MethodPost, "/api/auth/register", nil)
				r.RemoteAddr = "10.244.0.5:40001" // the ingress controller pod
				if c.chain != "" {
					r.Header.Set("X-Forwarded-For", c.chain)
				}
				if got := s.clientIP(r); got != c.want {
					t.Fatalf("the identity is %q, want %q — this witness needs the shape it reports", got, c.want)
				}
			}
			got := warnings(out.String())
			if len(got) != 1 {
				t.Fatalf("three such requests produced %d warning lines, want exactly 1:\n%s", len(got), strings.Join(got, "\n"))
			}
			// Actionable: the address keyed on, the variable that holds
			// the ranges, the cause the operator cannot see from inside
			// the pod, and where the remedies are written.
			for _, want := range []string{c.want, "ITERION_AUTH_TRUSTED_PROXY_CIDRS", "externalTrafficPolicy: Cluster", "docs/cloud-deployment.md"} {
				if !strings.Contains(got[0], want) {
					t.Errorf("the warning does not name %s, so the operator cannot act on it:\n%s", want, got[0])
				}
			}
		})
	}

	// The healthy internet-facing shape says nothing: the answer is a
	// client outside every range, which is what the ranges are for.
	t.Run("an answer outside the ranges is silent", func(t *testing.T) {
		out := &lockedBuffer{}
		s := &Server{cfg: chartDefault, logger: iterlog.New(iterlog.LevelWarn, out)}
		r := httptest.NewRequest(http.MethodPost, "/api/auth/register", nil)
		r.RemoteAddr = "10.244.0.5:40001"
		r.Header.Set("X-Forwarded-For", "203.0.113.1, 198.51.100.9")
		if got := s.clientIP(r); got != "198.51.100.9" {
			t.Fatalf("the identity is %q, want 198.51.100.9", got)
		}
		if got := warnings(out.String()); len(got) != 0 {
			t.Errorf("a client outside every range warned %d time(s):\n%s", len(got), strings.Join(got, "\n"))
		}
	})

	// And it is a net under the specific causes, not a line beside them:
	// a declared header the proxy does not write reports the same
	// outcome WITH its remedy, and hearing it twice would teach the
	// operator to skip both.
	t.Run("a specific cause leaves no second line", func(t *testing.T) {
		out := &lockedBuffer{}
		s := &Server{
			cfg:    Config{TrustedProxyCIDRs: []string{"10.0.0.0/8"}, ForwardedHeader: "X-Real-IP"},
			logger: iterlog.New(iterlog.LevelWarn, out),
		}
		for range 2 {
			r := httptest.NewRequest(http.MethodPost, "/api/auth/register", nil)
			r.RemoteAddr = "10.1.1.1:443"
			r.Header.Set("X-Forwarded-For", "203.0.113.5")
			s.clientIP(r)
		}
		got := warnings(out.String())
		if len(got) != 1 {
			t.Fatalf("a proxy writing another mechanism produced %d warning lines, want exactly 1:\n%s", len(got), strings.Join(got, "\n"))
		}
		if !strings.Contains(got[0], "ITERION_AUTH_FORWARDED_HEADER") {
			t.Errorf("the line that survived is not the one carrying the remedy:\n%s", got[0])
		}
	})
}

// An over-declared hop count is a silent forgery: the walk never reaches
// the bound, falls into the client's half of the chain, and the identity
// moves with what the client writes. The chain cannot supply the hops
// declared, and that is a fact about the chain — so it is said.
//
// The test is hops-1, not hops: N appending proxies write N entries of
// which the LEFTMOST is the client's own address, so a correctly
// declared chain whose client sits outside the ranges crosses exactly
// N-1 entries of ours. Warning at `ours < hops` would fire on every
// legitimate chained front end — the configuration the docs ask for.
func TestAHopCountTheChainCannotSupplyIsReportedOnce(t *testing.T) {
	warnings := func(out string) []string {
		lines := []string{}
		for _, line := range strings.Split(out, "\n") {
			if strings.Contains(line, "ITERION_AUTH_TRUSTED_PROXY_HOPS declares need at least") {
				lines = append(lines, line)
			}
		}
		return lines
	}
	send := func(t *testing.T, hops int, chain, want string) []string {
		t.Helper()
		out := &lockedBuffer{}
		s := &Server{
			cfg:    Config{TrustedProxyCIDRs: []string{"10.0.0.0/8"}, TrustedProxyHops: hops},
			logger: iterlog.New(iterlog.LevelWarn, out),
		}
		for range 2 {
			r := httptest.NewRequest(http.MethodPost, "/api/auth/login", nil)
			r.RemoteAddr = "10.1.1.1:443" // the one appending proxy really in front
			r.Header.Set("X-Forwarded-For", chain)
			if got := s.clientIP(r); got != want {
				t.Fatalf("hops=%d over %q: the identity is %q, want %q — this witness needs the shape it reports", hops, chain, got, want)
			}
		}
		return warnings(out.String())
	}

	// Over-declared: two appending hops announced, one really in front.
	// The bound never arbitrates, and a client whose own address were
	// in-range would be writing the identity.
	t.Run("two hops declared, one proxy in front", func(t *testing.T) {
		got := send(t, 2, "203.0.113.77, 198.51.100.9", "198.51.100.9")
		if len(got) != 1 {
			t.Fatalf("two such requests produced %d lines, want exactly 1:\n%s", len(got), strings.Join(got, "\n"))
		}
		for _, want := range []string{"10.1.1.1", "ITERION_AUTH_TRUSTED_PROXY_HOPS", "10.0.0.0/8", "docs/environment-variables.md"} {
			if !strings.Contains(got[0], want) {
				t.Errorf("the line does not name %s:\n%s", want, got[0])
			}
		}
	})

	t.Run("three hops declared, one proxy in front", func(t *testing.T) {
		if got := send(t, 3, "203.0.113.77, 198.51.100.9", "198.51.100.9"); len(got) != 1 {
			t.Errorf("got %d lines, want exactly 1:\n%s", len(got), strings.Join(got, "\n"))
		}
	})

	// The shapes it must NOT fire on, or the operator learns to skip it:
	// the chart's default, and the chained front end the docs describe —
	// a CDN edge appending ahead of the ingress, hops=2, where the
	// client's own address is the second entry from the right and the
	// walk legitimately crosses only ONE of ours.
	t.Run("the healthy counts stay silent", func(t *testing.T) {
		for _, c := range []struct {
			name  string
			hops  int
			chain string
			want  string
		}{
			{"one hop, one appending proxy", 1, "203.0.113.77, 198.51.100.9", "198.51.100.9"},
			{"two hops, two appending proxies", 2, "203.0.113.77, 198.51.100.9, 10.2.2.2", "198.51.100.9"},
			{"two hops, two appending proxies, in-range client", 2, "203.0.113.77, 10.9.9.9, 10.2.2.2", "10.9.9.9"},
		} {
			if got := send(t, c.hops, c.chain, c.want); len(got) != 0 {
				t.Errorf("%s warned %d time(s) about a count the chain supplies:\n%s", c.name, len(got), strings.Join(got, "\n"))
			}
		}
	})
}

// The RFC 7239 reader cuts twice from one header — elements on the
// comma, then each element's parameters on the semicolon — and the two
// passes spent ONE purse. A client could therefore spend it all in the
// first pass on elements that are no hops — 255 empty elements, 63 empty
// lines, 300 empty parameters of its own — and the proxy's element,
// collected intact at the very first look, yielded no for= in the
// second: the chain was empty, the identity fell to the socket peer, and
// the socket peer is the trusted proxy. One bucket for everyone behind
// it, the proxy's address in the audit row, the proxy's bucket drained
// by one client — at the client's choosing, through the one header whose
// grammar makes the read two-pass. X-Forwarded-For was never exposed:
// its single pass keeps the proxy's entry at the first look.
//
// The parameters now come from a purse of their own, read from the
// element nearest us outward, so what exhaustion costs is the client's
// far elements and never the one its proxy appended. Each shape is
// sized off the constants so it stays an attack when they move.
func TestTheProxysForwardedElementIsReadWhateverTheClientPadded(t *testing.T) {
	const proxy = "for=203.0.113.5"
	for _, c := range []struct {
		name   string
		values []string
		hops   int
	}{
		{"empty elements left of the proxy's spend the first pass's looks", []string{strings.Repeat(",", maxForwardedLooks-1) + proxy}, 1},
		{"empty parameters inside the client's own element spend the second's", []string{"for=198.51.100.1" + strings.Repeat(";", maxForwardedLooks+44) + ", " + proxy}, 1},
		{"empty lines of the client's spend the first pass's scans", append(slices.Repeat([]string{""}, maxForwardedScans-1), proxy), 1},
		{"two declared hops behind the padding", []string{strings.Repeat(",", maxForwardedLooks+44) + ", " + proxy + ", for=10.9.9.9"}, 2},
	} {
		t.Run(c.name, func(t *testing.T) {
			out := &lockedBuffer{}
			s := &Server{
				cfg:    Config{TrustedProxyCIDRs: []string{"10.0.0.0/8"}, ForwardedHeader: "Forwarded", TrustedProxyHops: c.hops},
				logger: iterlog.New(iterlog.LevelWarn, out),
			}
			r := httptest.NewRequest(http.MethodPost, "/api/auth/login", nil)
			r.RemoteAddr = "10.1.1.1:443" // the appending proxy, inside the range
			for _, v := range c.values {
				r.Header.Add("Forwarded", v)
			}
			if got := s.clientIP(r); got != "203.0.113.5" {
				t.Errorf("the identity is %q, want 203.0.113.5 — the client's padding starved the read of the element its proxy appended, and the identity collapsed onto the proxy", got)
			}
			if got := s.rateBucket(r); got != "203.0.113.5" {
				t.Errorf("the bucket is %q, want 203.0.113.5", got)
			}
			if lines := out.String(); strings.Contains(lines, "names no address") {
				t.Errorf("a chain that names the client was diagnosed as naming no address:\n%s", lines)
			}
		})
	}
}

// A chain SHORTER than the declared count whose every entry is one of
// ours reads no client — by design, the identity is the peer's own — and
// it was diagnosed as a header that "names no address", quoting a valid
// address and prescribing ITERION_AUTH_FORWARDED_HEADER, the one variable
// that cannot help. The shape is every in-cluster caller that reaches the
// ingress directly on a hops-2 deployment, and every request of a
// deployment whose front end does not append after all; and the line
// that fired stood the generic one down, so nothing in the process ever
// named the count. The cause is said as what it is: the count exceeds
// the appending proxies on this path.
func TestAChainShorterThanTheDeclaredCountIsNamedAsSuch(t *testing.T) {
	warnings := func(out string) []string {
		lines := []string{}
		for _, line := range strings.Split(out, "\n") {
			if strings.Contains(line, "server: ") {
				lines = append(lines, line)
			}
		}
		return lines
	}
	chartDefault := []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "fc00::/7"}
	for _, c := range []struct {
		name, header, value string
		hops                int
	}{
		{"two hops declared, one in-range entry", "X-Forwarded-For", "10.9.9.9", 2},
		{"the same through RFC 7239", "Forwarded", "for=10.9.9.9", 2},
		{"three hops declared, two in-range entries", "X-Forwarded-For", "192.168.1.50, 10.1.0.1", 3},
	} {
		t.Run(c.name, func(t *testing.T) {
			out := &lockedBuffer{}
			s := &Server{
				cfg:    Config{TrustedProxyCIDRs: chartDefault, TrustedProxyHops: c.hops, ForwardedHeader: c.header},
				logger: iterlog.New(iterlog.LevelWarn, out),
			}
			for range 2 {
				r := httptest.NewRequest(http.MethodPost, "/api/auth/login", nil)
				r.RemoteAddr = "10.244.0.5:40001" // the ingress controller pod
				r.Header.Set(c.header, c.value)
				if got := s.clientIP(r); got != "10.244.0.5" {
					t.Fatalf("the identity is %q, want the peer 10.244.0.5 — this witness needs the shape it reports", got)
				}
			}
			got := warnings(out.String())
			if len(got) != 1 {
				t.Fatalf("two such requests produced %d warning lines, want exactly 1:\n%s", len(got), strings.Join(got, "\n"))
			}
			if strings.Contains(got[0], "names no address") {
				t.Errorf("a chain of in-range address(es) was diagnosed as naming no address, with ITERION_AUTH_FORWARDED_HEADER as the remedy:\n%s", got[0])
			}
			// Actionable: the header, the peer, the addresses it named,
			// the count that exceeds them and where the remedy is.
			for _, want := range []string{c.header, "10.244.0.5", c.value, fmt.Sprintf("%d hops ITERION_AUTH_TRUSTED_PROXY_HOPS", c.hops), "ITERION_AUTH_TRUSTED_PROXY_CIDRS", "APPENDING proxies", "docs/environment-variables.md"} {
				if !strings.Contains(got[0], want) {
					t.Errorf("the line does not name %s, so the operator cannot act on it:\n%s", want, got[0])
				}
			}
		})
	}

	// The line it must not swallow: a declared header that really names
	// no address keeps its own diagnosis and its own remedy.
	t.Run("a header that names no address keeps its own line", func(t *testing.T) {
		out := &lockedBuffer{}
		s := &Server{
			cfg:    Config{TrustedProxyCIDRs: chartDefault, TrustedProxyHops: 2},
			logger: iterlog.New(iterlog.LevelWarn, out),
		}
		r := httptest.NewRequest(http.MethodPost, "/api/auth/login", nil)
		r.RemoteAddr = "10.244.0.5:40001"
		r.Header.Set("X-Forwarded-For", "proxy-7.internal")
		s.clientIP(r)
		got := warnings(out.String())
		if len(got) != 1 || !strings.Contains(got[0], "names no address") || !strings.Contains(got[0], "ITERION_AUTH_FORWARDED_HEADER") {
			t.Errorf("a header naming no address lost its line:\n%s", strings.Join(got, "\n"))
		}
	})

	// The same shape from a front end that named no client — an RFC
	// 7239 for=unknown ahead of the CDN's own entry, on the two-hop
	// deployment the docs describe — gets the same line, and the line
	// says so rather than prescribing a count that is right.
	t.Run("a front end that named no client gets the line, and the line says so", func(t *testing.T) {
		out := &lockedBuffer{}
		s := &Server{
			cfg:    Config{TrustedProxyCIDRs: chartDefault, TrustedProxyHops: 2, ForwardedHeader: "Forwarded"},
			logger: iterlog.New(iterlog.LevelWarn, out),
		}
		r := httptest.NewRequest(http.MethodPost, "/api/auth/login", nil)
		r.RemoteAddr = "10.244.0.5:40001"
		r.Header.Set("Forwarded", "for=unknown, for=10.9.9.9")
		if got := s.clientIP(r); got != "10.244.0.5" {
			t.Fatalf("the identity is %q, want the peer 10.244.0.5 — this witness needs the shape it reports", got)
		}
		got := warnings(out.String())
		if len(got) != 1 || !strings.Contains(got[0], "the front end named no client") {
			t.Errorf("a front end naming no client is reported with the over-declared count as the only cause:\n%s", strings.Join(got, "\n"))
		}
	})
}

// A quoted value carries whatever its writer put there, separators
// included: for="[2001:db8::1]:443" is the RFC's own spelling of an
// IPv6 address, and a proxy that writes the client's Host into host="…"
// — the hand-written nginx template `proxy_set_header Forwarded
// "for=$remote_addr;proto=$scheme;host=\"$host\""`, nginx accepting
// ';', ',' and '=' in a Host — puts client-typed bytes INSIDE the
// proxy's own element. Under plain cuts a ';' there ends the element
// with a for= the client wrote (a chosen identity, a fresh bucket per
// value), a quoted run of parameters pushes the proxy's own for= past
// the keep bound (the identity collapsing onto the proxy, one bucket for
// everyone behind it), and a ',' does the same through the elements
// cut. Every shape here is one request's header, its identity,
// and the bucket a second request with another injected address lands
// in: the same one. They are the shapes a client writes INSIDE a value
// its proxy quotes and escapes, and the only RFC 7239 separators nginx
// 1.29.4 and later let into a Host; behind an older nginx a template
// that does not escape DQUOTE also lets it close the string, which no
// reader undoes (lastSep), so no shape of that kind is pinned here.
func TestASeparatorInsideAQuotedValueCutsNothing(t *testing.T) {
	tenK := []string{"10.0.0.0/8"}
	cdn := []string{"10.0.0.0/8", "198.51.100.0/24"}
	pastKeep := strings.Repeat(";a", maxForwardedHops+1)
	for _, c := range []struct {
		name, value string
		hops        int
		cidrs       []string
		want        string
	}{
		{"a semicolon inside the proxy's quoted host", `for=203.0.113.5;host="x;for=9.9.9.9"`, 1, tenK, "203.0.113.5"},
		{"a quoted run of parameters past the keep bound", `for=203.0.113.5;host="x` + pastKeep + `"`, 1, tenK, "203.0.113.5"},
		{"the same behind the client's own element", `for=1.1.1.1, for=203.0.113.5;host="x` + pastKeep + `"`, 1, tenK, "203.0.113.5"},
		{"a quoted run of empty parameters past the look budget", `for=203.0.113.5;host="x` + strings.Repeat(";", maxForwardedLooks) + `"`, 1, tenK, "203.0.113.5"},
		{"HAProxy's parameter order behind a CDN, two hops", `for=203.0.113.5, proto=https;host="x;for=9.9.9.9";by=10.1.1.1;for=198.51.100.1`, 2, cdn, "203.0.113.5"},
		{"an escaped quote inside the value", `for=203.0.113.5;host="a\"b;for=9.9.9.9"`, 1, tenK, "203.0.113.5"},
		{"a comma inside the proxy's quoted host", `for=203.0.113.5;host="x,for=9.9.9.9"`, 1, tenK, "203.0.113.5"},
		// The proxy's own element outgrows the byte window when the host
		// it quotes is long: the window then opens inside the quotes, and
		// a comma there is no element boundary. The element is not one
		// the reader can delimit, so the identity is the proxy's own
		// address — never the for= the client typed inside its host.
		{"a comma inside a quoted host longer than the byte window", `for=203.0.113.5;host="` + strings.Repeat("a", maxForwardedBytes+20) + `,for=9.9.9.9"`, 1, tenK, "10.1.1.1"},
		{"a quoted for= still names its address", `for="[2001:db8::1]:443";proto=https`, 1, tenK, "2001:db8::1"},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := &Server{cfg: Config{TrustedProxyCIDRs: c.cidrs, ForwardedHeader: "Forwarded", TrustedProxyHops: c.hops}}
			buckets := map[string]bool{}
			for _, injected := range []string{"9.9.9.9", "9.9.9.8"} {
				r := httptest.NewRequest(http.MethodPost, "/api/auth/login", nil)
				r.RemoteAddr = "10.1.1.1:443" // the proxy that wrote the element, inside the range
				r.Header.Set("Forwarded", strings.ReplaceAll(c.value, "9.9.9.9", injected))
				if got := s.clientIP(r); got != c.want {
					t.Errorf("with %s injected the identity is %q, want %s: a separator inside the quoted value cut the proxy's element", injected, got, c.want)
				}
				buckets[s.rateBucket(r)] = true
			}
			if len(buckets) != 1 {
				t.Errorf("two requests differing only inside the quoted value landed in %d buckets, want 1", len(buckets))
			}
		})
	}
}

// A hop that names no client is still a hop, and nothing left of it is
// accountable. A front end that anonymises (RFC 7239 for=unknown or an
// obfuscated identifier), writes no for= at all, or an X-Forwarded-For
// "unknown", says it does not know the client; the element left of it
// is whatever the client sent. Dropping the hop instead walked straight
// into that element, so behind one correctly declared appending proxy
// the client chose its own identity and a fresh bucket per value. Every
// shape is two requests differing only in the client's own entry: both
// must land on the proxy's address, in one bucket.
func TestAHopThatNamesNoClientStopsTheWalk(t *testing.T) {
	longObfuscated := "_" + strings.Repeat("h", maxAddrLen)
	for _, c := range []struct {
		name, header, value string
		hops                int
	}{
		{"an RFC 7239 for=unknown", "Forwarded", "for=9.9.9.9, for=unknown", 1},
		{"an obfuscated identifier", "Forwarded", "for=9.9.9.9, for=_hidden", 1},
		{"an obfuscated identifier longer than any address", "Forwarded", "for=9.9.9.9, for=" + longObfuscated, 1},
		{"an element with no for= at all", "Forwarded", "for=9.9.9.9, proto=https;by=10.1.1.1", 1},
		{"an empty for=", "Forwarded", `for=9.9.9.9, for=""`, 1},
		{"an X-Forwarded-For unknown", "X-Forwarded-For", "9.9.9.9, unknown", 1},
		{"the outer of two declared hops", "Forwarded", "for=9.9.9.9, for=unknown, for=10.9.9.9", 2},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := &Server{cfg: Config{TrustedProxyCIDRs: []string{"10.0.0.0/8"}, ForwardedHeader: c.header, TrustedProxyHops: c.hops}}
			buckets := map[string]bool{}
			for _, forged := range []string{"9.9.9.9", "9.9.9.8"} {
				r := httptest.NewRequest(http.MethodPost, "/api/auth/login", nil)
				r.RemoteAddr = "10.1.1.1:443" // the appending proxy, inside the range
				r.Header.Set(c.header, strings.ReplaceAll(c.value, "9.9.9.9", forged))
				if got := s.clientIP(r); got != "10.1.1.1" {
					t.Errorf("with %s in the client's own entry the identity is %q, want the proxy 10.1.1.1: the walk stepped over a hop that names no client into what the client wrote", forged, got)
				}
				buckets[s.rateBucket(r)] = true
			}
			if len(buckets) != 1 {
				t.Errorf("two requests differing only in the client's own entry landed in %d buckets, want 1", len(buckets))
			}
		})
	}
}

// A proxy that adds its OWN Forwarded line puts every line before it in
// the client's hands. When the host it quotes outgrows the byte window,
// its element is one the reader cannot delimit; reading on into the
// earlier lines handed the identity to the client's own line — a chosen
// audit IP, a fresh bucket per value. The unreadable element is a hop
// that names no client and the read ends on it: the identity is the
// proxy's, and the chain carries nothing past the barrier.
func TestATruncatedProxyElementEndsTheRead(t *testing.T) {
	long := `for=203.0.113.5;host="` + strings.Repeat("a", maxForwardedBytes+920) + `"`
	for _, c := range []struct {
		name   string
		client []string
	}{
		{"one client line before the proxy's", []string{"for=9.9.9.9"}},
		{"several client elements and lines", []string{"for=198.51.100.7", "for=10.2.2.2, for=9.9.9.9"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := &Server{cfg: Config{TrustedProxyCIDRs: []string{"10.0.0.0/8"}, ForwardedHeader: "Forwarded"}}
			buckets := map[string]bool{}
			for _, forged := range []string{"9.9.9.9", "9.9.9.8"} {
				r := httptest.NewRequest(http.MethodPost, "/api/auth/login", nil)
				r.RemoteAddr = "10.1.1.1:443" // the proxy that added the last line, inside the range
				for _, line := range c.client {
					r.Header.Add("Forwarded", strings.ReplaceAll(line, "9.9.9.9", forged))
				}
				r.Header.Add("Forwarded", long)
				if got := s.clientIP(r); got != "10.1.1.1" {
					t.Errorf("with %s on the client's line the identity is %q, want the proxy 10.1.1.1: the read crossed an element it could not delimit into a line the client wrote", forged, got)
				}
				if got := s.forwardedChain(r); fmt.Sprint(got) != fmt.Sprint([]string{unnamedHop}) {
					t.Errorf("the chain is %v, want [%s]: nothing past an unreadable element is read", got, unnamedHop)
				}
				buckets[s.rateBucket(r)] = true
			}
			if len(buckets) != 1 {
				t.Errorf("two requests differing only in the client's own line landed in %d buckets, want 1", len(buckets))
			}
		})
	}
}

// The cut under quotedCut, pinned on the grammar it honours: a
// separator inside a quoted-string is part of the entry, a backslash
// escapes the DQUOTE after it and a doubled backslash does not, the
// elements cut reads the same way, and an unbalanced quote further left
// misreads nothing nearer us — the entry at our end is cut first, from
// a state that quote cannot reach. The address list keeps its plain cut.
func TestAQuotedCutHonoursTheQuotedString(t *testing.T) {
	for _, c := range []struct {
		name, value string
		sep         byte
		grammar     cutGrammar
		want        []string
	}{
		{"a separator inside quotes is part of the entry", `for=1.1.1.1;host="x;y";proto=https`, ';', quotedCut, []string{"for=1.1.1.1", `host="x;y"`, "proto=https"}},
		{"an escaped quote keeps the string open", `host="a\";b";for=1.1.1.1`, ';', quotedCut, []string{`host="a\";b"`, "for=1.1.1.1"}},
		{"an escaped backslash closes it", `host="a\\";for=1.1.1.1`, ';', quotedCut, []string{`host="a\\"`, "for=1.1.1.1"}},
		{"the elements cut reads the same way", `for=1.1.1.1;host="x,y", for=2.2.2.2`, ',', quotedCut, []string{`for=1.1.1.1;host="x,y"`, "for=2.2.2.2"}},
		{"an unbalanced quote further left misreads nothing nearer us", `for=1.1.1.1;host="abc, for=2.2.2.2`, ',', quotedCut, []string{`for=1.1.1.1;host="abc`, "for=2.2.2.2"}},
		{"an address list is cut on every separator", `1.1.1.1, "2.2.2.2, 3.3.3.3"`, ',', plainCut, []string{"1.1.1.1", `"2.2.2.2`, `3.3.3.3"`}},
	} {
		if got := lastEntries([]string{c.value}, c.sep, 0, c.grammar, newForwardedBudget()); fmt.Sprint(got) != fmt.Sprint(c.want) {
			t.Errorf("%s: lastEntries(%q, %q) = %q, want %q", c.name, c.value, c.sep, got, c.want)
		}
	}
}
