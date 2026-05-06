// Package privacy implements two iterion built-in tools that
// detect and redact personally identifiable information (PII):
//
//   - privacy_filter:   detect or redact 5 categories of PII
//     (account_number, email, phone, url, secret) in input text.
//   - privacy_unfilter: substitute placeholder tokens back to
//     their original values via a per-run vault.
//
// The detection backend (subpackage detector/) is pure Go: regular
// expressions plus heuristics (Shannon entropy, Luhn check, mod-97
// IBAN validation). No external model is downloaded; no Python or
// CGO is required. The detector is stateless and safe for
// concurrent use.
//
// Each redact run persists its placeholder→value mapping to a
// per-run vault file at <storeDir>/runs/<runID>/pii_vault.json
// (mode 0600). The vault is opened lazily and survives resumes —
// tokens are deterministic functions of (runID, value, category)
// so re-redacting the same input on a resume yields the same
// placeholder.
//
// Iterion specifically scrubs the persisted event stream (events.jsonl)
// for these two tools: privacy_filter's input "text" field and
// privacy_unfilter's output "text" field are replaced by markers
// before reaching the store hook. The vault file is the only
// place on disk where raw PII lives.
package privacy
