package runtime

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/compatgw"
	"github.com/SocialGouv/iterion/pkg/sandbox"
	"github.com/SocialGouv/iterion/pkg/sandbox/netproxy"
	"github.com/SocialGouv/iterion/pkg/sandbox/noop"
	"github.com/SocialGouv/iterion/pkg/secure/httpdial"
	"github.com/SocialGouv/iterion/pkg/store"
)

// gatewayPinChildEnv switches the test binary into the child that runs the
// rebinding scenario.
const gatewayPinChildEnv = "ITERION_RUNTIME_GATEWAY_PIN_CHILD"

// The in-container runner checks the gateway's address as it resolves the
// name, then hands the run's proxy the NAME — the proxy's allowlist matches
// names. A resolver that answers the proxy differently (DNS rebinding) must
// not land the connection, the gateway credential with it, on the proxy
// host's private network: the run's proxy resolves the gateway's name again
// through the same guard and dials only what the guard validated. The
// rebinding answer is refused on every way out — a CONNECT tunnel, a
// plain-HTTP forward, an inspected tunnel — and the refusal surfaces as a
// network_blocked event.
//
// End to end through startNetworkProxy, the proxy's own dialer and the
// process resolver, which a scripted DNS server answers: the first answer —
// the runner's check — is public, every later one is not. The process
// resolver is a global, so the scenario runs in a child process that swaps it
// before anything else runs: no goroutine of another test reads it meanwhile.
func TestTheRunProxyPinsTheGatewayNameAgainstARebindingAnswer(t *testing.T) {
	if os.Getenv(gatewayPinChildEnv) == "1" {
		gatewayPinChild(t)
		return
	}
	child := exec.Command(os.Args[0], "-test.run=^TestTheRunProxyPinsTheGatewayNameAgainstARebindingAnswer$", "-test.count=1", "-test.v")
	child.Env = append(os.Environ(), gatewayPinChildEnv+"=1")
	out, err := child.CombinedOutput()
	if err != nil {
		t.Fatalf("the rebinding child: %v\n%s", err, out)
	}
	t.Logf("the rebinding child:\n%s", out)
}

func gatewayPinChild(t *testing.T) {
	dns := startScriptedDNS(t)
	// Swapped once, before any proxy runs, and never restored: the child
	// exits with this test.
	net.DefaultResolver = &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "udp", dns.addr())
	}}
	// A configured gateway is the PAIR: the pin reads it through
	// compatgw.FromEnv, which refuses a gateway with no key by name. Set
	// once for every case below — what is pinned is the host, and nothing
	// here authenticates anything.
	t.Setenv(compatgw.APIKeyEnv, "gw-pin-fixture")
	drv, err := noop.New()
	if err != nil {
		t.Fatal(err)
	}
	// runnerCheck is the in-container runner's own guard: the gateway's
	// first answer, which must pass it.
	runnerCheck := func(t *testing.T, host string, strict bool) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if ip, err := httpdial.ResolvePublicHost(ctx, host, strict); err != nil || ip.String() != "203.0.113.7" {
			t.Fatalf("scenario broken: the runner's check got %v, %v; want the public first answer", ip, err)
		}
	}

	// Each way out, under each policy mode a proxy runs in.
	for _, c := range []struct {
		way, host, scheme, mode string
		inspect                 bool
	}{
		{"CONNECT", "gw-connect.rebind.test", "https", "allowlist", false},
		{"forward", "gw-forward.rebind.test", "http", "denylist", false},
		{"inspected CONNECT", "gw-inspect.rebind.test", "https", "open", true},
	} {
		t.Run(c.way+" ("+c.mode+")", func(t *testing.T) {
			t.Setenv(compatgw.BaseURLEnv, "https://"+c.host)
			t.Setenv("ITERION_LLM_ENDPOINT_ALLOW_PRIVATE", "")
			dns.script(c.host, "203.0.113.7", "10.255.255.1")
			events := &blockedEvents{}
			client := startGatewayRunProxy(t, drv, c.mode, []string{c.host}, c.inspect, events)

			runnerCheck(t, c.host, true)
			outcome := fetch(client, c.scheme+"://"+c.host+"/v1/chat/completions")
			if dns.asked(c.host) < 2 {
				t.Fatalf("scenario broken: the proxy never resolved %s again (dns: %s; client: %s)", c.host, dns.log(c.host), outcome)
			}
			if strings.HasPrefix(outcome, "200") {
				t.Errorf("the request crossed: %s", outcome)
			}
			if !events.refused(c.host, "10.255.255.1", "not a public unicast IP") {
				t.Errorf("no network_blocked event refuses %s's rebinding answer — the proxy connected where its own answer pointed (events: %v; dns: %s; client: %s)", c.host, events.all(), dns.log(c.host), outcome)
			}
		})
	}

	// The environment reloaded after the proxy started — another gateway,
	// as the desktop's refresh can — is the gateway the runner is handed:
	// it is pinned, and the gateway it replaced stays pinned.
	t.Run("gateway changed mid-run", func(t *testing.T) {
		const before, after = "gw-before.rebind.test", "gw-after.rebind.test"
		t.Setenv(compatgw.BaseURLEnv, "https://"+before)
		t.Setenv("ITERION_LLM_ENDPOINT_ALLOW_PRIVATE", "")
		dns.script(before, "10.255.255.2")
		dns.script(after, "203.0.113.7", "10.255.255.3")
		events := &blockedEvents{}
		client := startGatewayRunProxy(t, drv, "allowlist", []string{before, after}, false, events)
		_ = fetch(client, "http://"+before+"/v1/models")

		t.Setenv(compatgw.BaseURLEnv, "https://"+after)
		runnerCheck(t, after, true)
		outcome := fetch(client, "http://"+after+"/v1/chat/completions")
		if !events.refused(after, "10.255.255.3", "not a public unicast IP") {
			t.Errorf("the gateway named after the proxy started is not pinned (events: %v; dns: %s; client: %s)", events.all(), dns.log(after), outcome)
		}
		_ = fetch(client, "http://"+before+"/v1/models")
		if n := events.count(before, "10.255.255.2", "not a public unicast IP"); n != 2 {
			t.Errorf("the gateway the environment named before was refused %d time(s); want 2 — it stays pinned (events: %v)", n, events.all())
		}
	})

	// ITERION_LLM_ENDPOINT_ALLOW_PRIVATE=1 lifts the public-unicast rule for
	// the gateway on the proxy as on the runner: a private answer is not the
	// pin's to refuse — the proxy's own refusal of its host's addresses
	// still answers a loopback one — while the pin still resolves the name
	// itself, and says so when it does not resolve.
	t.Run("allow private", func(t *testing.T) {
		const private, missing = "gw-private.rebind.test", "gw-missing.rebind.test"
		t.Setenv(compatgw.BaseURLEnv, "https://"+private)
		t.Setenv("ITERION_LLM_ENDPOINT_ALLOW_PRIVATE", "1")
		dns.script(private, "203.0.113.7", "127.0.0.2")
		events := &blockedEvents{}
		client := startGatewayRunProxy(t, drv, "allowlist", []string{private, missing}, false, events)

		runnerCheck(t, private, false)
		outcome := fetch(client, "http://"+private+"/v1/chat/completions")
		if dns.asked(private) < 2 {
			t.Fatalf("scenario broken: the proxy never resolved %s again (dns: %s; client: %s)", private, dns.log(private), outcome)
		}
		if !strings.HasPrefix(outcome, "502") || !strings.Contains(outcome, "netproxy: dial refused") {
			t.Errorf("the forward answered %s; want the proxy's own refusal of a loopback destination", outcome)
		}
		if events.refused(private, "127.0.0.2", "not a public unicast IP") {
			t.Errorf("the gateway's private answer was refused as not public with ITERION_LLM_ENDPOINT_ALLOW_PRIVATE=1 (events: %v)", events.all())
		}

		t.Setenv(compatgw.BaseURLEnv, "https://"+missing)
		dns.script(missing, "203.0.113.7", "NX")
		runnerCheck(t, missing, false)
		outcome = fetch(client, "http://"+missing+"/v1/chat/completions")
		if !events.refused(missing, "pinned host:", "no such host") {
			t.Errorf("the pin did not resolve %s itself: no refusal names its missing answer (events: %v; client: %s)", missing, events.all(), outcome)
		}
	})

	// The escape hatch withdrawn after the proxy started: the runner is
	// strict again, and so is the pin.
	t.Run("allow private withdrawn mid-run", func(t *testing.T) {
		const host = "gw-withdrawn.rebind.test"
		t.Setenv(compatgw.BaseURLEnv, "https://"+host)
		t.Setenv("ITERION_LLM_ENDPOINT_ALLOW_PRIVATE", "1")
		dns.script(host, "203.0.113.7", "10.255.255.4")
		events := &blockedEvents{}
		client := startGatewayRunProxy(t, drv, "allowlist", []string{host}, false, events)

		t.Setenv("ITERION_LLM_ENDPOINT_ALLOW_PRIVATE", "")
		runnerCheck(t, host, true)
		outcome := fetch(client, "http://"+host+"/v1/chat/completions")
		if !events.refused(host, "10.255.255.4", "not a public unicast IP") {
			t.Errorf("the pin kept the escape hatch the environment withdrew (events: %v; dns: %s; client: %s)", events.all(), dns.log(host), outcome)
		}
	})

	// The gateway unset mid-run, its escape hatch with it: the gateway stays
	// pinned, at the strictness the environment now sets.
	t.Run("gateway unset mid-run", func(t *testing.T) {
		const host = "gw-unset.rebind.test"
		t.Setenv(compatgw.BaseURLEnv, "https://"+host)
		t.Setenv("ITERION_LLM_ENDPOINT_ALLOW_PRIVATE", "1")
		dns.script(host, "10.255.255.5")
		events := &blockedEvents{}
		client := startGatewayRunProxy(t, drv, "allowlist", []string{host}, false, events)

		t.Setenv(compatgw.BaseURLEnv, "")
		t.Setenv("ITERION_LLM_ENDPOINT_ALLOW_PRIVATE", "")
		outcome := fetch(client, "http://"+host+"/v1/chat/completions")
		if !events.refused(host, "10.255.255.5", "not a public unicast IP") {
			t.Errorf("the gateway the environment stopped naming was dialed on its private answer (events: %v; dns: %s; client: %s)", events.all(), dns.log(host), outcome)
		}
	})
}

// startGatewayRunProxy starts the run's proxy as the engine does — its
// network mode, naming the gateways in an allowlist, inspecting when asked —
// and returns a client crossing it.
func startGatewayRunProxy(t *testing.T, drv sandbox.Driver, mode string, gateways []string, inspect bool, events *blockedEvents) *http.Client {
	t.Helper()
	block := "    network:\n      mode: " + mode
	switch mode {
	case "allowlist":
		block += "\n      rules: [\"" + strings.Join(gateways, "\", \"") + "\"]"
	case "denylist":
		block += "\n      rules: [\"denied.rebind.test\"]"
	}
	spec := compileSandboxSpec(t, netPolicySource(block))
	var rewriter netproxy.SecretRewriter
	if inspect {
		rewriter, drv = &recordingRewriter{}, inspectingDriver{drv}
	}
	prx, endpoint, caPEM, err := startNetworkProxy(spec, drv, "run-gateway-pin", rewriter, events.emit, nil)
	if err != nil {
		t.Fatalf("startNetworkProxy: %v", err)
	}
	if prx == nil || inspect != (caPEM != nil) {
		t.Fatalf("scenario broken: proxy %v, inspecting %t, want inspecting %t", prx, caPEM != nil, inspect)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = prx.Shutdown(ctx)
	})
	proxyURL, err := url.Parse("http://iterion:" + tokenFromEndpoint(t, endpoint) + "@" + prx.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	transport := &http.Transport{Proxy: http.ProxyURL(proxyURL)}
	if caPEM != nil {
		pool := x509.NewCertPool()
		pool.AppendCertsFromPEM(caPEM)
		transport.TLSClientConfig = &tls.Config{RootCAs: pool}
	}
	return &http.Client{Timeout: 30 * time.Second, Transport: transport}
}

// fetch posts a model request through client and reports its outcome: the
// status and body, or the error.
func fetch(client *http.Client, rawURL string) string {
	resp, err := client.Post(rawURL, "application/json", strings.NewReader(`{"model":"m"}`))
	if err != nil {
		return "error: " + err.Error()
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	return fmt.Sprintf("%d %s", resp.StatusCode, strings.TrimSpace(string(body)))
}

// blockedEvents records the network_blocked events a run's proxy emits.
type blockedEvents struct {
	mu   sync.Mutex
	seen []map[string]any
}

func (e *blockedEvents) emit(typ store.EventType, data map[string]any) error {
	if typ == store.EventNetworkBlocked {
		e.mu.Lock()
		e.seen = append(e.seen, data)
		e.mu.Unlock()
	}
	return nil
}

// refused reports whether an event refused host for a reason naming every
// fragment.
func (e *blockedEvents) refused(host string, fragments ...string) bool {
	return e.count(host, fragments...) > 0
}

// count counts the events refusing host for a reason naming every fragment.
func (e *blockedEvents) count(host string, fragments ...string) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	n := 0
	for _, ev := range e.seen {
		reason, _ := ev["reason"].(string)
		if ev["host"] != host {
			continue
		}
		matched := true
		for _, f := range fragments {
			matched = matched && strings.Contains(reason, f)
		}
		if matched {
			n++
		}
	}
	return n
}

func (e *blockedEvents) all() []map[string]any {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]map[string]any(nil), e.seen...)
}

// scriptedDNS is a DNS server on the loopback. An A query for a scripted
// name gets the script's next answer, the last one repeating — "NX" answers
// NXDOMAIN; an AAAA query for it gets none; any other name is NXDOMAIN. It
// logs every A answer.
type scriptedDNS struct {
	conn net.PacketConn

	mu      sync.Mutex
	answers map[string][]string
	count   map[string]int
	queries []string
}

func startScriptedDNS(t *testing.T) *scriptedDNS {
	t.Helper()
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	d := &scriptedDNS{conn: conn, answers: map[string][]string{}, count: map[string]int{}}
	go func() {
		buf := make([]byte, 1500)
		for {
			n, from, err := conn.ReadFrom(buf)
			if err != nil {
				return
			}
			if reply := d.answer(buf[:n]); reply != nil {
				_, _ = conn.WriteTo(reply, from)
			}
		}
	}()
	return d
}

func (d *scriptedDNS) addr() string { return d.conn.LocalAddr().String() }

// script sets the A answers for name, one per query.
func (d *scriptedDNS) script(name string, answers ...string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.answers[name] = answers
}

// asked counts the A queries name received.
func (d *scriptedDNS) asked(name string) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.count[name]
}

// log lists the A answers name got, in order.
func (d *scriptedDNS) log(name string) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []string
	for _, q := range d.queries {
		if strings.HasPrefix(q, name+"#") {
			out = append(out, q)
		}
	}
	return strings.Join(out, " ")
}

// answer builds the reply to one query; nil drops a malformed one. A
// scripted name answers an A query only: any other type gets no answer.
func (d *scriptedDNS) answer(query []byte) []byte {
	const typeA = 1
	if len(query) < 12 {
		return nil
	}
	off := 12
	var labels []string
	for {
		if off >= len(query) {
			return nil
		}
		l := int(query[off])
		off++
		if l == 0 {
			break
		}
		if off+l > len(query) {
			return nil
		}
		labels = append(labels, string(query[off:off+l]))
		off += l
	}
	if off+4 > len(query) {
		return nil
	}
	qtype := binary.BigEndian.Uint16(query[off:])
	question := query[12 : off+4]
	name := strings.ToLower(strings.Join(labels, "."))

	var ips []net.IP
	d.mu.Lock()
	script, known := d.answers[name]
	missing := !known
	if known && qtype == typeA {
		d.count[name]++
		answer := script[min(d.count[name], len(script))-1]
		if missing = answer == "NX"; !missing {
			ips = []net.IP{net.ParseIP(answer).To4()}
		}
		d.queries = append(d.queries, fmt.Sprintf("%s#%d->%s", name, d.count[name], answer))
	}
	d.mu.Unlock()

	flags := uint16(0x8180) // a response, recursion desired and available
	if missing {
		flags |= 3 // NXDOMAIN
	}
	reply := append([]byte{}, query[0], query[1])
	reply = binary.BigEndian.AppendUint16(reply, flags)
	reply = binary.BigEndian.AppendUint16(reply, 1)
	reply = binary.BigEndian.AppendUint16(reply, uint16(len(ips)))
	reply = binary.BigEndian.AppendUint16(reply, 0)
	reply = binary.BigEndian.AppendUint16(reply, 0)
	reply = append(reply, question...)
	for _, ip := range ips {
		reply = append(reply, 0xC0, 0x0C) // the question's name
		reply = binary.BigEndian.AppendUint16(reply, typeA)
		reply = binary.BigEndian.AppendUint16(reply, 1) // class IN
		reply = binary.BigEndian.AppendUint32(reply, 0) // TTL
		reply = binary.BigEndian.AppendUint16(reply, uint16(len(ip)))
		reply = append(reply, ip...)
	}
	return reply
}
