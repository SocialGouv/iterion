package bots

import (
	"bufio"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/internal/gittest"
)

// The three outcomes the probe may answer, and the only ones. `hold` is a
// BLOCKING verdict published on a third party's pull request; `unavailable`
// ends the run typed and publishes nothing.
const (
	probeDeliver     = "deliver"
	probeHold        = "hold"
	probeUnavailable = "unavailable"
)

type deliveryVerdict struct {
	OK     bool   `json:"ok"`
	Denied bool   `json:"denied"`
	Code   string `json:"code"`
	Reason string `json:"reason"`
}

// outcomeOf classifies an answer, and refuses to classify one that is none of
// the three: a fourth shape is a state the graph has no edge for.
func outcomeOf(t *testing.T, v deliveryVerdict) string {
	t.Helper()
	switch {
	case v.OK && !v.Denied && v.Code == "":
		return probeDeliver
	case !v.OK && v.Denied && v.Code == "FORGE_PERMISSION_DENIED":
		return probeHold
	case !v.OK && !v.Denied && v.Code == "FORGE_PREFLIGHT_UNAVAILABLE":
		return probeUnavailable
	}
	t.Fatalf("the probe answered %+v — neither a permitted delivery, an answered denial nor an unavailable preflight; "+
		"every edge out of the probe is guarded on one of the three, so this answer routes nowhere", v)
	return ""
}

const (
	denyReason  = "workflows:write is missing or unverified on the runtime token; an operator must approve the App's delivery permissions, refresh its token and relaunch"
	allowReason = "runtime token has confirmed workflows:write and contents:write"
)

func denyBody() string {
	b, _ := json.Marshal(map[string]any{"ok": false, "code": "FORGE_PERMISSION_DENIED", "reason": denyReason})
	return string(b)
}

func allowBody() string {
	b, _ := json.Marshal(map[string]any{"ok": true, "code": "", "reason": allowReason})
	return string(b)
}

// hijackWrite answers the raw socket and drops it — what httptest cannot do
// through its ResponseWriter: die with the headers already out, cut a body
// mid-transfer, stop in the middle of a chunk.
func hijackWrite(t *testing.T, w http.ResponseWriter, raw string) {
	t.Helper()
	conn, _, err := w.(http.Hijacker).Hijack()
	if err != nil {
		t.Error(err)
		return
	}
	if raw != "" {
		_, _ = conn.Write([]byte(raw))
	}
	_ = conn.Close()
}

type faultRow struct {
	name string
	// serve answers call n (1-based); target is the address a redirecting
	// answer points at, so a row can prove nothing follows it.
	serve func(t *testing.T, w http.ResponseWriter, r *http.Request, n int32, target string)
	// dead points the probe at an address nothing listens on.
	dead bool
	// want is the outcome the probe owes, wantCalls the number of requests
	// the service must have seen (0 = unchecked: a dead address sees none).
	want      string
	wantCalls int
	wantIn    string
	wantNotIn string
	// token overrides the bytes written to the mounted token file, for a row
	// whose fault is in the credential the environment carries rather than in
	// what the service answers.
	token []byte
	// origin overrides the fixture's github.com remote and prURL the pull
	// request URL, for a row whose fault is in the forge the delivery is
	// addressed to rather than in what the service answers.
	origin string
	prURL  string
	// wantNoCalls asserts the service was never asked at all — distinct from
	// wantCalls 0, which leaves the count unchecked.
	wantNoCalls bool
}

// TestDeliveryProbeOnlyAWellFormedAnswerCanHold is the TEST OF THE GUARANTEE,
// and it replaces the enumeration it grew out of. Three adversarial rounds
// each found one more transport shape the probe published as a blocking hold
// on a third party's pull request — a URLError, then a reset the retry did
// not cover (RemoteDisconnected is a ConnectionResetError), then a
// Content-Length body cut mid-transfer, which raises NOTHING and returns a
// short read. A blacklist of an HTTP library's failure modes does not
// converge, so the probe now carries its complement: a hold needs a
// WELL-FORMED ANSWER, and every other shape — named here or not — is one
// outcome, the service did not answer.
//
// Each row drives the REAL compiled program of EVERY carrier against a stub
// that misbehaves in one way. The assertion that matters is the first one: no
// row but an answered denial may produce a hold.
//
// The last rows vary the OTHER side of the question instead — the credential
// and the pull request the run was handed, under a well-formed permission.
// A term keyed on one of those rather than on the answer is the same defect
// as a transport shape published as a hold, and the source counts of
// TestDeliveryProbeCarriersRefuseAnUnprovableDelivery cannot see it: they
// count the two spellings this program writes a verdict in, so a third one
// answers to no count.
//
// Two rows pay a real socket timeout — the refused connection and the service
// that never answers, ~11 s each — and the rest are bounded by the single 2 s
// pause between the two attempts; rows and carriers run in parallel, so the
// matrix costs about one slow row.
func TestDeliveryProbeOnlyAWellFormedAnswerCanHold(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not on PATH")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}

	const okHeaders = "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\n"
	rows := []faultRow{
		{
			name: "the connection is refused",
			dead: true, want: probeUnavailable,
			wantIn: "did not answer", wantNotIn: "is missing or unverified",
		},
		{
			name: "the connection is accepted and reset before the headers",
			serve: func(t *testing.T, w http.ResponseWriter, _ *http.Request, _ int32, _ string) {
				hijackWrite(t, w, "")
			},
			want: probeUnavailable, wantCalls: 2, wantIn: "did not answer",
		},
		{
			name: "the connection is reset in the middle of the headers",
			serve: func(t *testing.T, w http.ResponseWriter, _ *http.Request, _ int32, _ string) {
				hijackWrite(t, w, "HTTP/1.1 200 OK\r\nContent-Ty")
			},
			want: probeUnavailable, wantCalls: 2,
		},
		{
			name: "the connection is reset in the middle of the body",
			serve: func(t *testing.T, w http.ResponseWriter, _ *http.Request, _ int32, _ string) {
				hijackWrite(t, w, okHeaders+"Content-Length: 300\r\n\r\n"+denyBody()[:30])
			},
			want: probeUnavailable, wantCalls: 2,
		},
		{
			name: "the service answers 502",
			serve: func(_ *testing.T, w http.ResponseWriter, _ *http.Request, _ int32, _ string) {
				w.WriteHeader(http.StatusBadGateway)
			},
			want: probeUnavailable, wantCalls: 2, wantNotIn: "is missing or unverified",
		},
		{
			name: "the service answers 401 with an HTML body",
			serve: func(_ *testing.T, w http.ResponseWriter, _ *http.Request, _ int32, _ string) {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte("<html>401 Unauthorized</html>"))
			},
			want: probeUnavailable, wantCalls: 2, wantNotIn: "is missing or unverified",
		},
		{
			// The trap the whole inversion exists for: the body PARSES as a
			// complete denial and the framing says bytes are still owed.
			name: "a verdict whose Content-Length promises more than arrived",
			serve: func(t *testing.T, w http.ResponseWriter, _ *http.Request, _ int32, _ string) {
				body := denyBody()
				hijackWrite(t, w, okHeaders+fmt.Sprintf("Content-Length: %d\r\n\r\n", len(body)+50)+body)
			},
			want: probeUnavailable, wantCalls: 2, wantNotIn: "is missing or unverified",
		},
		{
			name: "an answer longer than the bound the probe reads",
			serve: func(_ *testing.T, w http.ResponseWriter, _ *http.Request, _ int32, _ string) {
				b, _ := json.Marshal(map[string]any{
					"ok": false, "code": "FORGE_PERMISSION_DENIED", "reason": denyReason,
					"padding": strings.Repeat("x", 9000),
				})
				_, _ = w.Write(b)
			},
			want: probeUnavailable, wantCalls: 2, wantNotIn: "is missing or unverified",
		},
		{
			name: "the body is not JSON",
			serve: func(_ *testing.T, w http.ResponseWriter, _ *http.Request, _ int32, _ string) {
				_, _ = w.Write([]byte("<html>502 Bad Gateway</html>"))
			},
			want: probeUnavailable, wantCalls: 2, wantNotIn: "is missing or unverified",
		},
		{
			name: "the verdict is missing the fields it is read for",
			serve: func(_ *testing.T, w http.ResponseWriter, _ *http.Request, _ int32, _ string) {
				_, _ = w.Write([]byte(`{"ok":false}`))
			},
			want: probeUnavailable, wantCalls: 2,
		},
		{
			name: "the chunked body stops in the middle of a chunk",
			serve: func(t *testing.T, w http.ResponseWriter, _ *http.Request, _ int32, _ string) {
				hijackWrite(t, w, "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nTransfer-Encoding: chunked\r\n\r\n40\r\n"+denyBody()[:20])
			},
			want: probeUnavailable, wantCalls: 2,
		},
		{
			// The pod that accepted the request and is still thinking when
			// the socket timeout fires.
			name: "the service never answers",
			serve: func(_ *testing.T, _ http.ResponseWriter, r *http.Request, _ int32, _ string) {
				select {
				case <-time.After(11 * time.Second):
				case <-r.Context().Done():
				}
			},
			want: probeUnavailable, wantCalls: 2,
		},
		{
			name: "the service redirects the question elsewhere",
			serve: func(_ *testing.T, w http.ResponseWriter, r *http.Request, _ int32, target string) {
				http.Redirect(w, r, target, http.StatusTemporaryRedirect)
			},
			want: probeUnavailable, wantCalls: 2,
		},
		{
			// A denial is a 200 with a verdict (writeJSON, forge_delivery_preflight.go);
			// a 4xx there is an error envelope written by httpError, never a
			// verdict. Reading one as a denial is the inversion in reverse.
			name: "a denial under a status the endpoint never answers verdicts with",
			serve: func(_ *testing.T, w http.ResponseWriter, _ *http.Request, _ int32, _ string) {
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte(denyBody()))
			},
			want: probeUnavailable, wantCalls: 2, wantNotIn: "is missing or unverified",
		},
		{
			// The endpoint's own answer when the token the run mounted is no
			// longer the credential its connection stores, or when that
			// connection holds no App credential to read a permission against:
			// an error envelope under 409, never a verdict
			// (pkg/server/forge_delivery_preflight.go).
			name: "the endpoint answers 409: this service knows nothing about the mounted credential",
			serve: func(_ *testing.T, w http.ResponseWriter, _ *http.Request, _ int32, _ string) {
				w.WriteHeader(http.StatusConflict)
				_, _ = w.Write([]byte(`{"error":"the token mounted in this run is not the credential currently stored for this connection, so its delivery permission is unknown"}`))
			},
			want: probeUnavailable, wantCalls: 2, wantNotIn: "is missing or unverified",
		},
		{
			// The same endpoint when its own store, sealer, connection record
			// or mint proof cannot establish anything: 503 and an envelope.
			name: "the endpoint answers 503: its own bookkeeping establishes nothing",
			serve: func(_ *testing.T, w http.ResponseWriter, _ *http.Request, _ int32, _ string) {
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = w.Write([]byte(`{"error":"no live mint proof for the credential this connection stores, so its delivery permission is unknown"}`))
			},
			want: probeUnavailable, wantCalls: 2, wantNotIn: "is missing or unverified",
		},
		{
			// 2xx and not the one the endpoint answers verdicts with. urllib
			// raises on every non-2xx, so the status term bites HERE and
			// nowhere else: a middlebox that acknowledges the request and
			// hands back a body must not have that body read as a verdict.
			name: "a verdict body under a 2xx the endpoint never answers with",
			serve: func(_ *testing.T, w http.ResponseWriter, _ *http.Request, _ int32, _ string) {
				w.WriteHeader(http.StatusAccepted)
				_, _ = w.Write([]byte(denyBody()))
			},
			want: probeUnavailable, wantCalls: 2, wantNotIn: "is missing or unverified",
		},
		{
			// The bound read ONE byte past itself, and why. Chunked framing
			// owes no length, so a body cut at the bound is indistinguishable
			// from a complete one by the framing alone: here the first 8192
			// bytes are a syntactically complete DENIAL and more bytes follow.
			// Reading exactly the bound would parse that prefix and publish a
			// hold; reading bound+1 sees the answer is over-long.
			name: "a chunked answer whose first 8192 bytes are a complete verdict",
			serve: func(t *testing.T, w http.ResponseWriter, _ *http.Request, _ int32, _ string) {
				head := `{"ok":false,"code":"FORGE_PERMISSION_DENIED","reason":"` + denyReason + `","padding":"`
				tail := `"}`
				body := head + strings.Repeat("x", 8192-len(head)-len(tail)) + tail
				if len(body) != 8192 {
					t.Fatalf("the crafted prefix is %d bytes, want exactly the bound 8192", len(body))
				}
				hijackWrite(t, w, "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nTransfer-Encoding: chunked\r\n\r\n"+
					fmt.Sprintf("%x\r\n", len(body))+body+"\r\n64\r\n"+strings.Repeat("y", 100)+"\r\n")
			},
			want: probeUnavailable, wantCalls: 2, wantNotIn: "is missing or unverified",
		},
		{
			name: "the service answers 204 with no content",
			serve: func(_ *testing.T, w http.ResponseWriter, _ *http.Request, _ int32, _ string) {
				w.WriteHeader(http.StatusNoContent)
			},
			want: probeUnavailable, wantCalls: 2,
		},
		{
			name: "the verdict carries a code the endpoint does not document",
			serve: func(_ *testing.T, w http.ResponseWriter, _ *http.Request, _ int32, _ string) {
				_, _ = w.Write([]byte(`{"ok":false,"code":"SOMETHING_ELSE","reason":"nope"}`))
			},
			want: probeUnavailable, wantCalls: 2,
		},
		{
			name: "the verdict contradicts itself",
			serve: func(_ *testing.T, w http.ResponseWriter, _ *http.Request, _ int32, _ string) {
				_, _ = w.Write([]byte(`{"ok":true,"code":"FORGE_PERMISSION_DENIED","reason":"both"}`))
			},
			want: probeUnavailable, wantCalls: 2,
		},
		{
			// A hold with no cause renders an empty "Code alignment" section
			// under a red required check: the operator reads that the
			// alignment was refused and nothing about why.
			name: "the denial carries no reason",
			serve: func(_ *testing.T, w http.ResponseWriter, _ *http.Request, _ int32, _ string) {
				_, _ = w.Write([]byte(`{"ok":false,"code":"FORGE_PERMISSION_DENIED","reason":"  "}`))
			},
			want: probeUnavailable, wantCalls: 2,
		},
		{
			// RFC 7230 3.3.3 case 4: two different Content-Length values make
			// the message invalid. http.client keeps the FIRST, so the
			// completeness term below would be reading one of two framings —
			// and a body "complete" under one of them is a verdict about
			// nothing.
			name: "a verdict under two disagreeing Content-Length headers",
			serve: func(t *testing.T, w http.ResponseWriter, _ *http.Request, _ int32, _ string) {
				body := denyBody()
				hijackWrite(t, w, okHeaders+
					fmt.Sprintf("Content-Length: %d\r\n", len(body))+
					fmt.Sprintf("Content-Length: %d\r\n\r\n", len(body)+7)+body)
			},
			want: probeUnavailable, wantCalls: 2, wantNotIn: "is missing or unverified",
		},
		{
			// The bound ITSELF, falsified: no Content-Length and no chunking,
			// so the framing owes nothing and the only thing standing between
			// this complete denial and a published hold is the comparison
			// against 8192. One byte over, and it is not an answer.
			name: "a close-delimited answer one byte past the bound",
			serve: func(t *testing.T, w http.ResponseWriter, _ *http.Request, _ int32, _ string) {
				head := `{"ok":false,"code":"FORGE_PERMISSION_DENIED","reason":"` + denyReason + `","padding":"`
				tail := `"}`
				body := head + strings.Repeat("x", 8193-len(head)-len(tail)) + tail
				if len(body) != 8193 {
					t.Fatalf("the crafted body is %d bytes, want exactly one past the bound", len(body))
				}
				hijackWrite(t, w, okHeaders+"\r\n"+body)
			},
			want: probeUnavailable, wantCalls: 2, wantNotIn: "is missing or unverified",
		},
		{
			// The last line of the complement: a shape that is not an HTTP
			// answer at all. The mounted token is read in TEXT mode, so bytes
			// that are not UTF-8 raise inside the program — the outer
			// handler's own path, and the one a shape nobody foresaw lands on.
			name:  "the mounted token is not valid UTF-8",
			token: []byte{0x67, 0x68, 0x73, 0x5f, 0xff, 0xfe, 0x0a},
			serve: func(_ *testing.T, w http.ResponseWriter, _ *http.Request, _ int32, _ string) {
				_, _ = w.Write([]byte(denyBody()))
			},
			want: probeUnavailable, wantNoCalls: true,
			wantIn: "could not establish delivery permission", wantNotIn: "is missing or unverified",
		},
		{
			// GitHub ENTERPRISE SERVER, whose App enforces the `workflows`
			// restriction exactly as github.com does — the probe speaks
			// https://<authority>/api/v3 to it — addressed in the REST
			// spelling, which is the path Forgejo serves its own pull
			// requests on. Read as an exemption, the guard turns ITSELF off
			// on a forge where the permission being proved is the permission
			// at stake, and #1595's 403 comes back diagnosed as a lost
			// alignment. The exemption is a statement about the CREDENTIAL,
			// not about the shape of a URL an operator typed.
			name:   "a GHES pull request in the REST spelling is asked about, not exempted",
			origin: "https://ghe.corp.example/acme/widgets.git",
			prURL:  "https://ghe.corp.example/acme/widgets/pulls/7",
			serve: func(_ *testing.T, w http.ResponseWriter, _ *http.Request, _ int32, _ string) {
				_, _ = w.Write([]byte(denyBody()))
			},
			want: probeHold, wantCalls: 1, wantIn: denyReason,
		},
		{
			// The same invalidity as the two disagreeing headers above, in
			// its ONE-header spelling: a comma-folded duplicate is a list of
			// lengths, which http.client reads as no length at all — and
			// "nothing is owed" is exactly what the completeness term reads
			// as a body that arrived whole.
			name: "a verdict under a comma-folded duplicate Content-Length",
			serve: func(t *testing.T, w http.ResponseWriter, _ *http.Request, _ int32, _ string) {
				body := denyBody()
				hijackWrite(t, w, okHeaders+
					fmt.Sprintf("Content-Length: %d, %d\r\n\r\n", len(body), len(body))+body)
			},
			want: probeUnavailable, wantCalls: 2, wantNotIn: "is missing or unverified",
		},
		{
			// The third spelling: a length declared beside a transfer coding,
			// which RFC 7230 3.3.3 forbids precisely because the two framings
			// can disagree. http.client lets the coding win and owes no
			// length, so a complete-looking chunked body carries a verdict
			// out of a message no proxy on the path needs to have read the
			// same way.
			name: "a verdict framed by both a Content-Length and a transfer coding",
			serve: func(t *testing.T, w http.ResponseWriter, _ *http.Request, _ int32, _ string) {
				body := denyBody()
				hijackWrite(t, w, okHeaders+
					fmt.Sprintf("Content-Length: %d\r\n", len(body))+
					"Transfer-Encoding: chunked\r\n\r\n"+
					fmt.Sprintf("%x\r\n", len(body))+body+"\r\n0\r\n\r\n")
			},
			want: probeUnavailable, wantCalls: 2, wantNotIn: "is missing or unverified",
		},
		{
			// The framing guard's other side: chunked is a LEGITIMATE
			// framing, and a complete answer under it is an answer. The
			// endpoint's own verdicts are small enough for net/http to
			// length-frame them, so without this row a guard refusing every
			// transfer coding would look free -- and any intermediary that
			// re-frames the response would silently cost the lane every
			// verdict it carries.
			name: "a well-formed denial under chunked framing",
			serve: func(t *testing.T, w http.ResponseWriter, _ *http.Request, _ int32, _ string) {
				body := denyBody()
				hijackWrite(t, w, okHeaders+"Transfer-Encoding: chunked\r\n\r\n"+
					fmt.Sprintf("%x\r\n", len(body))+body+"\r\n0\r\n\r\n")
			},
			want: probeHold, wantCalls: 1, wantIn: denyReason,
		},
		{
			// The credential's own SHAPE is not an answer. Every other row
			// mounts the 23-byte fixture token, shorter than any token a
			// forge mints: a term keyed on the token's length or spelling
			// would hold on every production run and on no row above.
			name:  "a production-length installation token under a well-formed permission",
			token: []byte("ghs_16C7e42F292c6912E7710c838347Ae178B4a"),
			serve: func(_ *testing.T, w http.ResponseWriter, _ *http.Request, _ int32, _ string) {
				_, _ = w.Write([]byte(allowBody()))
			},
			want: probeDeliver, wantCalls: 1, wantIn: "confirmed workflows:write",
		},
		{
			// The shape a MOUNTED secret really has: the file ends with the
			// newline whoever wrote it left. The probe strips the text it
			// reads, so this costs neither the question nor the answer.
			name:  "the mounted token file ends with a newline",
			token: []byte("ghs_vetty_runtime_token\n"),
			serve: func(_ *testing.T, w http.ResponseWriter, _ *http.Request, _ int32, _ string) {
				_, _ = w.Write([]byte(allowBody()))
			},
			want: probeDeliver, wantCalls: 1, wantIn: "confirmed workflows:write",
		},
		{
			// The pull request the launch named is an input too, and no term
			// of a verdict may read it: nine digits name a pull request as
			// much as the fixture's single one.
			name:  "the pull request number has more digits than the fixture's",
			prURL: "https://github.com/acme/widgets/pull/987654321",
			serve: func(_ *testing.T, w http.ResponseWriter, _ *http.Request, _ int32, _ string) {
				_, _ = w.Write([]byte(allowBody()))
			},
			want: probeDeliver, wantCalls: 1, wantIn: "confirmed workflows:write",
		},
		{
			name: "the well-formed permission",
			serve: func(_ *testing.T, w http.ResponseWriter, _ *http.Request, _ int32, _ string) {
				_, _ = w.Write([]byte(allowBody()))
			},
			want: probeDeliver, wantCalls: 1, wantIn: "confirmed workflows:write",
		},
		{
			name: "the well-formed denial",
			serve: func(_ *testing.T, w http.ResponseWriter, _ *http.Request, _ int32, _ string) {
				_, _ = w.Write([]byte(denyBody()))
			},
			want: probeHold, wantCalls: 1, wantIn: denyReason,
		},
	}

	for _, carrier := range discoverDeliveryProbeCarriers(t) {
		t.Run(carrier.rel+"/"+carrier.node, func(t *testing.T) {
			t.Parallel()
			// One read-only fixture for every row: a branch whose committed
			// change AND whose working tree both touch .github/workflows/,
			// so each carrier's own base binding (HEAD here, base_ref there)
			// sees a delivery that needs the permission.
			ws := workflowAlignmentFixture(t, "https://github.com/acme/widgets.git")
			tokenPath := filepath.Join(t.TempDir(), "forge_token")
			if err := os.WriteFile(tokenPath, []byte("ghs_vetty_runtime_token"), 0o600); err != nil {
				t.Fatal(err)
			}
			for _, row := range rows {
				t.Run(row.name, func(t *testing.T) {
					t.Parallel()
					var calls atomic.Int32
					var redirected atomic.Int32
					target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
						redirected.Add(1)
					}))
					defer target.Close()
					stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						row.serve(t, w, r, calls.Add(1), target.URL)
					}))
					defer stub.Close()
					checkURL := stub.URL + "/api/v1/forge/delivery-preflight"
					if row.dead {
						checkURL = deadAddress(t)
					}

					tokenAt := tokenPath
					if row.token != nil {
						tokenAt = filepath.Join(t.TempDir(), "forge_token")
						if err := os.WriteFile(tokenAt, row.token, 0o600); err != nil {
							t.Fatal(err)
						}
					}
					at, prURL := ws, "https://github.com/acme/widgets/pull/7"
					if row.origin != "" {
						at = workflowAlignmentFixture(t, row.origin)
					}
					if row.prURL != "" {
						prURL = row.prURL
					}
					got, elapsed := runDeliveryProbeAt(t, carrier, at, checkURL, tokenAt, prURL, nil)
					outcome := outcomeOf(t, got)
					// THE guarantee, asserted before the equality so that a
					// false hold fails on the sentence written for it rather
					// than on "outcome = hold, want unavailable".
					if outcome == probeHold && row.want != probeHold {
						t.Fatalf("a blocking verdict was published on a pull request with no answered denial behind it: %q", got.Reason)
					}
					if outcome != row.want {
						t.Fatalf("outcome = %s, want %s (reason %q)", outcome, row.want, got.Reason)
					}
					if strings.TrimSpace(got.Reason) == "" {
						t.Error("the answer carries no reason — it is what the operator reads")
					}
					if row.wantIn != "" && !strings.Contains(got.Reason, row.wantIn) {
						t.Errorf("reason %q does not name %q", got.Reason, row.wantIn)
					}
					if row.wantNotIn != "" && strings.Contains(got.Reason, row.wantNotIn) {
						t.Errorf("reason %q names %q — it reports one cause as another", got.Reason, row.wantNotIn)
					}
					if row.wantNoCalls && calls.Load() != 0 {
						t.Errorf("the service was asked %d time(s), want none — this row never gets as far as the question", calls.Load())
					}
					if row.wantCalls != 0 && int(calls.Load()) != row.wantCalls {
						t.Errorf("the service was asked %d time(s), want exactly %d — a question with no answer is asked once more, and only once",
							calls.Load(), row.wantCalls)
					}
					if redirected.Load() != 0 {
						t.Errorf("the token digest was carried to a redirect target %d time(s)", redirected.Load())
					}
					// A dead address answers nothing, so the pause between
					// the two attempts is the only witness that there were
					// two.
					if row.dead && elapsed < 2*time.Second {
						t.Errorf("the probe answered in %v — under the 2 s pause, so it asked once", elapsed.Round(time.Millisecond))
					}
				})
			}
		})
	}
}

// workflowAlignmentFixture is a checkout whose delivery touches
// .github/workflows/ under BOTH base bindings: a commit on top of main (what
// branch-improve-loop measures) and an unstaged edit on top of HEAD (what
// dep-update-guard measures).
func workflowAlignmentFixture(t *testing.T, origin string) string {
	t.Helper()
	ws := t.TempDir()
	const workflow = ".github/workflows/ci.yml"
	write := func(body string) {
		t.Helper()
		p := filepath.Join(ws, workflow)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gittest.Run(t, ws, "init", "-q", "-b", "main")
	write("jobs:\n  t:\n    steps:\n      - uses: actions/checkout@aaaa\n")
	gittest.Run(t, ws, "add", "-A")
	gittest.Run(t, ws, "commit", "-qm", "base")
	gittest.Run(t, ws, "checkout", "-qb", "renovate/actions-digests")
	write("jobs:\n  t:\n    steps:\n      - uses: actions/checkout@bbbb\n")
	gittest.Run(t, ws, "add", "-A")
	gittest.Run(t, ws, "commit", "-qm", "chore(deps): pin actions/checkout to bbbb")
	// The alignment, as `align` leaves it: unstaged, on top of HEAD.
	write("jobs:\n  t:\n    steps:\n      - uses: actions/checkout@bbbb\n        with:\n          fetch-depth: 0\n")
	gittest.Run(t, ws, "remote", "add", "origin", origin)
	return ws
}

// runDeliveryProbeAt executes one carrier's compiled probe command against the
// given workspace, preflight URL and pull request, with the host's own forge
// credentials kept out of the run and whatever extra environment the row needs
// (a CA file for a TLS stub, say).
func runDeliveryProbeAt(t *testing.T, c deliveryProbeCarrier, ws, checkURL, tokenPath, prURL string, env []string) (deliveryVerdict, time.Duration) {
	t.Helper()
	cmd := c.command
	for ref, val := range map[string]string{
		"{{vars.workspace_dir}}":                ws,
		"{{vars.base_ref}}":                     "main",
		"{{vars.pr_url}}":                       prURL,
		"{{vars.forge_delivery_preflight_url}}": checkURL,
		"{{vars.forge_publish_token}}":          "run-token",
		"{{secrets.forge_token.path}}":          tokenPath,
	} {
		cmd = strings.ReplaceAll(cmd, ref, val)
	}
	if rest := firstRef(cmd); rest != "" {
		t.Fatalf("unsubstituted ref %s left in %s's probe command — it would run in a state production never reaches", rest, c.rel)
	}
	run := exec.Command("sh", "-c", cmd)
	run.Env = append(append(os.Environ(), "GH_TOKEN=", "GITHUB_TOKEN="), env...)
	start := time.Now()
	out, err := run.Output()
	elapsed := time.Since(start)
	if err != nil {
		stderr := ""
		if ee, ok := err.(*exec.ExitError); ok {
			stderr = string(ee.Stderr)
		}
		t.Fatalf("%s's delivery probe failed to execute: %v (out %q, stderr %q)", c.rel, err, out, stderr)
	}
	var got deliveryVerdict
	if uerr := json.Unmarshal(out, &got); uerr != nil {
		t.Fatalf("%s's delivery probe output is not delivery_probe_state JSON: %v (out %q)", c.rel, uerr, out)
	}
	return got, elapsed
}

// deadAddress owns a bound, non-listening TCP socket until this row ends.
// Releasing a listener before returning its address lets a parallel fixture
// reclaim that port and receive this row's retries. Python is already a
// prerequisite of the probe matrix and exposes bind without listen on every
// platform; keeping stdin open owns the socket, closing it releases the child.
func deadAddress(t *testing.T) string {
	t.Helper()
	cmd := exec.Command("python3", "-I", "-c", `import socket, sys
with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as s:
    s.bind(("127.0.0.1", 0))
    print("%s:%d" % s.getsockname(), flush=True)
    sys.stdin.read()
`)
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		_ = in.Close()
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		_ = in.Close()
		_ = out.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = in.Close()
		if err := cmd.Wait(); err != nil {
			t.Errorf("release refused port: %v", err)
		}
	})
	line, err := bufio.NewReader(out).ReadString('\n')
	if err != nil {
		t.Fatalf("read reserved endpoint: %v", err)
	}
	addr := strings.TrimSpace(line)
	if _, _, err := net.SplitHostPort(addr); err != nil {
		t.Fatal(err)
	}
	return "http://" + addr + "/api/v1/forge/delivery-preflight"
}

func TestDeadAddressKeepsItsRefusedPortReserved(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not on PATH")
	}
	u, err := url.Parse(deadAddress(t))
	if err != nil {
		t.Fatal(err)
	}
	if l, err := net.Listen("tcp", u.Host); err == nil {
		_ = l.Close()
		t.Fatal("another fixture can reclaim the refused endpoint and receive its token digest")
	}
	if c, err := net.DialTimeout("tcp", u.Host, time.Second); err == nil {
		_ = c.Close()
		t.Fatal("the reserved endpoint accepted a connection")
	} else if !errors.Is(err, syscall.ECONNREFUSED) {
		t.Fatalf("the reserved endpoint did not refuse the connection: %v", err)
	}
}

// TestDeliveryProbeReadsTheForgeAPIsScopeListOnlyWhenItAnswers covers the
// OTHER question the probe asks through the same helper. A classic PAT or
// OAuth token (`forge.KindPAT` is a first-class connection kind) is judged on
// the scope list GitHub stamps on its own answer, not on the delivery
// preflight — the endpoint denies a non-App credential before reading any
// permission, so that branch runs first and never asks it.
//
// The ANSWER here is a header, so the well-formedness term is the header's
// presence: an absent `X-OAuth-Scopes` is not an empty scope list, it says
// whatever answered is not the forge API. Without this test the branch has
// only its wording — the previous rounds could not reach it at all, the probe
// speaking TLS to an address built out of the git remote.
func TestDeliveryProbeReadsTheForgeAPIsScopeListOnlyWhenItAnswers(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not on PATH")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	const token = "ghp_classic_personal_access_token"

	for _, row := range []struct {
		name string
		// scopes is the X-OAuth-Scopes header value; absent omits it.
		scopes    string
		absent    bool
		want      string
		wantDials int
		wantIn    string
	}{
		{
			name:   "the token's own scopes carry workflow and repo",
			scopes: "repo, workflow, read:org", want: probeDeliver, wantDials: 1,
			wantIn: "confirms workflow and repository scopes",
		},
		{
			// An ANSWERED denial, and the third cure of the hold: there is no
			// App behind a classic token, so "approve the App's delivery
			// permissions" is work this operator cannot perform.
			name:   "the token's own scopes stop short of workflow",
			scopes: "repo, read:org", want: probeHold, wantDials: 1,
			wantIn: "reissue the token with the workflow and repo scopes",
		},
		{
			// The OTHER half of the conjunct, and a real GitHub shape: the
			// classic scopes are independent checkboxes, so `workflow`
			// without `repo` grants the workflow files and not the push that
			// carries them. Delivered, it 403s on `contents` and the bump's
			// alignment is reported as lost instead of refused.
			name:   "the token carries workflow and no repository scope",
			scopes: "workflow, read:org", want: probeHold, wantDials: 1,
			wantIn: "reissue the token with the workflow and repo scopes",
		},
		{
			// 200 and no scope header: a proxy, a cache, anything but the
			// forge API. Reading that as "no scopes" publishes a blocking
			// verdict about a token nobody asked about.
			name:   "the answer carries no scope list at all",
			absent: true, want: probeUnavailable, wantDials: 2,
			wantIn: "the forge API did not answer (asked twice)",
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			var dials atomic.Int32
			forge := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				dials.Add(1)
				if r.URL.Path != "/api/v3/user" {
					t.Errorf("the forge API was asked for %q, want /api/v3/user", r.URL.Path)
				}
				if got := r.Header.Get("Authorization"); got != "Bearer "+token {
					t.Errorf("the forge API was asked under %q, want the mounted token as a bearer", got)
				}
				if !row.absent {
					w.Header().Set("X-OAuth-Scopes", row.scopes)
				}
				_, _ = w.Write([]byte(`{"login":"vetty"}`))
			}))
			defer forge.Close()
			// The CA python must trust: httptest's certificate is its own
			// root. OpenSSL reads SSL_CERT_FILE whatever -I does to the
			// PYTHON* environment; a nixpkgs OpenSSL reads NIX_SSL_CERT_FILE
			// FIRST and ignores the other, so both are set or the handshake
			// fails with "self-signed certificate" against the system store.
			ca := filepath.Join(t.TempDir(), "forge-ca.pem")
			if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: forge.Certificate().Raw}), 0o644); err != nil {
				t.Fatal(err)
			}
			authority := strings.TrimPrefix(forge.URL, "https://")
			tokenPath := filepath.Join(t.TempDir(), "forge_token")
			if err := os.WriteFile(tokenPath, []byte(token), 0o600); err != nil {
				t.Fatal(err)
			}
			// The preflight must never be asked on this branch: the digest of
			// a credential the endpoint would refuse anyway has no business
			// leaving the run.
			preflight := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
				t.Error("the classic-token branch asked the delivery preflight: the endpoint denies a non-App credential before reading any permission, and the digest left the run for nothing")
			}))
			defer preflight.Close()

			for _, carrier := range discoverDeliveryProbeCarriers(t) {
				t.Run(carrier.rel, func(t *testing.T) {
					ws := workflowAlignmentFixture(t, "https://"+authority+"/acme/widgets.git")
					before := dials.Load()
					got, _ := runDeliveryProbeAt(t, carrier, ws, preflight.URL, tokenPath,
						"https://"+authority+"/acme/widgets/pull/7",
						[]string{"SSL_CERT_FILE=" + ca, "NIX_SSL_CERT_FILE=" + ca})
					if outcome := outcomeOf(t, got); outcome != row.want {
						if outcome == probeUnavailable && dials.Load() > before {
							t.Fatalf("outcome = %s, want %s: the stub was dialled and answered, so the probe did not accept its certificate — the CA at %s reached neither SSL_CERT_FILE nor NIX_SSL_CERT_FILE (reason %q)",
								outcome, row.want, ca, got.Reason)
						}
						t.Fatalf("outcome = %s, want %s (reason %q)", outcome, row.want, got.Reason)
					}
					if n := int(dials.Load() - before); n != row.wantDials {
						t.Errorf("the forge API answered %d request(s), want exactly %d", n, row.wantDials)
					}
					if !strings.Contains(got.Reason, row.wantIn) {
						t.Errorf("reason %q does not name %q", got.Reason, row.wantIn)
					}
					if strings.Contains(got.Reason, token) {
						t.Error("the answer echoes the runtime token")
					}
				})
			}
		})
	}
}
