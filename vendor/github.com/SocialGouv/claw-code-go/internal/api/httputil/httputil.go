// Package httputil holds tiny formatting helpers shared by every HTTP-based
// provider (openai, foundry, vertex, bedrock-on-HTTP, ...). The helpers are
// deliberately stateless so they can be inlined into provider error paths
// without coupling providers to one another.
package httputil

import (
	"strings"

	"github.com/SocialGouv/claw-code-go/internal/api"
)

// Truncation budgets used across providers. They are exported so callers can
// pick the right budget at the call site instead of repeating the magic
// numbers we historically had inline.
const (
	// BodyTruncateForLog is the budget for the diagnostic body we attach to
	// api.APIError.Body. It needs to be long enough for a developer to spot
	// the offending field in a 4xx envelope, but short enough that we don't
	// dump multi-kilobyte HTML pages into logs when an upstream proxy
	// returns one instead of structured JSON.
	BodyTruncateForLog = 1000

	// BodyTruncateForMessage is the budget for the user-facing fallback
	// message we surface when a provider's error envelope can't be parsed.
	// Shorter than BodyTruncateForLog because this string ends up in
	// terminal output and engine traces.
	BodyTruncateForMessage = 200
)

// TruncateBody clamps body to maxRunes runes, appending an ellipsis when
// truncation actually happened. It counts runes (not bytes) so it stays
// well-behaved when an upstream returns UTF-8 with multibyte characters.
func TruncateBody(body string, maxRunes int) string {
	r := []rune(body)
	if len(r) <= maxRunes {
		return body
	}
	return string(r[:maxRunes]) + "…"
}

// ExtractText concatenates the Text field of every block in blocks with
// newlines. It mirrors how providers flatten Anthropic-style tool_result
// content into a single string for OpenAI-compatible wire formats which
// only carry plain string content for tool messages.
func ExtractText(blocks []api.ContentBlock) string {
	var parts []string
	for _, b := range blocks {
		if b.Text != "" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n")
}
