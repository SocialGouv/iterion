package runtime

import "strings"

// ---------------------------------------------------------------------------
// Transient outages, as the tools a bot's command calls word them
// ---------------------------------------------------------------------------
//
// An identical repeat is read as permanent (identical_failure.go) because
// nothing between two executions of an unchanged command says the next will
// differ. One family of failures breaks that reasoning: an outage of what the
// command talks to. A resolver that does not answer, a sink that hangs, a
// registry answering 503, a rate limit — each prints the same line two seconds
// later and a different one ten minutes later, which is exactly what the
// queue's redeliveries wait out.
//
// The engine's own network classification reads a command's report — its
// stderr and what exec itself reported, never its stdout — with a subset of
// networkTransientNeedles (error_classify.go); a shell command reports
// through curl, git, ssh, python, gh, npm or kubectl — or through the bot's
// own script — in THEIR words. This table is a list of the wordings known so
// far. A failure whose text carries one is never counted in a streak: it
// ends the node's streak and keeps the EXECUTION_FAILED reading — the retry
// and the redeliveries it always had. It changes no classification and no
// backoff: NETWORK_TRANSIENT stays what the command subset of
// networkTransientNeedles says it is.
//
// It classifies an open alphabet, so it is a heuristic, and both of its
// errors are bounded:
//
//   - a MISS (an outage worded in a way this table does not know) costs what
//     the identical-repeat verdict costs: the node parks TOOL_FAILED_PERMANENT
//     once its streak reaches the limit, and an operator's resume — or the
//     bot's next scheduled run — heals it once the service is back;
//   - a FALSE MATCH (a deterministic failure whose output carries one of
//     these wordings) costs what every such failure cost before the verdict
//     existed: redeliveries up to the queue's delivery bound.
//
// What makes an entry: a lowercase substring that names an outage by a tool's
// OWN phrase — never a bare word a bot's data can carry, and never a wording
// that also names a refusal. A proxy that answers 403 or 407 to a CONNECT is
// applying a policy (iterion's own sandbox proxy answers 403 for a host off
// the allowlist): only the 5xx forms of a tunnel failure are here. A 4xx is a
// decision, a certificate that does not verify is configuration: neither is
// an outage.
//
// transient_outage_test.go holds the corpus this table is judged on — real
// texts, each with the side it must fall on, and the losses the table
// accepts written as such. Add a wording with the text that showed it was
// missing, and the texts that show what it must not catch.
var transientOutageWordings = []string{
	// Name resolution. An unresolvable host reads the same whether the
	// resolver is down or the name is wrong: it keeps the redeliveries
	// whatever its cause.
	"could not resolve host",               // curl (6), git, ssh ("Could not resolve hostname")
	"unable to resolve host",               // wget
	"temporary failure in name resolution", // glibc EAI_AGAIN: ssh, python, pip
	"temporary failure resolving",          // apt
	"name or service not known",            // glibc EAI_NONAME: python
	"getaddrinfo",                          // node: "getaddrinfo EAI_AGAIN <host>", "getaddrinfo ENOTFOUND <host>"
	"eai_again",                            // node, without the call's name

	// Connecting, and a connection lost before the answer was whole.
	"failed to connect to",                       // curl (7), git
	"couldn't connect to server",                 // curl (7)
	"error connecting to",                        // gh: "error connecting to api.github.com"
	"empty reply from server",                    // curl (52)
	"send failure: broken pipe",                  // curl (55)
	"transfer closed with outstanding read data", // curl (18)
	"http2 framing layer",                        // curl (16)
	"http/2 stream",                              // curl (92), git: "HTTP/2 stream 5 was not closed cleanly"
	"rpc failed; curl",                           // git: a transport error inside its smart-HTTP exchange
	"the remote end hung up unexpectedly",        // git
	"early eof",                                  // git
	"remote end closed connection",               // python http.client.RemoteDisconnected
	"incompleteread",                             // python http.client.IncompleteRead
	"socket hang up",                             // node
	"econnreset",                                 // node, npm
	"econnrefused",                               // node, npm
	"kex_exchange_identification",                // ssh: the server dropped the connection at the key exchange
	"error dialing backend",                      // kubectl, through the API server

	// TLS handshakes and reads, and a proxy tunnel the proxy itself could
	// not open (its 5xx; a 403 or 407 there is its policy, and is counted).
	"ssl_connect", // curl (35): "OpenSSL SSL_connect: SSL_ERROR_SYSCALL"
	"ssl_read",    // git, curl: "OpenSSL SSL_read: SSL_ERROR_SYSCALL"
	"tls connection was non-properly terminated", // git over GnuTLS
	"eof occurred in violation of protocol",      // python ssl.SSLEOFError
	"connect tunnel failed, response 5",          // curl (56), git
	"tunnel connection failed: 5",                // python urllib through a proxy

	// Timeouts and hangs, the catalog's own included: a bot that arms its
	// own alarm words the hang itself.
	"timed out",                // curl (28), ssh, python, requests ("Read timed out.")
	"etimedout",                // npm
	"connecttimeouterror",      // node undici
	"client.timeout exceeded",  // Go net/http: "request canceled (Client.Timeout exceeded while awaiting headers)"
	"did not answer within",    // prod-watch's sinks and git, product-docs' git: their own wall clock
	"outlasted its wall clock", // prod-watch's poll lanes

	// HTTP 5xx and 429.
	"url returned error: 5",                  // curl -f, git: "The requested URL returned error: 503"
	"url returned error: 429",                // curl -f
	"http error 5",                           // python urllib: "HTTP Error 503: Service Unavailable"; pip
	"http error 429",                         // python urllib
	"http 500",                               // gh: "HTTP 500: Internal Server Error"
	"http 502",                               // gh; git: "RPC failed; HTTP 502"
	"http 503",                               // gh; a bot's own "HTTP 503 on <url>"
	"http 504",                               // gh
	"http 429",                               // a bot's own "HTTP 429 on <url>"
	"code e50",                               // npm: "npm error code E503"
	"internal server error",                  // git ("remote: Internal Server Error"), requests
	"bad gateway",                            // the status phrase, whatever tool quotes it
	"service unavailable",                    // the status phrase; wget, requests
	"service temporarily unavailable",        // nginx's 503 page
	"gateway time",                           // "Gateway Timeout", nginx's "Gateway Time-out"
	"too many requests",                      // the 429 phrase
	"error from server (serviceunavailable)", // kubectl
	"error from server (internalerror)",      // kubectl
	"error from server (timeout)",            // kubectl

	// Rate limits, in the phrases of the services that apply them.
	"rate limit exceeded",  // gh: "API rate limit exceeded"; Jira
	"secondary rate limit", // gh
	"pull rate limit",      // docker: "toomanyrequests: You have reached your pull rate limit."
}

// namesTransientOutage reports that a failure's text carries one of the
// wordings above. It is the ONE reading of the table.
func namesTransientOutage(text string) bool {
	text = strings.ToLower(text)
	for _, wording := range transientOutageWordings {
		if strings.Contains(text, wording) {
			return true
		}
	}
	return false
}
