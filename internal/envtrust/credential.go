package envtrust

import "strings"

// Package-level file: the credential-shape predicate — the one list behind
// every gate that asks "does this NAME read as a credential".
//
// It moved here from pkg/platformcfg (the bot-var gate's name half) when
// the H-10 review measured the two gates drifting: the dotenv loaders
// asked five substrings where the bot gate asked this shape, and twenty
// ITERION_ names sat refused to a bot yet planted by the file — structural
// drift, fixtures only that day. One list, every gate: the bot-var gate
// (pkg/platformcfg botVarNameOK), the dotenv plant (OperatorGradeRefused)
// and the run-record redaction (pkg/store IsSecretEnvName) all read this
// one, so the next credential spelling added answers everywhere the day it
// lands.

// secretNameSubstrings are the spellings that make a name a secret
// wherever they appear — mid-name included (SERVER_TOKEN,
// BOOTSTRAP_ADMIN_PASSWORD). This is pkg/store's redaction list
// (secretValuePatterns), the definition LaunchEnv redacts by.
var secretNameSubstrings = []string{
	"TOKEN", "KEY", "SECRET", "PASSWORD", "CREDENTIAL", "AUTH",
}

// credentialSegments are the credential words too short or too common to
// match inside a name (PAT in PATH) — answered as whole segments.
var credentialSegments = map[string]bool{
	"PAT": true, "PATS": true, "BEARER": true, "BEARERS": true,
	"PASS": true, "PASSWD": true, "PASSPHRASE": true, "PASSCODE": true,
	"PASSFILE": true, "USERPASS": true, "HTPASSWD": true,
	"PW": true, "PWD": true, "CRED": true, "CREDS": true,
	"COOKIE": true, "COOKIES": true, "JWT": true, "JWTS": true,
	"PEM": true, "PEMS": true, "NETRC": true,
	"OTP": true, "OTPS": true, "TOTP": true, "HOTP": true,
	"HMAC": true, "HMACS": true, "SIGNATURE": true,
	"SESSIONID": true, "DSN": true,
	"WEBHOOK": true, "HEADER": true, "HEADERS": true, "PROXY": true,
}

// SecretShapedName reports whether a name carries a secret spelling
// anywhere in it (secretNameSubstrings) — the predicate pkg/store redacts
// run records by (LaunchEnv values become "[redacted]", keys kept for
// audit). No segment exceptions here: the redaction question has none.
func SecretShapedName(name string) bool {
	upper := strings.ToUpper(name)
	for _, s := range secretNameSubstrings {
		if strings.Contains(upper, s) {
			return true
		}
	}
	return false
}

// CredentialShapedName reports whether an environment variable name reads as a
// credential: whatever the engine redacts as secret (SecretShapedName —
// TOKEN, KEY, SECRET, PASSWORD, CREDENTIAL, AUTH anywhere), a PRIVATE part,
// or a credential word too short or too common to match inside a name (PAT
// in PATH) as a whole segment. A webhook URL carries its own bearer, a
// header block carries whatever authenticates the call it rides on, and a
// proxy URL carries its own userinfo. Case-insensitive. The AUTHOR segment
// is removed before the substring ask, so a name about authorship
// (ITERION_GIT_AUTHOR_EMAIL, ITERION_SEC_PATCH_EFFORT_AUTHOR) is not
// refused for the AUTH inside it.
func CredentialShapedName(name string) bool {
	name = strings.ToUpper(name)
	if SecretShapedName(withoutSegment(name, "AUTHOR")) || strings.Contains(name, "PRIVATE") {
		return true
	}
	for _, segment := range strings.Split(name, "_") {
		if credentialSegments[segment] {
			return true
		}
	}
	return false
}

// withoutSegment is name with every `_`-separated segment equal to seg
// removed (and the joins closed up).
func withoutSegment(name, seg string) string {
	kept := make([]string, 0, 4)
	for _, part := range strings.Split(name, "_") {
		if part != seg {
			kept = append(kept, part)
		}
	}
	return strings.Join(kept, "_")
}
