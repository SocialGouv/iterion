package detector

import "regexp"

// secretRules returns the curated set of secret-category rules.
//
// The catalogue is inspired by gitleaks (MIT) — well-known token
// formats whose structure is tight enough to detect with a regex
// alone are scored 1.0. Patterns whose structure overlaps with
// benign strings (bearer/password-style assignments, generic
// high-entropy blocks) combine a regex candidate with an entropy
// post-filter to suppress dictionary-strength false positives.
//
// All regex are RE2-compatible (no backreferences, no lookaround).
// Adversarial inputs cannot trigger catastrophic backtracking by
// construction.
func secretRules() []Rule {
	return []Rule{
		// AWS
		&regexRule{
			name:     "aws_access_key",
			category: "secret",
			score:    1.0,
			re:       regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`),
		},
		&regexRule{
			name:       "aws_secret_key",
			category:   "secret",
			score:      0.95,
			re:         regexp.MustCompile(`(?i)aws(?:.{0,20})?(?:secret|access)?[_\-]?key[_\-]?(?:id)?["'\s:=]+([A-Za-z0-9/+=]{40})\b`),
			matchGroup: 1,
			postFilter: func(match string) bool {
				return shannonEntropy(match) >= 4.5
			},
		},

		// GitHub
		&regexRule{
			name:     "github_pat",
			category: "secret",
			score:    1.0,
			re:       regexp.MustCompile(`\bghp_[A-Za-z0-9]{36}\b`),
		},
		&regexRule{
			name:     "github_oauth",
			category: "secret",
			score:    1.0,
			re:       regexp.MustCompile(`\bgho_[A-Za-z0-9]{36}\b`),
		},
		&regexRule{
			name:     "github_app_token",
			category: "secret",
			score:    1.0,
			re:       regexp.MustCompile(`\b(?:ghu|ghs)_[A-Za-z0-9]{36}\b`),
		},
		&regexRule{
			name:     "github_refresh",
			category: "secret",
			score:    1.0,
			re:       regexp.MustCompile(`\bghr_[A-Za-z0-9]{76}\b`),
		},

		// Slack
		&regexRule{
			name:     "slack_bot_token",
			category: "secret",
			score:    1.0,
			re:       regexp.MustCompile(`\bxoxb-[0-9]{10,}-[0-9]{10,}-[A-Za-z0-9]{20,}\b`),
		},
		&regexRule{
			name:     "slack_user_token",
			category: "secret",
			score:    1.0,
			// Match `xoxp-` followed by any digit/dash/letter run
			// of at least 30 chars. The looser shape (vs the
			// segment-counted form) handles real Slack user tokens
			// whose tail length varies.
			re: regexp.MustCompile(`\bxoxp-[A-Za-z0-9\-]{30,}\b`),
		},
		&regexRule{
			name:     "slack_webhook",
			category: "secret",
			score:    1.0,
			re:       regexp.MustCompile(`https://hooks\.slack\.com/services/T[A-Z0-9]+/B[A-Z0-9]+/[A-Za-z0-9]+`),
		},

		// Stripe
		&regexRule{
			name:     "stripe_live_key",
			category: "secret",
			score:    1.0,
			re:       regexp.MustCompile(`\bsk_live_[0-9a-zA-Z]{24,}\b`),
		},
		&regexRule{
			name:     "stripe_test_key",
			category: "secret",
			score:    0.9,
			re:       regexp.MustCompile(`\bsk_test_[0-9a-zA-Z]{24,}\b`),
		},

		// Google
		&regexRule{
			name:     "google_api_key",
			category: "secret",
			score:    1.0,
			re:       regexp.MustCompile(`\bAIza[0-9A-Za-z_\-]{35}\b`),
		},
		&regexRule{
			name:     "gcp_service_account",
			category: "secret",
			score:    0.95,
			re:       regexp.MustCompile(`"type"\s*:\s*"service_account"`),
		},

		// PEM / SSH
		&regexRule{
			name:     "pem_private_key",
			category: "secret",
			score:    1.0,
			re:       regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`),
		},
		&regexRule{
			name:     "ssh_private_key",
			category: "secret",
			score:    1.0,
			re:       regexp.MustCompile(`-----BEGIN OPENSSH PRIVATE KEY-----`),
		},

		// JWT — three base64url segments separated by dots, the first
		// two start with `eyJ` (the base64 of `{"`).
		&regexRule{
			name:     "jwt",
			category: "secret",
			score:    0.95,
			re:       regexp.MustCompile(`\beyJ[A-Za-z0-9_\-]+\.eyJ[A-Za-z0-9_\-]+\.[A-Za-z0-9_\-]+\b`),
		},

		// Package registry tokens
		&regexRule{
			name:     "npm_token",
			category: "secret",
			score:    1.0,
			re:       regexp.MustCompile(`\bnpm_[A-Za-z0-9]{36}\b`),
		},
		&regexRule{
			name:     "pypi_token",
			category: "secret",
			score:    1.0,
			re:       regexp.MustCompile(`\bpypi-AgEIcHlwaS5vcmc[A-Za-z0-9_\-]+`),
		},

		// Bearer tokens — entropy gate to suppress `Bearer XYZ`-style
		// placeholder text.
		&regexRule{
			name:       "bearer_token_high_entropy",
			category:   "secret",
			score:      0.85,
			re:         regexp.MustCompile(`(?i)bearer\s+([A-Za-z0-9_\-\.]{32,})`),
			matchGroup: 1,
			postFilter: func(match string) bool {
				return shannonEntropy(match) >= 4.0
			},
		},

		// Password / api_key / secret assignments — broad; entropy
		// rejects "changeme", "password", "secret123", etc.
		&regexRule{
			name:       "password_assignment_high_entropy",
			category:   "secret",
			score:      0.7,
			re:         regexp.MustCompile(`(?i)(?:password|secret|api[_\-]?key|access[_\-]?token)\s*[:=]\s*["']?([A-Za-z0-9_\-\.]{8,})["']?`),
			matchGroup: 1,
			postFilter: func(match string) bool {
				return shannonEntropy(match) >= 3.5 && !looksLikeIdentifier(match)
			},
		},

		// Generic high-entropy fallback. Only fires when nothing else
		// has matched (the merge step keeps the highest score and
		// drops overlaps), and the entropy bar is set high so URLs and
		// commit hashes don't trip it.
		&regexRule{
			name:     "generic_high_entropy_string",
			category: "secret",
			score:    0.6,
			re:       regexp.MustCompile(`\b[A-Za-z0-9+/=_\-]{32,}\b`),
			postFilter: func(match string) bool {
				if shannonEntropy(match) < 4.5 {
					return false
				}
				// Heuristic: pure-hex strings of 32/40/64 chars are
				// usually commit hashes / digests, not secrets.
				if isAllHex(match) && (len(match) == 32 || len(match) == 40 || len(match) == 64) {
					return false
				}
				return true
			},
		},
	}
}

// looksLikeIdentifier returns true if s could plausibly be a
// configuration placeholder: only ASCII letters / digits / hyphens
// / underscores AND lacks at least one digit OR one of `_-./`. This
// is a heuristic to skip "MyServiceName" style identifiers that
// pass the entropy bar but aren't secrets.
func looksLikeIdentifier(s string) bool {
	if len(s) == 0 {
		return true
	}
	hasDigit := false
	hasSpecial := false
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
			hasDigit = true
		case r == '_' || r == '-' || r == '.' || r == '/' || r == '+' || r == '=':
			hasSpecial = true
		}
	}
	return !hasDigit && !hasSpecial
}

func isAllHex(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
		case r >= 'a' && r <= 'f':
		case r >= 'A' && r <= 'F':
		default:
			return false
		}
	}
	return true
}
