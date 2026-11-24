# HTTP failures and partial identity reads

`internal/httpx.WriteError` and `WriteErrorJSON` own public error responses.
A 5xx carries `error: "internal server error"`, a server-generated `request_id`
(also `X-Request-ID`), and `trace_id` when the request has a trace. The cause
goes through `pkg/log`, whose existing `pkg/errtrack` hook reports it. Stable
`error_code` and boolean `retryable` survive; nested diagnostics stay in logs.
4xx refusals keep their actionable payload. Readiness probes use `WriteJSON`:
their operational states are intentional public data, not driver errors.

The server's plain, origin-aware, coded and structured helpers, dispatcher
errors, and plugin failures share that boundary. A response buffer replacing
the network writer must preserve its request/logger binding; webhook workers
bind their own buffer. Keep flush and hijack working through every wrapper.
Correlation IDs must appear in human-readable logs as well as structured
fields: `Logger.WithFields` alone does not print them in human format.

Identity bulk reads decode each Mongo document separately. A completed cursor
returns readable rows plus `*identity.BulkDecodeError`, naming each corrupt
document. Query/cursor errors return no partial result. `/api/auth/me` accepts
only this specific partial result for organizations: a corrupt member org is
`unavailable: true`, with its known membership ID, no asserted org role/name,
and `teams: []`. Missing documents are still absent; a general outage still
fails the whole request. Other bulk consumers may refuse partial reads.

Studio and CLI retain that distinction: unavailable is neither an empty org
nor a restricted account. Org selection is disabled with a visible retry;
healthy siblings remain usable. The credential's team role still selects its
shell, including `config_editor`. CLI JSON keeps the marker, an unavailable org
cannot become the stored default, and slug lookup refuses an incomplete tree
because an unreadable org could hold a duplicate slug; explicit team UUIDs
remain usable and are authorized by the server.

An org memory-quota update answers 5xx if listing its teams or updating any
enforced counter fails. The org row may already be saved; retrying the same
request propagates its desired quota again. A 200 means propagation completed.

Witnesses: `pkg/server/error_chokepoint_test.go`,
`pkg/identity/bulk_decode_test.go` and the real-Mongo bulk suite,
`pkg/cli/remote_identity_partial_test.go`, plus Studio's partial identity and
organization switcher tests. Mutate production code, observe each witness fail
on its assertion, restore identical bytes, then require green.
