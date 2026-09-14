package natsconfig

import (
	"fmt"
	"slices"
	"strings"
	"unicode"
)

// SubjectPermissions models the supported static NATS 2.14.5 configuration
// profile. Omitted AND empty configured allow lists are unrestricted: the
// upstream config parser returns a nil slice for an empty []any. Dynamic
// allow_responses (which can construct a non-nil empty allow list) and queue
// qualified subscription rules must be refused by the profile loader.
type SubjectPermissions struct {
	Allow []string `json:"allow,omitempty"`
	Deny  []string `json:"deny,omitempty"`
}

type subjectPattern struct {
	tokens []string
	tail   bool
}

func parseSubjectPattern(s string) (subjectPattern, error) {
	if len(s) > 4096 {
		return subjectPattern{}, fmt.Errorf("NATS permission exceeds supported subject size")
	}
	parts := strings.Split(s, ".")
	if len(parts) > 64 {
		return subjectPattern{}, fmt.Errorf("NATS permission exceeds supported subject depth")
	}
	for i, part := range parts {
		if part == "" || strings.IndexFunc(part, unicode.IsSpace) >= 0 || strings.IndexByte(part, 0) >= 0 ||
			(part == ">" && i != len(parts)-1) || (part != "*" && part != ">" && strings.ContainsAny(part, "*>")) {
			return subjectPattern{}, fmt.Errorf("NATS permission requires a valid unqualified subject pattern")
		}
	}
	return subjectPattern{tokens: parts, tail: parts[len(parts)-1] == ">"}, nil
}

func (p subjectPattern) step(state int, token string) int {
	if state < 0 {
		return -1
	}
	if state == len(p.tokens) {
		if p.tail {
			return state
		}
		return -1
	}
	if p.tokens[state] == ">" {
		return len(p.tokens)
	}
	if p.tokens[state] == "*" || p.tokens[state] == token {
		return state + 1
	}
	return -1
}

// Intersects finds a concrete subject in protected ∩ allow ∖ deny. It checks
// the entire wildcard language, not a list of sample subjects. Deny rules can
// jointly cover a grant. NATS's trailing > consumes at least one token.
// Exceeding the finite search bound is an error, never proof of exclusion.
func (p SubjectPermissions) Intersects(protected []string) (string, bool, error) {
	return p.intersects(protected, nil)
}

// Extra exclusions are trusted catalog rules, not configured ACL rules.
// They share the deny semantics but cannot consume the profile's 128-rule
// budget for an operator's actual deny list.
func (p SubjectPermissions) intersects(protected, extraExclusions []string) (string, bool, error) {
	if len(p.Allow) > 128 || len(p.Deny) > 128 || len(protected) == 0 || len(protected) > 128 || len(extraExclusions) > 128 {
		return "", false, fmt.Errorf("NATS permissions exceed the supported rule count")
	}
	allow := p.Allow
	if len(allow) == 0 {
		allow = []string{">"}
	}
	patterns := make([]subjectPattern, 0, len(protected)+len(allow)+len(p.Deny)+len(extraExclusions))
	alphabet := make(map[string]bool)
	for _, group := range [][]string{protected, allow, p.Deny, extraExclusions} {
		for _, text := range group {
			pattern, err := parseSubjectPattern(text)
			if err != nil {
				return "", false, err
			}
			patterns = append(patterns, pattern)
			for _, token := range pattern.tokens {
				if token != "*" && token != ">" {
					alphabet[token] = true
				}
			}
		}
	}
	// All unmentioned literals have the same transitions. One representative
	// closes that infinite part of the subject alphabet without sampling it.
	other := "_other"
	for alphabet[other] {
		other += "_"
	}
	alphabet[other] = true
	letters := make([]string, 0, len(alphabet))
	for token := range alphabet {
		letters = append(letters, token)
	}
	slices.Sort(letters)
	type node struct {
		states  []int
		subject string
	}
	key := func(states []int) string {
		bytes := make([]byte, len(states))
		for i, state := range states {
			bytes[i] = byte(state + 1)
		}
		return string(bytes)
	}
	initial := make([]int, len(patterns))
	nodes := []node{{states: initial}}
	seen := map[string]bool{key(initial): true}
	transitions := 0
	for head := 0; head < len(nodes); head++ {
		current := nodes[head]
		for _, token := range letters {
			transitions += len(patterns)
			if transitions > 1_000_000 {
				return "", false, fmt.Errorf("NATS permission-language analysis exceeds supported complexity")
			}
			states := make([]int, len(patterns))
			protectedLive, allowedLive := false, false
			protectedAccept, allowedAccept, denied := false, false, false
			for i, pattern := range patterns {
				state := pattern.step(current.states[i], token)
				states[i] = state
				accepted := state == len(pattern.tokens)
				switch {
				case i < len(protected):
					protectedLive = protectedLive || state >= 0
					protectedAccept = protectedAccept || accepted
				case i < len(protected)+len(allow):
					allowedLive = allowedLive || state >= 0
					allowedAccept = allowedAccept || accepted
				default:
					denied = denied || accepted
				}
			}
			if !protectedLive || !allowedLive {
				continue
			}
			subject := token
			if current.subject != "" {
				subject = current.subject + "." + token
			}
			if protectedAccept && allowedAccept && !denied {
				return subject, true, nil
			}
			k := key(states)
			if seen[k] {
				continue
			}
			if len(seen) >= 10000 {
				return "", false, fmt.Errorf("NATS permission-language analysis exceeds supported complexity")
			}
			seen[k] = true
			nodes = append(nodes, node{states: states, subject: subject})
		}
	}
	return "", false, nil
}

func (p SubjectPermissions) Allows(subject string) (bool, error) {
	if strings.ContainsAny(subject, "*>") {
		return false, fmt.Errorf("NATS permission probe requires a concrete subject")
	}
	_, allowed, err := p.Intersects([]string{subject})
	return allowed, err
}
