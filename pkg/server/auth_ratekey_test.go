package server

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// The limiter's key is an identity, and an ephemeral source port is not one.
// #2673 measured the consequence on the public path: 60 requests on 60 fresh
// connections and zero 429, which let a refresh-token holder drive the
// refresh route at 201 rows/s (#1668's round-5 review, ratekey-2.log).
//
// These witnesses drive that same public path — the limiter wrapper in front
// of a route, over real TCP, so each request carries its own source port —
// and the forwarded-address half from the request side, where the socket
// address is held EQUAL so only the header moves.

// frozenLimiter pins the limiter's clock: a bucket refills by elapsed time,
// so a wall clock would make "how many of the 60 were refused" depend on how
// fast the machine answered 60 requests.
func frozenLimiter() *authRateLimiter {
	lim := newAuthRateLimiter()
	at := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	lim.now = func() time.Time { return at }
	return lim
}

// limitedServer serves one rate-limited route and records the RemoteAddr of
// every request that arrives — refused ones included, so a witness can prove
// the source ports really were distinct instead of assuming it.
type limitedServer struct {
	*httptest.Server
	mu    sync.Mutex
	peers map[string]int
}

func newLimitedServer(t *testing.T, s *Server, tier string, cfg authBucketCfg) *limitedServer {
	t.Helper()
	ls := &limitedServer{peers: map[string]int{}}
	limited := s.limitRoute(tier, cfg, nil)(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	ls.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Fed by the server's handler goroutines: the lock is not optional.
		ls.mu.Lock()
		ls.peers[r.RemoteAddr]++
		ls.mu.Unlock()
		limited(w, r)
	}))
	t.Cleanup(ls.Close)
	return ls
}

func (ls *limitedServer) distinctPeers() int {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	return len(ls.peers)
}

func TestFreshConnectionsFromOneClientShareOneBucket(t *testing.T) {
	s := &Server{authLimiter: frozenLimiter()}
	ls := newLimitedServer(t, s, "refresh", refreshRouteBucket)

	// One TCP connection per request, so a fresh source port each time —
	// what a client that reconnects for every refresh does.
	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
	const requests = 60
	refused, retryAfter := 0, ""
	for i := range requests {
		req, err := http.NewRequest(http.MethodPost, ls.URL, nil)
		if err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
		resp.Body.Close()
		if resp.StatusCode == http.StatusTooManyRequests {
			refused++
			if retryAfter == "" {
				retryAfter = resp.Header.Get("Retry-After")
			}
		}
	}

	// The premise of the measurement: the ports really were fresh. Without
	// it a reused connection would make the witness pass for the wrong
	// reason.
	if got := ls.distinctPeers(); got != requests {
		t.Fatalf("the %d requests came from %d distinct source addresses; this witness needs a fresh connection each", requests, got)
	}
	// The clock is frozen, so the bucket never refills: burst 30 of 60.
	if want := requests - int(refreshRouteBucket.burst); refused != want {
		t.Errorf("%d of %d requests refused, want %d: every fresh connection bought a fresh bucket (#2673)", refused, requests, want)
	}
	if refused > 0 && retryAfter == "" {
		t.Error("a 429 without Retry-After: the client cannot know when to come back")
	}
}

// Behind a trusted proxy the key must be the FORWARDED client, not the peer:
// keying on the peer would put every user of the instance in one bucket.
func TestForwardedClientsBehindATrustedPeerDoNotShareABucket(t *testing.T) {
	// A one-token bucket makes a shared bucket visible in one request per
	// client: the second request of the same client is refused.
	oneShot := authBucketCfg{rate: 1.0 / 60.0, burst: 1}
	s := &Server{cfg: Config{TrustedProxyCIDRs: []string{"127.0.0.0/8", "::1/128"}}, authLimiter: frozenLimiter()}
	ls := newLimitedServer(t, s, "refresh", oneShot)

	// httptest listens on loopback, so the immediate peer is inside the
	// configured ranges.
	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
	get := func(t *testing.T, forwarded string) int {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, ls.URL, nil)
		if err != nil {
			t.Fatalf("request for %s: %v", forwarded, err)
		}
		req.Header.Set("X-Forwarded-For", forwarded)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("request for %s: %v", forwarded, err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}

	if got := get(t, "203.0.113.5"); got != http.StatusNoContent {
		t.Fatalf("first request of 203.0.113.5: got %d, want %d", got, http.StatusNoContent)
	}
	if got := get(t, "203.0.113.5"); got != http.StatusTooManyRequests {
		t.Errorf("second request of 203.0.113.5: got %d, want %d — its own bucket is not spent", got, http.StatusTooManyRequests)
	}
	if got := get(t, "198.51.100.7"); got != http.StatusNoContent {
		t.Errorf("first request of 198.51.100.7: got %d, want %d — two clients behind one proxy share a bucket", got, http.StatusNoContent)
	}
}

// The other side: a forwarded header the deployment has NOT declared a
// trusted peer for is the client's own, so rotating it must buy nothing.
// The socket address is held equal across the requests, so the ephemeral
// port plays no part in this witness.
func TestAForgedForwardedHeaderBuysNoFreshBucket(t *testing.T) {
	for _, header := range []string{"X-Forwarded-For", "X-Real-IP", "Forwarded"} {
		t.Run(header, func(t *testing.T) {
			s := &Server{cfg: Config{}, authLimiter: frozenLimiter()}
			handler := s.limitRoute("refresh", authBucketCfg{rate: 1.0 / 60.0, burst: 1}, nil)(
				func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })

			send := func(forged string) int {
				r := httptest.NewRequest(http.MethodPost, "/api/auth/refresh", nil)
				r.RemoteAddr = "203.0.113.9:40001" // the same socket, twice
				if header == "Forwarded" {
					r.Header.Set(header, "for="+forged)
				} else {
					r.Header.Set(header, forged)
				}
				w := httptest.NewRecorder()
				handler(w, r)
				return w.Code
			}

			if got := send("9.9.9.1"); got != http.StatusNoContent {
				t.Fatalf("first request: got %d, want %d", got, http.StatusNoContent)
			}
			if got := send("9.9.9.2"); got != http.StatusTooManyRequests {
				t.Errorf("a forged %s moved the key: got %d, want %d", header, got, http.StatusTooManyRequests)
			}
		})
	}
}

// Behind a proxy that APPENDS to X-Forwarded-For rather than replacing it
// — ingress-nginx with `use-forwarded-headers` and
// `compute-full-forwarded-for` both on, the shape behind an L7 load
// balancer — the client's own entries arrive on the LEFT of the chain. Taking the leftmost would hand the
// client its own identity back: a fresh bucket per forged value, the
// victim's bucket drained by naming the victim, and a forged IP in the
// audit row.
func TestAForgedEntryInAnAppendedChainBuysNoFreshBucket(t *testing.T) {
	s := &Server{cfg: Config{TrustedProxyCIDRs: []string{"10.0.0.0/8"}}, authLimiter: frozenLimiter()}
	handler := s.limitRoute("refresh", authBucketCfg{rate: 1.0 / 60.0, burst: 1}, nil)(
		func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })

	send := func(chain string) int {
		r := httptest.NewRequest(http.MethodPost, "/api/auth/refresh", nil)
		r.RemoteAddr = "10.1.1.1:443" // the ingress pod, inside the trusted range
		r.Header.Set("X-Forwarded-For", chain)
		w := httptest.NewRecorder()
		handler(w, r)
		return w.Code
	}

	// What the appending proxy writes: "<what the client sent>, <the
	// address the proxy saw>". The real client is the same both times.
	if got := send("9.9.9.1, 203.0.113.77"); got != http.StatusNoContent {
		t.Fatalf("first request: got %d, want %d", got, http.StatusNoContent)
	}
	if got := send("9.9.9.2, 203.0.113.77"); got != http.StatusTooManyRequests {
		t.Errorf("a second forged leftmost entry: got %d, want %d — the key moved with a value the client wrote", got, http.StatusTooManyRequests)
	}
	// And the rule still separates two real clients behind that proxy.
	if got := send("9.9.9.1, 198.51.100.9"); got != http.StatusNoContent {
		t.Errorf("another real client: got %d, want %d — it must not pay for the first", got, http.StatusNoContent)
	}
}

// One client, two routes: the limiter keys per route tier, so draining the
// public refresh route must not spend the login bucket of everybody behind
// that address — allow() clamps a bucket's tokens to the burst of
// whichever route touched it last, so a shared key is also a shrinking one.
func TestOneClientsRoutesDoNotShareABucket(t *testing.T) {
	s := &Server{authLimiter: frozenLimiter()}
	noContent := func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }
	refresh := s.limitRoute("refresh", refreshRouteBucket, nil)(noContent)
	login := s.limitRoute("login", loginRouteBucket, nil)(noContent)

	call := func(h http.HandlerFunc) int {
		r := httptest.NewRequest(http.MethodPost, "/api/auth/x", nil)
		r.RemoteAddr = "203.0.113.5:40001"
		w := httptest.NewRecorder()
		h(w, r)
		return w.Code
	}

	// Drain the refresh bucket dry (frozen clock: no refill).
	for i := 0; i < int(refreshRouteBucket.burst); i++ {
		if got := call(refresh); got != http.StatusNoContent {
			t.Fatalf("refresh request %d: got %d, want %d", i, got, http.StatusNoContent)
		}
	}
	if got := call(refresh); got != http.StatusTooManyRequests {
		t.Fatalf("the refresh bucket is not empty: got %d, want %d", got, http.StatusTooManyRequests)
	}
	if got := call(login); got != http.StatusNoContent {
		t.Errorf("login after the refresh burst: got %d, want %d — hammering the public refresh route locked login out", got, http.StatusNoContent)
	}
}

// The chart trusts the private ranges by default, so an in-range client —
// every LAN client of a self-hosted instance, every in-cluster pod the
// NetworkPolicy admits — is itself inside a trusted range. When EVERY hop
// of the chain is one of ours the answer must be the rightmost hop, the
// address the last appending proxy saw: answering with the leftmost would
// hand the key back to the client and reproduce #2673's own defect
// through the header instead of the source port.
func TestAnInRangeClientsForgedChainBuysNoFreshBucket(t *testing.T) {
	s := &Server{cfg: Config{TrustedProxyCIDRs: []string{"10.0.0.0/8"}}, authLimiter: frozenLimiter()}
	handler := s.limitRoute("refresh", authBucketCfg{rate: 1.0 / 60.0, burst: 1}, nil)(
		func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })

	send := func(chain string) int {
		r := httptest.NewRequest(http.MethodPost, "/api/auth/refresh", nil)
		r.RemoteAddr = "10.1.1.1:443" // the ingress pod, inside the trusted range
		r.Header.Set("X-Forwarded-For", chain)
		w := httptest.NewRecorder()
		handler(w, r)
		return w.Code
	}

	// The client sits at 10.2.2.2 — inside the ranges too — and forges an
	// in-range entry the proxy then appends behind.
	if got := send("10.9.9.1, 10.2.2.2"); got != http.StatusNoContent {
		t.Fatalf("first request: got %d, want %d", got, http.StatusNoContent)
	}
	if got := send("10.9.9.2, 10.2.2.2"); got != http.StatusTooManyRequests {
		t.Errorf("a second forged in-range entry: got %d, want %d — rotating a value the client wrote bought a fresh bucket", got, http.StatusTooManyRequests)
	}
	// And a different real in-range client still gets its own bucket.
	if got := send("10.9.9.1, 10.3.3.3"); got != http.StatusNoContent {
		t.Errorf("another in-range client: got %d, want %d — it must not pay for the first", got, http.StatusNoContent)
	}
}

// The same client, the same appending proxy, a forged entry OUTSIDE every
// range: a walk that skipped every in-range hop skipped the appended one
// too — the client's own address — and answered with the forgery, so
// rotating it bought a fresh login bucket per value. The bound on how
// many of OUR hops may be skipped is what makes the shape above hold for
// a forged value of any range (#2673).
func TestAnOutOfRangeForgedEntryBuysNoFreshBucketEither(t *testing.T) {
	s := &Server{cfg: Config{TrustedProxyCIDRs: []string{"10.0.0.0/8"}}, authLimiter: frozenLimiter()}
	handler := s.limitRoute("login", authBucketCfg{rate: 1.0 / 60.0, burst: 1}, nil)(
		func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })

	send := func(chain string) int {
		r := httptest.NewRequest(http.MethodPost, "/api/auth/login", nil)
		r.RemoteAddr = "10.1.1.1:443" // the ingress pod, inside the trusted range
		r.Header.Set("X-Forwarded-For", chain)
		w := httptest.NewRecorder()
		handler(w, r)
		return w.Code
	}

	if got := send("203.0.113.100, 10.2.2.2"); got != http.StatusNoContent {
		t.Fatalf("first request: got %d, want %d", got, http.StatusNoContent)
	}
	refused := 0
	for i := 1; i < 6; i++ {
		if send(fmt.Sprintf("203.0.113.%d, 10.2.2.2", 100+i)) == http.StatusTooManyRequests {
			refused++
		}
	}
	if refused != 5 {
		t.Errorf("rotating an out-of-range forged entry: %d of 5 later requests refused, want 5 — one client bought a fresh login bucket per forged value", refused)
	}
}

// An IPv6 client is not one address. A residential or VPS client holds a
// whole /64 and RFC 4941 privacy extensions rotate its source address
// without it asking, so a /128 rate-limit key is a bucket the client
// replaces for free — and every tier keyed on the IP alone (register,
// refresh, the password-reset confirm whose body names no account, the
// desktop exchange, the config-share probe guard) is then unlimited for
// anybody holding a prefix, while ten thousand addresses out of one /64
// evict every other client's bucket from the limiter's LRU.
func TestAnIPv6ClientsPrefixDoesNotBuyFreshBuckets(t *testing.T) {
	s := &Server{cfg: Config{TrustedProxyCIDRs: []string{"10.0.0.0/8"}}, authLimiter: frozenLimiter()}
	handler := s.limitRoute("register", registerRouteBucket, nil)(
		func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })

	// A forwarded client behind the ingress pod (inside the trusted range),
	// or the socket peer itself when client is "".
	send := func(peer, client string) int {
		r := httptest.NewRequest(http.MethodPost, "/api/auth/register", nil)
		r.RemoteAddr = peer
		if client != "" {
			r.Header.Set("X-Forwarded-For", client)
		}
		w := httptest.NewRecorder()
		handler(w, r)
		return w.Code
	}
	const ingress = "10.1.1.1:443"

	// One /64, a fresh address per shot. The clock is frozen, so the
	// burst is the cap however many addresses the client spends.
	burst := int(registerRouteBucket.burst)
	allowed, shots := 0, 4*burst
	for i := range shots {
		if send(ingress, fmt.Sprintf("2001:db8:dead:beef::%x", i+1)) == http.StatusNoContent {
			allowed++
		}
	}
	if allowed != burst {
		t.Errorf("%d of %d requests from ONE /64 were allowed, want %d: an IPv6 client bought a fresh register bucket per address, as an IPv4 client cannot (#2673)", allowed, shots, burst)
	}

	// A different prefix is a different client and must not pay for the
	// first — a bucket per /64, not one for the internet's v6 half.
	if got := send(ingress, "2001:db8:dead:f00d::1"); got != http.StatusNoContent {
		t.Errorf("the first request of another /64: got %d, want %d — the prefixes share a bucket", got, http.StatusNoContent)
	}

	// And an IPv4 client is untouched: its single address is already one
	// client, so widening it would make neighbours pay for each other.
	v4 := 0
	for i := range shots {
		if send(ingress, fmt.Sprintf("203.0.113.%d", i+1)) == http.StatusNoContent {
			v4++
		}
	}
	if v4 != shots {
		t.Errorf("%d of %d requests from %d DIFFERENT IPv4 clients were allowed, want all of them: the limiter groups addresses it should not", v4, shots, shots)
	}

	// The socket path is the same class, through a spelling the forwarded
	// path never sees: a link-local peer arrives with its zone
	// ("[fe80::1%eth0]:443", how net's listener spells it), no IP parser
	// accepts the zone, and an identity that is not an address is its
	// own bucket — so a neighbour on the link, who can mint any address
	// of fe80::/64, had a fresh bucket per address and 2^64 of them to
	// flush everyone else out of the LRU with. The /64 holds there too.
	link := 0
	for i := range shots {
		if send(fmt.Sprintf("[fe80::%x%%eth0]:443", i+1), "") == http.StatusNoContent {
			link++
		}
	}
	if link != burst {
		t.Errorf("%d of %d requests from link-local peers of ONE fe80::/64 were allowed, want %d: a zone-scoped peer bought a fresh bucket per address", link, shots, burst)
	}
}

// The bucket is a group of clients on purpose; the audit row is the one
// client that acted. Both come off the same request, and only one of
// them is widened.
func TestTheBucketIsAPrefixAndTheAuditRowIsTheAddress(t *testing.T) {
	s := &Server{cfg: Config{TrustedProxyCIDRs: []string{"10.0.0.0/8"}}}
	r := httptest.NewRequest(http.MethodPost, "/api/auth/register", nil)
	r.RemoteAddr = "10.1.1.1:443"
	r.Header.Set("X-Forwarded-For", "2001:db8:dead:beef::17")
	if got := s.clientIP(r); got != "2001:db8:dead:beef::17" {
		t.Errorf("the audit identity is %q, want the full address 2001:db8:dead:beef::17 — a row that names a prefix names no client", got)
	}
	if got := s.rateBucket(r); got != "2001:db8:dead:beef::/64" {
		t.Errorf("the limiter bucket is %q, want 2001:db8:dead:beef::/64", got)
	}

	// A link-local peer arrives with its zone, which is the interface it
	// came in on and no part of the address: the row names the address,
	// the bucket its /64 — the one every neighbour on the link can mint
	// from. Spelled the way net's listener spells it.
	r = httptest.NewRequest(http.MethodPost, "/api/auth/register", nil)
	r.RemoteAddr = "[fe80::1%eth0]:443"
	if got := s.clientIP(r); got != "fe80::1" {
		t.Errorf("the audit identity of a zone-scoped peer is %q, want fe80::1 — the zone is no part of the address, and an identity that is not an address is a bucket of its own", got)
	}
	if got := s.rateBucket(r); got != "fe80::/64" {
		t.Errorf("the limiter bucket of a zone-scoped peer is %q, want fe80::/64", got)
	}

	for in, want := range map[string]string{
		"203.0.113.5":                            "203.0.113.5",
		"2001:db8:dead:beef::1":                  "2001:db8:dead:beef::/64",
		"2001:db8:dead:beef:ffff:ffff:ffff:fffe": "2001:db8:dead:beef::/64",
		"2001:db8:dead:f00d::1":                  "2001:db8:dead:f00d::/64",
		"fe80::1":                                "fe80::/64",
		// An IPv4-mapped address is one host, not a prefix of them —
		// canonicalIP already spells it as IPv4, and the bucket must not
		// disagree with it.
		"::ffff:203.0.113.5": "::ffff:203.0.113.5",
		// Identities that are not addresses pass through as themselves:
		// a unix socket, the no-peer sentinel.
		"@":         "@",
		unknownPeer: unknownPeer,
	} {
		if got := rateBucketIP(in); got != want {
			t.Errorf("rateBucketIP(%q) = %q, want %q", in, got, want)
		}
	}
}

// The prefix has to hold at EVERY limiter key that names the client, not
// at the one a report happened to name: limitRoute's tiers and the
// config-share per-IP guard both key on an inbound request, and a third
// site would drift. s.rateBucket is the one way a request becomes a key,
// and this reads the sources to say so — a helper called at two sites
// out of three is the shape of #2673's own first half.
//
// It follows the address through the LOCALS it flows into — short
// declarations, `var` declarations and range clauses alike — because the
// expression handed to the limiter is rarely the one that read the
// client: limitRoute builds a slice of keys and charges each in turn, so
// a guard reading only the call's argument stays green with the full
// address back in the key.
//
// And it is POSITIVE as well as negative. A taint from clientIP( cannot
// cross a function boundary: a one-line helper returning s.clientIP(r)
// put the full address back into the config-share key with the taint
// walk green and every behavioural witness green — no witness drove that
// site with an IPv6 client. So every allow() site must show where its
// key comes from: through s.rateBucket, or from one of the namespaces
// that name something other than a client. A key that shows neither
// fails here, whatever helper built it. The behavioural side of the same
// defect is TestAnIPv6ClientsPrefixDoesNotBuyFreshConfigShareProbes.
var limiterKeyNamespacesNotAClient = []string{`"wh:"`, `"cs:"`, `"orglaunch:"`}

// limiterKeyFullAddressSeeds are the expressions that read a full client
// address — the identity chokepoint, the forwarded walk under it, the
// two canonicalisers and the socket peer itself — and the taint is
// seeded on every one of them, not on the chokepoint's name alone: a
// key built from forwardedClientIP(, which the seed `clientIP(` never
// matches, carries the forwarded identity into a config-share key under
// a walk seeded on that name, and only a behavioural witness sees it.
var limiterKeyFullAddressSeeds = []string{"clientIP(", "forwardedClientIP(", "canonicalIP(", "addrHost(", "RemoteAddr"}

func TestNoLimiterKeyIsBuiltFromTheFullAddress(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	sites, chokepoint := 0, 0
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		file, err := parser.ParseFile(fset, name, src, 0)
		if err != nil {
			t.Fatal(err)
		}
		text := func(n ast.Node) string {
			return string(src[fset.Position(n.Pos()).Offset:fset.Position(n.End()).Offset])
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			// Everything the full address reaches inside this function,
			// everything the chokepoint reaches, and everything a
			// non-client namespace reaches: one walk, three seed sets. The
			// scope is the whole declaration, nested closures included:
			// limitRoute reads the client in one and charges the limiter
			// in another.
			fullAddress := flowsFrom(fn, text, limiterKeyFullAddressSeeds...)
			bucketed := flowsFrom(fn, text, "rateBucket(")
			namespaced := flowsFrom(fn, text, limiterKeyNamespacesNotAClient...)
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok || len(call.Args) == 0 {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || (sel.Sel.Name != "allow" && sel.Sel.Name != "refund") {
					return true
				}
				sites++
				key := text(call.Args[0])
				// Counted on the FUNCTION, not on the argument: the
				// chokepoint is as likely to be read into a local as
				// the address it replaced.
				if strings.Contains(text(fn), "rateBucket(") {
					chokepoint++
				}
				if fullAddress(call.Args[0]) {
					t.Errorf("%s:%d charges the limiter with %s, which carries the full client address: that address is the AUDIT identity, and an IPv6 client rotates it inside its own /64 — a limiter key goes through s.rateBucket",
						name, fset.Position(call.Pos()).Line, key)
				}
				// A refund returns what an allow charged, to the key it
				// charged: the positive requirement is on the charge.
				if sel.Sel.Name == "allow" && !bucketed(call.Args[0]) && !namespaced(call.Args[0]) {
					t.Errorf("%s:%d charges the limiter with %s, which neither goes through s.rateBucket nor starts with a namespace that names no client (%s): a key naming the client goes through s.rateBucket so an IPv6 client is charged by its /64, however many helpers sit between; a key naming something else starts with one of those literals, or this guard learns the new one",
						name, fset.Position(call.Pos()).Line, key, strings.Join(limiterKeyNamespacesNotAClient, " "))
				}
				return true
			})
		}
	}
	if sites == 0 {
		t.Fatal("this guard found no limiter call: it is reading the wrong sources")
	}
	if chokepoint == 0 {
		t.Fatal("no limiter call keys on s.rateBucket: either the chokepoint is gone or this guard no longer sees the key expressions")
	}
}

// flowsFrom returns the predicate "this expression carries one of the
// seeds", the seeds being substrings of the source text, computed to a
// fixpoint over the function's assignments — `:=` and `=`, `var`
// declarations, range clauses — so a seed is followed through as many
// locals as it takes, nested closures included. A `var` declaration is
// an assignment too: `var ip = s.clientIP(r)` is a DeclStmt, not an
// AssignStmt, and a walk reading only the latter was blind to it.
func flowsFrom(fn *ast.FuncDecl, text func(ast.Node) string, seeds ...string) func(ast.Node) bool {
	carried := map[string]bool{}
	carries := func(n ast.Node) bool {
		expr := text(n)
		for _, seed := range seeds {
			if strings.Contains(expr, seed) {
				return true
			}
		}
		for local := range carried {
			if containsIdent(expr, local) {
				return true
			}
		}
		return false
	}
	mark := func(lhs ast.Expr) {
		if id, ok := lhs.(*ast.Ident); ok && id.Name != "_" {
			carried[id.Name] = true
		}
	}
	anyCarries := func(exprs []ast.Expr) bool {
		for _, e := range exprs {
			if carries(e) {
				return true
			}
		}
		return false
	}
	// To a fixpoint: an assignment may precede or follow the one it
	// feeds, and ast.Inspect walks in source order.
	for range 4 {
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			switch stmt := n.(type) {
			case *ast.AssignStmt:
				if anyCarries(stmt.Rhs) {
					for _, lhs := range stmt.Lhs {
						mark(lhs)
					}
				}
			case *ast.DeclStmt:
				gen, ok := stmt.Decl.(*ast.GenDecl)
				if !ok {
					return true
				}
				for _, spec := range gen.Specs {
					if vs, ok := spec.(*ast.ValueSpec); ok && anyCarries(vs.Values) {
						for _, id := range vs.Names {
							mark(id)
						}
					}
				}
			case *ast.RangeStmt:
				if carries(stmt.X) {
					for _, v := range []ast.Expr{stmt.Key, stmt.Value} {
						if v != nil {
							mark(v)
						}
					}
				}
			}
			return true
		})
	}
	return carries
}

// The config-share probe guard is the OTHER site that keys on the
// client, and the one no behavioural witness drove: a helper returning
// the full address at that site alone left the whole suite green while
// three addresses of one /64 produced three csip keys. The guard above
// reads the sources; this drives the handler, past the per-IP guard and
// into the share lookup, which refuses an unknown share — the answer an
// id-guesser is after and the one the guard exists to ration.
func TestAnIPv6ClientsPrefixDoesNotBuyFreshConfigShareProbes(t *testing.T) {
	s := &Server{cfg: Config{TrustedProxyCIDRs: []string{"10.0.0.0/8"}}, authLimiter: frozenLimiter()}
	handler := s.configShareAuth(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	burst := int(configShareRateBucket.burst)
	probes, shots := 0, 4*burst
	for i := range shots {
		r := httptest.NewRequest(http.MethodGet, "/api/config-share/nope/config", nil)
		r.SetPathValue("id", "nope")
		r.RemoteAddr = "10.1.1.1:443" // the ingress pod, inside the trusted range
		r.Header.Set("X-Forwarded-For", fmt.Sprintf("2001:db8:dead:beef::%x", i+1))
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != http.StatusTooManyRequests {
			probes++
		}
	}
	if probes != burst {
		t.Errorf("%d of %d config-share probes from ONE /64 got past the per-IP guard, want %d: an IPv6 client bought a fresh csip bucket per address", probes, shots, burst)
	}
}

// containsIdent answers whether expr uses name as an identifier of its
// own rather than as a slice of a longer word: "csip:" is not the local
// `ip`, and a guard that cannot tell would flag every tier prefix.
func containsIdent(expr, name string) bool {
	word := func(b byte) bool {
		return b == '_' || b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9'
	}
	for i := 0; i+len(name) <= len(expr); i++ {
		if expr[i:i+len(name)] != name {
			continue
		}
		if i > 0 && word(expr[i-1]) {
			continue
		}
		if i+len(name) < len(expr) && word(expr[i+len(name)]) {
			continue
		}
		return true
	}
	return false
}
