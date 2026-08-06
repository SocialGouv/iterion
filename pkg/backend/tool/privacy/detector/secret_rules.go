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
			name:     "github_fine_grained_pat",
			category: "secret",
			score:    1.0,
			re:       regexp.MustCompile(`github_pat_[A-Za-z0-9]{22}_[A-Za-z0-9]{59}`),
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
		// Anchored on the KEY MATERIAL, not on the `"type": "service_account"`
		// marker. That marker is the string Google's own documentation uses to
		// describe the file format, so at this score it refused every note and
		// masked every tool output that so much as explained what a service
		// account key is — a blocking filter firing on prose about itself.
		// A file carrying the private_key field is the credential; a file
		// naming its own type is not.
		&regexRule{
			name:     "gcp_service_account",
			category: "secret",
			score:    0.95,
			re:       regexp.MustCompile(`"private_key"\s*:\s*"-----BEGIN`),
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
		//
		// Each segment carries a LENGTH FLOOR, because the shape alone is
		// common in prose: `{"alg":"HS256"}` is the shortest header any real
		// token can carry and encodes to exactly 20 characters, while the
		// base64 of a toy object like `{"foo":"bar"}` is 18. Without the
		// floor a one-character-per-segment match was enough, so every
		// tutorial showing two small base64 blobs — and `eyJa.eyJb.c` — was
		// refused as a credential. The signature floor is far under the 43
		// characters HMAC-SHA256 produces.
		&regexRule{
			name:     "jwt",
			category: "secret",
			score:    0.95,
			re:       regexp.MustCompile(`\beyJ[A-Za-z0-9_\-]{17,}\.eyJ[A-Za-z0-9_\-]{17,}\.[A-Za-z0-9_\-]{20,}\b`),
		},

		// LLM provider keys. No leading `\b`: the sub-prefix (`sk-ant-…`,
		// `sk-proj-…`) or the 48-character body is already tight enough to
		// identify, and requiring a word boundary loses a real key wrapped in
		// an ANSI colour sequence — `\x1b[32m` ends in a letter, and agents
		// capture coloured shell output routinely.
		&regexRule{
			name:     "anthropic_api_key",
			category: "secret",
			score:    1.0,
			re:       regexp.MustCompile(`sk-ant-[a-z]{3}[0-9]{2}-[A-Za-z0-9_\-]{24,}`),
		},
		&regexRule{
			name:     "openai_api_key",
			category: "secret",
			score:    1.0,
			re:       regexp.MustCompile(`sk-(?:proj|svcacct|admin)-[A-Za-z0-9_\-]{20,}`),
		},
		// The legacy form has no distinguishing sub-prefix — `sk-` plus 48
		// alphanumerics is also a Kubernetes secret name, a git branch, a cache
		// key, a truncated hash. Shape alone cannot tell them apart, so this one
		// carries the entropy gate the other structurally-ambiguous rules use:
		// a real key is random (~5.9 bits/char), the look-alikes repeat.
		&regexRule{
			name:     "openai_api_key_legacy",
			category: "secret",
			score:    1.0,
			re:       regexp.MustCompile(`sk-[A-Za-z0-9]{48}`),
			postFilter: func(match string) bool {
				return shannonEntropy(match) >= 4.5
			},
		},

		// AWS temporary credentials. Same published shape as AKIA, and it was
		// covered before the catalogue took over this job.
		&regexRule{
			name:     "aws_temp_key",
			category: "secret",
			score:    1.0,
			re:       regexp.MustCompile(`\bASIA[0-9A-Z]{16}\b`),
		},

		// GitLab
		&regexRule{
			name:     "gitlab_pat",
			category: "secret",
			score:    1.0,
			re:       regexp.MustCompile(`\bglpat-[A-Za-z0-9_\-]{20,}`),
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
