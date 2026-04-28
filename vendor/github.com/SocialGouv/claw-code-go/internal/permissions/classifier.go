package permissions

import (
	"context"
	"regexp"
	"strings"
)

// Classifier evaluates a tool invocation under ModeAuto and returns a Decision.
// Implementations may consult static rule sets, a remote classifier service,
// or any other heuristic. They MUST be safe for concurrent use.
type Classifier interface {
	// Classify inspects the tool name and parsed arguments and returns one of:
	//   - DecisionAllow: permit the call without prompting
	//   - DecisionDeny:  block the call without prompting
	//   - DecisionAsk:   defer to the existing prompt path
	//
	// Returning a non-nil error is treated as DecisionAsk by callers.
	Classify(ctx context.Context, toolName string, args map[string]any) (Decision, error)
}

// RuleClassifier is the default Classifier used under ModeAuto when no custom
// classifier is registered on the policy. It implements a conservative static
// rule set: a small allow-list of read-only tools, an optional deny-list of
// regex / path patterns, and DecisionAsk as the catch-all.
//
// The zero value of RuleClassifier is the documented default safe-list:
//   - read_file
//   - glob
//   - grep
//   - web_fetch (only when the "url" arg has scheme "https://")
//
// Callers that want a different policy can construct a RuleClassifier
// explicitly and register it via WithClassifier.
type RuleClassifier struct {
	// AllowTools is a set of tool names that are permitted unconditionally.
	// When nil, the default safe-list is used.
	AllowTools map[string]struct{}
	// HTTPSOnlyTools is a set of tool names that are permitted only when their
	// "url" argument starts with "https://". When nil, defaults to {web_fetch}.
	HTTPSOnlyTools map[string]struct{}
	// DenyTools is a set of tool names that are blocked unconditionally.
	DenyTools map[string]struct{}
	// DenyPatterns is a list of compiled regular expressions; if any pattern
	// matches the extracted permission subject (command, path, url, ...) the
	// call is denied.
	DenyPatterns []*regexp.Regexp
}

// defaultAllowTools is the documented safe-list used when AllowTools is nil.
var defaultAllowTools = map[string]struct{}{
	"read_file": {},
	"glob":      {},
	"grep":      {},
}

// defaultHTTPSOnly is the documented HTTPS-only safe-list used when
// HTTPSOnlyTools is nil.
var defaultHTTPSOnly = map[string]struct{}{
	"web_fetch": {},
}

// NewRuleClassifier returns a RuleClassifier wired with the documented default
// safe-list (read_file, glob, grep, and web_fetch on https://).
func NewRuleClassifier() *RuleClassifier {
	return &RuleClassifier{}
}

// Classify implements the Classifier interface.
func (rc *RuleClassifier) Classify(_ context.Context, toolName string, args map[string]any) (Decision, error) {
	allow := rc.AllowTools
	if allow == nil {
		allow = defaultAllowTools
	}
	httpsOnly := rc.HTTPSOnlyTools
	if httpsOnly == nil {
		httpsOnly = defaultHTTPSOnly
	}

	if _, blocked := rc.DenyTools[toolName]; blocked {
		return DecisionDeny, nil
	}

	if len(rc.DenyPatterns) > 0 {
		subject := subjectFromArgs(args)
		if subject != "" {
			for _, p := range rc.DenyPatterns {
				if p != nil && p.MatchString(subject) {
					return DecisionDeny, nil
				}
			}
		}
	}

	if _, ok := allow[toolName]; ok {
		return DecisionAllow, nil
	}

	if _, ok := httpsOnly[toolName]; ok {
		if url, _ := args["url"].(string); strings.HasPrefix(url, "https://") {
			return DecisionAllow, nil
		}
	}

	return DecisionAsk, nil
}

// subjectFromArgs extracts a permission-relevant string from a parsed args
// map, mirroring extractPermissionSubject's key priority.
func subjectFromArgs(args map[string]any) string {
	if args == nil {
		return ""
	}
	for _, key := range []string{
		"command", "path", "file_path", "filePath",
		"notebook_path", "notebookPath", "url",
		"pattern", "code", "message",
	} {
		if v, ok := args[key]; ok {
			if s, ok := v.(string); ok && s != "" {
				return s
			}
		}
	}
	return ""
}
