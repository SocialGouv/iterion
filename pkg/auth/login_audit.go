package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"sort"

	"github.com/SocialGouv/iterion/pkg/identity"
)

// RefusalReason names why a sign-in was refused, for the audit log. The
// sentinel a login path answers with deliberately collapses several
// refusals into one wire message — an unknown account, a wrong password and
// a locked account are all ErrInvalidCredentials, so a client cannot tell
// them apart — but the audit trail, read by platform admins only, needs the
// distinction: a wave of unknown_account rows is an enumeration campaign, a
// wave of bad_password rows on one account is a brute force.
type RefusalReason string

const (
	// ReasonUnknownAccount: no account answers at the presented identifier.
	ReasonUnknownAccount RefusalReason = "unknown_account"
	// ReasonBadPassword: the account exists, the password does not match.
	ReasonBadPassword RefusalReason = "bad_password"
	// ReasonLocked: the brute-force lockout refused the proof unexamined.
	ReasonLocked RefusalReason = "locked"
	// ReasonCorruptPasswordHash: the stored hash is unparseable — an
	// operator problem, logged under the user id by the login path.
	ReasonCorruptPasswordHash RefusalReason = "corrupt_password_hash"
	// ReasonWrongStatus: the path is reserved for accounts in another status
	// (a forced rotation on an account that is not pending one).
	ReasonWrongStatus RefusalReason = "wrong_status"
	// ReasonCredentialReplaced: the credential verified was replaced while
	// it was being checked, so it no longer signs the account in.
	ReasonCredentialReplaced RefusalReason = "credential_replaced"
	// ReasonResetTokenInvalid: the password-reset link is unknown, spent or
	// expired.
	ReasonResetTokenInvalid RefusalReason = "reset_token_invalid"
	// ReasonResetUnavailable: the deployment has no password-reset store,
	// so no link can be redeemed.
	ReasonResetUnavailable RefusalReason = "reset_unavailable"
	// ReasonDisabled: the account is disabled. The same token the server
	// records for a bare ErrAccountDisabled, so one reason names one fact.
	ReasonDisabled RefusalReason = "disabled"
	// ReasonErased: the account has been erased by anonymization and no
	// session may ever open on it again.
	ReasonErased RefusalReason = "erased"
)

// refusalErr wraps a sentinel with the reason the audit log records. Error() is
// the sentinel's own message, so every wire answer built from err.Error()
// stays exactly what the sentinel says; errors.Is reaches the sentinel
// through Unwrap.
type refusalErr struct {
	err    error
	reason RefusalReason
}

func (r refusalErr) Error() string { return r.err.Error() }
func (r refusalErr) Unwrap() error { return r.err }

// refused annotates sentinel err with the reason a sign-in was refused.
func refused(err error, reason RefusalReason) error {
	return refusalErr{err: err, reason: reason}
}

// ReasonOf answers the refusal reason annotated on err, and false when err
// carries none (a bare sentinel, a store failure).
func ReasonOf(err error) (RefusalReason, bool) {
	var r refusalErr
	if errors.As(err, &r) {
		return r.reason, true
	}
	return "", false
}

// auditSubjectLabel separates the audit-digest key from every other key
// derived from the signing key (jwt.go derivedKey).
const auditSubjectLabel = "iterion/audit/subject/v1"

// auditSubjectDigestHex is the length of the digest the audit log keeps: 64
// bits of a peppered HMAC, enough to correlate the attempts of one campaign,
// too short to be anything else.
const auditSubjectDigestHex = 16

// SubjectDigest answers the audit log's stand-in for an identifier a sign-in
// presented (an e-mail address): HMAC-SHA256 under a key derived from the
// active signing key, truncated to 16 hex characters. An e-mail address is
// enumerable, so a bare hash of it — truncated or not — is reversible by
// dictionary; the pepper is what makes the digest useless to anyone who
// does not hold the server's signing key. The subject is normalised the way
// the account store keys it, so every attempt on one address digests alike.
// The digest changes with the active key: a rotation starts a new series,
// which the audit trail tolerates. ok is false when the active key is not
// in the key set.
func (s *Service) SubjectDigest(subject string) (digest string, ok bool) {
	kid := s.signer.ActiveKID()
	if _, ok := s.signer.keys[kid]; !ok {
		return "", false
	}
	return s.subjectDigestUnder(kid, subject), true
}

// SubjectDigests answers subject's SubjectDigest under every key of the
// signing key set, sorted by key id: what an account's erasure looks the
// refusals by address up with (ADR-157). Each digest is the one a sign-in
// presenting subject recorded while that key was active; a key retired from
// the set takes the link from its digests to the address with it.
func (s *Service) SubjectDigests(subject string) []string {
	kids := make([]string, 0, len(s.signer.keys))
	for kid := range s.signer.keys {
		kids = append(kids, kid)
	}
	sort.Strings(kids)
	out := make([]string, 0, len(kids))
	for _, kid := range kids {
		out = append(out, s.subjectDigestUnder(kid, subject))
	}
	return out
}

// subjectDigestUnder is SubjectDigest under the key kid, which the caller
// read from the key set.
func (s *Service) subjectDigestUnder(kid, subject string) string {
	key, _ := s.signer.derivedKey(kid, auditSubjectLabel)
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(identity.NormalizeEmail(subject)))
	return hex.EncodeToString(mac.Sum(nil))[:auditSubjectDigestHex]
}
