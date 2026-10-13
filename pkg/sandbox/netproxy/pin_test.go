package netproxy

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/secure/httpdial"
)

// scriptedResolver answers the pin's lookups without DNS: the n-th lookup of
// a name gets the n-th answer of its script, the last one repeating; an
// answer lists its addresses comma-separated. Names compare as a resolver
// compares them — case-insensitive, a trailing root dot ignored. It records
// every lookup, and its context's deadline (zero when it has none).
type scriptedResolver struct {
	mu        sync.Mutex
	script    map[string][]string
	count     map[string]int
	lookups   []string
	deadlines []time.Time
}

func newScriptedResolver(name string, answers ...string) *scriptedResolver {
	return &scriptedResolver{script: map[string][]string{resolverKey(name): answers}, count: map[string]int{}}
}

func resolverKey(name string) string { return strings.ToLower(strings.TrimSuffix(name, ".")) }

// LookupIPAddr implements httpdial.HostResolver.
func (r *scriptedResolver) LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lookups = append(r.lookups, host)
	deadline, _ := ctx.Deadline()
	r.deadlines = append(r.deadlines, deadline)
	key := resolverKey(host)
	answers, ok := r.script[key]
	if !ok {
		return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
	}
	r.count[key]++
	var addrs []net.IPAddr
	for _, ip := range strings.Split(answers[min(r.count[key], len(answers))-1], ",") {
		addrs = append(addrs, net.IPAddr{IP: net.ParseIP(ip)})
	}
	return addrs, nil
}

func (r *scriptedResolver) asked() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.lookups...)
}

func (r *scriptedResolver) lookupDeadlines() []time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]time.Time(nil), r.deadlines...)
}

// recordingDial stands in for the network: it records every address the
// proxy asks its dialer for and connects each to target.
func recordingDial(target string) (dial func(ctx context.Context, network, addr string) (net.Conn, error), dialed func() []string) {
	var mu sync.Mutex
	var addrs []string
	dial = func(ctx context.Context, network, addr string) (net.Conn, error) {
		mu.Lock()
		addrs = append(addrs, addr)
		mu.Unlock()
		var d net.Dialer
		return d.DialContext(ctx, network, target)
	}
	dialed = func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), addrs...)
	}
	return dial, dialed
}

// blockedLog records what the proxy reports through OnBlocked.
type blockedLog struct {
	mu      sync.Mutex
	entries []string
}

func (b *blockedLog) record(host, reason string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.entries = append(b.entries, host+": "+reason)
}

func (b *blockedLog) all() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.entries...)
}

// startPinProxy starts a proxy on opts, its policy defaulting to open.
func startPinProxy(t *testing.T, opts Options) *Proxy {
	t.Helper()
	if opts.Policy == nil {
		pol, err := Compile(ModeOpen, nil)
		if err != nil {
			t.Fatal(err)
		}
		opts.Policy = pol
	}
	prx, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := prx.Start("127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = prx.Shutdown(ctx)
	})
	return prx
}

// forwardVia sends one hand-written plain-HTTP forward for target (a host,
// with or without a port) through prx — the host as written, which a client
// library would rewrite — and returns the status.
func forwardVia(t *testing.T, prx *Proxy, target string) int {
	t.Helper()
	return rawStatus(t, prx, "GET http://"+target+"/x HTTP/1.1\r\nHost: "+target+"\r\nConnection: close\r\n\r\n")
}

// rawStatus writes request to prx as is and returns the status of its
// answer.
func rawStatus(t *testing.T, prx *Proxy, request string) int {
	t.Helper()
	conn, err := net.Dial("tcp", prx.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(15 * time.Second))
	if _, err := io.WriteString(conn, request); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: strings.Fields(request)[0]})
	if err != nil {
		t.Fatalf("%q: %v", request, err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}

func allowlist(t *testing.T, hosts ...string) *Policy {
	t.Helper()
	pol, err := Compile(ModeAllowlist, hosts)
	if err != nil {
		t.Fatal(err)
	}
	return pol
}

// The rebinding case: the runner's check got a public answer for the
// gateway's name, the proxy's lookup a private one. The proxy judges its own
// answer through the same guard before any connection is made — on every way
// out — and reports the refusal; nothing reaches the address the answer
// points to.
func TestAPinnedHostsRebindingAnswerIsRefusedOnEveryWayOut(t *testing.T) {
	const gw = "gw.rebind.test"
	port, hits := localService(t)
	res := newScriptedResolver(gw, "203.0.113.7", "10.255.255.1")
	if ip, err := httpdial.ResolvePublicHostWith(context.Background(), res, gw, true); err != nil || ip.String() != "203.0.113.7" {
		t.Fatalf("scenario broken: the runner's check got %v, %v; want the public first answer", ip, err)
	}
	dial, dialed := recordingDial(net.JoinHostPort("127.0.0.1", port))
	var blocked blockedLog
	options := func(ca *EphemeralCA) Options {
		return Options{Policy: allowlist(t, gw), PinnedHosts: []string{gw}, PinResolver: res, Dial: dial, OnBlocked: blocked.record, InspectCA: ca}
	}
	const refusal = "resolved address 10.255.255.1 is not a public unicast IP"

	plain := startPinProxy(t, options(nil))
	if status, body := viaProxy(t, plain, "http://"+net.JoinHostPort(gw, port)+"/x", nil); status != http.StatusBadGateway || !strings.Contains(body, refusal) {
		t.Errorf("forward: status %d, body %q; want 502 naming the refusal", status, body)
	}
	if answer := connectVia(t, plain, gw+":443"); !strings.HasPrefix(answer, "502") || !strings.Contains(answer, refusal) {
		t.Errorf("CONNECT: %q; want 502 naming the refusal", answer)
	}
	ca, err := NewEphemeralCA()
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(ca.CertPEM())
	inspecting := startPinProxy(t, options(ca))
	if status, body := viaProxy(t, inspecting, "https://"+gw+"/x", pool); status != http.StatusBadGateway || !strings.Contains(body, refusal) {
		t.Errorf("inspected CONNECT: status %d, body %q; want 502 naming the refusal", status, body)
	}

	if got := dialed(); len(got) != 0 {
		t.Errorf("the proxy's dialer was asked for %q — past the pin, it resolves the name on its own", got)
	}
	if n := hits.Load(); n != 0 {
		t.Errorf("the service the rebinding answer points to took %d connection(s); want none", n)
	}
	reports := blocked.all()
	if len(reports) != 3 {
		t.Fatalf("OnBlocked reported %q; want the three refusals", reports)
	}
	for _, r := range reports {
		if !strings.HasPrefix(r, gw+": ") || !strings.Contains(r, refusal) {
			t.Errorf("OnBlocked reported %q; want %s and the refusal", r, gw)
		}
	}
}

// The pin dials the address its guard validated — never the name, which the
// dialer would resolve again — while the name keeps every other role: the
// Host header, the SNI and the certificate check, on every way out. The
// pinned name is example.com, the name the httptest certificate carries.
func TestAPinnedHostIsDialedOnTheAddressItsGuardValidated(t *testing.T) {
	const name = "example.com"
	gateway := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "gateway "+r.Host)
	}))
	defer gateway.Close()
	plainGateway := httptest.NewServer(gateway.Config.Handler)
	defer plainGateway.Close()
	trusted := x509.NewCertPool()
	trusted.AddCert(gateway.Certificate())

	t.Run("forward", func(t *testing.T) {
		dial, dialed := recordingDial(plainGateway.Listener.Addr().String())
		prx := startPinProxy(t, Options{Policy: allowlist(t, name), PinnedHosts: []string{name}, PinResolver: newScriptedResolver(name, "203.0.113.9"), Dial: dial})
		if status, body := viaProxy(t, prx, "http://"+name+":8080/x", nil); status != http.StatusOK || body != "gateway "+name+":8080" {
			t.Errorf("status %d, body %q; want the gateway's answer", status, body)
		}
		if got := dialed(); len(got) != 1 || got[0] != "203.0.113.9:8080" {
			t.Errorf("the dialer was asked for %q; want [203.0.113.9:8080], the validated address", got)
		}
	})
	t.Run("CONNECT", func(t *testing.T) {
		dial, dialed := recordingDial(gateway.Listener.Addr().String())
		prx := startPinProxy(t, Options{Policy: allowlist(t, name), PinnedHosts: []string{name}, PinResolver: newScriptedResolver(name, "203.0.113.9"), Dial: dial})
		client := &http.Client{Timeout: 15 * time.Second, Transport: &http.Transport{
			Proxy:           func(*http.Request) (*url.URL, error) { return url.Parse("http://" + prx.Addr().String()) },
			TLSClientConfig: &tls.Config{RootCAs: trusted},
		}}
		resp, err := client.Get("https://" + name + "/x")
		if err != nil {
			t.Fatalf("through the tunnel: %v", err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK || string(body) != "gateway "+name {
			t.Errorf("status %d, body %q; want the gateway's answer", resp.StatusCode, body)
		}
		if got := dialed(); len(got) != 1 || got[0] != "203.0.113.9:443" {
			t.Errorf("the dialer was asked for %q; want [203.0.113.9:443], the validated address", got)
		}
	})
	t.Run("inspected CONNECT", func(t *testing.T) {
		ca, err := NewEphemeralCA()
		if err != nil {
			t.Fatal(err)
		}
		pool := x509.NewCertPool()
		pool.AppendCertsFromPEM(ca.CertPEM())
		dial, dialed := recordingDial(gateway.Listener.Addr().String())
		prx := startPinProxy(t, Options{Policy: allowlist(t, name), PinnedHosts: []string{name}, PinResolver: newScriptedResolver(name, "203.0.113.9"), Dial: dial,
			InspectCA: ca, InspectUpstreamTLS: &tls.Config{RootCAs: trusted}})
		if status, body := viaProxy(t, prx, "https://"+name+"/x", pool); status != http.StatusOK || body != "gateway "+name {
			t.Errorf("status %d, body %q; want the gateway's answer", status, body)
		}
		if got := dialed(); len(got) != 1 || got[0] != "203.0.113.9:443" {
			t.Errorf("the dialer was asked for %q; want [203.0.113.9:443], the validated address", got)
		}
	})
}

// Every spelling of a pinned host meets the pin — case, a root dot, an IDN
// in its Unicode or its ASCII form (the one net/http dials), a name the IDN
// fold refuses (an underscore), any notation of an IP address — on every way
// out, and the entry itself may be spelt any of those ways.
func TestThePinMeetsEverySpellingOfTheHost(t *testing.T) {
	dial, dialed := recordingDial("127.0.0.1:9")
	var blocked blockedLog
	ca, err := NewEphemeralCA()
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(ca.CertPEM())
	res := &scriptedResolver{script: map[string][]string{"gw.rebind.test": {"10.0.0.7"}, "xn--bcher-kva.example": {"10.0.0.8"}, "gw_x.rebind.test": {"10.0.0.9"}}, count: map[string]int{}}
	options := func(ca *EphemeralCA) Options {
		return Options{PinnedHosts: []string{"GW.Rebind.Test.", "::FFFF:10.1.2.3", "Bücher.Example", "[FD00:0::5]", "GW_X.Rebind.Test"}, PinResolver: res, Dial: dial, OnBlocked: blocked.record, InspectCA: ca}
	}
	plain, inspecting := startPinProxy(t, options(nil)), startPinProxy(t, options(ca))
	targets := []string{
		"gw.rebind.test:443", "GW.REBIND.TEST:443", "gw.rebind.test.:8443",
		"xn--bcher-kva.example:443", "XN--BCHER-KVA.EXAMPLE.:443",
		"gw_x.rebind.test:443", "GW_X.REBIND.TEST:443",
		"10.1.2.3:443", "[::ffff:10.1.2.3]:443", "[0:0:0:0:0:ffff:10.1.2.3]:443", "[::ffff:a01:203]:443",
		"[fd00::5]:443", "[FD00:0:0::5]:443",
	}
	for _, target := range targets {
		if answer := connectVia(t, plain, target); !strings.HasPrefix(answer, "502") || !strings.Contains(answer, "is not a public unicast IP") {
			t.Errorf("CONNECT %s: %q; want the pin's refusal", target, answer)
		}
		if status := forwardVia(t, plain, target); status != http.StatusBadGateway {
			t.Errorf("forward to %s: status %d; want the pin's 502", target, status)
		}
	}
	inspected := []string{"GW.REBIND.TEST", "gw.rebind.test.", "bücher.example", "XN--BCHER-KVA.example"}
	for _, host := range inspected {
		if status, body := viaProxy(t, inspecting, "https://"+host+"/x", pool); status != http.StatusBadGateway || !strings.Contains(body, "is not a public unicast IP") {
			t.Errorf("inspected CONNECT to %s: status %d, body %q; want the pin's refusal", host, status, body)
		}
	}
	if got := dialed(); len(got) != 0 {
		t.Errorf("the dialer was asked for %q — a spelling slipped past the pin", got)
	}
	reports := blocked.all()
	if want := 2*len(targets) + len(inspected); len(reports) != want {
		t.Errorf("OnBlocked reported %q; want the %d refusals", reports, want)
	}
	for _, r := range reports {
		if !strings.HasPrefix(r, "gw.rebind.test: ") && !strings.HasPrefix(r, "xn--bcher-kva.example: ") && !strings.HasPrefix(r, "gw_x.rebind.test: ") && !strings.HasPrefix(r, "10.1.2.3: ") && !strings.HasPrefix(r, "fd00::5: ") {
			t.Errorf("OnBlocked reported %q; want the host in its pinned form", r)
		}
	}
}

// A forwarded or an inspected request is dialed on its host as the policy
// read it — lowercase, no root dot, IDN folded — never as the client wrote
// it, which net/http would fold with its own copy of the IDN tables into a
// name the policy, the pin and the choice of transport never read. A
// non-ASCII host the policy's fold refuses is refused, never dialed.
func TestARequestIsDialedOnTheNameThePolicyRead(t *testing.T) {
	const name = "example.com" // the name the httptest certificate carries
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "service") })
	plainService, tlsService := httptest.NewServer(handler), httptest.NewTLSServer(handler)
	defer plainService.Close()
	defer tlsService.Close()
	trusted := x509.NewCertPool()
	trusted.AddCert(tlsService.Certificate())
	ca, err := NewEphemeralCA()
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(ca.CertPEM())
	for _, way := range []string{"forward", "inspected"} {
		t.Run(way, func(t *testing.T) {
			opts, port := Options{}, "80"
			service := plainService
			if way == "inspected" {
				opts.InspectCA, opts.InspectUpstreamTLS, port, service = ca, &tls.Config{RootCAs: trusted}, "443", tlsService
			}
			var dialed func() []string
			opts.Dial, dialed = recordingDial(service.Listener.Addr().String())
			prx := startPinProxy(t, opts)
			spellings := []string{"EXAMPLE.COM", "example.com.", "example.com。", "ｅｘａｍｐｌｅ.com"}
			for _, spelling := range spellings {
				if status, body := requestVia(t, prx, way, spelling, name, "/x", pool); status != http.StatusOK || body != "service" {
					t.Errorf("%s: %d %q; want the service's answer", spelling, status, body)
				}
			}
			// Every spelling is dialed on the one name.
			if got := dialed(); len(got) == 0 || strings.Trim(strings.ReplaceAll(strings.Join(got, " "), name+":"+port, ""), " ") != "" {
				t.Errorf("the dialer was asked for %q; want %s:%s, whatever the spelling", got, name, port)
			}
			before := len(dialed())
			if status, body := requestVia(t, prx, way, "XN--GW-\u00ad.example.com", name, "/x", pool); status != http.StatusBadRequest {
				t.Errorf("a non-ASCII host the fold refuses: %d %q; want 400", status, body)
			}
			if got := dialed(); len(got) != before {
				t.Errorf("the dialer was asked for %q after a host the fold refuses", got[before:])
			}
		})
	}
}

// requestVia sends one GET for path, to the host spelt as written, through
// prx: a plain-HTTP forward, or an inspected tunnel whose proxy certificate
// pool trusts. The spelling rides the request line, the CONNECT authority
// and the tunnelled Host header; the forward's Host header — net/http's
// server refuses it outside ASCII, and ignores it beside an absolute
// request line — names host. It returns the status and the body.
func requestVia(t *testing.T, prx *Proxy, way, spelling, host, path string, pool *x509.CertPool) (int, string) {
	t.Helper()
	conn, err := net.Dial("tcp", prx.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(15 * time.Second))
	r := bufio.NewReader(conn)
	var w io.Writer = conn
	request := "GET http://" + spelling + path + " HTTP/1.1\r\nHost: " + host + "\r\n\r\n"
	if way == "inspected" {
		if _, err := io.WriteString(conn, "CONNECT "+spelling+":443 HTTP/1.1\r\nHost: "+host+":443\r\n\r\n"); err != nil {
			t.Fatal(err)
		}
		resp, err := http.ReadResponse(r, &http.Request{Method: http.MethodConnect})
		if err != nil {
			t.Fatalf("CONNECT %s: %v", spelling, err)
		}
		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			return resp.StatusCode, string(body)
		}
		tunnel := tls.Client(&bufferedConn{Conn: conn, r: r}, &tls.Config{RootCAs: pool, ServerName: host})
		if err := tunnel.Handshake(); err != nil {
			t.Fatalf("TLS to the proxy for %s: %v", spelling, err)
		}
		w, r = tunnel, bufio.NewReader(tunnel)
		request = "GET " + path + " HTTP/1.1\r\nHost: " + spelling + "\r\n\r\n"
	}
	if _, err := io.WriteString(w, request); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(r, nil)
	if err != nil {
		t.Fatalf("GET %s%s: %v", spelling, path, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

// The policy and the pin read a host as the name the proxy would dial: net/
// http folds a Unicode host to its IDN form as written, so U+0130 'İ' is
// 'i' + U+0307, never the ASCII 'i' a lowercasing gives — "apİ.gw.test" is
// another name than "api.gw.test", and an allowlist naming the pinned host
// refuses it on every way out instead of reading it as the pinned host and
// dialing that other name unpinned. The same holds for any allowlisted host.
func TestAHostIsReadAsTheNameTheProxyWouldDial(t *testing.T) {
	const gw = "api.gw.test"
	if got, want := canonicalHost("apİ.gw.test"), "xn--api-bec.gw.test"; got != want {
		t.Fatalf("canonicalHost(apİ.gw.test) = %q, want %q, the name net/http dials", got, want)
	}
	dial, dialed := recordingDial("127.0.0.1:9")
	ca, err := NewEphemeralCA()
	if err != nil {
		t.Fatal(err)
	}
	for _, ca := range []*EphemeralCA{nil, ca} {
		prx := startPinProxy(t, Options{Policy: allowlist(t, gw, "github.com"), PinnedHosts: []string{gw}, PinResolver: newScriptedResolver(gw, "203.0.113.9"), Dial: dial, InspectCA: ca})
		// The spelling rides the request line; the Host header, which the
		// server refuses outside ASCII, names the allowed host.
		for spelt, allowed := range map[string]string{"apİ.gw.test": gw, "ap%C4%B0.gw.test": gw, "gİthub.com": "github.com"} {
			if status := rawStatus(t, prx, "GET http://"+spelt+"/x HTTP/1.1\r\nHost: "+allowed+"\r\nConnection: close\r\n\r\n"); status != http.StatusForbidden {
				t.Errorf("forward to %s (inspecting %t): status %d; want the allowlist's 403", spelt, ca != nil, status)
			}
			if strings.Contains(spelt, "%") {
				continue // a CONNECT authority is not percent-decoded
			}
			if status := rawStatus(t, prx, "CONNECT "+spelt+":443 HTTP/1.1\r\nHost: "+allowed+":443\r\n\r\n"); status != http.StatusForbidden {
				t.Errorf("CONNECT %s (inspecting %t): status %d; want the allowlist's 403", spelt, ca != nil, status)
			}
		}
	}
	if got := dialed(); len(got) != 0 {
		t.Errorf("the dialer was asked for %q — a name the policy did not read", got)
	}
}

// The pin is its hosts' only: another host the policy allows dials as it
// always has, by name; the allowlist still matches by name, so the pinned
// host's address written as a literal stays refused; and the
// host.docker.internal alias, however spelt, keeps its road to the host's
// loopback, never resolved, even when named.
func TestThePinLeavesEverythingElseAsItWas(t *testing.T) {
	const gw = "gw.rebind.test"
	res := newScriptedResolver(gw, "203.0.113.9")
	port, _ := localService(t)
	dial, dialed := recordingDial(net.JoinHostPort("127.0.0.1", port))
	prx := startPinProxy(t, Options{Policy: allowlist(t, gw, "other.test", "host.docker.internal"), PinnedHosts: []string{gw, "host.docker.internal"}, PinResolver: res, Dial: dial})

	for _, target := range []string{gw + ":443", "other.test:443", "host.docker.internal:8080", "HOST.DOCKER.INTERNAL:8081", "host.docker.internal.:8082"} {
		if answer := connectVia(t, prx, target); !strings.HasPrefix(answer, "200") {
			t.Errorf("CONNECT %s: %q; want a tunnel", target, answer)
		}
	}
	if got, want := dialed(), []string{"203.0.113.9:443", "other.test:443", "127.0.0.1:8080", "127.0.0.1:8081", "127.0.0.1:8082"}; strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("the dialer was asked for %q; want %q", got, want)
	}
	if got := res.asked(); len(got) != 1 || got[0] != gw {
		t.Errorf("the pin's resolver was asked for %q; want [%s] only", got, gw)
	}
	if answer := connectVia(t, prx, "203.0.113.9:443"); !strings.HasPrefix(answer, "403") {
		t.Errorf("CONNECT to the pinned host's address literal: %q; want the allowlist's 403", answer)
	}
}

// The unpooled transports are the pinned hosts' own: a host the pin leaves
// alone keeps its pooled connections, on a forward and an inspected tunnel.
func TestAHostThePinLeavesAloneKeepsItsPooledConnections(t *testing.T) {
	const name = "example.com" // the name the httptest certificate carries
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "service") })
	plainService, tlsService := httptest.NewServer(handler), httptest.NewTLSServer(handler)
	defer plainService.Close()
	defer tlsService.Close()
	trusted := x509.NewCertPool()
	trusted.AddCert(tlsService.Certificate())
	ca, err := NewEphemeralCA()
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(ca.CertPEM())
	for _, way := range []string{"forward", "inspected"} {
		t.Run(way, func(t *testing.T) {
			opts := Options{PinSource: func() (string, bool) { return "gw.rebind.test", false }}
			service := plainService
			if way == "inspected" {
				opts.InspectCA, opts.InspectUpstreamTLS, service = ca, &tls.Config{RootCAs: trusted}, tlsService
			}
			var dialed func() []string
			opts.Dial, dialed = recordingDial(service.Listener.Addr().String())
			prx := startPinProxy(t, opts)
			// A connection returns to the pool once its response is read, which
			// can trail the client's next request: fewer dials than requests is
			// pooling, one dial per request is none.
			const requests = 4
			for i := range requests {
				if status, body := requestVia(t, prx, way, name, name, fmt.Sprintf("/%d", i), pool); status != http.StatusOK || body != "service" {
					t.Fatalf("request %d: %d %q; want the service's answer", i, status, body)
				}
			}
			if got := dialed(); len(got) >= requests {
				t.Errorf("the dialer was asked for %q; want fewer connections than the %d requests, pooled", got, requests)
			}
		})
	}
}

// PinAllowPrivate lifts the public-unicast rule: the first answer is dialed,
// a private one included, and still never the name. Without it, an answer
// listing a private address anywhere among public ones is refused.
func TestAPinnedHostAllowedPrivateIsDialedOnItsFirstAnswer(t *testing.T) {
	const gw = "gw.internal.test"
	for _, answer := range []string{"10.255.255.1,203.0.113.9", "203.0.113.9,10.255.255.1"} {
		for _, allowPrivate := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s allow private %t", answer, allowPrivate), func(t *testing.T) {
				dial, dialed := recordingDial("127.0.0.1:9")
				prx := startPinProxy(t, Options{PinnedHosts: []string{gw}, PinAllowPrivate: allowPrivate, PinResolver: newScriptedResolver(gw, answer), Dial: dial})
				got := connectVia(t, prx, gw+":443")
				if !allowPrivate {
					if !strings.HasPrefix(got, "502") || !strings.Contains(got, "resolved address 10.255.255.1 is not a public unicast IP") || len(dialed()) != 0 {
						t.Errorf("CONNECT: %q, dialed %q; want the pin's refusal and no dial", got, dialed())
					}
					return
				}
				if first := strings.Split(answer, ",")[0] + ":443"; len(dialed()) != 1 || dialed()[0] != first {
					t.Errorf("the dialer was asked for %q; want [%s], the first answer", dialed(), first)
				}
			})
		}
	}
}

// PinSource is read when New builds the proxy and again at every request and
// every upstream dial: a host it names joins the pinned hosts for the proxy's
// life — the one it named before stays pinned, the one it named at New
// included, replaced before any connection — and its allowPrivate is the
// pin's mode from then on, a source naming no host included.
func TestThePinSourceIsReadAtNewAndAtEveryConnection(t *testing.T) {
	var mu sync.Mutex
	named, allowPrivate := "gw-start.test", false
	source := func() (string, bool) {
		mu.Lock()
		defer mu.Unlock()
		return named, allowPrivate
	}
	set := func(host string, allow bool) {
		mu.Lock()
		defer mu.Unlock()
		named, allowPrivate = host, allow
	}
	res := &scriptedResolver{script: map[string][]string{"gw-start.test": {"10.0.0.6"}, "gw-a.test": {"10.0.0.7"}, "gw-b.test": {"10.0.0.8"}, "gw-x.test": {"10.0.0.5"}}, count: map[string]int{}}
	dial, dialed := recordingDial("127.0.0.1:9")
	prx := startPinProxy(t, Options{PinSource: source, PinResolver: res, Dial: dial})
	refused := func(target string) bool {
		answer := connectVia(t, prx, target)
		return strings.HasPrefix(answer, "502") && strings.Contains(answer, "is not a public unicast IP")
	}
	set("gw-a.test", false)
	if !refused("gw-a.test:443") {
		t.Error("gw-a, which the source names: not pinned")
	}
	if !refused("gw-start.test:443") {
		t.Error("gw-start, which the source named when the proxy was built and replaced before any connection: not pinned")
	}
	set("gw-b.test", false)
	if !refused("gw-b.test:443") || !refused("gw-a.test:443") {
		t.Error("after the source moved to gw-b: gw-b, or gw-a it named before, not pinned")
	}
	set("", false)
	if !refused("gw-a.test:443") {
		t.Error("a source naming no host unpinned gw-a")
	}
	_ = connectVia(t, prx, "other.test:443")
	set("gw-b.test", true)
	_ = connectVia(t, prx, "gw-a.test:443")
	if got, want := dialed(), []string{"other.test:443", "10.0.0.7:443"}; strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("the dialer was asked for %q; want %q — another host by name, then gw-a on its private answer once the source admits one", got, want)
	}
	set("", false)
	if !refused("gw-a.test:443") {
		t.Error("a source naming no host withdrew the escape hatch, yet gw-a's private answer was not refused")
	}
	// A request to the host alias, which no pin dials, reads the source too.
	set("gw-x.test", false)
	_ = connectVia(t, prx, "host.docker.internal:8080")
	set("", false)
	if !refused("gw-x.test:443") {
		t.Error("gw-x, named while the only request was to the host alias: not pinned")
	}
	if got, want := dialed(), []string{"other.test:443", "10.0.0.7:443", "127.0.0.1:8080"}; strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("the dialer was asked for %q; want %q", got, want)
	}
}

// No pooled connection carries a pinned host's request: one the proxy pooled
// to a host before the pin source named it — dialed by name, on whatever the
// answer was then — or under an escape hatch the source has since withdrawn
// would take the host's next request past the pin. On a plain-HTTP forward
// and an inspected tunnel alike, whatever spelling of the host the requests
// name, the request after the change is dialed through the pin, and refused.
func TestNoPooledConnectionCarriesAPinnedHostsRequest(t *testing.T) {
	const name = "example.com" // the name the httptest certificate carries
	ca, err := NewEphemeralCA()
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(ca.CertPEM())
	for _, way := range []string{"forward", "inspected"} {
		for _, change := range []string{"host pinned", "escape hatch withdrawn"} {
			for _, spelling := range []string{name, "EXAMPLE.COM.", name + "。"} {
				t.Run(way+", "+change+", "+spelling, func(t *testing.T) {
					var mu sync.Mutex
					var served []string
					handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						mu.Lock()
						served = append(served, r.URL.Path)
						mu.Unlock()
						_, _ = io.WriteString(w, "service")
					})
					opts := Options{PinResolver: newScriptedResolver(name, "10.0.0.9")}
					var service *httptest.Server
					if way == "inspected" {
						service = httptest.NewTLSServer(handler)
						trusted := x509.NewCertPool()
						trusted.AddCert(service.Certificate())
						opts.InspectCA, opts.InspectUpstreamTLS = ca, &tls.Config{RootCAs: trusted}
					} else {
						service = httptest.NewServer(handler)
					}
					defer service.Close()
					var dialed func() []string
					opts.Dial, dialed = recordingDial(service.Listener.Addr().String())
					var sourceMu sync.Mutex
					named, allowPrivate := "", false
					if change == "escape hatch withdrawn" {
						named, allowPrivate = name, true
					}
					opts.PinSource = func() (string, bool) {
						sourceMu.Lock()
						defer sourceMu.Unlock()
						return named, allowPrivate
					}
					prx := startPinProxy(t, opts)

					if status, body := requestVia(t, prx, way, spelling, name, "/before", pool); status != http.StatusOK || body != "service" {
						t.Fatalf("scenario broken: the first request got %d %q; want the service's answer", status, body)
					}
					sourceMu.Lock()
					named, allowPrivate = name, false
					sourceMu.Unlock()
					if status, body := requestVia(t, prx, way, spelling, name, "/after", pool); status != http.StatusBadGateway || !strings.Contains(body, "resolved address 10.0.0.9 is not a public unicast IP") {
						t.Errorf("after the change: %d %q; want the pin's refusal", status, body)
					}
					mu.Lock()
					defer mu.Unlock()
					if strings.Join(served, " ") != "/before" {
						t.Errorf("the service served %q; want /before only — a pooled connection carried the request past the pin", served)
					}
					if got := dialed(); len(got) != 1 {
						t.Errorf("the dialer was asked for %q; want the first request's dial only", got)
					}
				})
			}
		}
	}
}

// The pin's lookup and its connect share the bound the proxy's dialer puts
// on a resolution and its connect together — the connect runs under the
// lookup's very deadline — so a pinned name holds a request no longer than
// an unpinned one.
func TestThePinsLookupAndConnectShareTheDialTimeout(t *testing.T) {
	const gw = "gw.rebind.test"
	res := newScriptedResolver(gw, "203.0.113.9")
	var mu sync.Mutex
	var connectDeadline time.Time
	dial := func(ctx context.Context, _, _ string) (net.Conn, error) {
		mu.Lock()
		defer mu.Unlock()
		connectDeadline, _ = ctx.Deadline()
		return nil, errors.New("no network here")
	}
	prx := startPinProxy(t, Options{PinnedHosts: []string{gw}, PinResolver: res, Dial: dial})
	start := time.Now()
	_ = connectVia(t, prx, gw+":443")
	end := time.Now()
	deadlines := res.lookupDeadlines()
	if len(deadlines) != 1 || deadlines[0].Before(start.Add(dialTimeout)) || deadlines[0].After(end.Add(dialTimeout)) {
		t.Fatalf("the pin's lookup ran under the deadlines %v; want one, the dial timeout (%v) from the request", deadlines, dialTimeout)
	}
	mu.Lock()
	defer mu.Unlock()
	if !connectDeadline.Equal(deadlines[0]) {
		t.Fatalf("the connect ran under the deadline %v, the lookup under %v; want the lookup's bound, shared", connectDeadline, deadlines[0])
	}
}

// A client that goes away while the pin resolves its host ends the lookup
// with it, and its request was refused by nobody: no refusal is reported.
func TestAPinLookupEndsWithItsRequest(t *testing.T) {
	const gw = "gw.rebind.test"
	entered, ended := make(chan struct{}), make(chan error, 1)
	res := blockingResolver{entered: entered, ended: ended}
	var blocked blockedLog
	dial, _ := recordingDial("127.0.0.1:9")
	prx := startPinProxy(t, Options{PinnedHosts: []string{gw}, PinResolver: res, Dial: dial, OnBlocked: blocked.record})
	conn, err := net.Dial("tcp", prx.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(conn, "CONNECT "+gw+":443 HTTP/1.1\r\nHost: "+gw+":443\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the pin never looked the host up")
	}
	_ = conn.Close()
	select {
	case err := <-ended:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("the lookup ended with %v; want the request's cancellation", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the lookup outlived its request")
	}
	time.Sleep(100 * time.Millisecond)
	if got := blocked.all(); len(got) != 0 {
		t.Errorf("OnBlocked reported %q for a request its client abandoned", got)
	}
}

// blockingResolver never answers: a lookup waits for its context and
// reports how it ended.
type blockingResolver struct {
	entered chan struct{}
	ended   chan error
}

func (r blockingResolver) LookupIPAddr(ctx context.Context, _ string) ([]net.IPAddr, error) {
	close(r.entered)
	<-ctx.Done()
	r.ended <- ctx.Err()
	return nil, ctx.Err()
}

// An entry that cannot be a host — a port, a path, userinfo, whitespace,
// a stray bracket — fails New, never left unpinned; the error names it by
// position. Whatever a URL's host can hold is pinned as written.
func TestAPinnedHostEntryThatCannotBeAHostFailsNew(t *testing.T) {
	pol, err := Compile(ModeOpen, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range []string{"", " ", ".", "https://gw.corp", "gw.corp/v1", "gw.corp:8443", "gw corp", "gw\tcorp", "gw\rcorp", "gw\ncorp", "gw]corp", "user@gw.corp", "[::1", "[gw.corp]", "[10.1.2.3]", "10.1.2.3:443", "[2001:db8::1]:443"} {
		_, err := New(Options{Policy: pol, PinnedHosts: []string{"gw.ok", entry}})
		if err == nil || !strings.Contains(err.Error(), "pinned host #2") {
			t.Errorf("PinnedHosts entry %q: err = %v; want a refusal naming entry #2", entry, err)
		}
	}
	for _, entry := range []string{"gw.corp", "GW.Corp.", "10.1.2.3", "2001:db8::1", "[2001:db8::1]", "fe80::1%eth0", "xn--bcher-kva.example", "bücher.example", "single-label", "gw..corp", "a,b.example", "gw!.example", "*.corp"} {
		if _, err := New(Options{Policy: pol, PinnedHosts: []string{entry}}); err != nil {
			t.Errorf("PinnedHosts entry %q: %v; want it accepted", entry, err)
		}
	}
}

// Without an injected resolver the pin resolves through the process's own,
// on the proxy's default dialer — the production configuration: a name that
// resolves to the loopback is refused by the pin, as not public, before the
// dialer's own refusal of the proxy host's addresses is reached.
func TestAPinnedHostResolvesThroughTheSystemResolverByDefault(t *testing.T) {
	if addrs, err := net.DefaultResolver.LookupIPAddr(context.Background(), "localhost"); err != nil || len(addrs) == 0 {
		t.Fatalf("scenario broken: localhost does not resolve here (%v)", err)
	}
	port, hits := localService(t)
	prx := startPinProxy(t, Options{PinnedHosts: []string{"localhost"}})
	answer := connectVia(t, prx, net.JoinHostPort("localhost", port))
	if !strings.HasPrefix(answer, "502") || !strings.Contains(answer, "is not a public unicast IP") {
		t.Errorf("CONNECT: %q; want the pin's refusal", answer)
	}
	if n := hits.Load(); n != 0 {
		t.Errorf("the local service took %d connection(s); want none", n)
	}
}
