package runtime

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// What becomes of a failed command, by what it printed.
type outageVerdict int

const (
	// counted: EXECUTION_FAILED, and an identical repeat of it is permanent.
	counted outageVerdict = iota
	// waitedOut: EXECUTION_FAILED, never counted — its text names an outage
	// (transientOutageWordings), so it keeps the retry and the redeliveries.
	waitedOut
	// backedOff: NETWORK_TRANSIENT by the engine's own classification, which
	// this table does not touch — it never reaches a streak.
	backedOff
)

func (v outageVerdict) String() string {
	return [...]string{"counted", "waited out (EXECUTION_FAILED, not counted)", "backed off (NETWORK_TRANSIENT)"}[v]
}

// acceptedLoss starts the reason of a text that falls on the WRONG side, and
// is left there: a deterministic failure the table waits out because its
// output is worded like an outage. The table reads substrings of an open
// alphabet; these are the price of its wordings, written down so that nobody
// "fixes" one by loosening or tightening an entry without seeing what moves.
const acceptedLoss = "LOSS: "

// outageCorpus is THE regression corpus of the wording table: lines real
// tools and the catalog's own scripts print when they fail, each with the
// side it must fall on and why. It is the merge of every text the table was
// reviewed on — the tools' outage wordings, the failures no wait cures (the
// incident first), the engine's own network classification, and the losses.
// A change to transientOutageWordings is judged here: a text that changes
// side reddens with its reason.
var outageCorpus = []struct {
	text string
	want outageVerdict
	why  string
}{
	// deterministic: incident
	{"curl: (3) nested brace in URL position 31:\nhttps://api.github.com/repos/{{input.repo}}/pulls/1/merge\n                              ^", counted, "incident: a URL curl cannot parse"},
	{"curl: (3) bad range in URL position 9:\nhttp://[bad/\n        ^", counted, "incident: a URL curl cannot parse"},
	{"curl: (3) URL rejected: Bad hostname", counted, "incident: a URL curl cannot parse"},
	{"curl: (3) URL rejected: Malformed input to a URL function", counted, "incident: a URL curl cannot parse"},
	// deterministic: forge-4xx
	{"curl: (22) The requested URL returned error: 404", counted, "4xx: a decision, not an outage"},
	{"curl: (22) The requested URL returned error: 401", counted, "4xx: a decision, not an outage"},
	{"curl: (22) The requested URL returned error: 403", counted, "4xx: a decision, not an outage"},
	{"curl: (22) The requested URL returned error: 422", counted, "4xx: a decision, not an outage"},
	{"HTTP 404: Not Found (https://api.github.com/repos/o/r)", counted, "4xx: a decision, not an outage"},
	{"HTTP 422: Validation Failed (https://api.github.com/repos/o/r/pulls)", counted, "4xx: a decision, not an outage"},
	{"gh: Resource not accessible by integration (HTTP 403)", counted, "4xx: a decision, not an outage"},
	{"HTTPError: HTTP Error 404: Not Found", counted, "4xx: a decision, not an outage"},
	// deterministic: policy
	{"curl: (56) CONNECT tunnel failed, response 403", counted, "egress policy: the proxy refused the tunnel"},
	{"fatal: unable to access 'https://blocked.example/o/r/': CONNECT tunnel failed, response 403", counted, "egress policy: the proxy refused the tunnel"},
	{"urllib.error.URLError: <urlopen error Tunnel connection failed: 403 Forbidden>", counted, "egress policy: the proxy refused the tunnel"},
	{"curl: (56) CONNECT tunnel failed, response 407", counted, "egress policy: the proxy refused the tunnel"},
	// deterministic: lint
	{"./app.py:12:80: E501 line too long (88 > 79 characters)", counted, "lint"},
	{"app.py:12:89: E501 Line too long (100 > 88)\nFound 1 error.", counted, "lint"},
	{"app.py:12:89: E501 [*] Line too long (100 > 88)", counted, "lint"},
	// deterministic: npm-4xx
	{"npm error code E404\nnpm error 404 Not Found - GET https://registry.npmjs.org/nopkg - Not found", counted, "4xx: a decision, not an outage"},
	{"npm ERR! code E401\nnpm ERR! Unable to authenticate, your authentication token seems to be invalid.", counted, "4xx: a decision, not an outage"},
	// deterministic: timed-out
	{"panic: test timed out after 10m0s\n\trunning tests:\n\t\tTestDeadlock (10m0s)", waitedOut, "LOSS: deterministic, but worded as a timeout"},
	{"error: timed out waiting for the condition on deployments/app", waitedOut, "LOSS: deterministic, but worded as a timeout"},
	{"subprocess.TimeoutExpired: Command '['npm', 'test']' timed out after 600 seconds", waitedOut, "LOSS: deterministic, but worded as a timeout"},
	{"--- FAIL: TestClientGivesUp (0.01s)\n    client_test.go:41: request timed out: want a retry", waitedOut, "LOSS: deterministic, but worded as a timeout"},
	// deterministic: echoed-data
	{"--- FAIL: TestClientRetriesOn503 (0.00s)\n    client_test.go:41: got \"503 Service Unavailable\", want \"200 OK\"\nFAIL", waitedOut, "LOSS: outage words echoed as data"},
	{"{\"posted\": true, \"messages\": [{\"text\": \":red_circle: api: HTTP Error 503: Service Unavailable\"}]}\nprod-watch: delivery incomplete (required sink(s) down: mm2) -- the state is NOT consumed. delivered 1/2 -- FAILED: mm2: HTTPError: HTTP Error 404: Not Found", waitedOut, "LOSS: outage words echoed as data"},
	{"{\"posted\": true, \"messages\": [{\"text\": \":red_circle: api: probe timed out after 10 s\"}]}\nprod-watch: delivery incomplete (required sink(s) down: mm2) -- FAILED: mm2: HTTPError: HTTP Error 400: Bad Request", waitedOut, "LOSS: outage words echoed as data"},
	{"config: invalid rate limit \"ten\" for sink mm", counted, "a configuration error that merely names a rate limit"},
	// deterministic: unresolved-host
	{"curl: (6) Could not resolve host: None", waitedOut, "LOSS: an unresolvable host keeps the redeliveries whatever its cause"},
	{"Error: getaddrinfo ENOTFOUND undefined", waitedOut, "LOSS: an unresolvable host keeps the redeliveries whatever its cause"},
	{"urllib.error.URLError: <urlopen error [Errno -2] Name or service not known>", waitedOut, "LOSS: an unresolvable host keeps the redeliveries whatever its cause"},
	// deterministic: ordinary
	{"error: 5 files would be reformatted", counted, "an ordinary failure"},
	{"fetched http 50 items, expected 51", counted, "an ordinary failure"},
	{"", counted, "a bare non-zero exit"},
	// deterministic: a parser's EOF, worded like the engine's streaming needle
	{"SyntaxError: unexpected EOF while parsing", waitedOut, "LOSS: a parser's wording the engine's network list also holds; the command reading sets it aside and the streak never counts it"},
	{"tar: Unexpected EOF in archive", waitedOut, "LOSS: an archive tool's wording the engine's network list also holds; the command reading sets it aside and the streak never counts it"},
	// outages: gh
	{"HTTP 500: Internal Server Error (https://api.github.com/graphql)", waitedOut, "outage"},
	{"HTTP 504: Gateway Timeout (https://api.github.com/graphql)", waitedOut, "outage"},
	{"error connecting to api.github.com\ncheck your internet connection or https://githubstatus.com", waitedOut, "outage"},
	{"HTTP 403: You have exceeded a secondary rate limit. Please wait a few minutes before you try again.", waitedOut, "outage"},
	// outages: git
	{"error: RPC failed; curl 92 HTTP/2 stream 5 was not closed cleanly: CANCEL (err 8)\nfatal: the remote end hung up unexpectedly\nfatal: early EOF\nfatal: index-pack failed", waitedOut, "outage"},
	{"remote: Internal Server Error\nTo https://github.com/o/r\n ! [remote rejected] main -> main (Internal Server Error)\nerror: failed to push some refs to 'https://github.com/o/r'", waitedOut, "outage"},
	{"fatal: the remote end hung up unexpectedly", waitedOut, "outage"},
	{"fatal: early EOF", waitedOut, "outage"},
	{"fatal: unable to access 'https://github.com/o/r/': OpenSSL SSL_read: SSL_ERROR_SYSCALL, errno 0", waitedOut, "outage"},
	{"fatal: unable to access 'https://github.com/o/r/': gnutls_handshake() failed: The TLS connection was non-properly terminated.", waitedOut, "outage"},
	{"fatal: unable to access 'https://github.com/o/r/': GnuTLS recv error (-110): The TLS connection was non-properly terminated.", waitedOut, "outage"},
	// outages: docker
	{"Error response from daemon: Get \"https://registry-1.docker.io/v2/\": net/http: TLS handshake timeout", backedOff, "the engine's own network classification"},
	{"Error response from daemon: Get \"https://registry-1.docker.io/v2/\": net/http: request canceled while waiting for connection (Client.Timeout exceeded while awaiting headers)", waitedOut, "outage"},
	{"toomanyrequests: You have reached your pull rate limit.", waitedOut, "outage"},
	// outages: go
	{"Post \"https://api.osv.dev/v1/querybatch\": dial tcp 34.1.2.3:443: i/o timeout", backedOff, "the engine's own network classification"},
	{"Post \"https://api.osv.dev/v1/querybatch\": context deadline exceeded (Client.Timeout exceeded while awaiting headers)", waitedOut, "outage: the command reading sets `context deadline exceeded` aside, and the outage table (Client.Timeout exceeded) waits this failure out"},
	{"Get \"https://x/\": net/http: request canceled (Client.Timeout exceeded while awaiting headers)", waitedOut, "outage"},
	// outages: nginx-html
	{"parse error: Invalid numeric literal at line 1, column 7\nstdout-was: <html><head><title>503 Service Temporarily Unavailable</title></head><body><center><h1>503 Service Temporarily Unavailable</h1></center><hr><center>nginx</center></body></html>", waitedOut, "outage"},
	{"parse error: Invalid numeric literal at line 1, column 7\nstdout-was: <html><head><title>504 Gateway Time-out</title></head><body><center><h1>504 Gateway Time-out</h1></center></body></html>", waitedOut, "outage"},
	// outages: node
	{"npm error code ENOTFOUND\nnpm error syscall getaddrinfo\nnpm error errno ENOTFOUND\nnpm error network request to https://registry.npmjs.org/x failed, reason: getaddrinfo ENOTFOUND registry.npmjs.org", waitedOut, "outage"},
	{"Error: socket hang up", waitedOut, "outage"},
	{"Error: connect ECONNREFUSED 10.0.0.1:443", waitedOut, "outage"},
	{"npm error code ECONNREFUSED\nnpm error syscall connect\nnpm error errno ECONNREFUSED\nnpm error FetchError: request to https://registry.npmjs.org/x failed, reason: connect ECONNREFUSED 104.16.0.35:443", waitedOut, "outage"},
	{"TypeError: fetch failed\n    at node:internal/deps/undici/undici:13510:13 {\n  [cause]: ConnectTimeoutError: Connect Timeout Error (attempted address: api.x:443, timeout: 10000ms)", waitedOut, "outage"},
	// outages: kubectl
	{"Error from server (ServiceUnavailable): the server is currently unable to handle the request", waitedOut, "outage"},
	{"Error from server (InternalError): an error on the server (\"\") has prevented the request from succeeding", waitedOut, "outage"},
	{"Unable to connect to the server: net/http: TLS handshake timeout", backedOff, "the engine's own network classification"},
	{"Error from server (Timeout): the server was unable to return a response in the time allotted, but may still be processing the request", waitedOut, "outage"},
	// outages: curl
	{"curl: (16) Error in the HTTP2 framing layer", waitedOut, "outage"},
	{"curl: (18) transfer closed with outstanding read data remaining", waitedOut, "outage"},
	{"curl: (92) HTTP/2 stream 1 was not closed cleanly: INTERNAL_ERROR (err 2)", waitedOut, "outage"},
	{"curl: (55) Send failure: Broken pipe", waitedOut, "outage"},
	{"curl: (56) Recv failure: Connection reset by peer", backedOff, "the engine's own network classification"},
	{"curl: (22) The requested URL returned error: 500", waitedOut, "outage"},
	{"curl: (22) The requested URL returned error: 504", waitedOut, "outage"},
	// outages: requests
	{"requests.exceptions.HTTPError: 504 Server Error: Gateway Timeout for url: https://x/", waitedOut, "outage"},
	{"requests.exceptions.HTTPError: 500 Server Error: Internal Server Error for url: https://x/", waitedOut, "outage"},
	{"requests.exceptions.HTTPError: 503 Server Error: Service Unavailable for url: https://x/", waitedOut, "outage"},
	{"requests.exceptions.ReadTimeout: HTTPSConnectionPool(host='x', port=443): Read timed out. (read timeout=10)", waitedOut, "outage"},
	{"requests.exceptions.ConnectionError: ('Connection aborted.', RemoteDisconnected('Remote end closed connection without response'))", waitedOut, "outage"},
	// outages: urllib
	{"HTTPError: HTTP Error 500: Internal Server Error", waitedOut, "outage"},
	{"HTTPError: HTTP Error 403: Forbidden", counted, "a 403 that names no cause, counted as a refusal: a miss when it is GitHub's rate limit, which urllib prints as this same line"},
	{"URLError: <urlopen error [Errno 101] Network is unreachable>", backedOff, "the engine's own network classification"},
	{"ssl.SSLError: [SSL: UNEXPECTED_EOF_WHILE_READING] EOF occurred in violation of protocol (_ssl.c:1000)", waitedOut, "outage"},
	{"http.client.IncompleteRead: IncompleteRead(512 bytes read, 1024 more expected)", waitedOut, "outage"},
	// outages: ssh
	{"kex_exchange_identification: Connection closed by remote host\nConnection closed by 140.82.121.4 port 22", waitedOut, "outage"},
	// outages: apt
	{"Err:1 http://deb.debian.org/debian bookworm InRelease\n  Temporary failure resolving 'deb.debian.org'", waitedOut, "outage"},
	// outages: wget
	{"ERROR 503: Service Unavailable.", waitedOut, "outage"},
	// outages: go-mod
	{"go: github.com/x/y@v1.2.3: reading https://proxy.golang.org/github.com/x/y/@v/v1.2.3.info: 502 Bad Gateway", waitedOut, "outage"},
	// outages: pip
	{"ERROR: HTTP error 503 while getting https://files.pythonhosted.org/x.whl", waitedOut, "outage"},
	// outages: jira
	{"{\"context_note\": \"\", \"sources\": 0, \"errors\": \"jira: HTTP 429 on https://acme.atlassian.net — \"}", waitedOut, "outage"},
	{"{\"context_note\": \"\", \"sources\": 0, \"errors\": \"jira: HTTP 503 on https://acme.atlassian.net — \"}", waitedOut, "outage"},
	// outages: vuln-watch
	{"vuln-watch: every org poll failed: [{\"org\": \"acme\", \"error\": \"HTTPError: HTTP Error 403: Forbidden\"}]", counted, "a 403 that names no cause, counted as a refusal: a miss when it is GitHub's rate limit, which urllib prints as this same line"},
	{"vuln-watch: every configured source failed this run -- refusing to report a zero-alert facade", counted, "names no outage: a miss, the node parks"},
	// outages: prod-watch
	{"prod-watch: all 2 webhook post(s) failed: mm: ExchangeTimeout: the sink did not answer within its wall clock; mm2: ExchangeTimeout: the sink did not answer within its wall clock", waitedOut, "outage"},
	{"prod-watch: all 1 webhook post(s) failed: mm: HTTPError: HTTP Error 502: Bad Gateway", waitedOut, "outage"},
	{"prod-watch: all 1 webhook post(s) failed: mm: URLError: <urlopen error [Errno -3] Temporary failure in name resolution>", waitedOut, "outage"},
	{"prod-watch: all 1 webhook post(s) failed: mm: RemoteDisconnected: Remote end closed connection without response", waitedOut, "outage"},
	{"prod-watch: all 1 webhook post(s) failed: mm: ConnectionResetError: [Errno 104] Connection reset by peer", backedOff, "the engine's own network classification"},
	{"prod-watch: all 1 webhook post(s) failed: mm: IncompleteRead: IncompleteRead(0 bytes read)", waitedOut, "outage"},
	// round 1
	{"curl: (6) Could not resolve host: api.github.com", waitedOut, "outage"},
	{"curl: (7) Failed to connect to api.github.com port 443 after 3 ms: Couldn't connect to server", waitedOut, "outage"},
	{"curl: (7) Failed to connect to api.github.com port 443: Connection refused", backedOff, "the engine's own network classification"},
	{"curl: (22) The requested URL returned error: 503", waitedOut, "outage"},
	{"curl: (22) The requested URL returned error: 429", waitedOut, "outage"},
	{"curl: (28) Operation timed out after 10001 milliseconds with 0 bytes received", waitedOut, "outage"},
	{"curl: (35) OpenSSL SSL_connect: Connection reset by peer in connection to api.github.com:443", backedOff, "the engine's own network classification"},
	{"curl: (35) OpenSSL SSL_connect: SSL_ERROR_SYSCALL in connection to github.com:443", waitedOut, "outage"},
	{"curl: (52) Empty reply from server", waitedOut, "outage"},
	{"curl: (56) CONNECT tunnel failed, response 502", waitedOut, "outage"},
	{"fatal: unable to access 'https://github.com/o/r/': Could not resolve host: github.com", waitedOut, "outage"},
	{"fatal: unable to access 'https://github.com/o/r/': The requested URL returned error: 503", waitedOut, "outage"},
	{"fatal: unable to access 'https://github.com/o/r/': Failed to connect to github.com port 443 after 129 ms: Couldn't connect to server", waitedOut, "outage"},
	{"fatal: unable to access 'https://github.com/o/r.git/': Failed to connect to github.com port 443 after 130 ms: Connection refused", backedOff, "the engine's own network classification"},
	{"error: RPC failed; HTTP 502 curl 22 The requested URL returned error: 502", waitedOut, "outage"},
	{"error: failed to push some refs to 'https://github.com/o/r'\nhint: Updates were rejected because the remote contains work that you do\nhint: not have locally.", counted, "no wait cures it"},
	{" ! [rejected]        HEAD -> main (fetch first)", counted, "no wait cures it"},
	{"error: another git process seems to be running in this repository, e.g. an editor opened by 'git commit'. Unable to create '/w/.git/index.lock': File exists.", counted, "no wait cures it"},
	{"ssh: Could not resolve hostname github.com: Temporary failure in name resolution", waitedOut, "outage"},
	{"ssh: connect to host github.com port 22: Connection timed out", waitedOut, "outage"},
	{"wget: unable to resolve host address 'github.com'", waitedOut, "outage"},
	{"ERROR: Could not install packages due to an OSError: HTTPSConnectionPool(host='pypi.org', port=443): Max retries exceeded with url: /simple/x/ (Caused by NewConnectionError('<pip._vendor.urllib3.connection.HTTPSConnection object at 0x7f>: Failed to establish a new connection: [Errno -3] Temporary failure in name resolution'))", waitedOut, "outage"},
	{"HTTP 502: Bad Gateway (https://api.github.com/graphql)", waitedOut, "outage"},
	{"HTTP 503: Service Unavailable (https://api.github.com/repos/o/r/pulls/1/merge)", waitedOut, "outage"},
	{"API rate limit exceeded for installation ID 123. (HTTP 403)", waitedOut, "outage"},
	{"GraphQL: Pull Request is not mergeable (mergePullRequest)", counted, "no wait cures it"},
	{"X Pull request o/r#12 is not mergeable: the base branch policy prohibits the merge.", counted, "no wait cures it"},
	{"Post \"https://api.github.com/graphql\": dial tcp: lookup api.github.com: no such host", backedOff, "the engine's own network classification"},
	{"Post \"https://api.github.com/graphql\": unexpected EOF", backedOff, "the engine's own network classification: Go's url.Error quotes the URL whose stream was cut"},
	{"go: github.com/x/y@v1.2.3: Get \"https://proxy.golang.org/github.com/x/y/@v/v1.2.3.zip\": unexpected EOF", backedOff, "the engine's own network classification: Go's url.Error quotes the URL whose stream was cut"},
	{"Post \"https://api.github.com/graphql\": dial tcp 140.82.121.6:443: i/o timeout", backedOff, "the engine's own network classification"},
	{"urllib.error.URLError: <urlopen error [Errno -3] Temporary failure in name resolution>", waitedOut, "outage"},
	{"URLError: <urlopen error [Errno 111] Connection refused>", backedOff, "the engine's own network classification"},
	{"TimeoutError: The read operation timed out", waitedOut, "outage"},
	{"URLError: <urlopen error timed out>", waitedOut, "outage"},
	{"HTTPError: HTTP Error 503: Service Unavailable", waitedOut, "outage"},
	{"HTTPError: HTTP Error 429: Too Many Requests", waitedOut, "outage"},
	{"HTTPError: HTTP Error 502: Bad Gateway", waitedOut, "outage"},
	{"OSError: Tunnel connection failed: 502 Bad Gateway", waitedOut, "outage"},
	{"URLError: <urlopen error Tunnel connection failed: 403 Forbidden>", counted, "egress policy: the proxy refused the tunnel"},
	{"http.client.RemoteDisconnected: Remote end closed connection without response", waitedOut, "outage"},
	{"ConnectionResetError: [Errno 104] Connection reset by peer", backedOff, "the engine's own network classification"},
	{"ssl.SSLEOFError: EOF occurred in violation of protocol (_ssl.c:2427)", waitedOut, "outage"},
	{"feed-watch: all webhook post(s) failed across 1 sink(s): mm part 1/1: HTTPError: HTTP Error 503: Service Unavailable", waitedOut, "outage"},
	{"npm error code ETIMEDOUT\nnpm error network request to https://registry.npmjs.org/x failed, reason: ", waitedOut, "outage"},
	{"npm error code E503\nnpm error 503 Service Unavailable - GET https://registry.npmjs.org/x", waitedOut, "outage"},
	{"npm ERR! code ECONNRESET\nnpm ERR! network aborted", waitedOut, "outage"},
	{"ERR_PNPM_META_FETCH_FAIL  GET https://registry.npmjs.org/x: request to https://registry.npmjs.org/x failed, reason: getaddrinfo EAI_AGAIN registry.npmjs.org", waitedOut, "outage"},
	{"Error from server: error dialing backend: remote error: tls: internal error", waitedOut, "outage"},
	{"Error: Kubernetes cluster unreachable: Get \"https://10.0.0.1:443/version\": dial tcp 10.0.0.1:443: connect: connection refused", backedOff, "the engine's own network classification"},
	{"error: unable to upgrade connection: container not found (\"workload\")", counted, "no wait cures it"},
	{"Traceback (most recent call last):\n  File \"<string>\", line 4, in <module>\n  File \"/usr/lib/python3.12/json/__init__.py\", line 346, in loads\n    return _default_decoder.decode(s)\njson.decoder.JSONDecodeError: Unterminated string starting at: line 12 column 13 (char 408)", counted, "no wait cures it"},
	{"Traceback (most recent call last):\n  File \"<string>\", line 2, in <module>\nKeyError: 'pull_request'", counted, "no wait cures it"},
	{"bash: line 1: ./deploy.sh: Permission denied", counted, "no wait cures it"},
	{"curl: (60) SSL certificate problem: unable to get local issuer certificate", counted, "a certificate that does not verify is configuration"},
	{"ssl.SSLCertVerificationError: [SSL: CERTIFICATE_VERIFY_FAILED] certificate verify failed: self-signed certificate (_ssl.c:1000)", counted, "a certificate that does not verify is configuration"},
	{"bash: line 1: jqq: command not found", counted, "no wait cures it"},
	{"product-docs: http 0 after 12 tries", counted, "a poll that gave up in its own words: a miss, the node parks"},
	// the catalog, and the edges
	{"prod-watch: all 1 webhook post(s) failed: mm: ExchangeTimeout: the sink did not answer within its wall clock", waitedOut, "outage: prod-watch's sink hung past the bot's own alarm"},
	{"prod-watch: all 2 webhook post(s) failed: mm: ExchangeTimeout: the sink did not answer within its wall clock; mm: ExchangeTimeout: skipped: the sink timed out on an earlier post this tick", waitedOut, "outage"},
	{"loki: ExchangeTimeout: the answer outlasted its wall clock (it trickled in, or never finished)", waitedOut, "outage: a poll lane hung past the bot's own alarm"},
	{"prod-watch: git push did not answer within the state commit window (41 s, what the run budget left it) -- the state was committed locally and is NOT pushed, so this tick may replay next run; give the run more time (iterion run --max-duration)", waitedOut, "a hang past the bot's own alarm: waited out, though the remedy the bot names is a longer run budget, which no redelivery gives"},
	{"prod-watch: could not rebase onto origin/main to retry the push -- aborting; the tick replays. git said: fatal: unable to access 'https://github.com/acme/ops/': The requested URL returned error: 503", waitedOut, "outage"},
	{"prod-watch: state.json moved from generation 3 to 4 while this tick ran (another tick wrote it) -- refusing to overwrite; this tick replays", counted, "the bot's own generation guard"},
	{"prod-watch: all 4 webhook post(s) failed: mm: HTTPError: HTTP Error 503: Service Unavailable; mm2: HTTPError: HTTP Error 503: Service Unavailable; mm: HTTPError: HTTP Error 503: Service Unavailable; mm2: HTTPError: HTTP Error 503: Service Unavailable", waitedOut, "outage"},
	{"vuln-watch: all 2 advisory feed fetches failed: [{\"feed\": \"https://www.cert.ssi.gouv.fr/alerte/feed/\", \"error\": \"HTTPError: HTTP Error 503: Service Unavailable\"}, {\"feed\": \"https://www.cert.ssi.gouv.fr/avis/feed/\", \"error\": \"HTTPError: HTTP Error 503: Service Unavailable\"}]", waitedOut, "outage"},
	{"vuln-watch: could not rebase onto origin/main to retry the push -- aborting. git said: fatal: unable to access 'https://github.com/acme/ops/': The requested URL returned error: 503", waitedOut, "outage"},
	{"vuln-watch: could not rebase onto origin/main to retry the push -- aborting. git said: ore(vuln-watch): watch tick, 1 alert(s)\nhint: Resolve all conflicts manually, mark them as resolved with\nhint: \"git add/rm <conflicted_files>\", then run \"git rebase --continue\".\nCould not apply 0701fc3... chore(vuln-watch): watch tick, 1 alert(s)", counted, "a rebase conflict"},
	{"vuln-watch: every org poll failed: [{\"org\": \"acme\", \"error\": \"HTTPError: HTTP Error 429: Too Many Requests\"}]", waitedOut, "outage"},
	{"vuln-watch: every org poll failed: [{\"org\": \"acme\", \"error\": \"HTTPError: HTTP Error 503: Service Unavailable\"}]", waitedOut, "outage"},
	{"vuln-watch: every org poll failed: [{\"org\": \"acme\", \"error\": \"URLError: <urlopen error timed out>\"}]", waitedOut, "outage"},
	{"vuln-watch: every org poll failed: [{\"org\": \"acme\", \"error\": \"URLError: <urlopen error [Errno 111] Connection refused>\"}]", backedOff, "the engine's own network classification"},
	{"Traceback (most recent call last):\n  File \"/w/.iterion-script-*.py\", line 138, in <module>\n    _validate_host(base_host)\n  File \"/w/.iterion-script-*.py\", line 96, in _validate_host\n    infos = socket.getaddrinfo(host, None)\nsocket.gaierror: [Errno -3] Temporary failure in name resolution", waitedOut, "outage"},
	{"vuln-watch: all 2 webhook post(s) failed: mm: URLError: <urlopen error timed out>; mm: URLError: <urlopen error timed out>", waitedOut, "outage"},
	{"{\"context_note\": \"\", \"sources\": 0, \"errors\": \"specs: clone failed: Cloning into 'DEST'...\\nfatal: unable to access 'https://github.com/acme/specs/': The requested URL returned error: 503 | jira: HTTP 429 on https://acme.atlassian.net \u2014 {\\\"errorMessages\\\":[\\\"Rate limit exceeded.\\\"]}\"}", waitedOut, "outage"},
	{"{\"context_note\": \"\", \"sources\": 0, \"errors\": \"specs: clone failed: Cloning into 'DEST'...\\nfatal: unable to access 'https://github.com/acme/specs/': Failed to connect to github.com port 443 after 318 ms: Couldn't connect to server | jira: HTTP 429 on https://acme.atlassian.net \u2014 {}\"}", waitedOut, "outage"},
	{"{\"verified\": false, \"url\": \"https://docs.acme.org/app/\", \"detail\": \"http 0 after 12 tries\"}", counted, "a poll that gave up in its own words: a miss, the node parks"},
	{"fatal: Unable to create '/w/.git/index.lock': File exists.\n\nAnother git process seems to be running in this repository, e.g.\nan editor opened by 'git commit'. Please make sure all processes\nare terminated then try again. If it still fails, a git process\nmay have crashed in this repository earlier:\nremove the file manually to continue.", counted, "a stale lock"},
	{"scan_health: only 1 of 3 always-on generic scanners produced output (need >= 2). The scanner toolchain failed to run, so this audit cannot be trusted as a clean bill of health. Missing: trivy.json, semgrep-auto.json. Check that the sandbox image ships the scanners (iterion-sandbox-sec) and that egress for semgrep/trivy rule + DB fetches is permitted.", counted, "names no outage: a miss, the node parks"},
	{"Traceback (most recent call last):\n  File \"<string>\", line 79, in <module>\n  File \"/usr/lib/python3.12/subprocess.py\", line 1253, in _check_timeout\n    raise TimeoutExpired(\nsubprocess.TimeoutExpired: Command '['git', 'fetch', 'origin', 'feature-x']' timed out after 180 seconds", waitedOut, "outage: a git fetch that hung"},
	{"ssh: Could not resolve hostname github.com: Name or service not known", waitedOut, "outage"},
	{"curl: (6) Could not resolve host: hooks", waitedOut, "LOSS: the incident's neighbour \u2014 a template rendered an empty host; an unresolvable host keeps the redeliveries whatever its cause"},
	{"error: RPC failed; HTTP 413 curl 22 The requested URL returned error: 413\nsend-pack: unexpected disconnect while reading sideband packet\nfatal: the remote end hung up unexpectedly", waitedOut, "LOSS: git trails every RPC-stage refusal with its hang-up line"},
	{"error: RPC failed; HTTP 403 curl 22 The requested URL returned error: 403\nsend-pack: unexpected disconnect while reading sideband packet\nfatal: the remote end hung up unexpectedly", waitedOut, "LOSS: git trails every RPC-stage refusal with its hang-up line"},
	{"remote: Permission to o/r.git denied to bot.\nfatal: unable to access 'https://github.com/o/r.git/': The requested URL returned error: 403", counted, "4xx at git's info/refs stage, on a line of its own: a decision, not an outage"},
	{"fatal: unable to access 'https://blocked.example/o/r/': CONNECT tunnel failed, response 407", counted, "egress policy: the proxy refused the tunnel"},
	{"curl: (56) CONNECT tunnel failed, response 503", waitedOut, "outage: the proxy itself could not open the tunnel"},
	{"fetched http 503 items, expected 504", waitedOut, "LOSS: outage words echoed as data"},
}

// corpusVerdict is what the engine does about a shell tool node whose command
// exited non-zero printing text: its classification, then the streak's
// eligibility — the two readings that decide a failed command's fate.
func corpusVerdict(t *testing.T, text string) (outageVerdict, bool) {
	t.Helper()
	node := &ir.ToolNode{BaseNode: ir.BaseNode{ID: "gate"}, Command: "make test"}
	failure := shellFailure(t, 1, text)
	code := ClassifyError(failure)
	_, _, isCounted := countedToolFailure(node, code, failure)
	switch {
	case code == ErrCodeNetworkTransient:
		return backedOff, true
	case code != ErrCodeExecutionFailed:
		t.Errorf("%q is classified %s — a tool's output decides neither a provider code nor any other", text, code)
		return 0, false
	case isCounted:
		return counted, true
	}
	return waitedOut, true
}

// Every text of the corpus falls on the side written beside it.
func TestTheOutageCorpusFallsOnItsSides(t *testing.T) {
	sides := map[outageVerdict]int{}
	losses := 0
	seen := map[string]bool{}
	for _, c := range outageCorpus {
		if seen[c.text] {
			t.Errorf("%q is in the corpus twice", c.text)
		}
		seen[c.text] = true
		if c.why == "" {
			t.Errorf("%q carries no reason", c.text)
		}
		got, ok := corpusVerdict(t, c.text)
		if !ok {
			continue
		}
		if got != c.want {
			t.Errorf("%q: %s, want %s (%s)", c.text, got, c.want, c.why)
		}
		sides[c.want]++
		if strings.HasPrefix(c.why, acceptedLoss) {
			losses++
			if c.want != waitedOut {
				t.Errorf("%q is an accepted loss on the %s side: a loss is a deterministic failure the table waits out", c.text, c.want)
			}
		}
	}
	t.Logf("corpus: %d texts — %d waited out (%d of them accepted losses), %d backed off, %d counted",
		len(outageCorpus), sides[waitedOut], losses, sides[backedOff], sides[counted])
	for side, floor := range map[outageVerdict]int{waitedOut: 80, backedOff: 10, counted: 40} {
		if sides[side] < floor {
			t.Errorf("only %d text(s) %s, want at least %d: the corpus lost a side", sides[side], side, floor)
		}
	}
}

// What the corpus must keep saying, whatever is added to it: the failures no
// wait cures stay counted, and the losses stay written as losses.
func TestTheOutageCorpusKeepsItsLandmarks(t *testing.T) {
	find := func(fragment string) (outageVerdict, string, bool) {
		for _, c := range outageCorpus {
			if strings.Contains(c.text, fragment) {
				return c.want, c.why, true
			}
		}
		return 0, "", false
	}
	for _, landmark := range []struct {
		fragment string
		want     outageVerdict
		loss     bool
	}{
		// Counted: the incident, the 4xx each tool reports on a line of its
		// own, lint, the proxy's policy, a certificate, a bare exit.
		{"curl: (3) nested brace in URL", counted, false},
		{"curl: (22) The requested URL returned error: 404", counted, false},
		{"curl: (22) The requested URL returned error: 401", counted, false},
		{"curl: (22) The requested URL returned error: 403", counted, false},
		{"curl: (22) The requested URL returned error: 422", counted, false},
		{"HTTP 404: Not Found (https://api.github.com", counted, false},
		{"HTTP 422: Validation Failed", counted, false},
		{"gh: Resource not accessible by integration (HTTP 403)", counted, false},
		{"HTTPError: HTTP Error 404: Not Found", counted, false},
		{"HTTPError: HTTP Error 403: Forbidden", counted, false},
		{"E501 line too long", counted, false},
		{"npm error code E404", counted, false},
		{"npm ERR! code E401", counted, false},
		{"curl: (56) CONNECT tunnel failed, response 403", counted, false},
		{"curl: (56) CONNECT tunnel failed, response 407", counted, false},
		{"Tunnel connection failed: 403 Forbidden", counted, false},
		{"curl: (60) SSL certificate problem", counted, false},
		// git's 403 at the info/refs stage stands on a line of its own.
		{"remote: Permission to o/r.git denied to bot.", counted, false},
		// The catalog's own hang wordings are outages.
		{"the sink did not answer within its wall clock", waitedOut, false},
		{"the answer outlasted its wall clock", waitedOut, false},
		// The accepted losses.
		{"panic: test timed out after 10m0s", waitedOut, true},
		{"timed out waiting for the condition", waitedOut, true},
		{"subprocess.TimeoutExpired: Command '['npm', 'test']'", waitedOut, true},
		{"got \"503 Service Unavailable\", want \"200 OK\"", waitedOut, true},
		{"curl: (6) Could not resolve host: hooks", waitedOut, true},
		// git trails a refusal at the RPC stage with its hang-up line,
		// whatever the status.
		{"error: RPC failed; HTTP 403 curl 22", waitedOut, true},
		{"error: RPC failed; HTTP 413 curl 22", waitedOut, true},
	} {
		want, why, ok := find(landmark.fragment)
		switch {
		case !ok:
			t.Errorf("the corpus no longer holds a text with %q", landmark.fragment)
		case want != landmark.want || strings.HasPrefix(why, acceptedLoss) != landmark.loss:
			t.Errorf("%q is in the corpus as %s (%s), want %s, accepted loss = %v", landmark.fragment, want, why, landmark.want, landmark.loss)
		}
	}
	// A bare non-zero exit, nothing printed.
	bare := false
	for _, c := range outageCorpus {
		if c.text == "" {
			bare = c.want == counted
		}
	}
	if !bare {
		t.Error("the corpus no longer counts a bare non-zero exit")
	}
}

// Every wording is the reading of at least one corpus text the engine's own
// classification leaves EXECUTION_FAILED and that is no accepted loss: an
// entry nothing witnesses is a guess, one only NETWORK_TRANSIENT texts carry
// decides nothing, and one only a loss carries has no outage to show for it.
func TestEveryTransientOutageWordingHasARealWitness(t *testing.T) {
	for _, wording := range transientOutageWordings {
		if wording != strings.ToLower(wording) {
			t.Errorf("%q is not lowercase: namesTransientOutage lowercases the text it reads, never the table", wording)
		}
		witnessed := false
		for _, c := range outageCorpus {
			if c.want == waitedOut && !strings.HasPrefix(c.why, acceptedLoss) && strings.Contains(strings.ToLower(c.text), wording) {
				witnessed = true
				break
			}
		}
		if !witnessed {
			t.Errorf("no outage text of the corpus carries %q", wording)
		}
	}
}

// A catalog script that arms its own alarm words the hang itself, and no
// tool's phrase is in it: each message a shipped bot raises on its own wall
// clock must be one the table knows, or the bot parks on the very hang the
// alarm was written to survive.
func TestTheCatalogsOwnHangWordingsAreOutages(t *testing.T) {
	raised := regexp.MustCompile(`raise ExchangeTimeout\('([^']+)'\)`)
	bots, err := filepath.Glob(filepath.Join("..", "..", "bots", "*", "*.bot"))
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]string{}
	for _, bot := range bots {
		src, rerr := os.ReadFile(bot)
		if rerr != nil {
			t.Fatal(rerr)
		}
		for _, m := range raised.FindAllStringSubmatch(string(src), -1) {
			found[m[1]] = bot
		}
	}
	if len(found) < 3 {
		t.Fatalf("only %d own-alarm wording(s) found under bots/ (%v): the scan no longer reads what the catalog raises", len(found), found)
	}
	for message, bot := range found {
		if !namesTransientOutage(message) {
			t.Errorf("%s raises %q on its own alarm, and the table does not read it as an outage: a sink that hangs parks the node", bot, message)
		}
	}
}
