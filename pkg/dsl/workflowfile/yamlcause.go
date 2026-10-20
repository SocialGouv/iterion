package workflowfile

import (
	"errors"
	"strconv"
	"strings"

	yamlv2 "go.yaml.in/yaml/v2"
	yamlv3 "gopkg.in/yaml.v3"
)

// The two vendored YAML readers whose words reach an author: the author
// document's own (yaml.v3, decoding into a node tree) and the catalog
// identity's (yaml.v2, decoding into frontmatterFields — DecodeFrontmatter,
// the one reading).
type YAMLReader string

const (
	YAMLv3 YAMLReader = "yaml.v3"
	YAMLv2 YAMLReader = "yaml.v2"
)

// The messages a reader quotes an author's own value back into, spelled as
// the reader's own source builds them: the value stands where the
// placeholder stands and is read by no matcher. A cause names one of these
// skeletons, or a problem string the reader builds whole, never the
// rendered words an author reads.
const (
	YAMLUnmarshalFormat = "cannot unmarshal %s%s into %s"
	YAMLDecodeFormat    = "cannot decode %s `%s` as a %s"
	YAMLMapKeyFormat    = "invalid map key: %#v"
	YAMLUnknownAnchor   = "unknown anchor '%s' referenced"
	YAMLAnchorSelfRef   = "anchor '%s' value contains itself"
	// YAMLDepthFormat is the scanner's one parameterized message: the digit
	// it renders is the reader's own bound, not the author's text.
	YAMLDepthFormat = "exceeded max depth of %d"
)

// YAMLCause is the structured cause of a vendored YAML reader's refusal:
// the facts that tell the cause families apart, read where the reading runs
// and never out of the words the author is shown. The remedy an author reads
// for a refusal is a function of this cause alone.
type YAMLCause struct {
	// Reader is the vendored reader that raised the refusal — the caller's
	// label, taken as given: both readers build the same message shapes, so
	// YAMLCauses cannot verify it against err, and the cause is matched
	// against the remedies of the reader the caller names.
	Reader YAMLReader
	// Message is the reader's own problem: the string its source builds
	// whole, or — where it quotes a value back — the format the value
	// stands in, with the value itself in none of it.
	Message string
	// Target is what the reading was decoding the value into where the
	// message serves several causes and that target is what tells them
	// apart (`cannot unmarshal … into string` against `… into []string`);
	// for the tag-resolving format it is the tag the reader names. ""
	// where the message names its cause alone.
	Target string
	// Line is the line of the text the reading was given that the reader
	// named itself, 0 where it named none. A number the author's own value
	// spells is not here: the reader writes its line at the head of its
	// own message, ahead of anything it quoted back.
	Line int
}

// YAMLCauses reads the structured causes of a refusal a vendored reader
// returned: one per value a TypeError could not read — each its own target
// and its own line — and one for any other refusal. Both readers' type
// errors are read, whichever reader raised err.
func YAMLCauses(reader YAMLReader, err error) []YAMLCause {
	if err == nil {
		return nil
	}
	var e2 *yamlv2.TypeError
	var e3 *yamlv3.TypeError
	if errors.As(err, &e2) {
		return yamlCausesOfElements(reader, e2.Errors)
	}
	if errors.As(err, &e3) {
		return yamlCausesOfElements(reader, e3.Errors)
	}
	return []YAMLCause{yamlCauseOfMessage(reader, err.Error())}
}

// yamlCausesOfElements reads one cause per element the reader collected:
// each element is the reader's own line head, its message and — where it
// quotes the value back — the value and the target it names.
func yamlCausesOfElements(reader YAMLReader, elements []string) []YAMLCause {
	out := make([]YAMLCause, 0, len(elements))
	for _, e := range elements {
		out = append(out, yamlCauseOfMessage(reader, e))
	}
	return out
}

// yamlCauseOfMessage reads one rendered message: the reader's own heads off
// first — the "yaml: " a raised error carries and the "line N: " a reader
// writes ahead of anything it quoted back — then the message's own shape,
// recognized where the reader quotes a value into it and taken whole
// otherwise. A message whose shape is none of the quoted ones is kept as
// the cause's message entire, whether the reader built it whole or quotes
// an author value back in it: no entry is keyed on a shape the reader
// quotes no value into, so whatever the message carries can only stand
// inside a message no entry is keyed on.
func yamlCauseOfMessage(reader YAMLReader, msg string) YAMLCause {
	body := strings.TrimPrefix(msg, "yaml: ")
	c := YAMLCause{Reader: reader}
	if rest, line, ok := cutLineHead(body); ok {
		body, c.Line = rest, line
	}
	switch {
	case strings.HasPrefix(body, "cannot unmarshal "):
		c.Message = YAMLUnmarshalFormat
		// The target stands after the format's own " into ": the value
		// quoted back is before it, and the format writes nothing after
		// the target, so the last " into " is the reader's own.
		if i := strings.LastIndex(body, " into "); i >= 0 {
			c.Target = body[i+len(" into "):]
		}
	case strings.HasPrefix(body, "cannot decode ") && strings.Contains(body, "`"):
		c.Message = YAMLDecodeFormat
		// The tag stands after the format's own " as a ", which the value
		// between the backticks may spell and the reader spells again
		// after it: the last one is the reader's own.
		if i := strings.LastIndex(body, " as a "); i >= 0 {
			c.Target = body[i+len(" as a "):]
		}
	case strings.HasPrefix(body, "invalid map key: "):
		c.Message = YAMLMapKeyFormat
	case strings.HasPrefix(body, "exceeded max depth of ") && hasDigits(body[len("exceeded max depth of "):]):
		c.Message = YAMLDepthFormat
	case strings.HasPrefix(body, "unknown anchor '") && strings.HasSuffix(body, "' referenced"):
		c.Message = YAMLUnknownAnchor
	case strings.HasPrefix(body, "anchor '") && strings.HasSuffix(body, "' value contains itself"):
		c.Message = YAMLAnchorSelfRef
	default:
		c.Message = body
	}
	return c
}

// hasDigits reports whether s starts with a digit run — the tail the depth
// format's own bound renders into.
func hasDigits(s string) bool {
	return len(s) > 0 && s[0] >= '0' && s[0] <= '9'
}

// cutLineHead is msg with the reader's own "line N: " head cut off, and the
// N it names: the one line number the reader writes itself, ahead of any
// value it quotes back, so no value can spell one ahead of it.
func cutLineHead(msg string) (rest string, line int, ok bool) {
	rest, ok = strings.CutPrefix(msg, "line ")
	if !ok {
		return msg, 0, false
	}
	digits := 0
	for digits < len(rest) && rest[digits] >= '0' && rest[digits] <= '9' {
		digits++
	}
	if digits == 0 {
		return msg, 0, false
	}
	n, err := strconv.Atoi(rest[:digits])
	if err != nil || !strings.HasPrefix(rest[digits:], ": ") {
		return msg, 0, false
	}
	return rest[digits+len(": "):], n, true
}
