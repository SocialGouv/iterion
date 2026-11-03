// Package credheader reads ANTHROPIC_CUSTOM_HEADERS-format text — the
// newline-separated "Name: Value" lines a Claude-Code-compatible CLI applies
// LAST on every request it sends, over the headers its own auth set — and
// answers the one question a credential boundary asks of such a block: which
// of its lines authenticate.
//
// One reader for every side of that boundary: the claude_code delegate names
// the ambient Authorization line in an auth failure's remediation
// (spawnAuthorizationHeader), the usage meters key the credential lines the
// request carries (delegate.OverlayCustomHeaderCredentials), and the claw
// sandbox crossing refuses to carry a credential line into a container
// (forwardableProviderEnv). A second description of the format is where they
// drift apart — on a \r line ending, a colon inside a value, the word in a
// value instead of a name — so the walk lives here once and a test pins it
// against the CLI's own parser, claw's api.ParseCustomHeaders, rather than
// against the documentation.
//
// The reading is per LINE, deliberately more lenient than that parser, which
// refuses a whole block holding a line with no name: a line this package
// cannot name is KEPT, so a block the CLI refuses keeps being refused by it —
// the operator's own to fix — instead of being half-repaired here.
package credheader

import (
	"sort"
	"strings"
)

// NameAuthorization is the header a bearer credential travels in. claw sets it
// from an OAuth token or an API bearer on the Anthropic wire
// (internal/api/auth.go, internal/api/client.go), and on its openai, vertex
// and foundry providers.
const NameAuthorization = "authorization"

// NameXAPIKey is the header an API key travels in on the Anthropic wire: claw
// sets it from the key its client is built with (internal/api/auth.go,
// internal/api/client.go).
const NameXAPIKey = "x-api-key"

// credentialNames are the header names a provider's own auth SETS, which an
// ambient line of the same name replaces: http.Header.Set is last-write-wins
// and the custom-header block is applied last, so such a line authenticates
// the request IN PLACE OF the credential the engine resolved for it. A line of
// any other name adds a header beside the auth, which is the documented use of
// the channel (client identity, a tenant header, a gateway's own routing
// header) and stays the operator's.
//
// Read out of the PRODUCER rather than restated from the documentation: claw
// sets x-api-key and Authorization for the Anthropic wire, Authorization for
// openai and vertex, api-key for foundry — and
// TestTheCredentialHeaderNamesCoverClawsOwnAuthHeaders (pkg/backend/model)
// reads those names back out of the vendored CLI, so a provider claw gains
// arrives covered here or red there.
var credentialNames = []string{"api-key", NameAuthorization, NameXAPIKey}

// CredentialNames returns the credential header names, sorted and lower-case.
// For the guard that derives them from the vendored CLI.
func CredentialNames() []string {
	out := make([]string, len(credentialNames))
	copy(out, credentialNames)
	sort.Strings(out)
	return out
}

// IsCredentialName reports that a header name is one a provider's own auth
// sets. Case-insensitive, as HTTP header names and the CLI's own parser are.
func IsCredentialName(name string) bool {
	name = strings.TrimSpace(name)
	for _, candidate := range credentialNames {
		if strings.EqualFold(name, candidate) {
			return true
		}
	}
	return false
}

// CarriesName reports that raw holds a line whose header NAME is name — the
// name alone decides, so a value mentioning the word carries nothing.
func CarriesName(raw, name string) bool {
	for _, line := range strings.Split(raw, "\n") {
		if lineName, ok := headerLineName(line); ok && strings.EqualFold(lineName, strings.TrimSpace(name)) {
			return true
		}
	}
	return false
}

// Line returns the value of the LAST line whose header NAME is name, and
// whether raw holds such a line at all — one with an empty value included.
// The CLI applies the block per line into an http.Header, last write wins, so
// a later line of the same name is the one the request carries — the same
// order that puts the whole block over every route's own auth. An empty value
// is applied too: claw's ParseCustomHeaders keeps the line and
// Identity.Apply sets the header to "", which erases the credential the CLI's
// own auth put under that name. The value runs from the first colon, trimmed
// of the space around it, as that parser reads it.
func Line(raw, name string) (value string, present bool) {
	for _, line := range strings.Split(raw, "\n") {
		lineName, lineValue, found := headerLine(line)
		if !found || !strings.EqualFold(lineName, strings.TrimSpace(name)) {
			continue
		}
		value, present = lineValue, true
	}
	return value, present
}

// LineValue is the credential Line reads: the value the request carries
// under name. ok=false when raw holds no line of that name, or one with an
// empty value — no credential rides it, whatever that line erased.
func LineValue(raw, name string) (value string, ok bool) {
	value, present := Line(raw, name)
	return value, present && value != ""
}

// StripCredentialLines returns raw without the lines whose header name
// authenticates, and the names it refused — sorted, deduplicated, lower-case,
// and the NAMES only: the value of such a line IS the credential, so it never
// travels into a diagnostic.
//
// kept is raw itself, byte for byte, when nothing is refused: the common case
// hands the operator's block on untouched.
func StripCredentialLines(raw string) (kept string, refused []string) {
	lines := strings.Split(raw, "\n")
	keptLines := make([]string, 0, len(lines))
	seen := map[string]bool{}
	for _, line := range lines {
		if name, ok := headerLineName(line); ok && IsCredentialName(name) {
			lowered := strings.ToLower(name)
			if !seen[lowered] {
				seen[lowered] = true
				refused = append(refused, lowered)
			}
			continue
		}
		keptLines = append(keptLines, line)
	}
	if len(refused) == 0 {
		return raw, nil
	}
	sort.Strings(refused)
	return strings.Join(keptLines, "\n"), refused
}

// headerLine reads the "Name: Value" pair a line declares, in the CLI's
// format: the text before the first colon is the name, trimmed of space and
// of a \r line ending; the text after it, trimmed likewise, is the value.
// Reports false for a line with no name — a blank line, a comment, a line
// with no colon — which this package never judges and never drops.
func headerLine(line string) (name, value string, ok bool) {
	rawName, value, found := strings.Cut(strings.TrimSpace(strings.TrimSuffix(line, "\r")), ":")
	if !found {
		return "", "", false
	}
	name = strings.TrimSpace(rawName)
	if name == "" {
		return "", "", false
	}
	return name, strings.TrimSpace(value), true
}

// headerLineName reads the header name a line declares, in the CLI's format.
func headerLineName(line string) (string, bool) {
	name, _, ok := headerLine(line)
	return name, ok
}
